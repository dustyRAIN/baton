package store

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open returned an error: %v", err)
	}
	return store
}

func TestReadingBeforeAnythingIsWrittenGivesAnEmptyState(t *testing.T) {
	state, err := newStore(t).Read()
	if err != nil {
		t.Fatalf("Read returned an error: %v", err)
	}
	if len(state.Containers) != 0 {
		t.Errorf("containers = %d, want 0", len(state.Containers))
	}
}

func TestUpdatePersistsAcrossHandles(t *testing.T) {
	dir := t.TempDir()
	first, _ := Open(dir)

	err := first.Update(func(state *State) error {
		state.Get("cmp-client").Serving = "/trees/alpha"
		return nil
	})
	if err != nil {
		t.Fatalf("Update returned an error: %v", err)
	}

	second, _ := Open(dir)
	state, err := second.Read()
	if err != nil {
		t.Fatalf("Read returned an error: %v", err)
	}
	if got := state.Get("cmp-client").Serving; got != "/trees/alpha" {
		t.Errorf("serving = %q, want %q", got, "/trees/alpha")
	}
}

func TestUpdateWritesNothingWhenTheCallbackFails(t *testing.T) {
	store := newStore(t)

	store.Update(func(state *State) error {
		state.Get("cmp-client").Serving = "/trees/alpha"
		return nil
	})
	store.Update(func(state *State) error {
		state.Get("cmp-client").Serving = "/trees/beta"
		return os.ErrInvalid
	})

	state, _ := store.Read()
	if got := state.Get("cmp-client").Serving; got != "/trees/alpha" {
		t.Errorf("serving = %q, want the value from before the failed update", got)
	}
}

func TestConcurrentUpdatesDoNotLoseWrites(t *testing.T) {
	dir := t.TempDir()
	const writers = 12

	group := &sync.WaitGroup{}
	group.Add(writers)
	for index := 0; index < writers; index++ {
		go func(number int) {
			defer group.Done()
			store, err := Open(dir)
			if err != nil {
				return
			}
			store.Update(func(state *State) error {
				container := state.Get("cmp-client")
				container.Queue = append(container.Queue, Waiter{
					Tree:  filepath.Join("/trees", string(rune('a'+number))),
					Label: "w",
					PID:   os.Getpid(),
					Since: time.Now(),
				})
				return nil
			})
		}(index)
	}
	group.Wait()

	state, err := (&Store{}).openAt(dir)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if got := len(state.Get("cmp-client").Queue); got != writers {
		t.Errorf("queue length = %d, want %d — a write was lost under the lock", got, writers)
	}
}

// openAt is a test helper that reads a state directory without going through
// the package-level default.
func (store *Store) openAt(dir string) (*State, error) {
	opened, err := Open(dir)
	if err != nil {
		return nil, err
	}
	return opened.Read()
}

func TestExpiredSessionHoldsAreReaped(t *testing.T) {
	state := &State{Containers: map[string]*Container{}}
	container := state.Get("cmp-client")
	container.Holder = &Holder{
		Tree:    "/trees/alpha",
		Kind:    KindSession,
		Since:   time.Now().Add(-time.Hour),
		Expires: time.Now().Add(-time.Minute),
	}

	Reap(state, time.Now())

	if container.Holder != nil {
		t.Error("a lapsed session hold should be reaped so a closed session cannot block the queue")
	}
}

func TestHumanHoldsNeverExpire(t *testing.T) {
	state := &State{Containers: map[string]*Container{}}
	container := state.Get("cmp-client")
	container.Holder = &Holder{
		Tree:  "/trees/main",
		Kind:  KindHuman,
		Since: time.Now().Add(-24 * time.Hour),
	}

	Reap(state, time.Now())

	if container.Holder == nil {
		t.Error("a hand-held baton must survive reaping until it is dropped")
	}
}

func TestWaitersWithDeadProcessesAreReaped(t *testing.T) {
	state := &State{Containers: map[string]*Container{}}
	container := state.Get("cmp-client")
	container.Queue = []Waiter{
		{Tree: "/trees/alive", PID: os.Getpid()},
		// PID 0 is never a live user process, so this entry stands in for a
		// session that was closed while waiting.
		{Tree: "/trees/dead", PID: 0},
	}

	Reap(state, time.Now())

	if len(container.Queue) != 1 {
		t.Fatalf("queue length = %d, want 1", len(container.Queue))
	}
	if container.Queue[0].Tree != "/trees/alive" {
		t.Errorf("surviving waiter = %q, want the live one", container.Queue[0].Tree)
	}
}

func TestAFutureSchemaVersionIsRefused(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	os.WriteFile(statePath, []byte(`{"version":9999,"containers":{}}`), 0o644)

	store, _ := Open(dir)
	if _, err := store.Read(); err == nil {
		t.Error("reading a newer schema should fail loudly rather than silently misinterpret it")
	}
}
