package task

import "fmt"

// allowedTransitions defines the permitted state transitions. Any transition not
// listed here is rejected (the state is left unchanged).
var allowedTransitions = map[State]map[State]bool{
	Pending: {
		AwaitingPreApproval: true,
		Cancelled:           true,
	},
	AwaitingPreApproval: {
		Running:   true, // pre-approval granted
		Rejected:  true, // pre-approval denied
		Cancelled: true,
	},
	Running: {
		AwaitingResultApproval: true, // runtime finished, awaiting result approval
		Failed:                 true,
		Cancelled:              true,
		TimedOut:               true,
		Indeterminate:          true, // interrupted (e.g. process restart)
	},
	AwaitingResultApproval: {
		Finalizing: true, // result approved; confirm artifacts before success
		Rejected:   true, // result rejected
		Cancelled:  true,
	},
	Finalizing: {
		Succeeded:     true, // artifacts confirmed (saved + verified)
		Failed:        true, // finalization/verification failed
		Cancelled:     true,
		Indeterminate: true, // interrupted (e.g. process restart) during finalization
	},
	// Indeterminate is a resting state after an interrupted run. It does not
	// auto-resume; a human must decide. We permit explicit human-driven moves
	// to terminal states, but never automatic ones.
	Indeterminate: {
		Cancelled: true,
		Failed:    true,
	},
}

// CanTransition reports whether moving from -> to is allowed.
func CanTransition(from, to State) bool {
	return allowedTransitions[from][to]
}

// ErrInvalidTransition is returned when a disallowed transition is requested.
type ErrInvalidTransition struct {
	From State
	To   State
}

func (e ErrInvalidTransition) Error() string {
	return fmt.Sprintf("task: invalid state transition %s -> %s", e.From, e.To)
}
