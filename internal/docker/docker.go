// Package docker is baton's window onto a running container: what it is, where
// its code comes from, and whether the dev server inside it is answering.
//
// Handoffs deliberately avoid `docker exec`. The container's code directory is
// a bind mount, so baton writes control files on the host and the supervisor
// inside the container picks them up. That keeps the hot path to a file write
// and makes the whole mechanism inspectable with cat.
package docker

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ControlDir is the directory, relative to the code root, where baton keeps the
// files it and the supervisor use to talk to each other.
const ControlDir = ".baton"

const (
	currentTreeFile = "current-tree"
	servingFile     = "serving"
	statusFile      = "status"
	portFile        = "port"
	notesFile       = "notes"
	strategyFile    = "strategy.sh"
)

// healthURL pulls a port and path out of a container healthcheck command, which
// is where a compose file usually records how to tell whether the app is up.
var healthURL = regexp.MustCompile(`https?://[^/\s]*:(\d+)(/\S*)?`)

// Mount is one bind mount attached to a container.
type Mount struct {
	Source      string
	Destination string
}

// Container is the subset of `docker inspect` that baton cares about.
type Container struct {
	Name    string
	Running bool
	Mounts  []Mount

	// CodeRoot is the host directory holding the repository — in other words
	// the main clone. Every worktree baton can switch to lives underneath it.
	CodeRoot string

	// CodeMount is where CodeRoot appears inside the container. Conventionally
	// /code, but projects also use /app or /usr/src/app, so it is detected
	// rather than assumed.
	CodeMount string

	// Ports maps a container port to the host port it is published on.
	Ports map[int]int

	// HealthPort and HealthPath come from the container's own healthcheck,
	// which is the most reliable statement of how to tell whether the app is
	// answering. baton uses them rather than hardcoding anything per stack.
	HealthPort int
	HealthPath string

	// Service is the compose service name, which is not always the container
	// name — a compose file commonly sets container_name to something else.
	// The override has to be keyed by the service or compose reads it as a new
	// service with no image.
	Service string

	// Command is what the container is currently started with. Captured so
	// `baton init` can point at the runner script it is about to replace,
	// which is where any dependency waits and one-time setup steps live.
	Command []string
}

// DevPort is the host port to health-check, or 0 when there is nothing to check.
//
// The supervisor writes the port it actually bound to, which is authoritative
// because a strategy can choose one. Falling back to the healthcheck covers a
// container that has not been supervised yet.
func (container *Container) DevPort() int {
	if inside := readInt(container.ControlPath(portFile)); inside != 0 {
		if host, published := container.Ports[inside]; published {
			return host
		}
	}
	if host, published := container.Ports[container.HealthPort]; published {
		return host
	}
	return 0
}

// Note is something the supervisor wants a human to see.
type Note struct {
	// Level is "info" or "warning". A warning means results collected now may
	// not be trustworthy; info is merely worth knowing.
	Level string `json:"level"`
	Text  string `json:"text"`
}

// Notes reads what the supervisor recorded for the tree it is serving.
func (container *Container) Notes() []Note {
	raw := strings.TrimSpace(readFile(container.ControlPath(notesFile)))
	if raw == "" {
		return nil
	}
	notes := []Note{}
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Split(line, "\t")
		switch len(fields) {
		case 0, 1:
			notes = append(notes, Note{Level: "info", Text: line})
		case 2:
			// Written before notes carried a level.
			notes = append(notes, Note{Level: "info", Text: fields[1]})
		default:
			notes = append(notes, Note{Level: fields[1], Text: fields[2]})
		}
	}
	return notes
}

// HasStrategy reports whether this repo has customised the supervisor's hooks.
func (container *Container) HasStrategy() bool {
	_, err := os.Stat(container.ControlPath(strategyFile))
	return err == nil
}

