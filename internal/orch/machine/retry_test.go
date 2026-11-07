package machine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"aigate/internal/orch/store"
)

func infraFailing(step State, errText string) []Step {
	steps := happySteps()
	for i, s := range steps {
		if s.Name() == step {
			steps[i] = fakeStep{step, func(context.Context, store.Task) (Outcome, error) { return Outcome{}, errors.New(errText) }}
		}
	}
	return steps
}

// failInfra drives a new task to failed through an infrastructure error in step.
func failInfra(t *testing.T, step State, errText string) (*Machine, *store.Store, int64) {
	t.Helper()
	m, s, _ := setup(t, infraFailing(step, errText), WithMaxAttempts(1))
	id := create(t, s)
	drain(t, m)
	if got := state(t, s, id); got != StateFailed {
		t.Fatalf("state %s", got)
	}
	return m, s, id
}

func TestRetryableTable(t *testing.T) {
	cases := []struct {
		name string
		task store.Task
		ok   bool
		why  string
	}{
		{"infra failed verifying", store.Task{State: StateFailed, InfraFailed: true, RetryState: StateVerifying, ErrorText: "x"}, true, ""},
		{"infra failed preparing", store.Task{State: StateFailed, InfraFailed: true, RetryState: StatePreparing}, true, ""},
		{"infra failed agent_running", store.Task{State: StateFailed, InfraFailed: true, RetryState: StateAgentRunning}, true, ""},
		{"infra failed collecting", store.Task{State: StateFailed, InfraFailed: true, RetryState: StateCollecting}, true, ""},
		{"verdict failed", store.Task{State: StateFailed, RetryState: StateVerifying, ErrorText: "behavior_failure"}, false, "판정"},
		{"unknown step", store.Task{State: StateFailed, InfraFailed: true}, false, "단계"},
		{"publishing", store.Task{State: StateFailed, InfraFailed: true, RetryState: StatePublishing}, false, "게시"},
		{"limit reached", store.Task{State: StateFailed, InfraFailed: true, RetryState: StateVerifying, Retries: MaxRetries}, false, "3"},
		{"one left", store.Task{State: StateFailed, InfraFailed: true, RetryState: StateVerifying, Retries: MaxRetries - 1}, true, ""},
		{"cancelled", store.Task{State: StateCancelled, InfraFailed: true, RetryState: StateVerifying}, false, "취소"},
		{"done", store.Task{State: StateDone}, false, "끝"},
		{"running", store.Task{State: StateVerifying}, false, "failed"},
		{"error", store.Task{State: StateError, RetryState: StateVerifying}, false, "failed"},
		{"needs_approval", store.Task{State: StateNeedsApproval}, false, "failed"},
	}
	for _, c := range cases {
		err := Retryable(c.task)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v want ok=%v", c.name, err, c.ok)
			continue
		}
		if err != nil {
			var nr *NotRetryableError
			if !errors.As(err, &nr) || !strings.Contains(nr.Reason, c.why) {
				t.Errorf("%s: reason %v, want containing %q", c.name, err, c.why)
			}
		}
	}
}

func TestGenericTableStillForbidsFailedExits(t *testing.T) {
	if len(Allowed[StateFailed]) != 0 {
		t.Fatalf("Allowed[failed] = %v", Allowed[StateFailed])
	}
	for _, to := range []State{StateQueued, StatePreparing, StateAgentRunning, StateCollecting, StateVerifying,
		StatePassed, StatePublishing, StateDone, StateError, StateCancelled, StateNeedsApproval} {
		if CanTransition(StateFailed, to) {
			t.Errorf("failed -> %s allowed", to)
		}
	}
}

