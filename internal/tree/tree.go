// Package tree resolves a directory on disk to the worktree that owns it and
// works out a human-readable label for it.
//
// A worktree path is baton's notion of identity. Claude Code sessions do not
// carry a stable process or session id across tool calls, but each one works in
// exactly one worktree, so the tree path is both stable and self-describing.
package tree

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Tree is a resolved worktree.
type Tree struct {
	// Path is the absolute, symlink-resolved worktree root.
	Path string

	// Label is what shows up in status output — the branch name when there is
	// one, otherwise the directory name.
	Label string

	// Branch is the checked-out branch, empty when HEAD is detached.
	Branch string

	// Main is true when this is the primary clone rather than a linked
	// worktree. Human grabs default to it.
	Main bool
}

// Resolve walks up from startDir looking for the worktree root, which is the
// first directory containing a .git entry. A linked worktree has a .git file
// pointing at the main clone; the main clone has a .git directory.
func Resolve(startDir string) (*Tree, error) {
	if startDir == "" {
		working, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("determine working directory: %w", err)
		}
		startDir = working
	}

	absolute, err := filepath.Abs(startDir)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", startDir, err)
	}
	// Resolve symlinks so that /tmp and /private/tmp, or any other aliased
	// path, cannot register as two different holders of the same directory.
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = resolved
	}

	current := absolute
	for {
		gitPath := filepath.Join(current, ".git")
		info, statErr := os.Stat(gitPath)
		if statErr == nil {
			return describe(current, info.IsDir())
		}

		parent := filepath.Dir(current)
		if parent == current {
			return nil, fmt.Errorf("%s is not inside a git worktree", absolute)
		}
		current = parent
	}
}

func describe(root string, gitIsDirectory bool) (*Tree, error) {
	tree := &Tree{Path: root, Main: gitIsDirectory}

	branch := gitOutput(root, "rev-parse", "--abbrev-ref", "HEAD")
	if branch != "" && branch != "HEAD" {
		tree.Branch = branch
	}

	switch {
	case tree.Branch != "":
		tree.Label = tree.Branch
	default:
		tree.Label = filepath.Base(root)
	}
	return tree, nil
}

// LockfileHash fingerprints the dependency set of a tree. Trees that share a
// hash can share one node_modules, which is the difference between 3 GB per
// worktree and 3 GB per distinct lockfile.
func LockfileHash(root string) (string, error) {
	lockPath := filepath.Join(root, "pnpm-lock.yaml")
	contents, err := os.ReadFile(lockPath)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", lockPath, err)
	}
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:])[:12], nil
}

// Siblings lists the main clone and every linked worktree registered with it.
// The menu bar app and `baton line` use this to show trees that are not
// currently queued.
func Siblings(anyTreeInRepo string) ([]*Tree, error) {
	raw := gitOutput(anyTreeInRepo, "worktree", "list", "--porcelain")
	if raw == "" {
		return nil, fmt.Errorf("no worktrees found from %s", anyTreeInRepo)
	}

	trees := []*Tree{}
	for _, block := range strings.Split(raw, "\n\n") {
		path := ""
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "worktree ") {
				path = strings.TrimPrefix(line, "worktree ")
			}
		}
		if path == "" {
			continue
		}
		resolved, err := Resolve(path)
		if err != nil {
			continue
		}
		trees = append(trees, resolved)
	}
	return trees, nil
}

func gitOutput(dir string, args ...string) string {
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := command.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// Find resolves a worktree from a path or a name.
//
// Typing a full path to hand the baton somewhere is miserable, so a branch name
// or directory name works too. Matching is exact first, then case-insensitive,
// then a unique prefix; anything ambiguous is refused with the candidates
// rather than guessed at, because picking the wrong worktree silently is the
// class of mistake this whole tool exists to prevent.
func Find(repoRoot, query string) (*Tree, error) {
	if query == "" {
		return nil, fmt.Errorf("name a worktree")
	}

	// An actual path wins outright.
	if info, err := os.Stat(query); err == nil && info.IsDir() {
		return Resolve(query)
	}

	candidates, err := Siblings(repoRoot)
	if err != nil {
		return nil, err
	}

	for _, match := range []func(*Tree) bool{
		func(t *Tree) bool { return t.Label == query || t.Branch == query },
		func(t *Tree) bool { return filepath.Base(t.Path) == query },
		func(t *Tree) bool { return strings.EqualFold(t.Label, query) },
		func(t *Tree) bool { return strings.EqualFold(filepath.Base(t.Path), query) },
	} {
		if found := single(candidates, match); found != nil {
			return found, nil
		}
	}

	prefixed := []*Tree{}
	for _, candidate := range candidates {
		if strings.HasPrefix(strings.ToLower(candidate.Label), strings.ToLower(query)) ||
			strings.HasPrefix(strings.ToLower(filepath.Base(candidate.Path)), strings.ToLower(query)) {
			prefixed = append(prefixed, candidate)
		}
	}
	if len(prefixed) == 1 {
		return prefixed[0], nil
	}
	if len(prefixed) > 1 {
		return nil, fmt.Errorf("%q matches %s — be more specific", query, strings.Join(labelsOf(prefixed), ", "))
	}
	return nil, fmt.Errorf("no worktree called %q; known: %s", query, strings.Join(labelsOf(candidates), ", "))
}

func single(candidates []*Tree, match func(*Tree) bool) *Tree {
	var found *Tree
	for _, candidate := range candidates {
		if !match(candidate) {
			continue
		}
		if found != nil {
			return nil // ambiguous, let the next rule try
		}
		found = candidate
	}
	return found
}

func labelsOf(trees []*Tree) []string {
	labels := make([]string, 0, len(trees))
	for _, item := range trees {
		labels = append(labels, item.Label)
	}
	return labels
}
