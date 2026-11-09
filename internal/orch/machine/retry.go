package machine

import (
	"context"
	"errors"
	"fmt"

	"aigate/internal/orch/store"
)

// MaxRetries is how many times an operator may retry one task.
const MaxRetries = 3

// NotRetryableError says why a task cannot be retried (Reason is Korean, for the operator).
type NotRetryableError struct{ Reason string }

func (e *NotRetryableError) Error() string { return e.Reason }

func notRetryable(format string, a ...any) error {
	return &NotRetryableError{Reason: fmt.Sprintf(format, a...)}
}

// retryResumable are the steps a failed task may be resumed into. publishing is
// deliberately absent: a push may already have happened and the publisher is not
// proven idempotent for that head, so a publish is never re-run by retry.
func retryResumable(s State) bool {
	switch s {
	case StatePreparing, StateAgentRunning, StateCollecting, StateVerifying:
		return true
	}
	return false
}

// Retryable returns nil when the operator may retry t, else a *NotRetryableError.
// Only a failed task whose failure was an infrastructure failure after the
// attempt limit qualifies; verdict failures never do.
func Retryable(t store.Task) error {
	switch {
	case t.State == StateCancelled:
		return notRetryable("취소된 작업은 다시 시도할 수 없습니다")
	case t.State == StateDone:
		return notRetryable("이미 끝난(done) 작업입니다")
	case t.State != StateFailed:
		return notRetryable("failed 상태의 작업만 다시 시도할 수 있습니다(현재: %s)", t.State)
	case !t.InfraFailed:
		return notRetryable("판정·승인 거절 등으로 실패한 작업은 다시 시도할 수 없습니다(인프라 오류로 실패한 작업만 가능)")
	case t.RetryState == "":
		return notRetryable("실패한 단계를 알 수 없어 다시 시도할 수 없습니다")
	case t.RetryState == StatePublishing:
		return notRetryable("게시(publishing) 단계에서 실패한 작업은 다시 시도할 수 없습니다: push가 이미 일어났을 수 있고 같은 head의 재게시가 안전하다고 보장되지 않습니다")
	case !retryResumable(t.RetryState):
		return notRetryable("%s 단계부터는 다시 시도할 수 없습니다", t.RetryState)
	case t.Retries >= MaxRetries:
		return notRetryable("재시도 한도(%d회)를 모두 썼습니다", MaxRetries)
	}
	return nil
}

// Retry resumes an infrastructure-failed task from the step that failed, with
// attempts and the error text reset. The edge failed -> step exists only here,
// not in Allowed. The head and base are untouched. Of several concurrent calls
// exactly one succeeds; the others get a *NotRetryableError or ErrStateMismatch.
func (m *Machine) Retry(ctx context.Context, id int64) (store.Task, error) {
	t, err := m.st.GetTask(ctx, id)
	if err != nil {
		return store.Task{}, err
	}
	if err := Retryable(t); err != nil {
		return store.Task{}, err
	}
	note := "retry requested by operator (was: " + shortCause(t.ErrorText) + ")"
	if err := m.st.RetryFailed(ctx, id, t.RetryState, MaxRetries, note); err != nil {
		if errors.Is(err, store.ErrStateMismatch) {
			// lost a race: report the reason from the task as it is now
			if cur, gerr := m.st.GetTask(ctx, id); gerr == nil {
				if rerr := Retryable(cur); rerr != nil {
					return store.Task{}, rerr
				}
			}
		}
		return store.Task{}, err
	}
	m.notify(ctx, t, StateFailed, t.RetryState, note)
	return m.st.GetTask(ctx, id)
}

// shortCause is the first 120 bytes of an error, on one line.
func shortCause(text string) string {
	text = noteCause(text)
	if len(text) > 120 {
		text = text[:120] + "…"
	}
	if text == "" {
		text = "unknown"
	}
	return text
}
