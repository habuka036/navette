// Package kiroruntime implements the AgentRuntime port by launching the real
// Kiro CLI inside a bubblewrap sandbox and parsing its stream-json output.
//
// Safety properties (Milestone C):
//   - Runs inside internal/sandbox (work-copy-only writable, refs read-only,
//     tmpfs /tmp, dedicated HOME, no SSH agent / Docker socket exposure).
//   - Success is determined by stream-json runFinished.status / runError, never
//     by the process exit code.
//   - The CLI is launched in its own process group; on cancellation/timeout the
//     whole group is signalled so child/grandchild processes (the ACP server,
//     tool subprocesses) are reclaimed, not just the direct child.
//   - --trust-tools= is passed (empty) but is NOT relied upon as an approval
//     gate; isolation is the safety boundary. Approvals are enforced by the
//     Control Plane before this runtime is ever launched.
package kiroruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"syscall"
	"time"

	"github.com/habuka036/navette/internal/runtime"
	"github.com/habuka036/navette/internal/sandbox"
)

// Runtime launches Kiro CLI in a sandbox.
type Runtime struct {
	// Sandbox describes the isolation (work copy, dedicated HOME, read-only refs).
	Sandbox sandbox.Config
	// AgentEngine selects the Kiro agent engine ("v2" or "v3"). Must not be
	// hardcoded; defaults to "v2" when empty.
	AgentEngine string
	// Model optionally overrides the model.
	Model string
	// KillGrace is how long to wait after SIGTERM before SIGKILL on the process
	// group. Defaults to 5s.
	KillGrace time.Duration

	// cmdFactory builds the command to run. Defaults to the sandbox builder.
	// Overridable in tests to exercise process-group reclamation without the
	// real Kiro CLI / bwrap.
	cmdFactory func(ctx context.Context, argv ...string) (*exec.Cmd, error)
}

var _ runtime.AgentRuntime = (*Runtime)(nil)
var _ runtime.StreamingRuntime = (*Runtime)(nil)

// Name identifies the runtime.
func (r *Runtime) Name() string { return "kiro-cli" }

// Execute runs one Kiro CLI invocation without streaming (events are collected
// and returned in RunResult.Events).
func (r *Runtime) Execute(ctx context.Context, req runtime.RunRequest) (runtime.RunResult, error) {
	var events []runtime.OutputEvent
	emit := func(ev runtime.OutputEvent) error {
		events = append(events, ev)
		return nil
	}
	res, err := r.run(ctx, req, emit)
	res.Events = events
	return res, err
}

// ExecuteStreaming runs one Kiro CLI invocation, emitting each output event to
// sink as it is parsed (bounded memory). If sink.Emit returns an error, the run
// is aborted and that error is returned (persistence failure is fatal).
func (r *Runtime) ExecuteStreaming(ctx context.Context, req runtime.RunRequest, sink runtime.EventSink) (runtime.RunResult, error) {
	emit := func(ev runtime.OutputEvent) error { return nil }
	if sink != nil {
		emit = sink.Emit
	}
	return r.run(ctx, req, emit)
}

