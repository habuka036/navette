package sandbox_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/habuka036/navette/internal/sandbox"
)

func TestPrepareWorkCopy_ExcludesSecretsAndVCS(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "main.go"), "package main")
	writeFile(t, filepath.Join(src, ".git", "HEAD"), "ref")
	writeFile(t, filepath.Join(src, ".env"), "SECRET=1")
	writeFile(t, filepath.Join(src, "server.key"), "-----BEGIN KEY-----")
	writeFile(t, filepath.Join(src, "deploy.pem"), "-----BEGIN CERT-----")
	writeFile(t, filepath.Join(src, "node_modules", "dep.js"), "x")
	writeFile(t, filepath.Join(src, ".ssh", "id_rsa"), "key")

	dst, err := sandbox.PrepareWorkCopy(src)
	if err != nil {
		t.Fatalf("PrepareWorkCopy: %v", err)
	}
	defer sandbox.RemoveWorkCopy(dst)

	if !fileExists(filepath.Join(dst, "main.go")) {
		t.Error("main.go should be copied")
	}
	for _, excluded := range []string{".git/HEAD", ".env", "server.key", "deploy.pem", "node_modules/dep.js", ".ssh/id_rsa"} {
		if fileExists(filepath.Join(dst, excluded)) {
			t.Errorf("%s must be excluded from the work copy", excluded)
		}
	}
}

func TestPrepareWorkCopy_SizeLimitFailClosed(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "big.bin"), strings.Repeat("a", 2048))
	_, err := sandbox.PrepareWorkCopyOpts(src, sandbox.Options{MaxFileBytes: 1024})
	if err == nil {
		t.Fatal("expected fail-closed error when a file exceeds the per-file limit")
	}
}

func TestSaveArtifacts_And_FingerprintMatch(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a.go"), "package a")
	work, err := sandbox.PrepareWorkCopy(src)
	if err != nil {
		t.Fatalf("PrepareWorkCopy: %v", err)
	}
	defer sandbox.RemoveWorkCopy(work)
	os.WriteFile(filepath.Join(work, "a.go"), []byte("package a // edited"), 0o644)

	// Fingerprint of the work copy at "approval" time.
	approved, err := sandbox.Fingerprint(work)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "run-artifacts")
	saved, err := sandbox.SaveArtifacts(work, dest)
	if err != nil {
		t.Fatalf("SaveArtifacts: %v", err)
	}
	if approved != saved {
		t.Errorf("approved fingerprint %s != saved fingerprint %s", approved, saved)
	}
	if !fileExists(filepath.Join(dest, "a.go")) {
		t.Error("artifact file not saved")
	}
}

func TestSaveArtifacts_RefusesExistingDest(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a.go"), "package a")
	work, _ := sandbox.PrepareWorkCopy(src)
	defer sandbox.RemoveWorkCopy(work)
	dest := t.TempDir() // already exists
	if _, err := sandbox.SaveArtifacts(work, dest); err == nil {
		t.Error("expected refusal to save into an existing directory")
	}
}

func TestUnifiedDiff_TextAndBinary(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "text.go"), "line1\nline2\n")
	// binary file (contains NUL)
	os.WriteFile(filepath.Join(src, "bin.dat"), []byte{0x00, 0x01, 0x02}, 0o644)

	work, _ := sandbox.PrepareWorkCopy(src)
	defer sandbox.RemoveWorkCopy(work)
	os.WriteFile(filepath.Join(work, "text.go"), []byte("line1\nCHANGED\n"), 0o644)
	os.WriteFile(filepath.Join(work, "bin.dat"), []byte{0x00, 0x09}, 0o644)

	diffs, err := sandbox.UnifiedDiff(src, work)
	if err != nil {
		t.Fatalf("UnifiedDiff: %v", err)
	}
	var sawTextDiff, sawBinary bool
	for _, d := range diffs {
		if d.Path == "text.go" {
			if !strings.Contains(d.Unified, "-line2") || !strings.Contains(d.Unified, "+CHANGED") {
				t.Errorf("unified diff missing expected lines:\n%s", d.Unified)
			}
			sawTextDiff = true
		}
		if d.Path == "bin.dat" && d.Binary {
			sawBinary = true
		}
	}
	if !sawTextDiff {
		t.Error("expected a text unified diff for text.go")
	}
	if !sawBinary {
		t.Error("expected bin.dat flagged as binary")
	}
}
