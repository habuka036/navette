package kiroruntime

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// startGroup launches sh in its own process group running the given script.
func startGroup(t *testing.T, script string) (*exec.Cmd, int) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	return cmd, cmd.Process.Pid
}

// groupAlive reports whether the process group still has a reapable leader.
func processAlive(pid int) bool {
	// Signal 0 checks for existence without killing.
	return syscall.Kill(pid, 0) == nil
}

// TestReclaimGroup_NormalExit: process exits on its own before grace; reclaim
// returns promptly without needing SIGKILL.
func TestReclaimGroup_NormalExit(t *testing.T) {
	cmd, pgid := startGroup(t, "exit 0")
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	// Wait for natural exit first.
	<-exited
	start := time.Now()
	reclaimGroup(pgid, 2*time.Second, exited) // exited already closed
	if time.Since(start) > 500*time.Millisecond {
		t.Error("reclaimGroup should return promptly when the group already exited")
	}
}

// TestReclaimGroup_SigtermIgnoredThenKilled: the process ignores SIGTERM, so
// reclaimGroup must escalate to SIGKILL after the grace period.
func TestReclaimGroup_SigtermIgnoredThenKilled(t *testing.T) {
	// trap '' TERM makes the shell ignore SIGTERM; it sleeps for a long time.
	cmd, pgid := startGroup(t, "trap '' TERM; sleep 30")
	// Give the shell a moment to install the trap.
	time.Sleep(100 * time.Millisecond)

	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()

	grace := 500 * time.Millisecond
	start := time.Now()
	reclaimGroup(pgid, grace, exited)
	elapsed := time.Since(start)

	// reclaimGroup should have waited ~grace then sent SIGKILL.
	if elapsed < grace {
		t.Errorf("reclaimGroup returned before grace elapsed (%v < %v)", elapsed, grace)
	}
	// The process must be gone shortly after SIGKILL.
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Error("process group was not killed after SIGKILL escalation")
	}
	if processAlive(pgid) {
		t.Error("leader still alive after reclaim")
	}
}

// TestReclaimGroup_SigtermHonored: a process that exits on SIGTERM is reclaimed
// without needing SIGKILL.
func TestReclaimGroup_SigtermHonored(t *testing.T) {
	cmd, pgid := startGroup(t, "sleep 30")
	time.Sleep(50 * time.Millisecond)
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()

	reclaimGroup(pgid, 2*time.Second, exited)
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Error("SIGTERM-honoring process was not reclaimed")
	}
}
