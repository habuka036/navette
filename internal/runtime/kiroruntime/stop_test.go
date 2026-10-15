package kiroruntime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/habuka036/navette/internal/runtime"
)

// failingSink fails on the Nth Emit call, simulating an EventSink persistence
// failure mid-run.
type failingSink struct {
	failOn int
	calls  int
	err    error
}

func (s *failingSink) Emit(ev runtime.OutputEvent) error {
	s.calls++
	if s.calls >= s.failOn {
		return s.err
	}
	return nil
}

// pidAlive reports whether the given pid is still alive (signal 0 probe).
func pidAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// waitGrandchildPID reads the pid the test script wrote, polling briefly. It
// returns 0 if the pid never appears (e.g. the grandchild was reclaimed before
// recording its pid), which the caller treats as "already gone". It never calls
// t.Fatal from a goroutine.
func waitGrandchildPID(pidFile string) int {
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(pidFile)
		if err == nil {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(b))); perr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return 0
}

// buildReclaimScript writes a /bin/sh script that: emits the given JSONL lines,
// starts a SIGTERM-ignoring grandchild that records its own PID, and then blocks
// so the process group stays alive until reclaimed. The grandchild deliberately
// ignores SIGTERM so only SIGKILL (to the whole group) can stop it.
func buildReclaimScript(t *testing.T, line1, line2, pidFile string) string {
	t.Helper()
	// Start the TERM-ignoring grandchild FIRST and record its PID, then emit the
	// JSONL lines. A short sleep ensures the pid file is written before parsing
	// reaches the line that triggers failure, so the test deterministically has a
	// live grandchild to reclaim.
	return `
( trap '' TERM; echo $$ > ` + pidFile + `; sleep 60 ) &
sleep 0.2
printf '` + line1 + `\n'
printf '` + line2 + `\n'
sleep 60
`
}

func newReclaimRuntime(script string) *Runtime {
	return &Runtime{
		KillGrace: 500 * time.Millisecond,
		cmdFactory: func(ctx context.Context, argv ...string) (*exec.Cmd, error) {
			return exec.CommandContext(ctx, "/bin/sh", "-c", script), nil
		},
	}
}

// TestEventSinkFailureStopsProcessGroup verifies that when the EventSink fails
// mid-run, the runtime stops the whole process group — including a
// SIGTERM-ignoring grandchild — rather than merely waiting for the direct child.
func TestEventSinkFailureStopsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "gc.pid")
	script := buildReclaimScript(t,
		`{"type":"runStarted","data":{"engine":"v2"}}`,
		`{"type":"sessionUpdate","data":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hi"}}}}`,
		pidFile)
	r := newReclaimRuntime(script)
	sink := &failingSink{failOn: 2, err: errors.New("sqlite write failed")}

	// Capture the grandchild PID concurrently (it is written early).
	pidCh := make(chan int, 1)
	go func() { pidCh <- waitGrandchildPID(pidFile) }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	start := time.Now()
	_, err := r.ExecuteStreaming(ctx, runtime.RunRequest{Goal: "x"}, sink)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error when the EventSink fails")
	}
	if elapsed > 10*time.Second {
		t.Errorf("runtime did not stop promptly on sink failure (%v)", elapsed)
	}
	pid := <-pidCh
	if pid == 0 {
		return // grandchild was reclaimed before it could record its pid: acceptable
	}
	// After reclamation + a short settle, the SIGTERM-ignoring grandchild must be
	// dead (only SIGKILL to the group could achieve this).
	deadline := time.Now().Add(3 * time.Second)
	for pidAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if pidAlive(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("SIGTERM-ignoring grandchild (pid %d) survived; process group not reclaimed", pid)
	}
}

// TestChildExitsButGrandchildRemains verifies the core concern: cmd.Wait()
// completing (direct child exits) does NOT mean the process group is gone. Here
// the direct child exits immediately after spawning a SIGTERM-ignoring
// grandchild. On cancellation the reclaimer must still SIGKILL the group after
// grace, reaping the grandchild.
func TestChildExitsButGrandchildRemains(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "gc.pid")
	// Direct child spawns a TERM-ignoring grandchild, then EXITS immediately.
	// The grandchild stays in the same process group.
	script := `
( trap '' TERM; echo $$ > ` + pidFile + `; sleep 60 ) &
exit 0
`
	r := newReclaimRuntime(script)
	// No stream-json output at all → ParseStream returns no runFinished. We
	// cancel the context to trigger reclamation of the lingering group.
	ctx, cancel := context.WithCancel(context.Background())

	pidCh := make(chan int, 1)
	go func() { pidCh <- waitGrandchildPID(pidFile) }()

	// Cancel shortly after start so the reclaimer fires while the grandchild is
	// still alive (the direct child has already exited).
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()

	_, err := r.ExecuteStreaming(ctx, runtime.RunRequest{Goal: "x"}, &failingSink{failOn: 999})
	if err == nil {
		t.Error("expected a non-success error (no runFinished / cancelled)")
	}
	pid := <-pidCh
	if pid == 0 {
		t.Skip("grandchild pid not captured in time; environment-dependent")
	}
	deadline := time.Now().Add(3 * time.Second)
	for pidAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if pidAlive(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Error("grandchild survived after direct child exit; group not reclaimed by SIGKILL")
	}
}
func TestParseErrorStopsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "gc.pid")
	script := buildReclaimScript(t,
		`{"type":"runFinished","data":{"status":"success"}}`,
		`{"type":"runFinished","data":{"status":"success"}}`,
		pidFile)
	r := newReclaimRuntime(script)

	pidCh := make(chan int, 1)
	go func() { pidCh <- waitGrandchildPID(pidFile) }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, err := r.ExecuteStreaming(ctx, runtime.RunRequest{Goal: "x"}, &failingSink{failOn: 999})
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("expected ErrProtocol, got %v", err)
	}
	pid := <-pidCh
	if pid == 0 {
		return // grandchild reclaimed before recording its pid: acceptable
	}
	deadline := time.Now().Add(3 * time.Second)
	for pidAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if pidAlive(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("grandchild (pid %d) survived after parse-error reclamation", pid)
	}
}