func TestRetryResumesQuota413Task(t *testing.T) {
	m, s, id := failInfra(t, StateVerifying, "restore quota 413: volume snapshot quota exceeded")
	ctx := context.Background()
	failed, _ := s.GetTask(ctx, id)
	if !failed.InfraFailed || failed.RetryState != StateVerifying || failed.ErrorText == "" {
		t.Fatalf("%+v", failed)
	}
	// the generic Tick never picks up a failed task
	if err := m.Tick(ctx); !errors.Is(err, ErrNoWork) {
		t.Fatalf("tick %v", err)
	}
	// the infrastructure is fixed: swap in steps that succeed
	var calls []State
	var mu sync.Mutex
	var steps []Step
	for _, s := range happySteps() {
		name := s.Name()
		inner := s
		steps = append(steps, fakeStep{name, func(c context.Context, tk store.Task) (Outcome, error) {
			mu.Lock()
			calls = append(calls, name)
			mu.Unlock()
			return inner.Run(c, tk)
		}})
	}
	var seen []string
	m2 := New(s, steps, WithMaxAttempts(1), WithOnTransition(func(_ store.Task, from, to State, note string) {
		seen = append(seen, fmt.Sprintf("%s>%s", from, to))
	}))
	got, err := m2.Retry(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateVerifying || got.Attempts != 0 || got.ErrorText != "" || got.Retries != 1 || got.InfraFailed || got.RetryState != "" {
		t.Fatalf("after retry %+v", got)
	}
	if got.HeadSHA != failed.HeadSHA || got.Base != failed.Base {
		t.Fatal("head/base changed")
	}
	if len(seen) != 1 || seen[0] != "failed>verifying" {
		t.Fatalf("callbacks %v", seen)
	}
	evs, _ := s.Events(ctx, id)
	last := evs[len(evs)-1]
	if last.From != StateFailed || last.To != StateVerifying ||
		!strings.HasPrefix(last.Note, "retry requested by operator (was: ") || !strings.Contains(last.Note, "quota 413") {
		t.Fatalf("event %+v", last)
	}
	drain(t, m2)
	if got := state(t, s, id); got != StateDone {
		t.Fatalf("final %s", got)
	}
	if strings.Join(stateNames(calls), ",") != "verifying,publishing" {
		t.Fatalf("steps run after retry: %v (earlier steps must not re-run)", calls)
	}
}

func stateNames(ss []State) []string {
	var o []string
	for _, s := range ss {
		o = append(o, string(s))
	}
	return o
}

func TestRetryRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("verdict failure", func(t *testing.T) {
		steps := happySteps()
		steps[3] = fakeStep{StateVerifying, next(StateFailed)}
		m, s, _ := setup(t, steps, WithMaxAttempts(1))
		id := create(t, s)
		drain(t, m)
		if state(t, s, id) != StateFailed {
			t.Fatal("not failed")
		}
		var nr *NotRetryableError
		if _, err := m.Retry(ctx, id); !errors.As(err, &nr) {
			t.Fatalf("err %v", err)
		}
		if state(t, s, id) != StateFailed {
			t.Fatal("moved")
		}
	})
	t.Run("publishing", func(t *testing.T) {
		m, s, id := failInfra(t, StatePublishing, "push: connection reset")
		var nr *NotRetryableError
		_, err := m.Retry(ctx, id)
		if !errors.As(err, &nr) || !strings.Contains(nr.Reason, "게시") {
			t.Fatalf("err %v", err)
		}
		if state(t, s, id) != StateFailed {
			t.Fatal("moved")
		}
	})
	t.Run("cancelled, running, missing", func(t *testing.T) {
		m, s, _ := setup(t, happySteps())
		id := create(t, s)
		var nr *NotRetryableError
		if _, err := m.Retry(ctx, id); !errors.As(err, &nr) {
			t.Fatalf("queued: %v", err)
		}
		if err := m.Cancel(ctx, id); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Retry(ctx, id); !errors.As(err, &nr) {
			t.Fatalf("cancelled: %v", err)
		}
		if _, err := m.Retry(ctx, 999); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing: %v", err)
		}
	})
}

func TestRetryLimitIsThree(t *testing.T) {
	m, s, id := failInfra(t, StateVerifying, "vm stopped")
	ctx := context.Background()
	for i := 1; i <= MaxRetries; i++ {
		got, err := m.Retry(ctx, id)
		if err != nil || got.Retries != i {
			t.Fatalf("retry %d: %+v %v", i, got, err)
		}
		drain(t, m) // verifying fails again (the fake step still errors) -> failed
		if state(t, s, id) != StateFailed {
			t.Fatalf("round %d: %s", i, state(t, s, id))
		}
	}
	var nr *NotRetryableError
	if _, err := m.Retry(ctx, id); !errors.As(err, &nr) || !strings.Contains(nr.Reason, "3") {
		t.Fatalf("4th retry: %v", err)
	}
	if got, _ := s.GetTask(ctx, id); got.Retries != MaxRetries || got.State != StateFailed {
		t.Fatalf("%+v", got)
	}
}

func TestConcurrentRetryExactlyOneWins(t *testing.T) {
	m, s, id := failInfra(t, StateCollecting, "ssh outage")
	ctx := context.Background()
	var ok, mismatch, other atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := m.Retry(ctx, id)
			var nr *NotRetryableError
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, store.ErrStateMismatch), errors.As(err, &nr):
				mismatch.Add(1)
			default:
				other.Add(1)
				t.Errorf("unexpected %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if ok.Load() != 1 || other.Load() != 0 {
		t.Fatalf("ok=%d mismatch=%d other=%d", ok.Load(), mismatch.Load(), other.Load())
	}
	got, _ := s.GetTask(ctx, id)
	if got.Retries != 1 || got.State != StateCollecting {
		t.Fatalf("%+v", got)
	}
	evs, _ := s.Events(ctx, id)
	n := 0
	for _, e := range evs {
		if strings.HasPrefix(e.Note, "retry requested") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d retry events", n)
	}
}
