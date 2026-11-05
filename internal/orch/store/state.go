package store

// State is a task's position in its life cycle. The transition rules live in
// the machine package; the store only enforces compare-and-set.
type State string

const (
	StateQueued        State = "queued"
	StatePreparing     State = "preparing"
	StateAgentRunning  State = "agent_running"
	StateCollecting    State = "collecting"
	StateVerifying     State = "verifying"
	StateNeedsApproval State = "needs_approval"
	StatePassed        State = "passed"
	StatePublishing    State = "publishing"
	StateDone          State = "done"
	StateFailed        State = "failed"
	StateCancelled     State = "cancelled"
	StateError         State = "error"
)

// IsTerminal reports whether no further work happens in s.
func (s State) IsTerminal() bool {
	return s == StateDone || s == StateFailed || s == StateCancelled
}

// runnable lists the states a worker may pick up. needs_approval waits for a
// human; terminal states are finished.
var runnable = []State{
	StateQueued, StatePreparing, StateAgentRunning, StateCollecting,
	StateVerifying, StatePassed, StatePublishing, StateError,
}

// Approval kinds.
const (
	KindProtectedModified       = "protected_modified"
	KindPublishOutsideAllowlist = "publish_outside_allowlist"
	KindDestructiveBlocked      = "destructive_blocked" // record only, never approvable
)

// Approval statuses.
const (
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalRejected = "rejected"
	// ApprovalSuperseded: the task got a newer hash or head; never approvable.
	ApprovalSuperseded = "superseded"
)