// Inspect reads a container's current shape. It returns a Container with
// Running false rather than an error when the container exists but is stopped,
// because that is a state baton reports rather than fails on.
func Inspect(name string) (*Container, error) {
	command := exec.Command("docker", "inspect", name)
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("docker inspect %s: %w (is the container created?)", name, err)
	}

	var raw []struct {
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
		Mounts []struct {
			Source      string `json:"Source"`
			Destination string `json:"Destination"`
		} `json:"Mounts"`
		Config struct {
			Cmd         []string          `json:"Cmd"`
			Env         []string          `json:"Env"`
			Labels      map[string]string `json:"Labels"`
			Healthcheck struct {
				Test []string `json:"Test"`
			} `json:"Healthcheck"`
		} `json:"Config"`
		NetworkSettings struct {
			Ports map[string][]struct {
				HostIP   string `json:"HostIp"`
				HostPort string `json:"HostPort"`
			} `json:"Ports"`
		} `json:"NetworkSettings"`
	}
	if err := json.Unmarshal(output, &raw); err != nil {
		return nil, fmt.Errorf("parse docker inspect output for %s: %w", name, err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("docker knows no container named %s", name)
	}
	entry := raw[0]

	container := &Container{Name: name, Running: entry.State.Running, Command: entry.Config.Cmd}
	container.Service = entry.Config.Labels["com.docker.compose.service"]
	if container.Service == "" {
		container.Service = name
	}
	for _, mount := range entry.Mounts {
		container.Mounts = append(container.Mounts, Mount{Source: mount.Source, Destination: mount.Destination})
	}
	container.CodeRoot, container.CodeMount = findCodeMount(container.Mounts)
	if container.CodeRoot == "" {
		return nil, fmt.Errorf("container %s has no bind mount that looks like a git repository, "+
			"so baton cannot tell where its code lives — set BATON_CODE_MOUNT to the path inside "+
			"the container where the repo is mounted", name)
	}

	container.Ports = map[int]int{}
	for portSpec, bindings := range entry.NetworkSettings.Ports {
		if len(bindings) == 0 {
			continue
		}
		var inside, host int
		fmt.Sscanf(portSpec, "%d/", &inside)
		fmt.Sscanf(bindings[0].HostPort, "%d", &host)
		if inside != 0 && host != 0 {
			container.Ports[inside] = host
		}
	}

	// The healthcheck is the container's own statement of how to tell whether
	// it is up, so it beats guessing from the published port list — several
	// ports are usually published and only one of them serves the app.
	if match := healthURL.FindStringSubmatch(strings.Join(entry.Config.Healthcheck.Test, " ")); match != nil {
		fmt.Sscanf(match[1], "%d", &container.HealthPort)
		container.HealthPath = match[2]
	}
	// Not every container declares a healthcheck. The environment is the next
	// best statement of which published port serves the app. BATON_PORT is
	// checked first because someone who sets it has said so deliberately.
	if container.HealthPort == 0 {
		for _, prefix := range []string{"BATON_PORT=", "PORT="} {
			for _, variable := range entry.Config.Env {
				if value, found := strings.CutPrefix(variable, prefix); found {
					fmt.Sscanf(value, "%d", &container.HealthPort)
					break
				}
			}
			if container.HealthPort != 0 {
				break
			}
		}
	}
	if container.HealthPath == "" {
		container.HealthPath = "/"
	}
	return container, nil
}

// findCodeMount works out which bind mount carries the repository.
//
// An explicit BATON_CODE_MOUNT wins. Otherwise the mount whose host side is a
// git repository is the right one, which is more reliable than matching a path
// convention. /code is preferred only to break ties, since that is the most
// common convention and some setups mount the repo twice.
func findCodeMount(mounts []Mount) (hostRoot, containerPath string) {
	wanted := os.Getenv("BATON_CODE_MOUNT")

	var fallbackRoot, fallbackPath string
	for _, mount := range mounts {
		if wanted != "" {
			if mount.Destination == wanted {
				return mount.Source, mount.Destination
			}
			continue
		}
		if _, err := os.Stat(filepath.Join(mount.Source, ".git")); err != nil {
			continue
		}
		if mount.Destination == "/code" {
			return mount.Source, mount.Destination
		}
		if fallbackRoot == "" {
			fallbackRoot, fallbackPath = mount.Source, mount.Destination
		}
	}
	return fallbackRoot, fallbackPath
}

// ControlPath returns the host path of one of baton's control files.
func (container *Container) ControlPath(name string) string {
	return filepath.Join(container.CodeRoot, ControlDir, name)
}

