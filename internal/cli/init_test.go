package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"baton/internal/docker"
	"baton/internal/supervisor"
)

func TestInstallSupervisorLeavesARunningScriptUntouched(t *testing.T) {
	// On a live container this path is the script bash is executing. Bash reads
	// a script incrementally and seeks back to a saved offset, so rewriting it
	// in place drops the supervisor into the middle of a line.
	root := t.TempDir()
	controlDir := filepath.Join(root, docker.ControlDir)
	if err := os.MkdirAll(controlDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(controlDir, "supervisor.sh")

	previous := []byte("#!/bin/bash\n# the version the container is part-way through\n")
	if err := os.WriteFile(path, previous, 0o755); err != nil {
		t.Fatal(err)
	}
	running, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer running.Close()

	if err := installSupervisor(root); err != nil {
		t.Fatalf("installSupervisor: %v", err)
	}

	stillReading, err := io.ReadAll(running)
	if err != nil {
		t.Fatalf("read the already-open handle: %v", err)
	}
	if string(stillReading) != string(previous) {
		t.Error("the open handle saw the replacement; a running supervisor would have jumped into a different script")
	}
	installed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(installed) != string(supervisor.Script) {
		t.Error("the path should hold the new script once the rename lands")
	}
}

func TestInstallSupervisorLeavesTheScriptExecutable(t *testing.T) {
	// CreateTemp makes files 0600 and the container's command runs this
	// directly, so losing the mode would stop it starting at all.
	root := t.TempDir()
	if err := installSupervisor(root); err != nil {
		t.Fatalf("installSupervisor: %v", err)
	}

	info, err := os.Stat(filepath.Join(root, docker.ControlDir, "supervisor.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755", info.Mode().Perm())
	}
}

func TestInstallSupervisorCleansUpAfterItself(t *testing.T) {
	root := t.TempDir()
	if err := installSupervisor(root); err != nil {
		t.Fatalf("installSupervisor: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(root, docker.ControlDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "supervisor.sh.") {
			t.Errorf("left a temporary file behind: %s", entry.Name())
		}
	}
}
