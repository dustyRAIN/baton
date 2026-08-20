package cli

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"baton/internal/tree"
)

// repoWithAWorktree builds a throwaway repo with one linked worktree and
// returns both roots, already symlink-resolved the way baton stores them.
func repoWithAWorktree(t *testing.T, branch string) (root, worktree string) {
	t.Helper()

	run := func(dir string, args ...string) {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = dir
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
		}
	}

	root = t.TempDir()
	run(root, "init", "-q")
	run(root, "config", "user.email", "t@example.com")
	run(root, "config", "user.name", "t")
	run(root, "commit", "-q", "--allow-empty", "-m", "root")
	run(root, "branch", "-M", "main-branch")
	run(root, "worktree", "add", "-q", "-b", branch, filepath.Join(root, ".worktrees", branch))

	// Paths reach currentLabel by way of tree.Resolve, which resolves symlinks.
	// On macOS a temp dir sits under /var, a symlink to /private/var, so a
	// hand-built path would never match what Resolve returns.
	resolved := func(path string) string {
		found, err := tree.Resolve(path)
		if err != nil {
			t.Fatalf("resolve %s: %v", path, err)
		}
		return found.Path
	}
	return resolved(root), resolved(filepath.Join(root, ".worktrees", branch))
}

func TestCurrentLabelFollowsABranchSwitch(t *testing.T) {
	// The bug this guards: the label is recorded once when the baton is taken,
	// so switching branches afterwards left status naming a branch nobody was
	// on any more.
	_, worktree := repoWithAWorktree(t, "before")

	switchBranch := exec.Command("git", "switch", "-q", "-c", "after")
	switchBranch.Dir = worktree
	if output, err := switchBranch.CombinedOutput(); err != nil {
		t.Fatalf("git switch: %v\n%s", err, output)
	}

	if label := currentLabel(worktree, "before"); label != "after" {
		t.Errorf("label = %q, want after", label)
	}
}

func TestCurrentLabelKeepsTheRecordedNameWhenTheWorktreeIsGone(t *testing.T) {
	// Resolve walks up to the nearest worktree, so a removed path would come
	// back as the main clone. Reporting somebody else's branch as the holder's
	// is worse than reporting a stale one.
	root, worktree := repoWithAWorktree(t, "removed-later")

	remove := exec.Command("git", "worktree", "remove", "--force", worktree)
	remove.Dir = root
	if output, err := remove.CombinedOutput(); err != nil {
		t.Fatalf("git worktree remove: %v\n%s", err, output)
	}

	if label := currentLabel(worktree, "removed-later"); label != "removed-later" {
		t.Errorf("label = %q, want the recorded name back", label)
	}
}

func TestCurrentLabelReadsAnUnchangedWorktree(t *testing.T) {
	_, worktree := repoWithAWorktree(t, "steady")

	if label := currentLabel(worktree, "stale-placeholder"); label != "steady" {
		t.Errorf("label = %q, want steady", label)
	}
}
