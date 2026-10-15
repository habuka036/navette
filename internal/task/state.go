package task

// State is a task's lifecycle state. The set is intentionally minimal; NAVETTE
// is a harness, not a general workflow engine.
type State string

const (
	// Pending: task registered, not yet started.
	Pending State = "Pending"
	// AwaitingPreApproval: waiting for human approval to start the Runtime.
	// The Runtime must NOT be launched in this state.
	AwaitingPreApproval State = "AwaitingPreApproval"
	// Running: pre-approved; the Runtime is executing.
	Running State = "Running"
	// AwaitingResultApproval: Runtime finished; waiting for human to accept the
	// result (artifacts). Independent of pre-approval.
	AwaitingResultApproval State = "AwaitingResultApproval"
	// Finalizing: result approved; the engine is durably confirming (saving and
	// verifying) the approved artifact snapshot. The task is NOT yet successful.
	// It transitions to Succeeded only after confirmation, or Failed if
	// confirmation fails.
	Finalizing State = "Finalizing"
	// Succeeded: result approved AND artifacts confirmed (terminal). Means
	// artifacts accepted and durably saved; it does NOT imply any write-back or
	// git commit to a real repository.
	Succeeded State = "Succeeded"
	// Rejected: pre-approval or result approval was rejected (terminal).
	Rejected State = "Rejected"
	// Failed: execution error (terminal).
	Failed State = "Failed"
	// Cancelled: cancelled by the user (terminal).
	Cancelled State = "Cancelled"
	// TimedOut: execution exceeded the timeout (terminal).
	TimedOut State = "TimedOut"
	// Indeterminate: process was interrupted while Running and the outcome is
	// unknown on restart. Must not auto-transition to success/failure and must
	// not auto-resume.
	Indeterminate State = "Indeterminate"
)

// IsTerminal reports whether s is a terminal state (no outgoing transitions).
func (s State) IsTerminal() bool {
	switch s {
	case Succeeded, Rejected, Failed, Cancelled, TimedOut:
		return true
	default:
		return false
	}
}

// AwaitsApproval reports whether s is a state that blocks on human approval.
func (s State) AwaitsApproval() bool {
	return s == AwaitingPreApproval || s == AwaitingResultApproval
}
