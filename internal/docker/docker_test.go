package docker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHealthcheckYieldsThePortAndPath(t *testing.T) {
	// Real healthchecks from the compose file. This is where baton learns which
	// of several published ports actually serves the app, so getting it wrong
	// means health-checking the wrong thing forever.
	cases := map[string]struct {
		command  string
		wantPort string
		wantPath string
	}{
		"cmp-server": {
			"curl --fail http://localhost:7847/admin/_status", "7847", "/admin/_status",
		},
		"marketing-work-request": {
			"curl --fail http://localhost:5008/_status", "5008", "/_status",
		},
		"https": {
			"curl -k --fail https://127.0.0.1:3443/health", "3443", "/health",
		},
		"no path": {
			"curl --fail http://localhost:9000", "9000", "",
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			match := healthURL.FindStringSubmatch(testCase.command)
			if match == nil {
				t.Fatalf("no URL found in %q", testCase.command)
			}
			if match[1] != testCase.wantPort {
				t.Errorf("port = %q, want %q", match[1], testCase.wantPort)
			}
			if match[2] != testCase.wantPath {
				t.Errorf("path = %q, want %q", match[2], testCase.wantPath)
			}
		})
	}
}

func TestHealthcheckWithoutAUrlYieldsNothing(t *testing.T) {
	// A healthcheck that is not an HTTP probe must not produce a bogus port.
	// Falling through to the PORT environment variable is the correct outcome.
	for _, command := range []string{"pg_isready -U postgres", "test -f /tmp/ready", ""} {
		if healthURL.FindStringSubmatch(command) != nil {
			t.Errorf("%q is not an HTTP probe but a URL was matched", command)
		}
	}
}

func TestDevPortPrefersWhatTheSupervisorBound(t *testing.T) {
	// A strategy can choose a port, so what the supervisor actually bound beats
	// the healthcheck's guess. Both are container-side ports that have to be
	// translated to the published host port.
	container := &Container{
		Name:       "cmp-client",
		CodeRoot:   t.TempDir(),
		Ports:      map[int]int{3301: 3301, 8090: 8090},
		HealthPort: 8090,
	}

	// Nothing written yet: fall back to the healthcheck port.
	if got := container.DevPort(); got != 8090 {
		t.Errorf("DevPort = %d, want the healthcheck port 8090", got)
	}

	writeControl(t, container, portFile, "3301\n")
	if got := container.DevPort(); got != 3301 {
		t.Errorf("DevPort = %d, want 3301 from the supervisor", got)
	}
}

func TestDevPortIsZeroWhenNothingIsPublished(t *testing.T) {
	// An unpublished port cannot be checked from the host. Reporting 0 makes
	// the caller skip the check rather than dial a port that is not there.
	container := &Container{
		Name:       "internal",
		CodeRoot:   t.TempDir(),
		Ports:      map[int]int{},
		HealthPort: 5008,
	}

	if got := container.DevPort(); got != 0 {
		t.Errorf("DevPort = %d, want 0", got)
	}
}

func TestNotesAreReadWithoutTheirTimestamps(t *testing.T) {
	container := &Container{Name: "mwr", CodeRoot: t.TempDir()}
	writeControl(t, container, notesFile,
		"05:11:35\tapplied migrations abc -> def. Other worktrees share this database.\n"+
			"05:12:01\tdatabase is ahead of this branch.\n")

	notes := container.Notes()

	if len(notes) != 2 {
		t.Fatalf("notes = %d, want 2", len(notes))
	}
	if notes[0] != "applied migrations abc -> def. Other worktrees share this database." {
		t.Errorf("first note = %q", notes[0])
	}
}

func TestNoNotesFileMeansNoNotes(t *testing.T) {
	container := &Container{Name: "quiet", CodeRoot: t.TempDir()}

	if notes := container.Notes(); notes != nil {
		t.Errorf("notes = %v, want none", notes)
	}
}

func writeControl(t *testing.T, container *Container, name, contents string) {
	t.Helper()
	path := container.ControlPath(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create control dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
