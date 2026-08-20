// Package core holds baton's queue rules. Everything here is a pure function
// over a *store.State, which keeps the interesting logic testable without a
// filesystem, a Docker daemon, or a clock.
package core

import (
	"fmt"
	"time"

	"baton/internal/store"
)

// Outcome says what happened to a request for the baton.
type Outcome string

const (
	// Granted means the caller now holds the baton.
	Granted Outcome = "granted"
	// Queued means the caller is in line and should wait.
	Queued Outcome = "queued"
	// Blocked means a human has pinned the container. The caller stays queued
	// but nothing will move until the human drops it.
	Blocked Outcome = "blocked"
)

// TakeRequest asks for the baton on behalf of one worktree.
type TakeRequest struct {
	Container string
	Tree      string
	Label     string
	Lease     time.Duration
	PID       int
	Now       time.Time

	// Enqueue joins the line when the baton cannot be granted right now. Only
	// a caller that is going to stay alive and keep polling should set it — a
	// queue entry is tied to the calling process, and a one-shot `baton take`
	// that exits immediately would leave an entry nobody is behind.
	Enqueue bool
}

// TakeResult reports where the caller stands after a Take.
type TakeResult struct {
	Outcome Outcome

	// Position is the caller's 1-based place in line when queued. It is 0 when
	// the baton was granted.
	Position int

	// Ahead lists the labels queued in front of the caller, nearest first.
	Ahead []string

	// Holder is whoever owns the container right now, nil if nobody does.
	Holder *store.Holder

	// SwapNeeded is true when the container is not already serving the tree
	// that was just granted the baton, so the caller must perform a swap.
	SwapNeeded bool
}

// Take enqueues the caller if it is not already in line, then grants the baton
// if the caller is at the head and nothing is in the way. It is safe to call
// repeatedly — that is exactly what `baton take --wait` does while polling.
func Take(state *store.State, request TakeRequest) (TakeResult, error) {
	if request.Container == "" {
		return TakeResult{}, fmt.Errorf("container name is required")
	}
	if request.Tree == "" {
		return TakeResult{}, fmt.Errorf("tree path is required")
	}
	now := request.Now
	if now.IsZero() {
		now = time.Now()
	}

	container := state.Get(request.Container)

	// Re-taking a baton the caller already holds just extends the lease. This
	// makes take idempotent, so a session that lost track of its own state can
	// call it again without being sent to the back of the line.
	if container.Holder != nil && container.Holder.Tree == request.Tree && container.Holder.Kind == store.KindSession {
		container.Holder.Expires = now.Add(request.Lease)
		container.Queue = removeTree(container.Queue, request.Tree)
		return TakeResult{
			Outcome:    Granted,
			Holder:     container.Holder,
			SwapNeeded: container.Serving != request.Tree,
		}, nil
	}

	// A one-shot attempt never joins the line. It takes the baton only when
	// nothing at all is in the way, so it cannot cut in front of sessions that
	// are genuinely waiting for their turn.
	if !request.Enqueue {
		if container.Holder != nil || len(container.Queue) > 0 {
			outcome := Queued
			if container.Holder.Pinned() {
				outcome = Blocked
			}
			return TakeResult{
				Outcome: outcome,
				Ahead:   allLabels(container.Queue),
				Holder:  container.Holder,
			}, nil
		}
		container.Holder = &store.Holder{
			Tree:    request.Tree,
			Label:   request.Label,
			Kind:    store.KindSession,
			Since:   now,
			Expires: now.Add(request.Lease),
		}
		return TakeResult{
			Outcome:    Granted,
			Holder:     container.Holder,
			SwapNeeded: container.Serving != request.Tree,
		}, nil
	}

	container.Queue = upsertWaiter(container.Queue, store.Waiter{
		Tree:         request.Tree,
		Label:        request.Label,
		Since:        now,
		PID:          request.PID,
		LeaseSeconds: int(request.Lease / time.Second),
	})

	// A human hold outranks the queue completely. Stay in line, report why.
	if container.Holder.Pinned() {
		return TakeResult{
			Outcome:  Blocked,
			Position: positionOf(container.Queue, request.Tree),
			Ahead:    labelsAhead(container.Queue, request.Tree),
			Holder:   container.Holder,
		}, nil
	}

	atHead := len(container.Queue) > 0 && container.Queue[0].Tree == request.Tree
	if container.Holder != nil || !atHead {
		return TakeResult{
			Outcome:  Queued,
			Position: positionOf(container.Queue, request.Tree),
			Ahead:    labelsAhead(container.Queue, request.Tree),
			Holder:   container.Holder,
		}, nil
	}

	container.Holder = &store.Holder{
		Tree:    request.Tree,
		Label:   request.Label,
		Kind:    store.KindSession,
		Since:   now,
		Expires: now.Add(request.Lease),
	}
	container.Queue = removeTree(container.Queue, request.Tree)

	return TakeResult{
		Outcome:    Granted,
		Holder:     container.Holder,
		SwapNeeded: container.Serving != request.Tree,
	}, nil
}

