package machine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"aigate/internal/orch/store"
)

type fakeStep struct {
	name State
	run  func(ctx context.Context, t store.Task) (Outcome, error)
}

func (f fakeStep) Name() State { return f.name }
func (f fakeStep) Run(ctx context.Context, t store.Task) (Outcome, error) {
	return f.run(ctx, t)
}

func next(to State) func(context.Context, store.Task) (Outcome, error) {
	return func(context.Context, store.Task) (Outcome, error) { return Outcome{Next: to, Note: "ok"}, nil }
}

// happySteps is the full chain ending in done.
func happySteps() []Step {
	return []Step{
		fakeStep{StatePreparing, next(StateAgentRunning)},
		fakeStep{StateAgentRunning, next(StateCollecting)},
		fakeStep{StateCollecting, next(StateVerifying)},
		fakeStep{StateVerifying, next(StatePassed)},
		fakeStep{StatePublishing, next(StateDone)},
	}
}

func setup(t *testing.T, steps []Step, opts ...Option) (*Machine, *store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "orch.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return New(s, steps, opts...), s, path
}

func create(t *testing.T, s *store.Store) int64 {
	t.Helper()
	id, err := s.CreateTask(context.Background(), store.Task{Repo: "r", Base: "b", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func state(t *testing.T, s *store.Store, id int64) State {
	t.Helper()
	tk, err := s.GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return tk.State
}

// drain ticks until idle, with a safety bound.
func drain(t *testing.T, m *Machine) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if err := m.Tick(context.Background()); errors.Is(err, ErrNoWork) {
			return
		} else if err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("did not go idle")
}

func TestAllowedTransitionsTable(t *testing.T) {
	allowed := [][2]State{
		{StateQueued, StatePreparing}, {StatePreparing, StateAgentRunning},
		{StateAgentRunning, StateCollecting}, {StateCollecting, StateVerifying},
		{StateVerifying, StatePassed}, {StateVerifying, StateNeedsApproval},
		{StateVerifying, StateFailed}, {StateNeedsApproval, StateVerifying},
		{StatePassed, StatePublishing}, {StatePublishing, StateDone},
		{StatePreparing, StateError}, {StateVerifying, StateError},
		{StateQueued, StateCancelled}, {StateNeedsApproval, StateCancelled},
		{StateError, StateVerifying}, {StateError, StateFailed},
	}
	forbidden := [][2]State{
		{StateQueued, StateVerifying}, {StateQueued, StatePassed}, {StateQueued, StateDone},
		{StatePreparing, StatePassed}, {StateAgentRunning, StatePassed},
		{StateCollecting, StatePassed}, {StateCollecting, StateDone},
		{StateNeedsApproval, StatePassed}, {StateNeedsApproval, StatePublishing},
		{StateError, StateError}, {StateError, StateNeedsApproval},
		{StateVerifying, StateVerifying}, {StateVerifying, StatePublishing},
		{StateDone, StateQueued}, {StateFailed, StateQueued}, {StateCancelled, StatePreparing},
		{StateDone, StateCancelled}, {StateFailed, StateError}, {StateCancelled, StateError},
		{StatePublishing, StatePassed},
	}
	for _, p := range allowed {
		if !CanTransition(p[0], p[1]) {
			t.Errorf("%s -> %s should be allowed", p[0], p[1])
		}
	}
	for _, p := range forbidden {
		if CanTransition(p[0], p[1]) {
			t.Errorf("%s -> %s should be forbidden", p[0], p[1])
		}
	}
	all := []State{StateQueued, StatePreparing, StateAgentRunning, StateCollecting, StateVerifying,
		StateNeedsApproval, StatePassed, StatePublishing, StateDone, StateFailed, StateCancelled, StateError}
	for _, s := range all {
		if s.IsTerminal() && len(Allowed[s]) != 0 {
			t.Errorf("terminal %s has outgoing transitions", s)
		}
		if !s.IsTerminal() && !CanTransition(s, StateCancelled) {
			t.Errorf("%s must be cancellable", s)
		}
		if !s.IsTerminal() && s != StateError && !CanTransition(s, StateError) {
			t.Errorf("%s must be able to error", s)
		}
	}
	// Illegal attempts fail and change nothing.
	m, s, _ := setup(t, nil)
	id := create(t, s)
	err := m.transition(context.Background(), store.Task{ID: id, State: StateQueued}, StateDone, "x", store.TransitionOpts{})
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("got %v", err)
	}
	if state(t, s, id) != StateQueued {
		t.Fatal("state changed")
	}
}

func TestHappyPathToDone(t *testing.T) {
	var hooks []string
	m, s, _ := setup(t, happySteps(), WithOnTransition(func(_ store.Task, from, to State, _ string) {
		hooks = append(hooks, fmt.Sprintf("%s>%s", from, to))
	}))
	id := create(t, s)
	drain(t, m)
	if state(t, s, id) != StateDone {
		t.Fatalf("state %s", state(t, s, id))
	}
	evs, _ := s.Events(context.Background(), id)
	var seq []State
	for _, e := range evs {
		seq = append(seq, e.To)
	}
	want := []State{StateQueued, StatePreparing, StateAgentRunning, StateCollecting, StateVerifying, StatePassed, StatePublishing, StateDone}
	if fmt.Sprint(seq) != fmt.Sprint(want) {
		t.Fatalf("events %v", seq)
	}
	if len(hooks) != 7 {
		t.Fatalf("hooks %v", hooks)
	}
}

func TestCrashDuringStepResumesSameStep(t *testing.T) {
	var effects sync.Map // idempotent side effect keyed by task
	var runs atomic.Int32
	crash := true
	steps := happySteps()
	steps[1] = fakeStep{StateAgentRunning, func(_ context.Context, tk store.Task) (Outcome, error) {
		runs.Add(1)
		effects.Store(tk.ID, "agent-output")
		if crash {
			panic("simulated crash mid-step")
		}
		return Outcome{Next: StateCollecting}, nil
	}}
	m, s, path := setup(t, steps)
	id := create(t, s)
	ctx := context.Background()
	tick := func(m *Machine) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
			}
		}()
		return m.Tick(ctx)
	}
	for tick(m) == nil && state(t, s, id) != StateAgentRunning {
	}
	if err := tick(m); err == nil {
		t.Fatal("expected the crash")
	}
	if got := state(t, s, id); got != StateAgentRunning {
		t.Fatalf("state after crash %s", got)
	}
	// While the dead process still holds the claim nobody else may run it.
	if err := m.Tick(ctx); !errors.Is(err, ErrNoWork) {
		t.Fatalf("claimed task was picked up again: %v", err)
	}

	// Restart: reopen the same file, new Machine, Recover.
	s.Close()
	s2, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	crash = false
	m2 := New(s2, steps)
	if err := m2.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if got := state(t, s2, id); got == StateError || got != StateAgentRunning {
		t.Fatalf("recover left task in %s, want agent_running", got)
	}
	drain(t, m2)
	if state(t, s2, id) != StateDone {
		t.Fatalf("final %s", state(t, s2, id))
	}
	if runs.Load() != 2 {
		t.Fatalf("step ran %d times, want 2 (crash + rerun)", runs.Load())
	}
	evs, _ := s2.Events(ctx, id)
	n := 0
	for _, e := range evs {
		if e.From == StateAgentRunning && e.To == StateCollecting {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("result applied %d times", n)
	}
}

