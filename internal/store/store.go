// Package store holds baton's on-disk state and the file locking that keeps
// concurrent CLI invocations from trampling each other.
//
// There is deliberately no daemon. Every command opens the state file under an
// exclusive flock, mutates it, and writes it back. Contention is tiny — a
// handful of sessions on one laptop — so the simplicity is worth more than the
// throughput a long-running server would buy.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Version is the schema version written into every state file. Bump it when the
// shape changes so old files can be migrated or rejected rather than silently
// misread.
const Version = 1

// HolderKind distinguishes an automated session from a human who has grabbed
// the baton by hand. The difference matters: session holds expire on their own,
// human holds never do.
type HolderKind string

const (
	KindSession HolderKind = "session"
	KindHuman   HolderKind = "human"
)

// Holder is whoever currently owns a container.
type Holder struct {
	Tree  string     `json:"tree"`
	Label string     `json:"label"`
	Kind  HolderKind `json:"kind"`
	Since time.Time  `json:"since"`

	// Expires is when an unrenewed session hold lapses. A zero value means the
	// hold never expires, which is how a human grab pins the container until
	// it is dropped by hand.
	Expires time.Time `json:"expires,omitempty"`

	// Note is free text shown in status output, e.g. why a human took over.
	Note string `json:"note,omitempty"`
}

// Expired reports whether a session hold has lapsed. Human holds never expire.
func (holder *Holder) Expired(now time.Time) bool {
	if holder == nil || holder.Expires.IsZero() {
		return false
	}
	return now.After(holder.Expires)
}

// Pinned reports whether this hold blocks the queue entirely. Human grabs do;
// session holds do not, since they drain normally.
func (holder *Holder) Pinned() bool {
	return holder != nil && holder.Kind == KindHuman
}

// Waiter is a session sitting in line for a container.
type Waiter struct {
	Tree  string    `json:"tree"`
	Label string    `json:"label"`
	Since time.Time `json:"since"`

	// PID is the waiting `baton take --wait` process. A queue entry is only
	// meaningful while that process is alive, so a dead PID is how we detect
	// and reap abandoned waiters.
	PID int `json:"pid"`

	// Lease is how long the hold should last once this waiter is granted it.
	LeaseSeconds int `json:"leaseSeconds"`
}

// Lease returns the hold duration this waiter asked for.
func (waiter Waiter) Lease() time.Duration {
	return time.Duration(waiter.LeaseSeconds) * time.Second
}

// Container is the queue and current owner for one Docker container.
type Container struct {
	Name   string   `json:"name"`
	Holder *Holder  `json:"holder,omitempty"`
	Queue  []Waiter `json:"queue"`

	// Serving is the tree the container was last actually switched to. It can
	// lag Holder.Tree while a swap is in flight, and it is what status output
	// compares against reality to spot drift.
	Serving string `json:"serving,omitempty"`

	// Initialized records that `baton init` has installed the supervisor for
	// this container. The compose override is regenerated from every container
	// carrying this flag, so one file can cover several repos.
	Initialized bool `json:"initialized,omitempty"`

	// CodeRoot is the host directory mounted at /code, remembered from init so
	// the override can be rebuilt without Docker being up.
	CodeRoot string `json:"codeRoot,omitempty"`
}

// State is the whole file.
type State struct {
	Version    int                   `json:"version"`
	Containers map[string]*Container `json:"containers"`
}

// Get returns the container entry, creating it on first use.
func (state *State) Get(name string) *Container {
	if state.Containers == nil {
		state.Containers = map[string]*Container{}
	}
	container, found := state.Containers[name]
	if !found {
		container = &Container{Name: name}
		state.Containers[name] = container
	}
	return container
}

// Names returns every known container name in sorted order.
func (state *State) Names() []string {
	names := make([]string, 0, len(state.Containers))
	for name := range state.Containers {
		names = append(names, name)
	}
	sortStrings(names)
	return names
}

// Store is a handle to the state file and its lock file.
type Store struct {
	dir       string
	statePath string
	lockPath  string
}

// DefaultDir is where baton keeps its state unless BATON_HOME says otherwise.
func DefaultDir() string {
	if override := os.Getenv("BATON_HOME"); override != "" {
		return override
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "baton")
	}
	return filepath.Join(home, ".baton")
}

// Open prepares the state directory and returns a handle. It does not read or
// lock anything yet — that happens per transaction.
func Open(dir string) (*Store, error) {
	if dir == "" {
		dir = DefaultDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create state dir %s: %w", dir, err)
	}
	return &Store{
		dir:       dir,
		statePath: filepath.Join(dir, "state.json"),
		lockPath:  filepath.Join(dir, "state.lock"),
	}, nil
}

