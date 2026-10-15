// Command navette is the NAVETTE Control Plane CLI (Milestone B / PoC).
//
// It registers tasks, runs them through the pre-approval → execute → result-
// approval flow using a fake AgentRuntime (no real Kiro CLI is launched in this
// milestone), and renders a Markdown review view. SQLite is the source of truth.
//
// Subcommands:
//
//	navette --db PATH add    --goal "..." [--max-iter N] [--timeout D] [--workdir P]
//	navette --db PATH run    --task ID [--approve-pre=y/n] [--approve-result=y/n]
//	navette --db PATH list
//	navette --db PATH show   --task ID            # prints Markdown review view
//	navette --db PATH recover                     # Running -> Indeterminate on restart
//
// The --approve-* flags make approvals scriptable for end-to-end runs. When
// omitted, approvals are prompted interactively over stdin.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/habuka036/navette/internal/approval"
	"github.com/habuka036/navette/internal/engine"
	"github.com/habuka036/navette/internal/runtime/fakeruntime"
	"github.com/habuka036/navette/internal/runtime/kiroruntime"
	"github.com/habuka036/navette/internal/sandbox"
	"github.com/habuka036/navette/internal/store"
	"github.com/habuka036/navette/internal/task"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		var ee *exitError
		if errors.As(err, &ee) {
			os.Exit(ee.code)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: navette --db PATH <add|run|list|show|recover> [flags]")
}

