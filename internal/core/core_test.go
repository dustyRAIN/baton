package core

import (
	"testing"
	"time"

	"baton/internal/store"
)

const container = "cmp-client"

func newState() *store.State {
	return &store.State{Version: store.Version, Containers: map[string]*store.Container{}}
}

// take is a waiting caller: it joins the line when it cannot be granted.
func take(t *testing.T, state *store.State, tree string, now time.Time) TakeResult {
	t.Helper()
	return request(t, state, tree, now, true)
}

// tryTake is a one-shot caller that will not wait around.
func tryTake(t *testing.T, state *store.State, tree string, now time.Time) TakeResult {
	t.Helper()
	return request(t, state, tree, now, false)
}

func request(t *testing.T, state *store.State, tree string, now time.Time, enqueue bool) TakeResult {
	t.Helper()
	result, err := Take(state, TakeRequest{
		Container: container,
		Tree:      tree,
		Label:     tree,
		Lease:     20 * time.Minute,
		PID:       1,
		Now:       now,
		Enqueue:   enqueue,
	})
	if err != nil {
		t.Fatalf("Take(%s) returned an error: %v", tree, err)
	}
	return result
}

func TestFirstTakeIsGranted(t *testing.T) {
	state := newState()
	now := time.Now()

	result := take(t, state, "/trees/alpha", now)

	if result.Outcome != Granted {
		t.Fatalf("outcome = %q, want %q", result.Outcome, Granted)
	}
	if !result.SwapNeeded {
		t.Error("SwapNeeded = false, want true: the container is not serving this tree yet")
	}
	if !Holds(state, container, "/trees/alpha") {
		t.Error("alpha should hold the baton")
	}
}

func TestSecondTakeIsQueuedBehindTheFirst(t *testing.T) {
	state := newState()
	now := time.Now()

	take(t, state, "/trees/alpha", now)
	result := take(t, state, "/trees/beta", now)

	if result.Outcome != Queued {
		t.Fatalf("outcome = %q, want %q", result.Outcome, Queued)
	}
	if result.Position != 1 {
		t.Errorf("position = %d, want 1", result.Position)
	}
	if Holds(state, container, "/trees/beta") {
		t.Error("beta must not hold the baton while alpha does")
	}
}

func TestQueueIsFirstComeFirstServed(t *testing.T) {
	state := newState()
	now := time.Now()

	take(t, state, "/trees/alpha", now)
	take(t, state, "/trees/beta", now.Add(time.Second))
	take(t, state, "/trees/gamma", now.Add(2*time.Second))

	// gamma polling again must not let it jump ahead of beta.
	take(t, state, "/trees/gamma", now.Add(3*time.Second))

	Pass(state, container, "/trees/alpha")

	result := take(t, state, "/trees/gamma", now.Add(4*time.Second))
	if result.Outcome != Queued {
		t.Fatalf("gamma outcome = %q, want %q: beta was ahead of it", result.Outcome, Queued)
	}

	result = take(t, state, "/trees/beta", now.Add(5*time.Second))
	if result.Outcome != Granted {
		t.Fatalf("beta outcome = %q, want %q", result.Outcome, Granted)
	}
}

func TestRetakingExtendsTheLeaseWithoutLosingTheBaton(t *testing.T) {
	state := newState()
	now := time.Now()

	take(t, state, "/trees/alpha", now)
	firstExpiry := state.Get(container).Holder.Expires

	result := take(t, state, "/trees/alpha", now.Add(5*time.Minute))

	if result.Outcome != Granted {
		t.Fatalf("outcome = %q, want %q", result.Outcome, Granted)
	}
	if !state.Get(container).Holder.Expires.After(firstExpiry) {
		t.Error("re-taking should push the expiry out")
	}
}

func TestPassGivesTheBatonToTheNextInLine(t *testing.T) {
	state := newState()
	now := time.Now()

	take(t, state, "/trees/alpha", now)
	take(t, state, "/trees/beta", now)

	if !Pass(state, container, "/trees/alpha") {
		t.Fatal("passing a held baton should report it was released")
	}

	result := take(t, state, "/trees/beta", now.Add(time.Second))
	if result.Outcome != Granted {
		t.Fatalf("beta outcome = %q, want %q", result.Outcome, Granted)
	}
}

func TestPassingABatonYouDoNotHoldIsHarmless(t *testing.T) {
	state := newState()
	take(t, state, "/trees/alpha", time.Now())

	if Pass(state, container, "/trees/beta") {
		t.Error("beta does not hold the baton, so Pass should report false")
	}
	if !Holds(state, container, "/trees/alpha") {
		t.Error("alpha should still hold the baton")
	}
}