// ContainerPath maps a host path inside the code root to where the container
// sees it. Worktrees under the main clone are already visible to the container
// without any extra mount, which is the whole reason handoffs are cheap.
func (container *Container) ContainerPath(hostPath string) (string, error) {
	absolute, err := filepath.Abs(hostPath)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", hostPath, err)
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = resolved
	}

	codeRoot := container.CodeRoot
	if resolved, err := filepath.EvalSymlinks(codeRoot); err == nil {
		codeRoot = resolved
	}

	relative, err := filepath.Rel(codeRoot, absolute)
	if err != nil || strings.HasPrefix(relative, "..") {
		return "", fmt.Errorf("%s is outside %s, so container %s cannot see it",
			absolute, codeRoot, container.Name)
	}
	if relative == "." {
		return container.CodeMount, nil
	}
	return filepath.ToSlash(filepath.Join(container.CodeMount, relative)), nil
}

// RequestTree asks the supervisor to serve a different worktree by writing the
// path it should switch to. Returns the container-side path that was requested.
func (container *Container) RequestTree(hostTreePath string) (string, error) {
	containerPath, err := container.ContainerPath(hostTreePath)
	if err != nil {
		return "", err
	}
	controlDir := filepath.Join(container.CodeRoot, ControlDir)
	if err := os.MkdirAll(controlDir, 0o755); err != nil {
		return "", fmt.Errorf("create control dir %s: %w", controlDir, err)
	}
	target := container.ControlPath(currentTreeFile)
	if err := os.WriteFile(target, []byte(containerPath+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", target, err)
	}
	return containerPath, nil
}

// Serving reports the container-side path the supervisor says it is currently
// running, and the supervisor's own status word.
func (container *Container) Serving() (tree string, status string) {
	return strings.TrimSpace(readFile(container.ControlPath(servingFile))),
		strings.TrimSpace(readFile(container.ControlPath(statusFile)))
}

// Supervised reports whether the supervisor is installed and has ever run.
func (container *Container) Supervised() bool {
	_, status := container.Serving()
	return status != ""
}

// WaitReady blocks until the supervisor reports it is serving wantTree and the
// dev server answers, or until timeout. The two checks together are what make a
// grant trustworthy: the bookkeeping agrees and the port actually responds.
func (container *Container) WaitReady(wantTree string, timeout time.Duration, poll time.Duration) error {
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	lastStatus := ""

	for time.Now().Before(deadline) {
		serving, status := container.Serving()
		lastStatus = status

		if status == "failed" {
			return fmt.Errorf("supervisor failed to start %s — check `docker logs %s`", wantTree, container.Name)
		}
		if serving == wantTree && status == "ready" {
			// The supervisor already health-checked using the strategy's own
			// definition of healthy. This second check from outside only
			// confirms the port is reachable from the host, and is skipped when
			// nothing is published.
			port := container.DevPort()
			if port == 0 || container.devServerAnswers(port) {
				return nil
			}
		}
		time.Sleep(poll)
	}
	return fmt.Errorf("timed out after %s waiting for %s to serve %s (last status %q)",
		timeout, container.Name, wantTree, lastStatus)
}

// devServerAnswers does a cheap liveness check against the published dev port.
// Any HTTP response counts — we are proving the server is up, not that a
// particular route works.
func (container *Container) devServerAnswers(port int) bool {
	address := net.JoinHostPort("127.0.0.1", fmt.Sprint(port))
	connection, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		return false
	}
	connection.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get("http://" + address + "/")
	if err != nil {
		return false
	}
	defer response.Body.Close()
	return true
}

// Restart bounces the container. This is the slow path, needed only when the
// supervisor is not installed yet or has wedged.
func Restart(name string) error {
	command := exec.Command("docker", "restart", name)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command.Run()
}

// DaemonUp reports whether the Docker daemon is reachable.
func DaemonUp() bool {
	return exec.Command("docker", "version", "--format", "{{.Server.Version}}").Run() == nil
}

func readInt(path string) int {
	value, err := strconv.Atoi(strings.TrimSpace(readFile(path)))
	if err != nil {
		return 0
	}
	return value
}

func readFile(path string) string {
	contents, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(contents)
}
