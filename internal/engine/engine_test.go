package engine_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/habuka036/navette/internal/approval"
	"github.com/habuka036/navette/internal/engine"
	"github.com/habuka036/navette/internal/runtime"
	"github.com/habuka036/navette/internal/runtime/fakeruntime"
	"github.com/habuka036/navette/internal/store"
	"github.com/habuka036/navette/internal/task"
)

// scriptedGate returns predetermined decisions in order.
type scriptedGate struct {
	decisions []bool
	calls     int
	err       error
}

func (g *scriptedGate) Decide(ctx context.Context, req approval.Request) (task.Decision, error) {
	if g.err != nil {
		return task.Decision{}, g.err
	}
	approve := false
	if g.calls < len(g.decisions) {
		approve = g.decisions[g.calls]
	}
	g.calls++
	return task.Decision{Kind: req.Kind, Approved: approve, By: "test"}, nil
}

func newEngine(t *testing.T, rt runtime.AgentRuntime, gate approval.Gate) (*engine.Engine, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &engine.Engine{Store: st, Runtime: rt, Gate: gate}, st
}

func seed(t *testing.T, st *store.Store, id string, c task.Constraints) {
	t.Helper()
	now := time.Now().UTC()
	err := st.CreateTask(context.Background(), task.Task{
		ID: id, Goal: "goal", State: task.Pending, Constraints: c, CreatedAt: now, UpdatedAt: now,
	}, task.Event{TaskID: id, Kind: task.EventNote, Reason: "created"})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
}

// trackingRuntime records whether Execute was ever called.
type trackingRuntime struct {
	mu      sync.Mutex
	started bool
	inner   runtime.AgentRuntime
}

func (r *trackingRuntime) Name() string { return "tracking" }
func (r *trackingRuntime) Execute(ctx context.Context, req runtime.RunRequest) (runtime.RunResult, error) {
	r.mu.Lock()
	r.started = true
	r.mu.Unlock()
	return r.inner.Execute(ctx, req)
}
func (r *trackingRuntime) wasStarted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started
}

func TestPreApprovalRequiredBeforeRuntime(t *testing.T) {
	rt := &trackingRuntime{inner: &fakeruntime.Fake{}}
	eng, st := newEngine(t, rt, &scriptedGate{decisions: []bool{false}})
	seed(t, st, "t1", task.Constraints{})

	out := eng.RunTask(context.Background(), "t1")
	if out.Err != nil {
		t.Fatalf("RunTask err: %v", out.Err)
	}
	if out.State != task.Rejected {
		t.Errorf("expected Rejected, got %s", out.State)
	}
	if rt.wasStarted() {
		t.Error("Runtime must NOT start when pre-approval is rejected")
	}
}

func TestHappyPath_ResultApprovalSucceeds(t *testing.T) {
	rt := &trackingRuntime{inner: &fakeruntime.Fake{Final: "done"}}
	eng, st := newEngine(t, rt, &scriptedGate{decisions: []bool{true, true}})
	seed(t, st, "t1", task.Constraints{})

	out := eng.RunTask(context.Background(), "t1")
	if out.Err != nil {
		t.Fatalf("RunTask err: %v", out.Err)
	}
	if !rt.wasStarted() {
		t.Error("Runtime should start after pre-approval")
	}
	if !out.Succeeded() || out.State != task.Succeeded {
		t.Errorf("expected Succeeded, got %s", out.State)
	}
}

func TestResultRejected(t *testing.T) {
	rt := &fakeruntime.Fake{}
	eng, st := newEngine(t, rt, &scriptedGate{decisions: []bool{true, false}})
	seed(t, st, "t1", task.Constraints{})

	out := eng.RunTask(context.Background(), "t1")
	if out.State != task.Rejected {
		t.Errorf("expected Rejected on result rejection, got %s", out.State)
	}
	if out.Succeeded() {
		t.Error("rejected result must not report success")
	}
}

func TestTimeout(t *testing.T) {
	rt := &fakeruntime.Fake{Delay: 500 * time.Millisecond}
	eng, st := newEngine(t, rt, &scriptedGate{decisions: []bool{true, true}})
	seed(t, st, "t1", task.Constraints{Timeout: 50 * time.Millisecond})

	out := eng.RunTask(context.Background(), "t1")
	if out.State != task.TimedOut {
		t.Errorf("expected TimedOut, got %s (err=%v)", out.State, out.Err)
	}
	// Persisted state must be durably recorded as TimedOut.
	got, _ := st.GetTask(context.Background(), "t1")
	if got.State != task.TimedOut {
		t.Errorf("persisted state should be TimedOut, got %s", got.State)
	}
}