func run(args []string) error {
	// Global flags (shared --db) are parsed per subcommand for simplicity.
	if len(args) < 1 {
		usage()
		return fmt.Errorf("missing subcommand")
	}

	// Allow --db before the subcommand: navette --db x add ...
	var dbPath string
	rest := args
	if args[0] == "--db" {
		if len(args) < 2 {
			return fmt.Errorf("--db requires a path")
		}
		dbPath = args[1]
		rest = args[2:]
	}
	if len(rest) < 1 {
		usage()
		return fmt.Errorf("missing subcommand")
	}
	sub := rest[0]
	subArgs := rest[1:]

	ctx := context.Background()
	if dbPath == "" {
		dbPath = "navette.db"
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	switch sub {
	case "add":
		return cmdAdd(ctx, st, subArgs)
	case "run":
		return cmdRun(ctx, st, subArgs)
	case "run-kiro":
		return cmdRunKiro(ctx, st, subArgs)
	case "list":
		return cmdList(ctx, st)
	case "show":
		return cmdShow(ctx, st, subArgs)
	case "recover":
		n, err := st.RecoverInterrupted(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("recovered %d interrupted task(s) to Indeterminate\n", n)
		return nil
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", sub)
	}
}

func cmdAdd(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	goal := fs.String("goal", "", "task goal (required)")
	maxIter := fs.Int("max-iter", 0, "max Control Plane iterations (0 = engine default)")
	timeout := fs.Duration("timeout", 0, "run timeout (e.g. 30s; 0 = none)")
	workdir := fs.String("workdir", "", "writable temp working copy path (.git excluded)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *goal == "" {
		return fmt.Errorf("add: --goal is required")
	}
	id := newID("task")
	now := time.Now().UTC()
	t := task.Task{
		ID: id, Goal: *goal, State: task.Pending,
		Constraints: task.Constraints{Timeout: *timeout, MaxIterations: *maxIter, WorkDir: *workdir},
		CreatedAt:   now, UpdatedAt: now,
	}
	if err := st.CreateTask(ctx, t, task.Event{TaskID: id, Kind: task.EventNote, Reason: "task created"}); err != nil {
		return err
	}
	fmt.Printf("created task %s (state=%s)\n", id, t.State)
	return nil
}

func cmdRun(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	taskID := fs.String("task", "", "task ID (required)")
	useFake := fs.Bool("fake", false, "use the fake AgentRuntime (required in Milestone B; no real agent is launched)")
	approvePre := fs.String("approve-pre", "", "TEST-ONLY scripted pre-approval (y/n); requires --fake")
	approveResult := fs.String("approve-result", "", "TEST-ONLY scripted result-approval (y/n); requires --fake")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *taskID == "" {
		return fmt.Errorf("run: --task is required")
	}

	// Milestone B only supports the fake runtime. Real Kiro CLI is Milestone C.
	if !*useFake {
		return fmt.Errorf("run: Milestone B requires --fake (real AgentRuntime is not available until Milestone C)")
	}

	// Scripted auto-approval is a TEST-ONLY convenience and is permitted ONLY
	// with the fake runtime. It can never bypass approval for a real agent.
	scripted := *approvePre != "" || *approveResult != ""
	var gate approval.Gate
	if scripted {
		if !*useFake {
			return fmt.Errorf("run: --approve-pre/--approve-result are test-only and require --fake")
		}
		gate = &scriptedGate{pre: parseYN(*approvePre), result: parseYN(*approveResult)}
	} else {
		gate = &approval.CLIGate{In: os.Stdin, Out: os.Stdout}
	}

	eng := &engine.Engine{
		Store:   st,
		Runtime: &fakeruntime.Fake{Final: "fake runtime completed the task"},
		Gate:    gate,
	}
	out := eng.RunTask(ctx, *taskID)

	// Report and choose an exit outcome. Success (exit 0) ONLY for Succeeded.
	switch {
	case out.Err != nil && out.State.AwaitsApproval():
		fmt.Printf("task %s is %s (awaiting human approval): %v\n", *taskID, out.State, out.Err)
		return &exitError{code: 3, msg: "awaiting approval"}
	case out.PersistError:
		fmt.Printf("task %s reached %s but PERSISTENCE FAILED: %v\n", *taskID, out.State, out.Err)
		return &exitError{code: 5, msg: "persistence failure"}
	case out.Err != nil:
		fmt.Printf("task %s ended in state=%s: %v\n", *taskID, out.State, out.Err)
		return &exitError{code: 1, msg: "run error"}
	case out.State == task.Rejected:
		fmt.Printf("task %s was REJECTED\n", *taskID)
		return &exitError{code: 4, msg: "rejected"}
	case out.State == task.Succeeded:
		fmt.Printf("task %s final state: Succeeded\n", *taskID)
		return nil
	default:
		// TimedOut / Cancelled / Failed and any other non-success terminal.
		fmt.Printf("task %s ended in state=%s (not successful)\n", *taskID, out.State)
		return &exitError{code: 1, msg: "not successful"}
	}
}

func cmdList(ctx context.Context, st *store.Store) error {
	tasks, err := st.ListTasks(ctx)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		fmt.Println("(no tasks)")
		return nil
	}
	for _, t := range tasks {
		fmt.Printf("%s  %-22s  %s\n", t.ID, t.State, t.Goal)
	}
	return nil
}

func cmdShow(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	taskID := fs.String("task", "", "task ID (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *taskID == "" {
		return fmt.Errorf("show: --task is required")
	}
	md, err := st.RenderTaskMarkdown(ctx, *taskID)
	if err != nil {
		return err
	}
	fmt.Println(md)
	return nil
}

func parseYN(s string) bool { return s == "y" || s == "Y" || s == "yes" }

// scriptedGate answers approvals from flags (TEST-ONLY; fake runtime only).
type scriptedGate struct{ pre, result bool }

func (g *scriptedGate) Decide(ctx context.Context, req approval.Request) (task.Decision, error) {
	approved := g.pre
	if req.Kind == task.ApprovalResult {
		approved = g.result
	}
	return task.Decision{Kind: req.Kind, Approved: approved, By: "cli-flag(test)"}, nil
}

// exitError carries a specific process exit code and a short message.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

// cmdRunKiro runs a task using the REAL Kiro CLI inside a bubblewrap sandbox.
// Approvals are always interactive here: there is NO auto-approve path for the
// real runtime (scripted approval is fake-only, enforced in cmdRun). The result
// approval shows the working-copy diff and recent run log before prompting.
func cmdRunKiro(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("run-kiro", flag.ContinueOnError)
	taskID := fs.String("task", "", "task ID (required)")
	src := fs.String("src", "", "source project directory to copy (.git excluded) (required)")
	engineV := fs.String("agent-engine", "v2", "Kiro agent engine: v2 or v3")
	kiroHome := fs.String("kiro-home", defaultKiroHome(), "dedicated Kiro HOME (persistent, authenticated)")
	refs := fs.String("ref", "", "optional read-only reference dir mounted at /refs/ref")
	artifacts := fs.String("artifacts", defaultArtifactsDir(), "base directory for durable per-run artifacts")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *taskID == "" || *src == "" {
		return fmt.Errorf("run-kiro: --task and --src are required")
	}

	// Fail closed if isolation is unavailable.
	if !sandbox.Available() {
		return &exitError{code: 6, msg: "sandbox unavailable (bwrap missing): refusing to run real agent"}
	}
	// Require the dedicated profile to be authenticated; never auto-login.
	if !kiroAuthenticated(ctx, *kiroHome) {
		fmt.Fprintf(os.Stderr, "Kiro dedicated profile is not authenticated.\n"+
			"Run this once yourself (interactive device flow):\n"+
			"  HOME=%s kiro-cli login --license free --use-device-flow\n", *kiroHome)
		return &exitError{code: 7, msg: "dedicated Kiro profile not authenticated"}
	}

	// Prepare a disposable .git-excluded work copy. Fail closed on error.
	workCopy, err := sandbox.PrepareWorkCopy(*src)
	if err != nil {
		return &exitError{code: 6, msg: "failed to prepare isolated work copy: " + err.Error()}
	}
	// The work copy is NOT unconditionally deleted. Artifacts are preserved to a
	// durable per-run directory below (even on rejection/interruption); the
	// transient work copy is removed only after artifacts are saved.

	runID := newID("run")
	if err := st.CreateRun(ctx, runID, *taskID); err != nil {
		sandbox.RemoveWorkCopy(workCopy)
		return err
	}

	sbx := sandbox.Config{
		WorkCopy:      workCopy,
		DedicatedHome: *kiroHome,
		ROBinaries:    sandbox.DefaultROBinaries(),
	}
	if *refs != "" {
		sbx.ReadOnly = map[string]string{*refs: "/refs/ref"}
	}

	rt := &kiroruntime.Runtime{Sandbox: sbx, AgentEngine: *engineV}
	gate := newKiroReviewGate(os.Stdin, os.Stdout, st, *src, workCopy, sbx.Describe())

	// The finalizer runs AFTER result approval is recorded but BEFORE the task
	// becomes Succeeded. It saves the approved snapshot to a durable per-run
	// directory, records it in SQLite, and verifies the saved artifacts match
	// the approved fingerprint. Any failure here makes the task Failed (never
	// Succeeded) and preserves the work copy + any partial save for recovery.
	artifactDir := filepath.Join(*artifacts, runID)
	fin := &kiroFinalizer{
		store:       st,
		runID:       runID,
		workCopy:    workCopy,
		artifactDir: artifactDir,
		gate:        gate,
	}

	eng := &engine.Engine{Store: st, Runtime: rt, Gate: gate, Finalizer: fin}
	out := eng.RunTask(ctx, *taskID)

	// Record the run's terminal outcome. A failure to record it must NOT be
	// reported as success: surface it as a persistence error (non-zero exit).
	finishErr := st.FinishRun(ctx, runID, task.Outcome(out.State))
	if finishErr != nil {
		fmt.Fprintf(os.Stderr, "ERROR: failed to record run completion for %s: %v\n", runID, finishErr)
	}

	// Determine whether it is safe to remove the transient work copy. We only
	// remove it when the run reached a successful, fully-confirmed terminal
	// state AND artifacts were durably saved. Otherwise (failure, rejection,
	// awaiting approval, save/record error) we KEEP the work copy and any saved
	// artifacts so the state is recoverable and auditable.
	safeToRemove := out.State == task.Succeeded && fin.saved && fin.recorded && fin.verified && finishErr == nil
	if safeToRemove {
		sandbox.RemoveWorkCopy(workCopy)
	} else if out.State == task.Rejected {
		// On explicit rejection, still try to preserve a copy of the artifacts
		// for audit, but keep the work copy too (do not delete) for recovery.
		if !fin.saved {
			if h, serr := sandbox.SaveArtifacts(workCopy, artifactDir); serr == nil {
				_ = st.SetRunArtifacts(ctx, runID, artifactDir, h)
				fmt.Printf("artifacts retained for audit: %s\n", artifactDir)
			}
		}
		fmt.Printf("work copy retained for recovery: %s\n", workCopy)
	} else {
		fmt.Printf("work copy retained for recovery: %s\n", workCopy)
		if fin.saved {
			fmt.Printf("artifacts retained: %s\n", artifactDir)
		}
	}

	switch {
	case out.Err != nil && out.State.AwaitsApproval():
		fmt.Printf("task %s is %s (awaiting human approval): %v\n", *taskID, out.State, out.Err)
		return &exitError{code: 3, msg: "awaiting approval"}
	case out.PersistError:
		fmt.Printf("task %s reached %s but PERSISTENCE FAILED: %v\n", *taskID, out.State, out.Err)
		return &exitError{code: 5, msg: "persistence failure"}
	case finishErr != nil:
		// The run outcome could not be durably recorded. Do not report success;
		// the work copy is retained for recovery.
		fmt.Printf("task %s reached %s but run-completion recording FAILED: %v\n", *taskID, out.State, finishErr)
		return &exitError{code: 5, msg: "run completion persistence failure"}
	case out.State == task.Failed && fin.finalizeAttempted:
		// Finalization (save/record/verify) failed after approval: never success.
		fmt.Printf("task %s FAILED during result finalization: %v\n", *taskID, out.Err)
		return &exitError{code: 8, msg: "result finalization failed"}
	case out.Err != nil:
		fmt.Printf("task %s ended in state=%s: %v\n", *taskID, out.State, out.Err)
		return &exitError{code: 1, msg: "run error"}
	case out.State == task.Rejected:
		fmt.Printf("task %s was REJECTED (artifacts/work copy retained for audit)\n", *taskID)
		return &exitError{code: 4, msg: "rejected"}
	case out.State == task.Succeeded:
		fmt.Printf("task %s final state: Succeeded (artifacts preserved; NOT applied to any real repo)\n", *taskID)
		return nil
	default:
		fmt.Printf("task %s ended in state=%s (not successful)\n", *taskID, out.State)
		return &exitError{code: 1, msg: "not successful"}
	}
}

func defaultKiroHome() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".navette", "kiro-home")
}

func defaultArtifactsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".navette", "artifacts")
}

// kiroFinalizer confirms an approved result: it saves the approved snapshot to a
// durable per-run directory, records it in SQLite, and verifies the saved
// artifacts match the fingerprint captured at approval time. Any failure returns
// an error (so the engine fails the task, never Succeeded) and preserves the
// work copy + any partial save for recovery (nothing is deleted here).
type kiroFinalizer struct {
	store       *store.Store
	runID       string
	workCopy    string
	artifactDir string
	gate        *kiroReviewGate

	finalizeAttempted bool
	saved             bool
	recorded          bool
	verified          bool
	savedHash         string
}

func (f *kiroFinalizer) Finalize(ctx context.Context, taskID string) error {
	f.finalizeAttempted = true

	// The gate must have captured the approved fingerprint before this runs.
	if f.gate.approvedHash == "" {
		return fmt.Errorf("no approved fingerprint recorded; refusing to finalize")
	}

	// Save the approved snapshot durably. On failure, keep everything.
	savedHash, err := sandbox.SaveArtifacts(f.workCopy, f.artifactDir)
	if err != nil {
		return fmt.Errorf("save artifacts: %w", err)
	}
	f.saved = true
	f.savedHash = savedHash

	// Record the artifact location + fingerprint in SQLite.
	if err := f.store.SetRunArtifacts(ctx, f.runID, f.artifactDir, savedHash); err != nil {
		return fmt.Errorf("record artifacts: %w", err)
	}
	f.recorded = true

	// Verify the saved artifacts match exactly what was approved.
	if savedHash != f.gate.approvedHash {
		return fmt.Errorf("approved/saved artifact mismatch (approved=%s saved=%s)", f.gate.approvedHash, savedHash)
	}
	f.verified = true
	fmt.Printf("artifacts saved and verified: %s\n", f.artifactDir)
	return nil
}

