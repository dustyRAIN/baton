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
)

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

	// CodeRoot is the host directory mounted at /code — in other words the
	// main clone. Every worktree baton can switch to lives underneath it.
	CodeRoot string

	// DevPort is the host port the dev server is published on, 0 if none.
	DevPort int
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

	container := &Container{Name: name, Running: entry.State.Running}
	for _, mount := range entry.Mounts {
		container.Mounts = append(container.Mounts, Mount{Source: mount.Source, Destination: mount.Destination})
		if mount.Destination == "/code" {
			container.CodeRoot = mount.Source
		}
	}
	if container.CodeRoot == "" {
		return nil, fmt.Errorf("container %s has no bind mount at /code, so baton cannot tell where its code lives", name)
	}

	for portSpec, bindings := range entry.NetworkSettings.Ports {
		if !strings.HasPrefix(portSpec, "3301/") || len(bindings) == 0 {
			continue
		}
		fmt.Sscanf(bindings[0].HostPort, "%d", &container.DevPort)
	}
	return container, nil
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
		return "/code", nil
	}
	return filepath.ToSlash(filepath.Join("/code", relative)), nil
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
			if container.DevPort == 0 || container.devServerAnswers() {
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
func (container *Container) devServerAnswers() bool {
	address := net.JoinHostPort("127.0.0.1", fmt.Sprint(container.DevPort))
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

func readFile(path string) string {
	contents, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(contents)
}
