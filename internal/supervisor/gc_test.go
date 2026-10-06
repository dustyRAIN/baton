package supervisor

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fixture is a code root laid out like a real one: a main clone, linked
// worktrees git knows about, and a control directory with stores and caches.
type fixture struct {
	t    *testing.T
	code string
	host string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	f := &fixture{t: t, code: t.TempDir(), host: "/Users/someone/repo"}
	f.mkdir(".git/worktrees")
	f.write("pnpm-lock.yaml", "main")
	return f
}

func (f *fixture) mkdir(relative string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Join(f.code, relative), 0o755); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) write(relative, content string) {
	f.t.Helper()
	path := filepath.Join(f.code, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// worktree registers a linked worktree the way git does, recording its
// location as a host path.
func (f *fixture) worktree(name, lockfile string) {
	f.t.Helper()
	f.write(".worktrees/"+name+"/pnpm-lock.yaml", lockfile)
	f.write(".git/worktrees/"+name+"/gitdir", f.host+"/.worktrees/"+name+"/.git\n")
}

func (f *fixture) exists(relative string) bool {
	_, err := os.Lstat(filepath.Join(f.code, relative))
	return err == nil
}

// fingerprint runs the supervisor's own fingerprint hook on a tree.
func (f *fixture) fingerprint(relative string) string {
	f.t.Helper()
	out := f.run(nil, `BATON_TREE="$BATON_CODE/`+relative+`" BATON_STACK=pnpm baton_fingerprint`)
	return strings.TrimSpace(out)
}

// run sources the supervisor's functions, without its boot sequence, and runs
// body against the fixture.
func (f *fixture) run(env []string, body string) string {
	f.t.Helper()
	functions, _, found := bytes.Cut(Script, []byte("# ---------------------------------------------------------------- boot"))
	if !found {
		f.t.Fatal("the supervisor no longer has a boot marker to stop sourcing at")
	}
	library := filepath.Join(f.t.TempDir(), "supervisor-functions.sh")
	if err := os.WriteFile(library, functions, 0o644); err != nil {
		f.t.Fatal(err)
	}

	shims := f.t.TempDir()
	if _, err := exec.LookPath("sha256sum"); err != nil {
		shim := "#!/bin/sh\nexec shasum -a 256 \"$@\"\n"
		if err := os.WriteFile(filepath.Join(shims, "sha256sum"), []byte(shim), 0o755); err != nil {
			f.t.Fatal(err)
		}
	}

	script := ". " + library + "\n" + body + "\nwait\n"
	command := exec.Command("bash", "-c", script)
	command.Env = append(os.Environ(),
		"PATH="+shims+string(os.PathListSeparator)+os.Getenv("PATH"),
		"BATON_CODE="+f.code,
	)
	command.Env = append(command.Env, env...)
	out, err := command.CombinedOutput()
	if err != nil {
		f.t.Fatalf("supervisor functions failed: %v\n%s", err, out)
	}
	return string(out)
}

func (f *fixture) collect(env ...string) string {
	return f.run(env, "collect_garbage")
}

func TestStoresForLockfilesNoTreeHasAreDiscarded(t *testing.T) {
	f := newFixture(t)
	f.worktree("feature", "feature")
	main, feature := f.fingerprint(""), f.fingerprint(".worktrees/feature")

	f.mkdir("node_modules")
	f.mkdir(".baton/store")
	if err := os.Symlink(filepath.Join(f.code, "node_modules"), filepath.Join(f.code, ".baton/store", main)); err != nil {
		t.Fatal(err)
	}
	f.write(".baton/store/"+feature+"/.modules.yaml", "")
	f.write(".baton/store/0123456789ab/.modules.yaml", "")

	f.collect("BATON_HOST_CODE=" + f.host)

	if f.exists(".baton/store/0123456789ab") {
		t.Error("a store for a lockfile no tree has should be discarded")
	}
	if !f.exists(".baton/store/" + main) {
		t.Error("the main clone's adopted store is in use and must be kept")
	}
	if !f.exists(".baton/store/" + feature) {
		t.Error("a store for a live worktree's lockfile must be kept")
	}
	if f.exists(".baton/trash") && entries(t, filepath.Join(f.code, ".baton/trash")) != 0 {
		t.Error("discarded stores should be deleted, not left in the trash")
	}
}

func TestCachesForRemovedTreesAreDiscarded(t *testing.T) {
	f := newFixture(t)
	f.worktree("feature", "main")
	f.write(".baton/cache/main/rspack/x", "")
	f.write(".baton/cache/worktrees_feature/rspack/x", "")
	f.write(".baton/cache/worktrees_merged-last-week/rspack/x", "")

	f.collect("BATON_HOST_CODE=" + f.host)

	if f.exists(".baton/cache/worktrees_merged-last-week") {
		t.Error("the cache of a worktree that no longer exists should be discarded")
	}
	if !f.exists(".baton/cache/main") || !f.exists(".baton/cache/worktrees_feature") {
		t.Error("caches of live trees must be kept")
	}
}

func TestARegisteredWorktreeWhoseDirectoryIsGoneCountsAsRemoved(t *testing.T) {
	// git keeps a worktree's metadata until it is pruned, so a directory
	// deleted by hand still has a gitdir entry.
	f := newFixture(t)
	f.write(".git/worktrees/deleted/gitdir", f.host+"/.worktrees/deleted/.git\n")
	f.write(".baton/cache/worktrees_deleted/rspack/x", "")

	f.collect("BATON_HOST_CODE=" + f.host)

	if f.exists(".baton/cache/worktrees_deleted") {
		t.Error("a worktree whose directory is gone should lose its cache")
	}
}

func TestTheServingStoreIsKeptAfterItsLockfileChanges(t *testing.T) {
	// The running app is using it. Discarding it would pull the dependencies
	// out from under the server.
	f := newFixture(t)
	f.write(".baton/store/0123456789ab/.modules.yaml", "")

	f.run([]string{"BATON_HOST_CODE=" + f.host}, "serving_fingerprint=0123456789ab; collect_garbage")

	if !f.exists(".baton/store/0123456789ab") {
		t.Error("the store the serving tree started with must survive")
	}
}

func TestNothingIsDiscardedWhenWorktreePathsCannotBeMapped(t *testing.T) {
	// Without BATON_HOST_CODE a worktree's host path means nothing inside the
	// container. Treating those trees as gone would delete stores in use.
	f := newFixture(t)
	f.worktree("feature", "feature")
	f.write(".baton/store/0123456789ab/.modules.yaml", "")
	f.write(".baton/cache/worktrees_feature/rspack/x", "")

	out := f.collect()

	if !f.exists(".baton/store/0123456789ab") || !f.exists(".baton/cache/worktrees_feature") {
		t.Error("nothing should be discarded when liveness cannot be established")
	}
	if !strings.Contains(out, "skipping garbage collection") {
		t.Errorf("the skip should be logged, got:\n%s", out)
	}
}

func TestWorktreesOutsideTheCodeRootDoNotBlockCollection(t *testing.T) {
	// The container cannot see them, so they have no store to protect.
	f := newFixture(t)
	f.write(".git/worktrees/elsewhere/gitdir", "/Users/someone/elsewhere/.git\n")
	f.write(".baton/store/0123456789ab/.modules.yaml", "")

	f.collect("BATON_HOST_CODE=" + f.host)

	if f.exists(".baton/store/0123456789ab") {
		t.Error("an unused store should still be discarded")
	}
}

func TestAnAdoptedStoreIsUnlinkedWithoutTouchingItsTarget(t *testing.T) {
	f := newFixture(t)
	f.write("elsewhere/node_modules/react/index.js", "")
	f.mkdir(".baton/store")
	if err := os.Symlink(filepath.Join(f.code, "elsewhere/node_modules"), filepath.Join(f.code, ".baton/store/0123456789ab")); err != nil {
		t.Fatal(err)
	}

	f.collect("BATON_HOST_CODE=" + f.host)

	if f.exists(".baton/store/0123456789ab") {
		t.Error("the stale link should be removed")
	}
	if !f.exists("elsewhere/node_modules/react/index.js") {
		t.Error("removing an adopted store must never delete what it pointed at")
	}
}

func TestCollectionCanBeTurnedOff(t *testing.T) {
	f := newFixture(t)
	f.write(".baton/store/0123456789ab/.modules.yaml", "")

	f.collect("BATON_HOST_CODE="+f.host, "BATON_GC=0")

	if !f.exists(".baton/store/0123456789ab") {
		t.Error("BATON_GC=0 should leave everything in place")
	}
}

func entries(t *testing.T, dir string) int {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(list)
}
