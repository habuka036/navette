package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/habuka036/navette/internal/store"
	"github.com/habuka036/navette/internal/task"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "navette.db")
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func seedTask(t *testing.T, st *store.Store, id string) {
	t.Helper()
	now := time.Now().UTC()
	tk := task.Task{
		ID: id, Goal: "do a thing", State: task.Pending,
		Constraints: task.Constraints{MaxIterations: 10, WorkDir: "/tmp/wc"},
		CreatedAt:   now, UpdatedAt: now,
	}
	ev := task.Event{TaskID: id, Kind: task.EventNote, Reason: "created"}
	if err := st.CreateTask(context.Background(), tk, ev); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
}

func TestCreateAndGet(t *testing.T) {
	st := openStore(t)
	seedTask(t, st, "t1")
	got, err := st.GetTask(context.Background(), "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.State != task.Pending || got.Constraints.MaxIterations != 10 || got.Constraints.WorkDir != "/tmp/wc" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestTransition_AtomicAndValid(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	seedTask(t, st, "t1")

	if err := st.Transition(ctx, "t1", task.AwaitingPreApproval, "need approval", nil); err != nil {
		t.Fatalf("transition to AwaitingPreApproval: %v", err)
	}
	got, _ := st.GetTask(ctx, "t1")
	if got.State != task.AwaitingPreApproval {
		t.Fatalf("expected AwaitingPreApproval, got %s", got.State)
	}
	// A transition event must have been recorded atomically.
	evs, err := st.Events(ctx, "t1")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var sawTransition bool
	for _, e := range evs {
		if e.Kind == task.EventStateTransition && e.To == task.AwaitingPreApproval {
			sawTransition = true
		}
	}
	if !sawTransition {
		t.Error("expected a state.transition event recorded with the transition")
	}
}

func TestTransition_InvalidRejectedStateUnchanged(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	seedTask(t, st, "t1")

	// Pending -> Succeeded is not allowed.
	err := st.Transition(ctx, "t1", task.Succeeded, "skip", nil)
	var invalid task.ErrInvalidTransition
	if !errors.As(err, &invalid) {
		t.Fatalf("expected ErrInvalidTransition, got %v", err)
	}
	got, _ := st.GetTask(ctx, "t1")
	if got.State != task.Pending {
		t.Errorf("state must be unchanged after invalid transition, got %s", got.State)
	}
	// No transition event should have been written.
	evs, _ := st.Events(ctx, "t1")
	for _, e := range evs {
		if e.Kind == task.EventStateTransition {
			t.Error("invalid transition must not record a transition event")
		}
	}
}

func TestRecoverInterrupted_RunningToIndeterminate(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	seedTask(t, st, "t1")
	// Drive to Running.
	mustTransition(t, st, "t1", task.AwaitingPreApproval)
	mustTransition(t, st, "t1", task.Running)

	n, err := st.RecoverInterrupted(ctx)
	if err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 recovered task, got %d", n)
	}
	got, _ := st.GetTask(ctx, "t1")
	if got.State != task.Indeterminate {
		t.Errorf("expected Indeterminate after recovery, got %s", got.State)
	}
	// Indeterminate must not auto-resume: Running is not reachable from it.
	if task.CanTransition(task.Indeterminate, task.Running) {
		t.Error("Indeterminate must never transition to Running")
	}
}

func TestRecoverInterrupted_FinalizingToIndeterminate(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	seedTask(t, st, "t1")
	// Drive to Finalizing (interrupted mid-finalization).
	mustTransition(t, st, "t1", task.AwaitingPreApproval)
	mustTransition(t, st, "t1", task.Running)
	mustTransition(t, st, "t1", task.AwaitingResultApproval)
	mustTransition(t, st, "t1", task.Finalizing)

	n, err := st.RecoverInterrupted(ctx)
	if err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 recovered task, got %d", n)
	}
	got, _ := st.GetTask(ctx, "t1")
	if got.State != task.Indeterminate {
		t.Errorf("expected Indeterminate after Finalizing interruption, got %s", got.State)
	}
	// Must NOT auto-advance to Succeeded.
	if task.CanTransition(task.Indeterminate, task.Succeeded) {
		t.Error("Indeterminate must never auto-transition to Succeeded")
	}
}

func TestSetRunArtifacts_UnknownRunErrors(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if err := st.SetRunArtifacts(ctx, "no-such-run", "/x", "hash"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound for unknown run, got %v", err)
	}
}

func TestFinishRun_UnknownRunErrors(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if err := st.FinishRun(ctx, "no-such-run", task.OutcomeSucceeded); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound for unknown run, got %v", err)
	}
}

func TestRunArtifacts_RoundTrip(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	seedTask(t, st, "t1")
	if err := st.CreateRun(ctx, "run1", "t1"); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.SetRunArtifacts(ctx, "run1", "/art/run1", "abc123"); err != nil {
		t.Fatalf("SetRunArtifacts: %v", err)
	}
	if err := st.FinishRun(ctx, "run1", task.OutcomeSucceeded); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	dir, hash, err := st.RunArtifacts(ctx, "run1")
	if err != nil {
		t.Fatalf("RunArtifacts: %v", err)
	}
	if dir != "/art/run1" || hash != "abc123" {
		t.Errorf("round-trip mismatch: dir=%q hash=%q", dir, hash)
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "persist.db")
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	now := time.Now().UTC()
	_ = st.CreateTask(ctx, task.Task{ID: "p1", Goal: "g", State: task.AwaitingPreApproval, CreatedAt: now, UpdatedAt: now},
		task.Event{TaskID: "p1", Kind: task.EventNote})
	_ = st.Close()

	// Reopen and confirm the unapproved state is preserved.
	st2, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	got, err := st2.GetTask(ctx, "p1")
	if err != nil {
		t.Fatalf("GetTask after reopen: %v", err)
	}
	if got.State != task.AwaitingPreApproval {
		t.Errorf("unapproved state must persist across reopen, got %s", got.State)
	}
}

func mustTransition(t *testing.T, st *store.Store, id string, to task.State) {
	t.Helper()
	if err := st.Transition(context.Background(), id, to, "", nil); err != nil {
		t.Fatalf("transition %s: %v", to, err)
	}
}
