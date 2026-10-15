// Package engine implements NAVETTE's Control Plane execution loop: it mediates
// pre-execution approval, runs the AgentRuntime, bounds execution with timeout /
// cancellation / iteration cap, and mediates result approval. Every approval and
// state change is persisted atomically with its event via the store.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/habuka036/navette/internal/approval"
	"github.com/habuka036/navette/internal/runtime"
	"github.com/habuka036/navette/internal/task"
)

// DefaultMaxIterations is the Control Plane loop cap applied when a task does not
// set its own (RD-4). This counts Control Plane turns, not the agent's internal
// iterations.
const DefaultMaxIterations = 10

// persistTimeout bounds the detached writes used to durably record terminal
// outcomes after the run context was cancelled or timed out.
const persistTimeout = 5 * time.Second

// maxEventTextBytes caps the stored size of a single runtime output event's text
// to avoid unbounded growth in the event log.
const maxEventTextBytes = 8 << 10 // 8 KiB

// Store is the persistence surface the engine needs. It is satisfied by
// *store.Store.
type Store interface {
	GetTask(ctx context.Context, id string) (task.Task, error)
	Transition(ctx context.Context, taskID string, to task.State, reason string, ev *task.Event) error
	// ApproveAndTransition atomically records the approval (row + event) and the
	// resulting state transition (+ event) in one transaction, validating the
	// expected current state and rejecting duplicate decisions.
	ApproveAndTransition(ctx context.Context, taskID string, expectFrom task.State, d task.Decision, toApproved, toRejected task.State) (task.State, error)
	AppendEvent(ctx context.Context, ev task.Event) error
}

// Engine runs tasks. Runtime and Gate are required (approvals are mandatory).
// There is intentionally no Provider field in Milestone B: the fake runtime
// needs no LLM provider, and Provider-less tasks must still run.
type Engine struct {
	Store   Store
	Runtime runtime.AgentRuntime
	Gate    approval.Gate
	// Finalizer, if set, confirms the approved result (snapshot/save/verify) AFTER
	// result approval is recorded but BEFORE the task becomes Succeeded. If it
	// returns an error the task becomes Failed, never Succeeded. When nil, an
	// approved result transitions to Succeeded directly.
	Finalizer ResultFinalizer
}

// ResultFinalizer confirms that an approved result is durably saved and matches
// what was approved. Finalize must be idempotent-safe and must not mutate real
// repositories. A non-nil error prevents the task from reaching Succeeded.
type ResultFinalizer interface {
	Finalize(ctx context.Context, taskID string) error
}

// Outcome bundles the final state and whether it represents success, so callers
// (such as the CLI) can choose an exit code without re-deriving it.
type Outcome struct {
	State        task.State
	Err          error
	PersistError bool // true when durably recording the outcome failed
}

// Succeeded reports whether the task reached the Succeeded terminal state.
func (o Outcome) Succeeded() bool {
	return o.State == task.Succeeded && o.Err == nil && !o.PersistError
}