func TestCancel(t *testing.T) {
	rt := &fakeruntime.Fake{Delay: 1 * time.Second}
	eng, st := newEngine(t, rt, &scriptedGate{decisions: []bool{true, true}})
	seed(t, st, "t1", task.Constraints{})

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()

	out := eng.RunTask(ctx, "t1")
	if out.State != task.Cancelled {
		t.Errorf("expected Cancelled, got %s", out.State)
	}
	got, _ := st.GetTask(context.Background(), "t1")
	if got.State != task.Cancelled {
		t.Errorf("persisted state should be Cancelled, got %s", got.State)
	}
}

// TestRuntimeSuccessButContextDone: runtime returns success, but the deadline
// already expired — the engine must NOT proceed to result approval.
func TestRuntimeSuccessButContextDone(t *testing.T) {
	// Fake returns Done immediately with no delay, but we set a 1ns timeout so
	// runCtx.Err() is set by the time we check.
	rt := &fakeruntime.Fake{Final: "done"}
	eng, st := newEngine(t, rt, &scriptedGate{decisions: []bool{true, true}})
	seed(t, st, "t1", task.Constraints{Timeout: 1})

	time.Sleep(2 * time.Millisecond) // ensure any deadline is in the past
	out := eng.RunTask(context.Background(), "t1")
	if out.State != task.TimedOut {
		t.Errorf("expected TimedOut when context done despite runtime success, got %s", out.State)
	}
}

func TestIterationCap(t *testing.T) {
	rt := &fakeruntime.Fake{Steps: 1_000_000}
	eng, st := newEngine(t, rt, &scriptedGate{decisions: []bool{true, true}})
	seed(t, st, "t1", task.Constraints{MaxIterations: 3})

	out := eng.RunTask(context.Background(), "t1")
	if out.State != task.Failed {
		t.Errorf("expected Failed on iteration cap, got %s (err=%v)", out.State, out.Err)
	}
}

func TestNoProviderNoMemoryStillRuns(t *testing.T) {
	rt := &fakeruntime.Fake{}
	eng, st := newEngine(t, rt, &scriptedGate{decisions: []bool{true, true}})
	seed(t, st, "t1", task.Constraints{})

	out := eng.RunTask(context.Background(), "t1")
	if out.Err != nil || out.State != task.Succeeded {
		t.Errorf("expected Succeeded without provider/memory, got %s (err=%v)", out.State, out.Err)
	}
}

func TestMissingRuntimeErrors(t *testing.T) {
	st, _ := store.Open(context.Background(), filepath.Join(t.TempDir(), "e.db"))
	defer st.Close()
	eng := &engine.Engine{Store: st, Runtime: nil, Gate: &scriptedGate{}}
	out := eng.RunTask(context.Background(), "x")
	if out.Err == nil {
		t.Error("expected error when Runtime is not configured")
	}
}

func TestGateErrorLeavesAwaitingState(t *testing.T) {
	rt := &trackingRuntime{inner: &fakeruntime.Fake{}}
	gate := &scriptedGate{err: errors.New("cli disconnected")}
	eng, st := newEngine(t, rt, gate)
	seed(t, st, "t1", task.Constraints{})

	out := eng.RunTask(context.Background(), "t1")
	if out.Err == nil {
		t.Error("expected error when gate fails")
	}
	if out.State != task.AwaitingPreApproval {
		t.Errorf("expected AwaitingPreApproval, got %s", out.State)
	}
	if rt.wasStarted() {
		t.Error("Runtime must not start when pre-approval could not be obtained")
	}
	got, _ := st.GetTask(context.Background(), "t1")
	if got.State != task.AwaitingPreApproval {
		t.Errorf("persisted state must be AwaitingPreApproval, got %s", got.State)
	}
}

// TestConcurrentRunNoDoubleRuntime verifies that two concurrent RunTask calls on
// the same task do not both start the runtime: exactly one proceeds, the other
// is rejected by the state machine / atomic approval.
func TestConcurrentRunNoDoubleRuntime(t *testing.T) {
	var starts int
	var mu sync.Mutex
	counting := &countingRuntime{onStart: func() { mu.Lock(); starts++; mu.Unlock() }}
	eng, st := newEngine(t, counting, &scriptedGate{decisions: []bool{true, true}})
	seed(t, st, "t1", task.Constraints{})

	var wg sync.WaitGroup
	outcomes := make([]engine.Outcome, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outcomes[i] = eng.RunTask(context.Background(), "t1")
		}(i)
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if starts > 1 {
		t.Errorf("runtime started %d times; must start at most once per task", starts)
	}
}

type countingRuntime struct {
	onStart func()
}

func (r *countingRuntime) Name() string { return "counting" }
func (r *countingRuntime) Execute(ctx context.Context, req runtime.RunRequest) (runtime.RunResult, error) {
	if r.onStart != nil {
		r.onStart()
	}
	return runtime.RunResult{Done: true, FinalText: "ok"}, nil
}

// emittingStreamingRuntime emits one output event (via the sink) then reports
// done. Used to exercise event-persistence failure propagation.
type emittingStreamingRuntime struct{}