func TestRepeatedCrashesEndInFailed(t *testing.T) {
	steps := happySteps()
	steps[0] = fakeStep{StatePreparing, func(context.Context, store.Task) (Outcome, error) { panic("always") }}
	m, s, path := setup(t, steps, WithMaxAttempts(2))
	id := create(t, s)
	ctx := context.Background()
	for i := 0; i < 6 && state(t, s, id) != StateFailed; i++ {
		func() {
			defer func() { recover() }()
			m.Tick(ctx)
		}()
		s.Close()
		var err error
		if s, err = store.Open(path); err != nil {
			t.Fatal(err)
		}
		m = New(s, steps, WithMaxAttempts(2))
		if err := m.Recover(ctx); err != nil {
			t.Fatal(err)
		}
	}
	defer s.Close()
	if state(t, s, id) != StateFailed {
		t.Fatalf("crash loop did not stop: %s", state(t, s, id))
	}
}

func TestInfraErrorNeverBecomesPassed(t *testing.T) {
	var runs atomic.Int32
	steps := happySteps()
	steps[3] = fakeStep{StateVerifying, func(context.Context, store.Task) (Outcome, error) {
		runs.Add(1)
		// Even a (wrongly) successful-looking outcome must not matter with an error.
		return Outcome{Next: StatePassed}, errors.New("gate exited 64: transport forged")
	}}
	m, s, _ := setup(t, steps)
	id := create(t, s)
	drain(t, m)
	tk, _ := s.GetTask(context.Background(), id)
	if tk.State != StateFailed {
		t.Fatalf("state %s", tk.State)
	}
	if runs.Load() != int32(DefaultMaxAttempts) {
		t.Fatalf("ran %d times, want %d", runs.Load(), DefaultMaxAttempts)
	}
	if tk.ErrorText == "" {
		t.Fatal("error text lost")
	}
	evs, _ := s.Events(context.Background(), id)
	sawError := false
	for _, e := range evs {
		if e.To == StatePassed || e.To == StatePublishing || e.To == StateDone {
			t.Fatalf("infra error reached %s", e.To)
		}
		if e.To == StateError {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("never went through error")
	}
}

func TestErrorTextIsCapped(t *testing.T) {
	steps := happySteps()
	steps[0] = fakeStep{StatePreparing, func(context.Context, store.Task) (Outcome, error) {
		return Outcome{}, errors.New(string(make([]byte, 200000)))
	}}
	m, s, _ := setup(t, steps)
	id := create(t, s)
	drain(t, m)
	tk, _ := s.GetTask(context.Background(), id)
	if len(tk.ErrorText) > store.MaxErrorText+16 {
		t.Fatalf("error text %d bytes", len(tk.ErrorText))
	}
}

func TestAttemptLimit(t *testing.T) {
	var runs atomic.Int32
	steps := happySteps()
	steps[1] = fakeStep{StateAgentRunning, func(context.Context, store.Task) (Outcome, error) {
		runs.Add(1)
		return Outcome{}, errors.New("vm down")
	}}
	m, s, _ := setup(t, steps, WithMaxAttempts(2))
	id := create(t, s)
	drain(t, m)
	if runs.Load() != 2 || state(t, s, id) != StateFailed {
		t.Fatalf("runs=%d state=%s", runs.Load(), state(t, s, id))
	}
	// Failed is terminal: nothing more happens.
	if err := m.Tick(context.Background()); !errors.Is(err, ErrNoWork) {
		t.Fatalf("got %v", err)
	}
}

func TestAttemptsAreCountedPerStep(t *testing.T) {
	// Each step fails once and then succeeds; with a limit of 2 the task must
	// still finish because success resets the count.
	failed := map[State]bool{}
	mk := func(name State, to State) Step {
		return fakeStep{name, func(context.Context, store.Task) (Outcome, error) {
			if !failed[name] {
				failed[name] = true
				return Outcome{}, errors.New("flaky")
			}
			return Outcome{Next: to}, nil
		}}
	}
	steps := []Step{mk(StatePreparing, StateAgentRunning), mk(StateAgentRunning, StateCollecting),
		mk(StateCollecting, StateVerifying), mk(StateVerifying, StatePassed), mk(StatePublishing, StateDone)}
	m, s, _ := setup(t, steps, WithMaxAttempts(2))
	id := create(t, s)
	drain(t, m)
	if state(t, s, id) != StateDone {
		t.Fatalf("state %s", state(t, s, id))
	}
}

func TestCancelStopsFurtherSteps(t *testing.T) {
	var runs atomic.Int32
	var m *Machine
	var id int64
	steps := happySteps()
	steps[0] = fakeStep{StatePreparing, func(ctx context.Context, tk store.Task) (Outcome, error) {
		runs.Add(1)
		// Cancel arrives while the step is running.
		if err := m.Cancel(ctx, tk.ID); err != nil {
			t.Error(err)
		}
		return Outcome{Next: StateAgentRunning}, nil
	}}
	var s *store.Store
	m, s, _ = setup(t, steps)
	id = create(t, s)
	drain(t, m)
	if state(t, s, id) != StateCancelled {
		t.Fatalf("state %s", state(t, s, id))
	}
	if runs.Load() != 1 {
		t.Fatalf("ran %d", runs.Load())
	}
	if err := m.Cancel(context.Background(), id); !errors.Is(err, ErrTerminal) {
		t.Fatalf("cancel of cancelled: %v", err)
	}
	// A queued task can be cancelled before it ever runs.
	id2 := create(t, s)
	if err := m.Cancel(context.Background(), id2); err != nil {
		t.Fatal(err)
	}
	drain(t, m)
	if state(t, s, id2) != StateCancelled {
		t.Fatal("not cancelled")
	}
}

func TestIllegalOutcomeIsAStepError(t *testing.T) {
	steps := happySteps()
	steps[0] = fakeStep{StatePreparing, next(StateDone)} // skipping everything
	m, s, _ := setup(t, steps)
	id := create(t, s)
	drain(t, m)
	tk, _ := s.GetTask(context.Background(), id)
	if tk.State != StateFailed {
		t.Fatalf("state %s", tk.State)
	}
}

func TestMissingStepFailsTask(t *testing.T) {
	m, s, _ := setup(t, happySteps()[1:]) // no preparing step
	id := create(t, s)
	drain(t, m)
	if state(t, s, id) != StateFailed {
		t.Fatalf("state %s", state(t, s, id))
	}
}

func TestApprovalFlow(t *testing.T) {
	steps := happySteps()
	steps[3] = fakeStep{StateVerifying, next(StateNeedsApproval)}
	m, s, _ := setup(t, steps)
	ctx := context.Background()
	id := create(t, s)
	drain(t, m)
	if state(t, s, id) != StateNeedsApproval {
		t.Fatalf("state %s", state(t, s, id))
	}
	blocked, _ := s.AddApproval(ctx, store.Approval{TaskID: id, Kind: store.KindDestructiveBlocked, Hash: "d"})
	a1, _ := s.AddApproval(ctx, store.Approval{TaskID: id, Kind: store.KindProtectedModified, Hash: "h1"})
	a2, _ := s.AddApproval(ctx, store.Approval{TaskID: id, Kind: store.KindPublishOutsideAllowlist, Hash: "h2"}) // another kind: both stay pending
	if err := m.Approve(ctx, blocked); !errors.Is(err, store.ErrNotApprovable) {
		t.Fatalf("approve destructive: %v", err)
	}
	if err := m.Tick(ctx); !errors.Is(err, ErrNoWork) {
		t.Fatalf("needs_approval must wait: %v", err)
	}
	if err := m.Approve(ctx, a1); err != nil {
		t.Fatal(err)
	}
	if state(t, s, id) != StateNeedsApproval {
		t.Fatal("moved on with an approval still pending")
	}
	if err := m.Approve(ctx, a2); err != nil {
		t.Fatal(err)
	}
	if state(t, s, id) != StateVerifying {
		t.Fatalf("state %s, want verifying (re-judge)", state(t, s, id))
	}

	// Rejection fails the task.
	steps2 := happySteps()
	steps2[3] = fakeStep{StateVerifying, next(StateNeedsApproval)}
	m2, s2, _ := setup(t, steps2)
	id2 := create(t, s2)
	drain(t, m2)
	a, _ := s2.AddApproval(ctx, store.Approval{TaskID: id2, Kind: store.KindPublishOutsideAllowlist, Hash: "x"})
	if err := m2.Reject(ctx, a); err != nil {
		t.Fatal(err)
	}
	if state(t, s2, id2) != StateFailed {
		t.Fatalf("state %s", state(t, s2, id2))
	}
}

func TestConcurrentWorkersRunEachStepOnce(t *testing.T) {
	const tasks = 12
	var mu sync.Mutex
	runs := map[string]int{}
	count := func(name State, to State) Step {
		return fakeStep{name, func(_ context.Context, tk store.Task) (Outcome, error) {
			mu.Lock()
			runs[fmt.Sprintf("%d/%s", tk.ID, name)]++
			mu.Unlock()
			return Outcome{Next: to}, nil
		}}
	}
	steps := []Step{count(StatePreparing, StateAgentRunning), count(StateAgentRunning, StateCollecting),
		count(StateCollecting, StateVerifying), count(StateVerifying, StatePassed), count(StatePublishing, StateDone)}
	m, s, path := setup(t, steps)
	s2, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	m2 := New(s2, steps)
	for i := 0; i < tasks; i++ {
		create(t, s)
	}
	var wg sync.WaitGroup
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			mm := m
			if w%2 == 1 {
				mm = m2
			}
			for {
				err := mm.Tick(context.Background())
				if errors.Is(err, ErrNoWork) {
					// another worker may still be mid-step; re-check until all done
					mu.Lock()
					done := 0
					for k, v := range runs {
						if len(k) > 0 && v > 0 && k[len(k)-10:] == "publishing" {
							done++
						}
					}
					mu.Unlock()
					if done == tasks {
						return
					}
					continue
				}
				if err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	for k, v := range runs {
		if v != 1 {
			t.Errorf("%s ran %d times", k, v)
		}
	}
	if len(runs) != tasks*5 {
		t.Fatalf("ran %d distinct steps, want %d", len(runs), tasks*5)
	}
}

// The cause of a failed attempt must stay in the event history: tasks.error_text is
// cleared when a retry succeeds, and a cause that only lives there is lost.
func TestErrorCauseIsKeptInEventNotes(t *testing.T) {
	first := true
	steps := happySteps()
	steps[1] = fakeStep{StateAgentRunning, func(context.Context, store.Task) (Outcome, error) {
		if first {
			first = false
			return Outcome{}, errors.New("ssh connection failed (exit 255):\n  Connection timed out " + strings.Repeat("x", 500))
		}
		return Outcome{Next: StateCollecting}, nil
	}}
	m, s, _ := setup(t, steps)
	id := create(t, s)
	drain(t, m)
	if state(t, s, id) != StateDone {
		t.Fatalf("state=%s", state(t, s, id))
	}
	evs, err := s.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var note string
	for _, e := range evs {
		if e.To == StateError {
			note = e.Note
		}
	}
	if !strings.Contains(note, "failed (attempt 1): ssh connection failed (exit 255): Connection timed out") {
		t.Fatalf("event note lost the cause: %q", note)
	}
	if strings.Contains(note, "\n") || len(note) > 400 {
		t.Fatalf("note must be one short line: %d bytes %q", len(note), note)
	}
}