// Pass releases a baton the given tree holds and takes it out of the queue. It
// is not an error to pass a baton you do not hold — that keeps cleanup paths
// and double-release bugs harmless.
func Pass(state *store.State, containerName, tree string) (released bool) {
	container := state.Get(containerName)
	container.Queue = removeTree(container.Queue, tree)

	if container.Holder == nil || container.Holder.Tree != tree {
		return false
	}
	if container.Holder.Kind == store.KindHuman {
		// A human hold is only cleared by Drop, so that an automated pass can
		// never quietly undo a deliberate takeover.
		return false
	}
	container.Holder = nil
	return true
}

// Grab is the human override. It displaces whatever session holds the baton and
// pins the container until Drop. Waiters stay in line and simply do not move.
func Grab(state *store.State, containerName, tree, label, note string, now time.Time) (displaced *store.Holder) {
	container := state.Get(containerName)
	if container.Holder != nil && container.Holder.Kind == store.KindSession {
		displaced = container.Holder
	}
	container.Holder = &store.Holder{
		Tree:  tree,
		Label: label,
		Kind:  store.KindHuman,
		Since: now,
		Note:  note,
	}
	container.Queue = removeTree(container.Queue, tree)
	return displaced
}

// Drop clears a human hold and lets the queue start moving again.
func Drop(state *store.State, containerName string) (dropped bool) {
	container := state.Get(containerName)
	if !container.Holder.Pinned() {
		return false
	}
	container.Holder = nil
	return true
}

// Renew extends a session hold. It fails if the caller is not the holder, which
// is how a session finds out it was preempted while it was busy.
func Renew(state *store.State, containerName, tree string, lease time.Duration, now time.Time) error {
	container := state.Get(containerName)
	if container.Holder == nil {
		return fmt.Errorf("nobody holds %s", containerName)
	}
	if container.Holder.Tree != tree {
		return fmt.Errorf("%s is held by %s, not %s", containerName, container.Holder.Label, tree)
	}
	if container.Holder.Kind == store.KindHuman {
		return fmt.Errorf("%s is pinned by hand; renew does not apply", containerName)
	}
	container.Holder.Expires = now.Add(lease)
	return nil
}

// Holds reports whether the given tree currently holds the baton. This is the
// question a session must ask after every test batch, because the answer going
// false means its results were produced against somebody else's code.
func Holds(state *store.State, containerName, tree string) bool {
	container := state.Get(containerName)
	return container.Holder != nil &&
		container.Holder.Tree == tree &&
		container.Holder.Kind == store.KindSession
}

// upsertWaiter adds a waiter to the back of the line, or refreshes it in place
// if that tree is already queued. Refreshing in place preserves FIFO order, so
// a session that re-polls does not lose the spot it was waiting for.
func upsertWaiter(queue []store.Waiter, incoming store.Waiter) []store.Waiter {
	for index := range queue {
		if queue[index].Tree == incoming.Tree {
			queue[index].PID = incoming.PID
			queue[index].Label = incoming.Label
			queue[index].LeaseSeconds = incoming.LeaseSeconds
			return queue
		}
	}
	return append(queue, incoming)
}

func removeTree(queue []store.Waiter, tree string) []store.Waiter {
	kept := make([]store.Waiter, 0, len(queue))
	for _, waiter := range queue {
		if waiter.Tree != tree {
			kept = append(kept, waiter)
		}
	}
	return kept
}

func positionOf(queue []store.Waiter, tree string) int {
	for index, waiter := range queue {
		if waiter.Tree == tree {
			return index + 1
		}
	}
	return 0
}

func allLabels(queue []store.Waiter) []string {
	labels := []string{}
	for _, waiter := range queue {
		labels = append(labels, waiter.Label)
	}
	return labels
}

func labelsAhead(queue []store.Waiter, tree string) []string {
	ahead := []string{}
	for _, waiter := range queue {
		if waiter.Tree == tree {
			break
		}
		ahead = append(ahead, waiter.Label)
	}
	return ahead
}

// Give hands the baton to a named worktree as an ordinary session hold.
//
// Distinct from Grab, which pins the container to a human. A hold given here
// behaves like one the session took itself: it expires, it satisfies Holds so
// `baton check` passes, and the queue keeps moving afterwards. Handing the
// container to somebody and simultaneously making their check fail would be a
// trap.
//
// It jumps the queue by design — that is the point of doing it by hand.
func Give(state *store.State, containerName, tree, label string, lease time.Duration, now time.Time) (displaced *store.Holder) {
	container := state.Get(containerName)
	if container.Holder != nil && container.Holder.Tree != tree {
		displaced = container.Holder
	}
	container.Holder = &store.Holder{
		Tree:    tree,
		Label:   label,
		Kind:    store.KindSession,
		Since:   now,
		Expires: now.Add(lease),
	}
	container.Queue = removeTree(container.Queue, tree)
	return displaced
}