// kiroAuthenticated reports whether the dedicated profile is logged in. It does
// NOT read or log credential values.
func kiroAuthenticated(ctx context.Context, kiroHome string) bool {
	cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "kiro-cli", "whoami")
	cmd.Env = append(os.Environ(), "HOME="+kiroHome)
	outb, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(outb)), "logged in")
}

// kiroReviewGate is the interactive approval gate for the real runtime. Pre-exec
// shows the sandbox summary; result approval renders the working-copy diff and
// recent run log before prompting. There is no auto-approve. It owns a single
// buffered reader so a buffered first read does not swallow the second prompt's
// input.
type kiroReviewGate struct {
	reader       *bufio.Reader
	out          io.Writer
	store        *store.Store
	src          string
	workCopy     string
	sandboxDesc  string
	approvedHash string // fingerprint of the work copy at the moment of result approval
}

func newKiroReviewGate(in io.Reader, out io.Writer, st *store.Store, src, workCopy, desc string) *kiroReviewGate {
	return &kiroReviewGate{reader: bufio.NewReader(in), out: out, store: st, src: src, workCopy: workCopy, sandboxDesc: desc}
}

func (g *kiroReviewGate) Decide(ctx context.Context, req approval.Request) (task.Decision, error) {
	if req.Kind == task.ApprovalPreExec {
		fmt.Fprintf(g.out, "\n[PRE-EXEC APPROVAL] task=%s\n  goal: %s\n", req.TaskID, req.Goal)
		fmt.Fprintf(g.out, "  sandbox:\n%s", indent(g.sandboxDesc))
		fmt.Fprintf(g.out, "  NOTE: network is OPEN to the Kiro cloud backend; destinations/content are NOT restricted.\n")
	} else {
		fmt.Fprintf(g.out, "\n[RESULT APPROVAL] task=%s\n", req.TaskID)
		// A unified diff is REQUIRED for result approval. If it cannot be
		// produced, refuse approval (fail-closed): the human cannot review what
		// they would be accepting.
		diffs, derr := sandbox.UnifiedDiff(g.src, g.workCopy)
		if derr != nil {
			fmt.Fprintf(g.out, "  diff unavailable: %v\n  -> refusing result approval (cannot review changes).\n", derr)
			return task.Decision{}, fmt.Errorf("result approval: diff unavailable: %w", derr)
		}
		fmt.Fprintf(g.out, "%s", indent(sandbox.RenderUnifiedDiffs(diffs)))
		g.printRecentLog(ctx, req.TaskID)
		// Record the fingerprint of exactly what is being reviewed/approved, so
		// the caller can verify the saved artifacts match the approved state.
		if fp, ferr := sandbox.Fingerprint(g.workCopy); ferr == nil {
			g.approvedHash = fp
		} else {
			fmt.Fprintf(g.out, "  fingerprint unavailable: %v\n  -> refusing result approval.\n", ferr)
			return task.Decision{}, fmt.Errorf("result approval: fingerprint unavailable: %w", ferr)
		}
		fmt.Fprintf(g.out, "  NOTE: approving ACCEPTS artifacts only. It does NOT apply changes to any real repo or create a git commit.\n")
	}

	label := "pre-execution"
	if req.Kind == task.ApprovalResult {
		label = "result"
	}
	fmt.Fprintf(g.out, "  approve %s? [y/N]: ", label)

	type res struct {
		text string
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		line, err := g.reader.ReadString('\n')
		ch <- res{text: line, err: err}
	}()
	select {
	case <-ctx.Done():
		return task.Decision{}, ctx.Err()
	case r := <-ch:
		if r.err != nil && r.text == "" {
			return task.Decision{}, fmt.Errorf("approval: read input: %w", r.err)
		}
		approved := strings.EqualFold(strings.TrimSpace(r.text), "y")
		return task.Decision{Kind: req.Kind, Approved: approved, By: "cli-user"}, nil
	}
}

func (g *kiroReviewGate) printRecentLog(ctx context.Context, taskID string) {
	evs, err := g.store.Events(ctx, taskID)
	if err != nil {
		return
	}
	fmt.Fprintf(g.out, "  recent run log:\n")
	// Show the last few runtime output events.
	start := 0
	if len(evs) > 12 {
		start = len(evs) - 12
	}
	for _, e := range evs[start:] {
		if e.Kind == task.EventRuntimeOutput || e.Kind == task.EventStateTransition {
			fmt.Fprintf(g.out, "    - %s %s\n", e.Kind, e.Reason)
		}
	}
}

func indent(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		fmt.Fprintf(&b, "    %s\n", line)
	}
	return b.String()
}

func newID(prefix string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return prefix + "-" + hex.EncodeToString(b[:])
}
