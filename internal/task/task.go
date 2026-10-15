// Package task defines NAVETTE's core domain types and the task state machine.
// This is the heart of the Control Plane and has no dependency on any Runtime,
// Provider, or storage backend.
package task

import (
	"encoding/json"
	"time"
)

// Constraints bound a task's execution.
type Constraints struct {
	// Timeout bounds a single run. Zero means no timeout.
	Timeout time.Duration
	// MaxIterations caps the Control Plane execution-loop iterations. Zero means
	// the engine default. This counts Control Plane loop turns, NOT the Agent's
	// internal iterations.
	MaxIterations int
	// WorkDir is the path to the writable temporary working copy the Agent may
	// modify (with .git excluded). It is NOT the real repository itself.
	WorkDir string
}

// Task is a unit of work tracked by NAVETTE.
type Task struct {
	ID          string
	Goal        string
	Input       map[string]any
	Constraints Constraints
	State       State
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Outcome is the terminal result of a run.
type Outcome string

const (
	OutcomeSucceeded Outcome = "Succeeded"
	OutcomeFailed    Outcome = "Failed"
	OutcomeCancelled Outcome = "Cancelled"
	OutcomeTimedOut  Outcome = "TimedOut"
	OutcomeRejected  Outcome = "Rejected"
)

// Run is a single execution attempt of a task.
type Run struct {
	ID        string
	TaskID    string
	StartedAt time.Time
	EndedAt   time.Time
	Outcome   Outcome
}

// EventKind classifies an event record.
type EventKind string

const (
	EventStateTransition EventKind = "state.transition"
	EventApproval        EventKind = "approval"
	EventRuntimeOutput   EventKind = "runtime.output"
	EventNote            EventKind = "note"
)

// Event is an append-only record of something that happened to a task. Events
// and the task state are persisted atomically (same transaction).
type Event struct {
	ID     int64
	TaskID string
	RunID  string
	At     time.Time
	Kind   EventKind
	From   State // for state.transition
	To     State // for state.transition
	Reason string
	Detail json.RawMessage
}

// ApprovalKind distinguishes pre-execution approval from result approval.
type ApprovalKind string

const (
	ApprovalPreExec ApprovalKind = "pre_exec"
	ApprovalResult  ApprovalKind = "result"
)

// Decision records a human approval decision.
type Decision struct {
	Kind     ApprovalKind
	Approved bool
	By       string
	At       time.Time
	Reason   string
}