// RunTask drives a task from Pending to a terminal or approval-waiting state.
// Flow:
//
//	Pending -> AwaitingPreApproval -> (approve) Running -> AwaitingResultApproval -> Succeeded
//	                               -> (reject)  Rejected
//
// The Runtime is launched ONLY after pre-approval. Timeout, cancellation, and
// the iteration cap bound the Running phase.
func (e *Engine) RunTask(ctx context.Context, taskID string) Outcome {
	if e.Runtime == nil {
		return Outcome{Err: errors.New("engine: AgentRuntime is not configured")}
	}
	if e.Gate == nil {
		return Outcome{Err: errors.New("engine: approval Gate is not configured")}
	}

	t, err := e.Store.GetTask(ctx, taskID)
	if err != nil {
		return Outcome{Err: err}
	}
	if t.State != task.Pending {
		return Outcome{Err: fmt.Errorf("engine: task %s is not Pending (state=%s)", taskID, t.State)}
	}

	// 1. Enter pre-approval.
	if err := e.Store.Transition(ctx, taskID, task.AwaitingPreApproval, "awaiting pre-execution approval", nil); err != nil {
		return Outcome{Err: err}
	}

	// 2. Request pre-execution approval. The Runtime is NOT started yet.
	preDec, err := e.Gate.Decide(ctx, approval.Request{TaskID: taskID, Kind: task.ApprovalPreExec, Goal: t.Goal})
	if err != nil {
		// Approval could not be obtained (e.g. cancelled / disconnected). Leave
		// the task in AwaitingPreApproval; do NOT auto-run.
		return Outcome{State: task.AwaitingPreApproval, Err: err}
	}
	preDec.Kind = task.ApprovalPreExec
	final, err := e.Store.ApproveAndTransition(ctx, taskID, task.AwaitingPreApproval, preDec, task.Running, task.Rejected)
	if err != nil {
		return Outcome{Err: err}
	}
	if final == task.Rejected {
		return Outcome{State: task.Rejected}
	}

	// 3. Bounded execution loop (task is now Running).
	outcome, runErr := e.runLoop(ctx, t)
	switch outcome {
	case task.TimedOut, task.Cancelled, task.Failed:
		// Persist the terminal outcome on a context detached from cancellation
		// but bounded by a short independent timeout, so the state/event is
		// durably recorded even when ctx was cancelled or timed out.
		if perr := e.persistTerminal(ctx, taskID, outcome, reasonFor(outcome, runErr)); perr != nil {
			return Outcome{State: outcome, Err: errors.Join(runErr, perr), PersistError: true}
		}
		return Outcome{State: outcome, Err: runErr}
	}

	// 4. Runtime finished normally: enter result approval.
	if err := e.Store.Transition(ctx, taskID, task.AwaitingResultApproval, "runtime finished; awaiting result approval", nil); err != nil {
		return Outcome{Err: err}
	}
	resDec, err := e.Gate.Decide(ctx, approval.Request{
		TaskID: taskID, Kind: task.ApprovalResult, Goal: t.Goal,
		Summary: "review the agent's artifacts (working-copy diff / run log) before accepting",
	})
	if err != nil {
		// Leave in AwaitingResultApproval; do not auto-accept.
		return Outcome{State: task.AwaitingResultApproval, Err: err}
	}
	resDec.Kind = task.ApprovalResult

	// Rejection path: record the decision and transition to Rejected atomically.
	if !resDec.Approved {
		final, aerr := e.Store.ApproveAndTransition(ctx, taskID, task.AwaitingResultApproval, resDec, task.Succeeded, task.Rejected)
		if aerr != nil {
			return Outcome{Err: aerr}
		}
		return Outcome{State: final}
	}

	// Approval path. The result approval decision is recorded first (so the
	// approval is auditable), but the task does NOT become Succeeded yet: it
	// moves to a Finalizing state while artifacts are durably confirmed. Only
	// after the Finalizer confirms the approved snapshot is saved and verified
	// does the task transition to Succeeded (atomically). If finalization fails,
	// the task becomes Failed (never Succeeded) and artifacts are preserved.
	if _, aerr := e.Store.ApproveAndTransition(ctx, taskID, task.AwaitingResultApproval, resDec, task.Finalizing, task.Rejected); aerr != nil {
		return Outcome{Err: aerr}
	}

	if e.Finalizer == nil {
		// No finalizer configured: nothing to confirm, so Succeeded directly.
		if err := e.Store.Transition(ctx, taskID, task.Succeeded, "result approved (no finalizer)", nil); err != nil {
			return Outcome{Err: err}
		}
		return Outcome{State: task.Succeeded}
	}

	if ferr := e.Finalizer.Finalize(ctx, taskID); ferr != nil {
		// Finalization failed (save/record/verify). Do NOT reach Succeeded.
		// Preserve artifacts and record Failed with the reason.
		if perr := e.persistTerminal(ctx, taskID, task.Failed, "result finalization failed: "+ferr.Error()); perr != nil {
			return Outcome{State: task.Failed, Err: errors.Join(ferr, perr), PersistError: true}
		}
		return Outcome{State: task.Failed, Err: fmt.Errorf("engine: result finalization failed: %w", ferr)}
	}

	if err := e.Store.Transition(ctx, taskID, task.Succeeded, "result approved and artifacts confirmed", nil); err != nil {
		return Outcome{Err: err}
	}
	return Outcome{State: task.Succeeded}
}

// runLoop executes agent steps with timeout, cancellation, and the iteration
// cap. It returns the running-phase outcome: empty State means "finished
// normally" (caller proceeds to result approval); otherwise a terminal outcome.
func (e *Engine) runLoop(ctx context.Context, t task.Task) (task.State, error) {
	maxIter := t.Constraints.MaxIterations
	if maxIter <= 0 {
		maxIter = DefaultMaxIterations
	}

	runCtx := ctx
	var cancel context.CancelFunc
	if t.Constraints.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, t.Constraints.Timeout)
		defer cancel()
	}

	for i := 0; i < maxIter; i++ {
		res, err := e.executeStep(runCtx, t)

		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return task.TimedOut, fmt.Errorf("engine: run timed out: %w", err)
			}
			if errors.Is(err, context.Canceled) {
				return task.Cancelled, fmt.Errorf("engine: run cancelled: %w", err)
			}
			return task.Failed, fmt.Errorf("engine: runtime error: %w", err)
		}

		// Even if the runtime reported success, honour cancellation / deadline:
		// do NOT proceed to result approval when the run context is done.
		if cerr := runCtx.Err(); cerr != nil {
			if errors.Is(cerr, context.DeadlineExceeded) {
				return task.TimedOut, fmt.Errorf("engine: run timed out: %w", cerr)
			}
			return task.Cancelled, fmt.Errorf("engine: run cancelled: %w", cerr)
		}

		if res.Done {
			return "", nil // finished normally
		}
	}
	// Iteration cap exceeded without completion: treat as a bounded failure.
	return task.Failed, fmt.Errorf("engine: exceeded max iterations (%d)", maxIter)
}

