// Package machine drives tasks through the life cycle in design section 3.
//
// Crash model: a task in state S means "step S has to run". Tick claims a task,
// runs the step, then moves S -> Next in one compare-and-set transaction. If
// the process dies in between, the task is still in S, so the same step runs
// again after Recover (steps must be idempotent). The claim only keeps two
// workers from running the same task at once.
package machine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"aigate/internal/orch/store"
)

// DefaultMaxAttempts is how many times one step may fail (or be interrupted by
// a crash) before the task becomes failed.
const DefaultMaxAttempts = 3

// ErrNoWork is returned by Tick when no task can run right now.
var ErrNoWork = errors.New("machine: no runnable task")

// ErrTerminal is returned when asking to change a task that is already finished.
var ErrTerminal = errors.New("machine: task is in a terminal state")

// Outcome is what a step wants to happen next.
type Outcome struct {
	Next State
	Note string
}

// Step runs the work of one state. A returned error is an infrastructure
// error: it is never turned into a verdict and never leads to passed.
// A step must be idempotent because a crash re-runs it.
type Step interface {
	Name() State
	Run(ctx context.Context, t store.Task) (Outcome, error)
}

// Machine advances tasks. It is safe for concurrent use.
type Machine struct {
	st           *store.Store
	steps        map[State]Step
	maxAttempts  int
	onTransition func(t store.Task, from, to State, note string)
}

// Option configures a Machine.
type Option func(*Machine)

// WithMaxAttempts sets the per-step attempt limit (values below 1 are ignored).
func WithMaxAttempts(n int) Option {
	return func(m *Machine) {
		if n >= 1 {
			m.maxAttempts = n
		}
	}
}

// WithOnTransition registers a callback invoked after each successful
// transition, with the task as it was before the move. It must not block.
func WithOnTransition(f func(t store.Task, from, to State, note string)) Option {
	return func(m *Machine) { m.onTransition = f }
}

