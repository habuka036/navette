package sandbox_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/habuka036/navette/internal/sandbox"
)

func TestPrepareWorkCopy_ExcludesGit(t *testing.T) {
	src := t.TempDir()
	// Create a repo-like tree with a .git dir and nested content.
	writeFile(t, filepath.Join(src, "main.go"), "package main")
	writeFile(t, filepath.Join(src, "pkg", "util.go"), "package pkg")
	writeFile(t, filepath.Join(src, ".git", "HEAD"), "ref: refs/heads/main")
	writeFile(t, filepath.Join(src, ".git", "config"), "[core]")

	dst, err := sandbox.PrepareWorkCopy(src)
	if err != nil {
		t.Fatalf("PrepareWorkCopy: %v", err)
	}
	defer sandbox.RemoveWorkCopy(dst)

	// Project files copied.
	if !fileExists(filepath.Join(dst, "main.go")) || !fileExists(filepath.Join(dst, "pkg", "util.go")) {
		t.Error("expected project files to be copied")
	}
	// .git excluded.
	if fileExists(filepath.Join(dst, ".git", "HEAD")) || dirExists(filepath.Join(dst, ".git")) {
		t.Error(".git must be excluded from the work copy")
	}
}

func TestRemoveWorkCopy_RefusesNonWorkcopy(t *testing.T) {
	d := t.TempDir() // not a navette-workcopy- path
	if err := sandbox.RemoveWorkCopy(d); err == nil {
		t.Error("expected refusal to remove a non-workcopy path")
	}
	if !dirExists(d) {
		t.Error("non-workcopy path must not be removed")
	}
}

func TestRemoveWorkCopy_StrictValidation(t *testing.T) {
	// 1. Substring (not prefix) must be refused: a dir whose name merely contains
	//    the token but does not start with it.
	tricky := filepath.Join(os.TempDir(), "evil-navette-workcopy-x")
	if err := os.MkdirAll(tricky, 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tricky)
	if err := sandbox.RemoveWorkCopy(tricky); err == nil {
		t.Error("must refuse a name that only CONTAINS the prefix (not starts-with)")
	}
	if !dirExists(tricky) {
		t.Error("tricky path must not be deleted")
	}

	// 2. Correct prefix but OUTSIDE the temp dir must be refused.
	outside := filepath.Join(t.TempDir(), "navette-workcopy-abc")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.RemoveWorkCopy(outside); err == nil {
		t.Error("must refuse a correctly-prefixed path outside the OS temp dir")
	}
	if !dirExists(outside) {
		t.Error("outside path must not be deleted")
	}

	// 3. Empty and relative paths must be refused.
	if err := sandbox.RemoveWorkCopy(""); err == nil {
		t.Error("must refuse empty path")
	}
	if err := sandbox.RemoveWorkCopy("navette-workcopy-rel"); err == nil {
		t.Error("must refuse relative path")
	}

	// 4. A real work copy IS removable.
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a.go"), "package a")
	real, err := sandbox.PrepareWorkCopy(src)
	if err != nil {
		t.Fatalf("PrepareWorkCopy: %v", err)
	}
	if err := sandbox.RemoveWorkCopy(real); err != nil {
		t.Errorf("must remove a genuine work copy, got %v", err)
	}
	if dirExists(real) {
		t.Error("genuine work copy should be removed")
	}
}

func TestRemoveWorkCopy_RefusesSymlink(t *testing.T) {
	// A symlink in the temp dir with a valid-looking name must not be followed
	// and must not cause deletion of its target.
	target := t.TempDir()
	writeFile(t, filepath.Join(target, "important.txt"), "keep me")
	link := filepath.Join(os.TempDir(), "navette-workcopy-symlink-test")
	_ = os.Remove(link)
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	defer os.Remove(link)
	if err := sandbox.RemoveWorkCopy(link); err == nil {
		t.Error("must refuse to remove a symlink")
	}
	if !fileExists(filepath.Join(target, "important.txt")) {
		t.Error("symlink target contents must be preserved")
	}
}

func TestCommand_FailClosedWithoutWorkCopy(t *testing.T) {
	cfg := sandbox.Config{DedicatedHome: t.TempDir()} // no WorkCopy
	_, err := cfg.Command(context.Background(), "echo", "hi")
	if err == nil {
		t.Fatal("expected fail-closed error without a work copy")
	}
	if !strings.Contains(err.Error(), "sandbox") {
		t.Errorf("expected sandbox error, got %v", err)
	}
}

func TestCommand_ClearsEnvAndNoHostLeak(t *testing.T) {
	// Set a host env var that must not leak into the sandbox command.
	t.Setenv("EVIL_API_KEY", "super-secret")
	work := t.TempDir()
	// Rename to a workcopy-looking dir so Command accepts it (it only checks dir).
	cfg := sandbox.Config{
		WorkCopy:      work,
		DedicatedHome: t.TempDir(),
		ExtraEnv:      map[string]string{"KIRO_TELEMETRY_ENABLED": "false"},
	}
	if !sandbox.Available() {
		t.Skip("bwrap not available")
	}
	cmd, err := cfg.Command(context.Background(), "echo", "hi")
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "--clearenv") {
		t.Error("expected --clearenv in sandbox command")
	}
	if strings.Contains(joined, "super-secret") || strings.Contains(joined, "EVIL_API_KEY") {
		t.Error("host env var leaked into sandbox command args")
	}
	// Allowlisted ExtraEnv should be set explicitly.
	if !strings.Contains(joined, "KIRO_TELEMETRY_ENABLED") {
		t.Error("expected allowlisted ExtraEnv var to be set")
	}
	// bwrap's own Env must not carry the host secret either.
	for _, e := range cmd.Env {
		if strings.Contains(e, "super-secret") || strings.HasPrefix(e, "EVIL_API_KEY=") {
			t.Errorf("bwrap parent env leaked host secret: %q", e)
		}
	}
}

func TestDescribe_IsSecretFree(t *testing.T) {
	cfg := sandbox.Config{
		WorkCopy:      "/tmp/navette-workcopy-abc",
		DedicatedHome: "/home/u/.navette/kiro-home",
		ReadOnly:      map[string]string{"/repos/imagorium": "/refs/imagorium"},
	}
	d := cfg.Describe()
	for _, want := range []string{"writable work copy", "tmpfs", "network: shared", "read-only references"} {
		if !strings.Contains(d, want) {
			t.Errorf("Describe missing %q:\n%s", want, d)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}
func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
