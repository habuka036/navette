// Package runtime defines the AgentRuntime port: the pluggable abstraction for
// how an agent executes a task. The Control Plane depends only on this
// interface, never on a concrete runtime's details.
//
// Note on the Runtime/Provider relationship: an Agent Runtime (for example the
// future Kiro CLI runtime) may itself use an LLM internally. NAVETTE's
// LLMProvider abstraction does NOT replace a runtime's internal model; it is a
// separate concern. The fake runtime in this package uses no LLM at all, which
// is sufficient for Milestone B Control Plane validation.
package runtime

import "context"

// RunRequest is the input to a single agent execution step.
type RunRequest struct {
	TaskID  string
	Goal    string
	WorkDir string         // writable temporary working copy (.git excluded)
	Input   map[string]any // task input
}

// OutputEvent is a structured event emitted during execution. For the Kiro CLI
// runtime (Milestone C) these will be derived from stream-json lines; the fake
// runtime synthesises them directly.
type OutputEvent struct {
	Kind string // e.g. "message", "tool_call", "error"
	Text string
}

// EventSink receives runtime output events incrementally during a run. It lets
// the Control Plane persist events one at a time (streaming) rather than
// accumulating them unboundedly in memory. Milestone C's Kiro runtime calls
// Emit for each stream-json line as it is parsed.
//
// Emit returns an error when the event could not be durably recorded. A runtime
// MUST treat a non-nil Emit error as fatal to the step (stop and surface it) so
// that a persistence failure cannot be silently masked and the run cannot be
// reported as successful. Implementations must treat event text as untrusted
// (see the engine's size/redaction handling).
type EventSink interface {
	Emit(ev OutputEvent) error
}

// StreamingRuntime is an optional interface a runtime may implement to emit
// output events incrementally via a sink, instead of (or in addition to)
// returning them in RunResult.Events. The Control Plane prefers Emit-based
// streaming when available to bound memory use.
type StreamingRuntime interface {
	AgentRuntime
	// ExecuteStreaming runs one step, calling sink.Emit for each output event as
	// it occurs. If sink.Emit returns an error, the runtime must abort the step
	// and return that error (persistence failure is fatal). The returned
	// RunResult should omit Events already emitted.
	ExecuteStreaming(ctx context.Context, req RunRequest, sink EventSink) (RunResult, error)
}

// RunResult is the outcome of an agent execution step.
type RunResult struct {
	// Done reports whether the agent considers the task complete. When false the
	// Control Plane may iterate again (subject to MaxIterations).
	Done bool
	// FinalText is the agent's final message (when Done).
	FinalText string
	// Events are the structured events produced during this step.
	Events []OutputEvent
}

// AgentRuntime executes agent steps. Implementations must respect context
// cancellation and deadlines.
type AgentRuntime interface {
	// Execute performs one agent execution step.
	Execute(ctx context.Context, req RunRequest) (RunResult, error)
	// Name identifies the runtime (for logging/attribution).
	Name() string
}
