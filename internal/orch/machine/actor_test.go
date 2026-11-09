package machine

import (
	"context"
	"fmt"
	"testing"

	"aigate/internal/orch/store"
)

// WithOnTransitionCtx gets the caller's context at every call site (R1):
// cancel, retry, approve, reject, and the worker's own transitions; the old
// WithOnTransition is not called when the ctx variant is set.
func TestOnTransitionCtxGetsCallerContext(t *testing.T) {
	var got []string
	oldCalled := false
	m, s, _ := setup(t, nil,
		WithOnTransition(func(store.Task, State, State, string) { oldCalled = true }),
		WithOnTransitionCtx(func(ctx context.Context, _ store.Task, from, to State, _ string) {
			got = append(got, fmt.Sprintf("%s>%s@%s", from, to, store.ActorFrom(ctx)))
		}))
	bg := context.Background()
	as := func(a string) context.Context { return store.WithActor(bg, a) }

	// cancel
	id := create(t, s)
	if err := m.Cancel(as("session:c"), id); err != nil {
		t.Fatal(err)
	}
	// worker transition: no actor
	id2 := create(t, s)
	if err := m.Tick(bg); err != nil { // queued -> preparing (no step registered for queued)
		t.Fatal(err)
	}
	// preparing has no step: the machine fails the task by itself
	if err := m.Tick(bg); err != nil {
		t.Fatal(err)
	}
	// retry: make id3 an infrastructure failure in preparing first
	id3 := create(t, s)
	if err := s.Transition(bg, id3, StateQueued, StatePreparing, "x"); err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionWith(bg, id3, StatePreparing, StateFailed, "boom",
		store.TransitionOpts{InfraFailed: true, RetryState: StatePreparing, ErrorText: "boom"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Retry(as("token:r"), id3); err != nil {
		t.Fatal(err)
	}
	// approve and reject
	park := func() int64 {
		id := create(t, s)
		if err := s.Transition(bg, id, StateQueued, StateNeedsApproval, "park"); err != nil {
			t.Fatal(err)
		}
		aid, err := s.AddApproval(bg, store.Approval{TaskID: id, Kind: store.KindProtectedModified,
			Hash: fmt.Sprint("h", id), Base: "b", Head: "hd", Reason: "r"})
		if err != nil {
			t.Fatal(err)
		}
		return aid
	}
	if err := m.Approve(as("session:a"), park()); err != nil {
		t.Fatal(err)
	}
	if err := m.Reject(as("socket:uid:7"), park()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"queued>cancelled@session:c",
		"queued>preparing@system",
		"preparing>failed@system",
		"failed>preparing@token:r",
		"needs_approval>verifying@session:a",
		"needs_approval>failed@socket:uid:7",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("callbacks\n got %v\nwant %v", got, want)
	}
	if oldCalled {
		t.Fatal("WithOnTransition called although WithOnTransitionCtx is set")
	}
	_ = id2
}
