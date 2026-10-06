package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"baton/internal/docker"
)

// servedRepo is a main clone with two linked worktrees, one of which the
// container is serving.
func servedRepo(t *testing.T) *docker.Container {
	t.Helper()
	root := t.TempDir()
	for path, content := range map[string]string{
		".git/HEAD":                    "ref: refs/heads/main\n",
		".worktrees/served/.git":       "gitdir: ../../.git/worktrees/served\n",
		".worktrees/idle/.git":         "gitdir: ../../.git/worktrees/idle\n",
		".worktrees/idle/src/app.ts":   "",
		docker.ControlDir + "/serving": "/code/.worktrees/served\n",
	} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &docker.Container{Name: "cmp-client", CodeRoot: root, CodeMount: "/code"}
}

func TestInstallsIntoATreeNotBeingServedAreBlocked(t *testing.T) {
	// Only the serving tree has the shared store mounted. Anywhere else an
	// install writes gigabytes of private copy that nothing ever cleans up.
	container := servedRepo(t)
	commands := []string{
		"docker exec -w /code/.worktrees/idle cmp-client pnpm install",
		"docker exec --workdir=/code/.worktrees/idle cmp-client pnpm i --frozen-lockfile",
		"docker exec cmp-client sh -c 'cd /code/.worktrees/idle && pnpm install'",
		"docker exec -w /code/.worktrees/idle/src cmp-client npm ci",
		"docker exec -w /code/.worktrees/idle cmp-client pnpm add lodash",
		"docker exec -w /code cmp-client pnpm install",
	}
	for _, command := range commands {
		reason := strayInstall(container, command)
		if reason == "" {
			t.Errorf("%q installs outside the shared store and should be blocked", command)
		} else if !strings.Contains(reason, "baton take cmp-client --wait") {
			t.Errorf("the denial should say what to do instead, got: %s", reason)
		}
	}
}

func TestInstallsIntoTheServingTreeAreAllowed(t *testing.T) {
	container := servedRepo(t)
	command := "docker exec -w /code/.worktrees/served cmp-client pnpm install"
	if reason := strayInstall(container, command); reason != "" {
		t.Errorf("the serving tree's node_modules is the shared store, so %q is fine, got: %s", command, reason)
	}
}

func TestOtherContainerWorkInAnIdleTreeIsNotTreatedAsAnInstall(t *testing.T) {
	container := servedRepo(t)
	commands := []string{
		"docker exec -w /code/.worktrees/idle cmp-client pnpm run lint",
		"docker exec -w /code/.worktrees/idle cmp-client pnpm test --single-run",
		"docker exec -w /code/.worktrees/idle cmp-client npm init -y",
		"docker exec -w /code/.worktrees/idle cmp-client pnpm install-completion",
	}
	for _, command := range commands {
		if reason := strayInstall(container, command); reason != "" {
			t.Errorf("%q does not install dependencies, got: %s", command, reason)
		}
	}
}

func TestInstallsTheGuardCannotPlaceAreAllowed(t *testing.T) {
	// Blocking on a guess would stop legitimate work. Without a directory, or
	// with one outside the code mount, the guard stays out of the way.
	container := servedRepo(t)
	commands := []string{
		"docker exec cmp-client pnpm install",
		"docker exec -w /tmp/scratch cmp-client pnpm install",
	}
	for _, command := range commands {
		if reason := strayInstall(container, command); reason != "" {
			t.Errorf("%q cannot be placed in a tree and should be allowed, got: %s", command, reason)
		}
	}
}

func TestInstallsAreAllowedBeforeTheSupervisorHasServedAnything(t *testing.T) {
	container := servedRepo(t)
	if err := os.Remove(container.ControlPath("serving")); err != nil {
		t.Fatal(err)
	}
	command := "docker exec -w /code/.worktrees/idle cmp-client pnpm install"
	if reason := strayInstall(container, command); reason != "" {
		t.Errorf("with no store shared yet there is nothing to protect, got: %s", reason)
	}
}