func TestHumanGrabDisplacesTheHolderAndBlocksTheQueue(t *testing.T) {
	state := newState()
	now := time.Now()

	take(t, state, "/trees/alpha", now)
	take(t, state, "/trees/beta", now)

	displaced := Grab(state, container, "/trees/main", "main", "debugging by hand", now)

	if displaced == nil || displaced.Label != "/trees/alpha" {
		t.Fatalf("displaced = %v, want the alpha holder", displaced)
	}
	if Holds(state, container, "/trees/alpha") {
		t.Error("alpha should have lost the baton")
	}

	result := take(t, state, "/trees/beta", now.Add(time.Second))
	if result.Outcome != Blocked {
		t.Fatalf("beta outcome = %q, want %q while a human holds it", result.Outcome, Blocked)
	}
	if result.Position != 1 {
		t.Errorf("beta position = %d, want 1: it should keep its place in line", result.Position)
	}
}

func TestDropLetsTheQueueMoveAgain(t *testing.T) {
	state := newState()
	now := time.Now()

	take(t, state, "/trees/beta", now)
	Grab(state, container, "/trees/main", "main", "", now)

	if !Drop(state, container) {
		t.Fatal("dropping a hand-held baton should report true")
	}

	result := take(t, state, "/trees/beta", now.Add(time.Second))
	if result.Outcome != Granted {
		t.Fatalf("beta outcome = %q, want %q after the drop", result.Outcome, Granted)
	}
}

func TestAutomatedPassCannotUndoAHumanGrab(t *testing.T) {
	state := newState()
	now := time.Now()

	Grab(state, container, "/trees/main", "main", "", now)

	if Pass(state, container, "/trees/main") {
		t.Error("Pass must not release a hand-held baton; only Drop does")
	}
	if !state.Get(container).Holder.Pinned() {
		t.Error("the container should still be pinned")
	}
}

func TestRenewFailsForANonHolder(t *testing.T) {
	state := newState()
	now := time.Now()
	take(t, state, "/trees/alpha", now)

	if err := Renew(state, container, "/trees/beta", time.Minute, now); err == nil {
		t.Error("renewing a baton you do not hold should fail — that is how a session learns it was preempted")
	}
}

func TestHoldsIsFalseForAHumanHold(t *testing.T) {
	state := newState()
	now := time.Now()

	Grab(state, container, "/trees/main", "main", "", now)

	if Holds(state, container, "/trees/main") {
		t.Error("Holds reports session ownership; a human grab is not a session hold")
	}
}

func TestOneShotTakeDoesNotJoinTheLine(t *testing.T) {
	state := newState()
	now := time.Now()

	take(t, state, "/trees/alpha", now)
	result := tryTake(t, state, "/trees/beta", now)

	if result.Outcome != Queued {
		t.Fatalf("outcome = %q, want %q", result.Outcome, Queued)
	}
	if result.Position != 0 {
		t.Errorf("position = %d, want 0: a one-shot attempt holds no place in line", result.Position)
	}
	if len(state.Get(container).Queue) != 0 {
		t.Error("a one-shot attempt must leave no queue entry behind for nobody to occupy")
	}
}

func TestOneShotTakeWillNotCutInFrontOfWaiters(t *testing.T) {
	state := newState()
	now := time.Now()

	take(t, state, "/trees/alpha", now)
	take(t, state, "/trees/beta", now.Add(time.Second))
	Pass(state, container, "/trees/alpha")

	// beta is waiting its turn. A drive-by attempt must not steal it.
	result := tryTake(t, state, "/trees/gamma", now.Add(2*time.Second))

	if result.Outcome == Granted {
		t.Error("a one-shot take jumped a queue that had somebody waiting in it")
	}
	if Holds(state, container, "/trees/gamma") {
		t.Error("gamma should not hold the baton")
	}
}

func TestOneShotTakeSucceedsWhenNothingIsInTheWay(t *testing.T) {
	state := newState()

	result := tryTake(t, state, "/trees/alpha", time.Now())

	if result.Outcome != Granted {
		t.Fatalf("outcome = %q, want %q on a completely free container", result.Outcome, Granted)
	}
	if !Holds(state, container, "/trees/alpha") {
		t.Error("alpha should hold the baton")
	}
}

func TestSwapIsSkippedWhenAlreadyServingTheTree(t *testing.T) {
	state := newState()
	state.Get(container).Serving = "/trees/alpha"

	result := take(t, state, "/trees/alpha", time.Now())

	if result.SwapNeeded {
		t.Error("SwapNeeded = true, want false: the container is already serving this tree")
	}
}