// Dir returns the directory holding the state file.
func (store *Store) Dir() string { return store.dir }

// Path returns the state file location.
func (store *Store) Path() string { return store.statePath }

// Read loads the state under a shared lock. Use it for read-only commands so
// several `baton status` calls can run at once without blocking each other.
func (store *Store) Read() (*State, error) {
	lock, err := store.acquire(syscall.LOCK_SH)
	if err != nil {
		return nil, err
	}
	defer lock.release()
	return store.load()
}

// Update runs mutate under an exclusive lock and writes the result back. The
// state passed to mutate is already reaped, so callers never see expired
// holders or dead waiters. If mutate returns an error nothing is written.
func (store *Store) Update(mutate func(*State) error) error {
	lock, err := store.acquire(syscall.LOCK_EX)
	if err != nil {
		return err
	}
	defer lock.release()

	state, err := store.load()
	if err != nil {
		return err
	}
	if err := mutate(state); err != nil {
		return err
	}
	return store.save(state)
}

// load reads and parses the state file, tolerating a missing or empty file by
// returning a fresh state. It assumes the caller already holds the lock.
func (store *Store) load() (*State, error) {
	raw, err := os.ReadFile(store.statePath)
	if errors.Is(err, os.ErrNotExist) || len(raw) == 0 {
		return &State{Version: Version, Containers: map[string]*Container{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", store.statePath, err)
	}

	state := &State{}
	if err := json.Unmarshal(raw, state); err != nil {
		return nil, fmt.Errorf("parse %s: %w (delete it to reset)", store.statePath, err)
	}
	if state.Containers == nil {
		state.Containers = map[string]*Container{}
	}
	if state.Version == 0 {
		state.Version = Version
	}
	if state.Version > Version {
		return nil, fmt.Errorf("state file %s is version %d but this baton understands %d — upgrade baton",
			store.statePath, state.Version, Version)
	}
	Reap(state, time.Now())
	return state, nil
}

// save writes the state atomically: a temp file in the same directory followed
// by a rename, so a crash mid-write cannot leave a truncated state file.
func (store *Store) save(state *State) error {
	state.Version = Version

	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	encoded = append(encoded, '\n')

	temp, err := os.CreateTemp(store.dir, "state-*.json")
	if err != nil {
		return fmt.Errorf("create temp state: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)

	if _, err := temp.Write(encoded); err != nil {
		temp.Close()
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync temp state: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temp state: %w", err)
	}
	if err := os.Rename(tempPath, store.statePath); err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}
	return nil
}

// fileLock is an flock held on a dedicated lock file. We lock a separate file
// rather than the state file itself so the atomic rename in save cannot swap
// the inode out from under a held lock.
type fileLock struct{ file *os.File }

func (store *Store) acquire(mode int) (*fileLock, error) {
	file, err := os.OpenFile(store.lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", store.lockPath, err)
	}
	if err := syscall.Flock(int(file.Fd()), mode); err != nil {
		file.Close()
		return nil, fmt.Errorf("lock %s: %w", store.lockPath, err)
	}
	return &fileLock{file: file}, nil
}

func (lock *fileLock) release() {
	syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	lock.file.Close()
}

// Reap drops holds that have lapsed and waiters whose processes are gone. It
// runs on every load, which is what makes a session that was closed mid-test
// release the baton without anyone having to notice.
func Reap(state *State, now time.Time) {
	for _, container := range state.Containers {
		if container.Holder.Expired(now) {
			container.Holder = nil
		}
		container.Queue = filterWaiters(container.Queue, func(waiter Waiter) bool {
			return processAlive(waiter.PID)
		})
	}
}

func filterWaiters(waiters []Waiter, keep func(Waiter) bool) []Waiter {
	kept := waiters[:0]
	for _, waiter := range waiters {
		if keep(waiter) {
			kept = append(kept, waiter)
		}
	}
	return kept
}

// processAlive reports whether a PID is still running. Signal 0 performs the
// permission and existence checks without actually delivering anything.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	// EPERM means the process exists but belongs to someone else. That should
	// not happen for baton's own waiters, but treating it as alive is the safe
	// reading — we would rather leave a stale entry than evict a live one.
	return errors.Is(err, syscall.EPERM)
}

func sortStrings(values []string) {
	for outer := 1; outer < len(values); outer++ {
		for inner := outer; inner > 0 && values[inner] < values[inner-1]; inner-- {
			values[inner], values[inner-1] = values[inner-1], values[inner]
		}
	}
}