// executeStep runs one runtime step, persisting output events. If the runtime
// supports streaming (runtime.StreamingRuntime), events are persisted
// incrementally via a sink (bounded memory); a persistence failure in the sink
// aborts the step and is returned as an error (the run must not be reported
// successful). Otherwise the returned RunResult.Events are persisted after the
// step, and a persistence failure is returned as an error.
func (e *Engine) executeStep(ctx context.Context, t task.Task) (runtime.RunResult, error) {
	req := runtime.RunRequest{TaskID: t.ID, Goal: t.Goal, WorkDir: t.Constraints.WorkDir, Input: t.Input}
	if sr, ok := e.Runtime.(runtime.StreamingRuntime); ok {
		sink := &sqlSink{engine: e, ctx: ctx, taskID: t.ID}
		res, err := sr.ExecuteStreaming(ctx, req, sink)
		if err != nil {
			return res, err
		}
		if sink.err != nil {
			return res, fmt.Errorf("engine: runtime output persistence failed: %w", sink.err)
		}
		return res, nil
	}
	res, err := e.Runtime.Execute(ctx, req)
	if err != nil {
		return res, err
	}
	if perr := e.recordOutputs(ctx, t.ID, res.Events); perr != nil {
		return res, fmt.Errorf("engine: runtime output persistence failed: %w", perr)
	}
	return res, nil
}

// sqlSink persists runtime output events incrementally. On a persistence
// failure it records the error and returns it from Emit so the runtime aborts
// the step. It does not accumulate events in memory.
type sqlSink struct {
	engine *Engine
	ctx    context.Context
	taskID string
	err    error
}

func (s *sqlSink) Emit(ev runtime.OutputEvent) error {
	pctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), persistTimeout)
	defer cancel()
	if err := s.engine.Store.AppendEvent(pctx, task.Event{
		TaskID: s.taskID, Kind: task.EventRuntimeOutput, Reason: ev.Kind, Detail: encodeOutput(ev),
	}); err != nil {
		s.err = err
		return err
	}
	return nil
}

// recordOutputs persists runtime output events with a size cap on text. It
// streams one event at a time (no unbounded accumulation in memory) and returns
// the first persistence error encountered.
func (e *Engine) recordOutputs(ctx context.Context, taskID string, events []runtime.OutputEvent) error {
	if len(events) == 0 {
		return nil
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	for _, oe := range events {
		if err := e.Store.AppendEvent(pctx, task.Event{
			TaskID: taskID, Kind: task.EventRuntimeOutput, Reason: oe.Kind, Detail: encodeOutput(oe),
		}); err != nil {
			return err
		}
	}
	return nil
}

// persistTerminal durably records a terminal outcome using a detached, bounded
// context so cancellation/timeout of the run does not prevent recording.
func (e *Engine) persistTerminal(ctx context.Context, taskID string, to task.State, reason string) error {
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	return e.Store.Transition(pctx, taskID, to, reason, nil)
}

func reasonFor(s task.State, err error) string {
	if err != nil {
		return err.Error()
	}
	return string(s)
}

// encodeOutput serialises a runtime output event to a JSON detail payload with
// the text truncated to maxEventTextBytes. NAVETTE stores the text as-is up to
// the cap; it does not interpret or redact content. Secrets must not be routed
// through runtime output (NAVETTE never writes credentials/env to the event
// log; see SEC-002). A truncation marker is set when the text was clipped.
func encodeOutput(oe runtime.OutputEvent) []byte {
	text := oe.Text
	truncated := false
	if len(text) > maxEventTextBytes {
		text = text[:maxEventTextBytes]
		truncated = true
	}
	b, err := json.Marshal(struct {
		Kind      string `json:"kind"`
		Text      string `json:"text"`
		Truncated bool   `json:"truncated,omitempty"`
	}{Kind: oe.Kind, Text: text, Truncated: truncated})
	if err != nil {
		return nil
	}
	return b
}
