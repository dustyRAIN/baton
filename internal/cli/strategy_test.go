package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"baton/internal/docker"
)

func repoWith(t *testing.T, files ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return root
}

func TestStackIsDetectedFromTheLockfile(t *testing.T) {
	cases := map[string]struct {
		files []string
		want  stackKind
	}{
		"pnpm":      {[]string{"pnpm-lock.yaml", "package.json"}, stackPnpm},
		"yarn":      {[]string{"yarn.lock", "package.json"}, stackYarn},
		"npm":       {[]string{"package-lock.json"}, stackNpm},
		"pip":       {[]string{"requirements.txt", "setup.py"}, stackPip},
		"setuponly": {[]string{"setup.py"}, stackPip},
		"nothing":   {[]string{"README.md"}, stackUnknown},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			if got := detectStack(repoWith(t, testCase.files...)); got != testCase.want {
				t.Errorf("detectStack = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestPnpmWinsOverAStrayYarnLock(t *testing.T) {
	// A repo that migrated package managers can carry both. pnpm is checked
	// first, and installing with the wrong one would produce a tree that does
	// not match the lockfile baton keyed its shared store on.
	root := repoWith(t, "pnpm-lock.yaml", "yarn.lock")

	if got := detectStack(root); got != stackPnpm {
		t.Errorf("detectStack = %q, want %q", got, stackPnpm)
	}
}

func TestMigrationsAreDetected(t *testing.T) {
	if !hasMigrations(repoWith(t, "alembic.ini")) {
		t.Error("alembic.ini means migrations")
	}
	if !hasMigrations(repoWith(t, "manage.py")) {
		t.Error("manage.py means migrations")
	}
	if hasMigrations(repoWith(t, "package.json")) {
		t.Error("a plain node repo has no migrations")
	}
}

func TestStrategyRecordsTheDetectedPort(t *testing.T) {
	container := &docker.Container{
		Name:       "cmp-server",
		CodeRoot:   repoWith(t, "yarn.lock"),
		HealthPort: 7847,
		HealthPath: "/admin/_status",
	}

	strategy := strategyTemplate(stackYarn, container)

	if !strings.Contains(strategy, "BATON_PORT=7847") {
		t.Error("the detected port should be written into the strategy")
	}
	if !strings.Contains(strategy, "BATON_HEALTH_PATH=/admin/_status") {
		t.Error("the detected health path should be written into the strategy")
	}
}

func TestStrategySaysSoWhenThePortIsUnknown(t *testing.T) {
	container := &docker.Container{Name: "mystery", CodeRoot: repoWith(t, "yarn.lock")}

	strategy := strategyTemplate(stackYarn, container)

	if strings.Contains(strategy, "\nBATON_PORT=") {
		t.Error("an undetected port must not be written as if it were real")
	}
	if !strings.Contains(strategy, "not detected") {
		t.Error("the strategy should say the port needs filling in")
	}
}

func TestStrategyIncludesTheMigrationWarningOnlyWhenRelevant(t *testing.T) {
	withMigrations := &docker.Container{Name: "mwr", CodeRoot: repoWith(t, "requirements.txt", "alembic.ini")}
	without := &docker.Container{Name: "client", CodeRoot: repoWith(t, "pnpm-lock.yaml")}

	if !strings.Contains(strategyTemplate(stackPip, withMigrations), "shared between every worktree") {
		t.Error("a repo with migrations needs the shared-database warning")
	}
	if strings.Contains(strategyTemplate(stackPnpm, without), "shared between every worktree") {
		t.Error("a repo without migrations should not carry an irrelevant warning")
	}
}

func TestStrategyDoesNotReportBatonsOwnSupervisorAsTheReplacedCommand(t *testing.T) {
	// After the first init the container's command IS baton's supervisor.
	// Echoing that back tells the reader nothing and hides what was really
	// replaced, so re-running init must stay quiet about it.
	container := &docker.Container{
		Name:     "cmp-client",
		CodeRoot: repoWith(t, "pnpm-lock.yaml"),
		Command:  []string{"/code/.baton/supervisor.sh"},
	}

	if strings.Contains(strategyTemplate(stackPnpm, container), "command baton replaced") {
		t.Error("baton should not report its own supervisor as the command it replaced")
	}
}

func TestStrategyReportsARealReplacedCommand(t *testing.T) {
	container := &docker.Container{
		Name:     "cmp-server",
		CodeRoot: repoWith(t, "yarn.lock"),
		Command:  []string{"/code/docker/runner.sh"},
	}

	strategy := strategyTemplate(stackYarn, container)

	if !strings.Contains(strategy, "/code/docker/runner.sh") {
		t.Error("the original runner should be named, since its dependency waits need porting")
	}
}

func TestWriteStrategyNeverOverwritesAnEditedFile(t *testing.T) {
	root := repoWith(t, "pnpm-lock.yaml")
	controlDir := filepath.Join(root, docker.ControlDir)
	os.MkdirAll(controlDir, 0o755)
	existing := filepath.Join(controlDir, "strategy.sh")
	os.WriteFile(existing, []byte("# hand written, do not clobber\n"), 0o644)

	container := &docker.Container{Name: "cmp-client", CodeRoot: root}
	_, written, err := writeStrategy(container)
	if err != nil {
		t.Fatalf("writeStrategy: %v", err)
	}

	if written {
		t.Error("writeStrategy reported a write over an existing strategy")
	}
	contents, _ := os.ReadFile(existing)
	if string(contents) != "# hand written, do not clobber\n" {
		t.Error("re-running init destroyed a hand-maintained strategy")
	}
}
