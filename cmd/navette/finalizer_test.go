package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/habuka036/navette/internal/sandbox"
	"github.com/habuka036/navette/internal/store"
	"github.com/habuka036/navette/internal/task"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "n.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func makeWorkCopy(t *testing.T, files map[string]string) string {
	t.Helper()
	src := t.TempDir()
	for name, content := range files {
		p := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wc, err := sandbox.PrepareWorkCopy(src)
	if err != nil {
		t.Fatalf("PrepareWorkCopy: %v", err)
	}
	t.Cleanup(func() { _ = sandbox.RemoveWorkCopy(wc) })
	return wc
}

// TestFinalizer_SaveFailureReturnsError: when the artifact destination cannot be
// written, Finalize returns an error (so the engine fails the task) and does not
// mark saved/verified.
func TestFinalizer_SaveFailureReturnsError(t *testing.T) {
	st := newTestStore(t)
	wc := makeWorkCopy(t, map[string]string{"main.go": "package main"})
	seedTaskForFinalizer(t, st, "t1")
	_ = st.CreateRun(context.Background(), "run1", "t1")

	gate := &kiroReviewGate{approvedHash: mustFingerprint(t, wc)}

	// Point artifactDir at a path whose parent is a FILE, so MkdirAll fails.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fin := &kiroFinalizer{
		store:       st,
		runID:       "run1",
		workCopy:    wc,
		artifactDir: filepath.Join(blocker, "run1"), // parent is a file
		gate:        gate,
	}
	err := fin.Finalize(context.Background(), "t1")
	if err == nil {
		t.Fatal("expected Finalize to fail when artifacts cannot be saved")
	}
	if fin.saved || fin.verified {
		t.Error("saved/verified must be false on save failure")
	}
	// Work copy must still exist (not deleted on failure).
	if _, serr := os.Stat(wc); serr != nil {
		t.Error("work copy must be preserved on finalize failure")
	}
}

// TestFinalizer_FingerprintMismatchReturnsError: if the work copy changes after
// the approved fingerprint was captured, Finalize detects the mismatch and
// returns an error (never verified).
func TestFinalizer_FingerprintMismatchReturnsError(t *testing.T) {
	st := newTestStore(t)
	wc := makeWorkCopy(t, map[string]string{"main.go": "package main // v1"})
	seedTaskForFinalizer(t, st, "t1")
	_ = st.CreateRun(context.Background(), "run1", "t1")

	// Approved fingerprint captured at v1.
	gate := &kiroReviewGate{approvedHash: mustFingerprint(t, wc)}

	// Tamper the work copy AFTER approval but before finalize.
	if err := os.WriteFile(filepath.Join(wc, "main.go"), []byte("package main // TAMPERED"), 0o644); err != nil {
		t.Fatal(err)
	}

	fin := &kiroFinalizer{
		store:       st,
		runID:       "run1",
		workCopy:    wc,
		artifactDir: filepath.Join(t.TempDir(), "art", "run1"),
		gate:        gate,
	}
	err := fin.Finalize(context.Background(), "t1")
	if err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("expected fingerprint mismatch error, got %v", err)
	}
	if fin.verified {
		t.Error("verified must be false on fingerprint mismatch")
	}
	// It saved (then detected mismatch); artifacts are retained for audit.
	if !fin.saved {
		t.Error("artifacts should have been saved before verification")
	}
}

// TestFinalizer_SuccessVerifies: unchanged work copy finalizes cleanly.
func TestFinalizer_SuccessVerifies(t *testing.T) {
	st := newTestStore(t)
	wc := makeWorkCopy(t, map[string]string{"main.go": "package main"})
	seedTaskForFinalizer(t, st, "t1")
	_ = st.CreateRun(context.Background(), "run1", "t1")
	gate := &kiroReviewGate{approvedHash: mustFingerprint(t, wc)}
	fin := &kiroFinalizer{
		store:       st,
		runID:       "run1",
		workCopy:    wc,
		artifactDir: filepath.Join(t.TempDir(), "art", "run1"),
		gate:        gate,
	}
	if err := fin.Finalize(context.Background(), "t1"); err != nil {
		t.Fatalf("Finalize should succeed, got %v", err)
	}
	if !fin.saved || !fin.recorded || !fin.verified {
		t.Errorf("expected saved+recorded+verified true, got %+v", *fin)
	}
	dir, hash, _ := st.RunArtifacts(context.Background(), "run1")
	if dir == "" || hash == "" {
		t.Error("run artifacts should be recorded in SQLite")
	}
}

func seedTaskForFinalizer(t *testing.T, st *store.Store, id string) {
	t.Helper()
	now := time.Now().UTC()
	err := st.CreateTask(context.Background(), task.Task{
		ID: id, Goal: "g", State: task.Finalizing, CreatedAt: now, UpdatedAt: now,
	}, task.Event{TaskID: id, Kind: task.EventNote})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
}

func mustFingerprint(t *testing.T, dir string) string {
	t.Helper()
	fp, err := sandbox.Fingerprint(dir)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	return fp
}