func (r *Runtime) run(ctx context.Context, req runtime.RunRequest, emit func(runtime.OutputEvent) error) (runtime.RunResult, error) {
	engine := r.AgentEngine
	if engine == "" {
		engine = "v2"
	}
	grace := r.KillGrace
	if grace <= 0 {
		grace = 5 * time.Second
	}

	argv := []string{
		"kiro-cli", "chat", req.Goal,
		"--no-interactive",
		"--output-format", "stream-json",
		"--agent-engine", engine,
		"--trust-tools=", // not an approval gate; isolation is the boundary
	}
	if r.Model != "" {
		argv = append(argv, "--model", r.Model)
	}

	// Build the sandboxed command. Fail closed if isolation is unavailable.
	build := r.cmdFactory
	if build == nil {
		build = r.Sandbox.Command
	}
	cmd, err := build(ctx, argv...)
	if err != nil {
		return runtime.RunResult{}, fmt.Errorf("kiroruntime: build sandbox command: %w", err)
	}

	// Launch in its own process group so we can reclaim child + grandchildren.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return runtime.RunResult{}, fmt.Errorf("kiroruntime: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return runtime.RunResult{}, fmt.Errorf("kiroruntime: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return runtime.RunResult{}, fmt.Errorf("kiroruntime: start: %w", err)
	}
	pgid := cmd.Process.Pid // child is group leader (Setpgid)

	// exited is closed exactly once, after cmd.Wait() returns.
	exited := make(chan struct{})
	// reclaimTrigger lets the main path proactively request process-group
	// reclamation (e.g. on parse/emit failure), independent of ctx cancellation.
	reclaimTrigger := make(chan struct{})
	reclaimerDone := make(chan struct{})
	go func() {
		defer close(reclaimerDone)
		select {
		case <-ctx.Done():
			reclaimGroup(pgid, grace, exited)
		case <-reclaimTrigger:
			reclaimGroup(pgid, grace, exited)
		case <-exited:
		}
	}()

	// Drain stderr in the background (best-effort).
	stderrCh := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(stderr)
		stderrCh <- string(b)
	}()

	// Parse stream-json from stdout, emitting events as they arrive. A non-nil
	// parseErr means a parse/protocol error OR an emit (persistence) failure; in
	// all cases the run must not be reported success AND any still-running
	// process group must be stopped (cmd.Wait on the direct child is NOT
	// sufficient — grandchildren such as the ACP server or tool subprocesses may
	// still be alive).
	parsed, parseErr := ParseStream(stdout, emit)

	if parseErr != nil {
		// Proactively reclaim the whole process group; do not merely wait for the
		// direct child, which can exit while grandchildren keep running.
		select {
		case reclaimTrigger <- struct{}{}:
		default:
		}
	}

	// Drain any remaining stdout so the child is not blocked on a full pipe.
	_, _ = io.Copy(io.Discard, stdout)

	waitErr := cmd.Wait()
	close(exited)   // stop the reclaimer's graceful escalation (direct child reaped)
	<-reclaimerDone // ensure the reclaimer goroutine has returned
	stderrText := <-stderrCh

	// cmd.Wait() reaps ONLY the direct child. A SIGTERM-ignoring or detached
	// grandchild (ACP server / tool subprocess) may still be alive in the group.
	// Unconditionally SIGKILL the whole process group now to guarantee no group
	// member lingers after this runtime returns. This is safe: by the time we
	// return, the run is over and any remaining group members are stragglers.
	sweepGroup(pgid)

	res := runtime.RunResult{Done: parsed.Finished}

	// Persistence/emit failure is fatal and takes precedence: do not mask it.
	if parseErr != nil {
		if errors.Is(parseErr, ErrProtocol) {
			return res, fmt.Errorf("kiroruntime: %w", parseErr)
		}
		return res, fmt.Errorf("kiroruntime: event persistence/parse failed: %w", parseErr)
	}

	// Determine success from the stream, NOT the exit code.
	switch {
	case parsed.Errored:
		return res, fmt.Errorf("kiroruntime: run error [%s]: %s", parsed.ErrorStage, parsed.ErrorMessage)
	case !parsed.Finished:
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		msg := "no runFinished event"
		if stderrText != "" {
			msg += ": " + truncateForLog(stderrText)
		}
		return res, errors.New("kiroruntime: " + msg)
	case !parsed.Succeeded():
		return res, fmt.Errorf("kiroruntime: run finished with status %q", parsed.Status)
	}

	res.FinalText = parsed.FinalText
	_ = waitErr // exit code intentionally not used to decide success
	return res, nil
}

// sweepGroup sends SIGKILL to the whole process group as a final reap. The
// direct child has already been reaped by cmd.Wait(); this removes any remaining
// group members (SIGTERM-ignoring or detached grandchildren). Errors (e.g. the
// group no longer exists) are ignored.
//
// Note on guarantees: this reaps processes that remain in the original process
// group. A grandchild that deliberately escapes the group (its own setsid /
// double-fork into a new session) is NOT covered by a process-group signal; such
// escape is outside the guaranteed reclamation boundary (documented in
// docs/specs/harness/notes/milestone-c-sandbox.md). Within bubblewrap with
// --die-with-parent, the sandbox teardown provides an additional backstop.
func sweepGroup(pgid int) {
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

// reclaimGroup signals the whole process group: SIGTERM, then SIGKILL after
// grace unless the group has already exited. It reclaims the ACP server and
// tool subprocesses (Milestone A finding: grandchildren can outlive the direct
// child). The exited channel is closed by the caller once cmd.Wait() returns.
func reclaimGroup(pgid int, grace time.Duration, exited <-chan struct{}) {
	// Negative pid => signal the whole process group.
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-exited:
		// Process group reaped within grace; nothing more to do.
		return
	case <-timer.C:
		// Grace elapsed and the group has not exited: force kill.
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
}
