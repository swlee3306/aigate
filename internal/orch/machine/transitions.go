package machine

import (
	"errors"
	"slices"

	"aigate/internal/orch/store"
)

// State is the task state; the type and constants live in store so the store
// does not import machine.
type State = store.State

const (
	StateQueued        = store.StateQueued
	StatePreparing     = store.StatePreparing
	StateAgentRunning  = store.StateAgentRunning
	StateCollecting    = store.StateCollecting
	StateVerifying     = store.StateVerifying
	StateNeedsApproval = store.StateNeedsApproval
	StatePassed        = store.StatePassed
	StatePublishing    = store.StatePublishing
	StateDone          = store.StateDone
	StateFailed        = store.StateFailed
	StateCancelled     = store.StateCancelled
	StateError         = store.StateError
)

// ErrIllegalTransition is returned for a move that Allowed does not list.
var ErrIllegalTransition = errors.New("machine: illegal transition")

// Allowed is the transition table (design section 3). A state S doubles as the
// name of the step that runs while the task is in S. Terminal states (done,
// failed, cancelled) have no outgoing edges. error is a holding state: the
// retry logic is the only code that leaves it for a step, and only toward the
// step recorded in Task.RetryState.
var Allowed = map[State][]State{
	StateQueued:        {StatePreparing, StateCancelled, StateError},
	StatePreparing:     {StateAgentRunning, StateFailed, StateCancelled, StateError},
	StateAgentRunning:  {StateCollecting, StateFailed, StateCancelled, StateError},
	StateCollecting:    {StateVerifying, StateFailed, StateCancelled, StateError},
	StateVerifying:     {StatePassed, StateNeedsApproval, StateFailed, StateCancelled, StateError},
	StateNeedsApproval: {StateVerifying, StateFailed, StateCancelled, StateError},
	StatePassed:        {StatePublishing, StateCancelled, StateError},
	// Publishing may park the task when an action falls outside the allow list.
	StatePublishing: {StateDone, StateNeedsApproval, StateFailed, StateCancelled, StateError},
	StateError: {StateQueued, StatePreparing, StateAgentRunning, StateCollecting, StateVerifying,
		StatePassed, StatePublishing, StateFailed, StateCancelled},
	StateDone:      nil,
	StateFailed:    nil,
	StateCancelled: nil,
}

// CanTransition reports whether Allowed lists from -> to.
func CanTransition(from, to State) bool { return slices.Contains(Allowed[from], to) }

// retryable are the states error may resume into.
func retryable(s State) bool {
	switch s {
	case StateQueued, StatePreparing, StateAgentRunning, StateCollecting,
		StateVerifying, StatePassed, StatePublishing:
		return true
	}
	return false
}
