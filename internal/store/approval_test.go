package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/habuka036/navette/internal/store"
	"github.com/habuka036/navette/internal/task"
)

func TestApproveAndTransition_Atomic(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	seedTask(t, st, "t1")
	mustTransition(t, st, "t1", task.AwaitingPreApproval)

	final, err := st.ApproveAndTransition(ctx, "t1", task.AwaitingPreApproval,
		task.Decision{Kind: task.ApprovalPreExec, Approved: true, By: "u", At: time.Now()},
		task.Running, task.Rejected)
	if err != nil {
		t.Fatalf("ApproveAndTransition: %v", err)
	}
	if final != task.Running {
		t.Errorf("expected Running, got %s", final)
	}
	// State, approval, approval event, and transition event must all exist.
	got, _ := st.GetTask(ctx, "t1")
	if got.State != task.Running {
		t.Errorf("state should be Running, got %s", got.State)
	}
	evs, _ := st.Events(ctx, "t1")
	var sawApproval, sawTransition bool
	for _, e := range evs {
		if e.Kind == task.EventApproval {
			sawApproval = true
		}
		if e.Kind == task.EventStateTransition && e.To == task.Running {
			sawTransition = true
		}
	}
	if !sawApproval || !sawTransition {
		t.Errorf("expected both approval and transition events (approval=%v transition=%v)", sawApproval, sawTransition)
	}
}

func TestApproveAndTransition_RejectPath(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	seedTask(t, st, "t1")
	mustTransition(t, st, "t1", task.AwaitingPreApproval)

	final, err := st.ApproveAndTransition(ctx, "t1", task.AwaitingPreApproval,
		task.Decision{Kind: task.ApprovalPreExec, Approved: false, By: "u"},
		task.Running, task.Rejected)
	if err != nil {
		t.Fatalf("ApproveAndTransition: %v", err)
	}
	if final != task.Rejected {
		t.Errorf("expected Rejected, got %s", final)
	}
}

func TestApproveAndTransition_UnexpectedState(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	seedTask(t, st, "t1") // still Pending, not AwaitingPreApproval

	_, err := st.ApproveAndTransition(ctx, "t1", task.AwaitingPreApproval,
		task.Decision{Kind: task.ApprovalPreExec, Approved: true, By: "u"},
		task.Running, task.Rejected)
	var ue store.ErrUnexpectedState
	if !errors.As(err, &ue) {
		t.Fatalf("expected ErrUnexpectedState, got %v", err)
	}
	// Nothing must have been written: state unchanged, no approval/transition events.
	got, _ := st.GetTask(ctx, "t1")
	if got.State != task.Pending {
		t.Errorf("state must remain Pending, got %s", got.State)
	}
	evs, _ := st.Events(ctx, "t1")
	for _, e := range evs {
		if e.Kind == task.EventApproval || (e.Kind == task.EventStateTransition) {
			t.Errorf("no approval/transition event should exist after unexpected-state rejection, saw %s", e.Kind)
		}
	}
}

func TestApproveAndTransition_DuplicateRejected(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	seedTask(t, st, "t1")
	mustTransition(t, st, "t1", task.AwaitingPreApproval)

	// First pre-exec approval succeeds (-> Running).
	if _, err := st.ApproveAndTransition(ctx, "t1", task.AwaitingPreApproval,
		task.Decision{Kind: task.ApprovalPreExec, Approved: true, By: "u"},
		task.Running, task.Rejected); err != nil {
		t.Fatalf("first approval: %v", err)
	}
	// A second pre-exec approval attempt must be rejected as duplicate. We expect
	// AwaitingPreApproval as the "from" but the task is now Running; the duplicate
	// guard should fire regardless, protecting against double decisions.
	_, err := st.ApproveAndTransition(ctx, "t1", task.Running,
		task.Decision{Kind: task.ApprovalPreExec, Approved: true, By: "u"},
		task.Running, task.Rejected)
	var dup store.ErrDuplicateApproval
	if !errors.As(err, &dup) {
		t.Fatalf("expected ErrDuplicateApproval, got %v", err)
	}
}

// TestApproveAndTransition_PartialFailureRollsBack forces the transition step to
// fail AFTER the approval row/event were inserted in the same tx, and verifies
// nothing is persisted (no approval row, no events, state unchanged).
func TestApproveAndTransition_PartialFailureRollsBack(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	seedTask(t, st, "t1")
	mustTransition(t, st, "t1", task.AwaitingPreApproval)

	// toApproved = Succeeded is NOT reachable from AwaitingPreApproval, so the
	// approval insert happens first, then applyTransition fails -> rollback.
	_, err := st.ApproveAndTransition(ctx, "t1", task.AwaitingPreApproval,
		task.Decision{Kind: task.ApprovalPreExec, Approved: true, By: "u"},
		task.Succeeded, task.Rejected)
	var inv task.ErrInvalidTransition
	if !errors.As(err, &inv) {
		t.Fatalf("expected ErrInvalidTransition (transition step), got %v", err)
	}
	// State unchanged.
	got, _ := st.GetTask(ctx, "t1")
	if got.State != task.AwaitingPreApproval {
		t.Errorf("state must be unchanged, got %s", got.State)
	}
	// No approval event should remain (the approval insert was rolled back).
	evs, _ := st.Events(ctx, "t1")
	for _, e := range evs {
		if e.Kind == task.EventApproval {
			t.Error("approval event must not persist after rolled-back transaction")
		}
	}
	// A subsequent, valid approval must still succeed (proves no duplicate row leaked).
	if _, err := st.ApproveAndTransition(ctx, "t1", task.AwaitingPreApproval,
		task.Decision{Kind: task.ApprovalPreExec, Approved: true, By: "u"},
		task.Running, task.Rejected); err != nil {
		t.Fatalf("valid approval after rollback should succeed (no leaked duplicate): %v", err)
	}
}

// failing store wrapper is unnecessary: we assert partial-record absence via the
// unexpected-state path (rolls back before any write) and the duplicate path
// (rejects before the transition). Both exercise rollback within one tx.
