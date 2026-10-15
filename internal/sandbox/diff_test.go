package sandbox_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/habuka036/navette/internal/sandbox"
)

func TestDiffWorkCopy(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "keep.go"), "same")
	writeFile(t, filepath.Join(src, "change.go"), "old")
	writeFile(t, filepath.Join(src, "gone.go"), "bye")
	writeFile(t, filepath.Join(src, ".git", "HEAD"), "ref") // must be ignored

	work, err := sandbox.PrepareWorkCopy(src)
	if err != nil {
		t.Fatalf("PrepareWorkCopy: %v", err)
	}
	defer sandbox.RemoveWorkCopy(work)

	// Simulate agent edits in the work copy.
	os.WriteFile(filepath.Join(work, "change.go"), []byte("new"), 0o644)
	os.Remove(filepath.Join(work, "gone.go"))
	os.WriteFile(filepath.Join(work, "added.go"), []byte("hi"), 0o644)

	changes, err := sandbox.DiffWorkCopy(src, work)
	if err != nil {
		t.Fatalf("DiffWorkCopy: %v", err)
	}
	got := map[string]string{}
	for _, c := range changes {
		got[c.Path] = c.Status
	}
	if got["change.go"] != "modified" {
		t.Errorf("change.go: want modified, got %q", got["change.go"])
	}
	if got["gone.go"] != "deleted" {
		t.Errorf("gone.go: want deleted, got %q", got["gone.go"])
	}
	if got["added.go"] != "added" {
		t.Errorf("added.go: want added, got %q", got["added.go"])
	}
	if _, ok := got["keep.go"]; ok {
		t.Error("keep.go should not appear (unchanged)")
	}
	if _, ok := got[filepath.Join(".git", "HEAD")]; ok {
		t.Error(".git changes must be ignored")
	}
}
