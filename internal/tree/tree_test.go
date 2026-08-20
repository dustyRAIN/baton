package tree

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repoWithWorktrees builds a throwaway repo whose worktree names mirror the
// shapes that actually occur: shared prefixes, mixed case, and a branch name
// that differs from its directory name.
func repoWithWorktrees(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	run := func(dir string, args ...string) {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = dir
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
		}
	}

	run(root, "init", "-q")
	run(root, "config", "user.email", "t@example.com")
	run(root, "config", "user.name", "t")
	run(root, "commit", "-q", "--allow-empty", "-m", "root")

	for _, branch := range []string{"pr-100-review", "pr-1000-review", "Feature-Search"} {
		run(root, "worktree", "add", "-q", "-b", branch, filepath.Join(root, ".worktrees", branch))
	}
	return root
}

func TestFindMatchesAnExactName(t *testing.T) {
	root := repoWithWorktrees(t)

	found, err := Find(root, "pr-100-review")
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if found.Label != "pr-100-review" {
		t.Errorf("label = %q, want pr-100-review", found.Label)
	}
}

func TestExactMatchBeatsALongerNameSharingItsPrefix(t *testing.T) {
	// "pr-100-review" is also a prefix of "pr-1000-review". Treating that as
	// ambiguous would make the shorter name unusable.
	root := repoWithWorktrees(t)

	found, err := Find(root, "pr-100-review")
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if found.Label != "pr-100-review" {
		t.Errorf("label = %q, want the exact match", found.Label)
	}
}

func TestFindIsCaseInsensitive(t *testing.T) {
	root := repoWithWorktrees(t)

	found, err := Find(root, "feature-search")
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if found.Label != "Feature-Search" {
		t.Errorf("label = %q, want Feature-Search", found.Label)
	}
}

func TestAUniquePrefixIsEnough(t *testing.T) {
	root := repoWithWorktrees(t)

	found, err := Find(root, "Feat")
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if found.Label != "Feature-Search" {
		t.Errorf("label = %q, want Feature-Search", found.Label)
	}
}

func TestAnAmbiguousPrefixIsRefusedRatherThanGuessed(t *testing.T) {
	// Silently picking one would point the container at the wrong branch, which
	// is the exact failure baton exists to prevent.
	root := repoWithWorktrees(t)

	_, err := Find(root, "pr-")
	if err == nil {
		t.Fatal("an ambiguous prefix should be refused")
	}
	for _, expected := range []string{"pr-100-review", "pr-1000-review"} {
		if !strings.Contains(err.Error(), expected) {
			t.Errorf("error should list %q as a candidate; got %v", expected, err)
		}
	}
}

func TestAnUnknownNameListsWhatExists(t *testing.T) {
	root := repoWithWorktrees(t)

	_, err := Find(root, "nope")
	if err == nil {
		t.Fatal("an unknown name should fail")
	}
	if !strings.Contains(err.Error(), "pr-100-review") {
		t.Errorf("error should say what is available; got %v", err)
	}
}

func TestAPathStillWins(t *testing.T) {
	root := repoWithWorktrees(t)
	path := filepath.Join(root, ".worktrees", "pr-1000-review")

	found, err := Find(root, path)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if found.Label != "pr-1000-review" {
		t.Errorf("label = %q, want pr-1000-review", found.Label)
	}
}

func TestAnEmptyQueryIsRejected(t *testing.T) {
	if _, err := Find(repoWithWorktrees(t), ""); err == nil {
		t.Error("an empty name should be rejected rather than resolving to anything")
	}
}