// New builds a Machine. queued and passed advance on their own unless a step
// with that name is registered.
func New(st *store.Store, steps []Step, opts ...Option) *Machine {
	m := &Machine{st: st, steps: map[State]Step{}, maxAttempts: DefaultMaxAttempts}
	for _, s := range steps {
		m.steps[s.Name()] = s
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// transition validates against Allowed, then does the compare-and-set.
func (m *Machine) transition(ctx context.Context, t store.Task, to State, note string, o store.TransitionOpts) error {
	if !CanTransition(t.State, to) {
		return fmt.Errorf("%w: %s -> %s", ErrIllegalTransition, t.State, to)
	}
	if err := m.st.TransitionWith(ctx, t.ID, t.State, to, note, o); err != nil {
		return err
	}
	if m.onTransition != nil {
		m.onTransition(t, t.State, to, note)
	}
	return nil
}

// Tick advances one runnable task by one step. It returns ErrNoWork when
// nothing is runnable. Losing a race (the task changed state, e.g. it was
// cancelled while the step ran) is not an error: the step's result is dropped.
func (m *Machine) Tick(ctx context.Context) error {
	t, ok, err := m.st.NextRunnable(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNoWork
	}
	return ignoreLost(m.advance(ctx, t))
}

func ignoreLost(err error) error {
	if errors.Is(err, store.ErrStateMismatch) {
		return nil
	}
	return err
}

func (m *Machine) advance(ctx context.Context, t store.Task) error {
	switch t.State {
	case StateError:
		// Retry only the step that failed; never anything else.
		if !retryable(t.RetryState) {
			return m.transition(ctx, t, StateFailed, "error without a retry step", store.TransitionOpts{ErrorText: t.ErrorText})
		}
		return m.transition(ctx, t, t.RetryState, fmt.Sprintf("retry %s (attempt %d)", t.RetryState, t.Attempts+1), store.TransitionOpts{})
	}
	step, ok := m.steps[t.State]
	if !ok {
		switch t.State {
		case StateQueued:
			return m.transition(ctx, t, StatePreparing, "started", store.TransitionOpts{})
		case StatePassed:
			return m.transition(ctx, t, StatePublishing, "verdict passed", store.TransitionOpts{})
		}
		return m.transition(ctx, t, StateFailed, "no step registered for "+string(t.State),
			store.TransitionOpts{ErrorText: "no step registered for " + string(t.State)})
	}

	out, runErr := step.Run(ctx, t)
	if runErr != nil {
		if ctx.Err() != nil {
			// Shutting down: keep the claim; Recover accounts for the interruption.
			return ctx.Err()
		}
		return m.stepFailed(ctx, t, runErr)
	}
	if out.Next == t.State || !CanTransition(t.State, out.Next) || out.Next == StateError {
		return m.stepFailed(ctx, t, fmt.Errorf("step %s returned illegal next state %q", t.State, out.Next))
	}
	return m.transition(ctx, t, out.Next, out.Note, store.TransitionOpts{})
}

// stepFailed records an infrastructure error: retry via error until the
// attempt limit, then failed. It never produces passed.
func (m *Machine) stepFailed(ctx context.Context, t store.Task, cause error) error {
	text := cause.Error()
	// tasks.error_text is cleared once a retry succeeds, so keep a short cause in the event history too
	why := noteCause(text)
	if t.Attempts+1 >= m.maxAttempts {
		return m.transition(ctx, t, StateFailed,
			fmt.Sprintf("%s failed after %d attempts: %s", t.State, t.Attempts+1, why),
			store.TransitionOpts{ErrorText: text, InfraFailed: true, RetryState: t.State})
	}
	return m.transition(ctx, t, StateError, fmt.Sprintf("%s failed (attempt %d): %s", t.State, t.Attempts+1, why),
		store.TransitionOpts{ErrorText: text, RetryState: t.State})
}

// Recover must run once at process start, before ticking. Tasks that were in
// flight when the previous process died stay in the state of their step, so
// the same step runs again; the interruption counts as one attempt, and a step
// that keeps killing the process ends in failed instead of looping forever.
func (m *Machine) Recover(ctx context.Context) error {
	tasks, err := m.st.ReleaseClaims(ctx)
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if t.Attempts >= m.maxAttempts {
			err := m.transition(ctx, t, StateFailed,
				fmt.Sprintf("%s interrupted %d times", t.State, t.Attempts),
				store.TransitionOpts{ErrorText: "step repeatedly interrupted by a crash"})
			if err = ignoreLost(err); err != nil {
				return err
			}
		}
	}
	return nil
}

// Cancel stops a task. A step that is running finishes, but its result is
// dropped and no further step runs.
func (m *Machine) Cancel(ctx context.Context, id int64) error {
	for i := 0; i < 5; i++ {
		t, err := m.st.GetTask(ctx, id)
		if err != nil {
			return err
		}
		if t.State.IsTerminal() {
			return ErrTerminal
		}
		err = m.transition(ctx, t, StateCancelled, "cancelled", store.TransitionOpts{})
		if !errors.Is(err, store.ErrStateMismatch) {
			return err
		}
	}
	return store.ErrStateMismatch
}

// Approve approves one pending entry. When the task has no approvable entries
// left, it goes back to verifying so the verdict is re-judged. The status
// change and the task transition are ONE transaction.
// destructive_blocked entries are refused.
func (m *Machine) Approve(ctx context.Context, approvalID int64) error {
	before, moved, err := m.st.ResolveAndAdvance(ctx, approvalID, store.ApprovalApproved,
		StateVerifying, StateFailed, "approved", store.TransitionOpts{})
	if err != nil {
		return err
	}
	if moved && m.onTransition != nil {
		m.onTransition(before, before.State, StateVerifying, "approved")
	}
	return nil
}

// Reject rejects one pending entry; for an approvable kind the task fails (in
// the same transaction).
func (m *Machine) Reject(ctx context.Context, approvalID int64) error {
	a, err := m.st.GetApproval(ctx, approvalID)
	if err != nil {
		return err
	}
	before, moved, err := m.st.ResolveAndAdvance(ctx, approvalID, store.ApprovalRejected,
		StateVerifying, StateFailed, "approval rejected",
		store.TransitionOpts{ErrorText: "approval rejected: " + a.Reason})
	if err != nil {
		return err
	}
	if moved && m.onTransition != nil {
		m.onTransition(before, before.State, StateFailed, "approval rejected")
	}
	return nil
}

// noteCause shortens an error to one line of at most 300 bytes for an event note.
func noteCause(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 300 {
		text = text[:300] + "…"
	}
	return text
}