func (r *emittingStreamingRuntime) Name() string { return "emitting" }
func (r *emittingStreamingRuntime) Execute(ctx context.Context, req runtime.RunRequest) (runtime.RunResult, error) {
	return runtime.RunResult{Done: true}, nil
}
func (r *emittingStreamingRuntime) ExecuteStreaming(ctx context.Context, req runtime.RunRequest, sink runtime.EventSink) (runtime.RunResult, error) {
	if err := sink.Emit(runtime.OutputEvent{Kind: "message", Text: "hi"}); err != nil {
		// A real runtime aborts the step on a sink error.
		return runtime.RunResult{}, err
	}
	return runtime.RunResult{Done: true, FinalText: "ok"}, nil
}

// failEventStore wraps the real store but fails AppendEvent for runtime output
// events, simulating a SQLite write failure during streaming persistence.
type failEventStore struct {
	*store.Store
	failRuntimeOutput bool
}

func (s *failEventStore) AppendEvent(ctx context.Context, ev task.Event) error {
	if s.failRuntimeOutput && ev.Kind == task.EventRuntimeOutput {
		return errors.New("simulated SQLite write failure")
	}
	return s.Store.AppendEvent(ctx, ev)
}

// TestEventPersistenceFailurePropagates verifies that when runtime output
// persistence fails, the run does NOT reach result approval or Succeeded; it
// fails safely.
func TestEventPersistenceFailurePropagates(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fs := &failEventStore{Store: st, failRuntimeOutput: true}

	// Track whether the result-approval gate was ever consulted.
	resultAsked := false
	gate := &probeGate{onResult: func() { resultAsked = true }}

	eng := &engine.Engine{Store: fs, Runtime: &emittingStreamingRuntime{}, Gate: gate}
	seed(t, st, "t1", task.Constraints{})

	out := eng.RunTask(context.Background(), "t1")
	if out.State == task.Succeeded {
		t.Error("run must not succeed when event persistence fails")
	}
	if out.State != task.Failed {
		t.Errorf("expected Failed on persistence failure, got %s (err=%v)", out.State, out.Err)
	}
	if resultAsked {
		t.Error("result approval must not be requested when event persistence failed")
	}
}

// probeGate approves pre-exec, and records whether result approval was asked.
type probeGate struct {
	onResult func()
}

func (g *probeGate) Decide(ctx context.Context, req approval.Request) (task.Decision, error) {
	if req.Kind == task.ApprovalResult && g.onResult != nil {
		g.onResult()
	}
	return task.Decision{Kind: req.Kind, Approved: true, By: "probe"}, nil
}

// failFinalizer always fails, simulating artifact save/verify failure after
// result approval.
type failFinalizer struct{ err error }

func (f *failFinalizer) Finalize(ctx context.Context, taskID string) error { return f.err }

// recordingFinalizer records the state observed at finalize time, to prove the
// task is NOT already Succeeded when finalization runs.
type recordingFinalizer struct {
	store     *store.Store
	seenState task.State
	returnErr error
}

func (f *recordingFinalizer) Finalize(ctx context.Context, taskID string) error {
	t, _ := f.store.GetTask(ctx, taskID)
	f.seenState = t.State
	return f.returnErr
}

func TestFinalizerFailure_NeverSucceeds(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	eng := &engine.Engine{
		Store:     st,
		Runtime:   &fakeruntime.Fake{Final: "done"},
		Gate:      &scriptedGate{decisions: []bool{true, true}}, // approve pre + result
		Finalizer: &failFinalizer{err: errors.New("artifact save/verify failed")},
	}
	seed(t, st, "t1", task.Constraints{})

	out := eng.RunTask(context.Background(), "t1")
	if out.State == task.Succeeded {
		t.Fatal("task must NOT be Succeeded when finalization fails")
	}
	if out.State != task.Failed {
		t.Errorf("expected Failed on finalization failure, got %s (err=%v)", out.State, out.Err)
	}
	// Persisted state must be Failed, and must never have been Succeeded.
	got, _ := st.GetTask(context.Background(), "t1")
	if got.State != task.Failed {
		t.Errorf("persisted state must be Failed, got %s", got.State)
	}
}

func TestFinalizer_RunsBeforeSuccess(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	fin := &recordingFinalizer{store: st}
	eng := &engine.Engine{
		Store:     st,
		Runtime:   &fakeruntime.Fake{Final: "done"},
		Gate:      &scriptedGate{decisions: []bool{true, true}},
		Finalizer: fin,
	}
	seed(t, st, "t1", task.Constraints{})

	out := eng.RunTask(context.Background(), "t1")
	if out.State != task.Succeeded {
		t.Fatalf("expected Succeeded, got %s", out.State)
	}
	// The finalizer must have observed the task in Finalizing (not Succeeded)
	// when it ran — proving confirmation precedes success.
	if fin.seenState != task.Finalizing {
		t.Errorf("finalizer should observe Finalizing before success, saw %s", fin.seenState)
	}
}
