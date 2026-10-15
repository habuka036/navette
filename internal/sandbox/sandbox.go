// Package sandbox builds an isolated execution environment for a runtime using
// bubblewrap (bwrap). The isolation model (Milestone C) is "network open,
// filesystem confined":
//
//   - Only a disposable working copy (with .git excluded) is writable.
//   - Reference paths are mounted read-only.
//   - /tmp is a private tmpfs (the host /tmp is never shared).
//   - A dedicated, persistent HOME holds the agent's own auth/state; it is
//     bind-mounted writable. Host SSH agent, Docker socket, and the normal
//     ~/.ssh / ~/.kiro / ~/.local/share/kiro-cli are NOT shared.
//   - Network is shared because the initial runtime (Kiro CLI) is a cloud agent.
//     This does NOT restrict destinations or transmitted content (documented as
//     a residual risk).
//
// Fail-closed: if bwrap is unavailable or the working copy cannot be prepared,
// callers must not fall back to running against a real repository.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrUnavailable indicates the sandbox cannot be constructed safely (for example
// bwrap is missing). Callers must fail closed on this error.
var ErrUnavailable = errors.New("sandbox: isolation unavailable")

// Config describes an isolated run.
type Config struct {
	// WorkCopy is the host path to the disposable, writable working copy. It is
	// mounted at WorkMount inside the sandbox and is the only writable project
	// area.
	WorkCopy string
	// WorkMount is the in-sandbox path for the working copy (default /work).
	WorkMount string
	// DedicatedHome is the host path to the persistent agent HOME (auth/state).
	// Mounted writable at the same path, and HOME is set to it.
	DedicatedHome string
	// ReadOnly maps host paths -> in-sandbox mount paths, each mounted read-only
	// (for reference repositories such as Imagorium).
	ReadOnly map[string]string
	// ROBinaries are host paths mounted read-only verbatim (toolchain/binaries
	// the runtime needs, e.g. kiro-cli and its bun/node).
	ROBinaries []string
	// PathEnv is the PATH value set inside the sandbox.
	PathEnv string
	// ExtraEnv is an explicit allowlist of environment variables to set inside
	// the sandbox (in addition to HOME and PATH). Values are provided by the
	// caller; the host environment is otherwise cleared (--clearenv).
	ExtraEnv map[string]string
}

// DefaultROBinaries returns the read-only binary paths Kiro CLI needs. Only the
// executable and its embedded bun/node runtimes are exposed; the host Kiro
// profile (auth/state) is deliberately excluded.
func DefaultROBinaries() []string {
	home, _ := os.UserHomeDir()
	return []string{
		filepath.Join(home, ".local/bin"),
		filepath.Join(home, ".local/share/mise"),
		filepath.Join(home, ".local/share/kiro-cli/bun"),
		filepath.Join(home, ".local/share/kiro-cli/node"),
	}
}

// Available reports whether bwrap is on PATH. When false, callers must fail
// closed.
func Available() bool {
	_, err := exec.LookPath("bwrap")
	return err == nil
}

// Command builds an *exec.Cmd that runs argv inside the bubblewrap sandbox
// described by cfg. It returns ErrUnavailable if bwrap is missing or the config
// is unsafe (missing work copy / dedicated home).
func (c Config) Command(ctx context.Context, argv ...string) (*exec.Cmd, error) {
	if !Available() {
		return nil, fmt.Errorf("%w: bwrap not found on PATH", ErrUnavailable)
	}
	if c.WorkCopy == "" {
		return nil, fmt.Errorf("%w: no work copy configured", ErrUnavailable)
	}
	if fi, err := os.Stat(c.WorkCopy); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%w: work copy %q not a directory: %v", ErrUnavailable, c.WorkCopy, err)
	}
	if c.DedicatedHome == "" {
		return nil, fmt.Errorf("%w: no dedicated HOME configured", ErrUnavailable)
	}
	if fi, err := os.Stat(c.DedicatedHome); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%w: dedicated HOME %q not a directory: %v", ErrUnavailable, c.DedicatedHome, err)
	}

	workMount := c.WorkMount
	if workMount == "" {
		workMount = "/work"
	}
	pathEnv := c.PathEnv
	if pathEnv == "" {
		// Include the user's ~/.local/bin (where kiro-cli lives) so the runtime
		// binary resolves inside the sandbox.
		home, _ := os.UserHomeDir()
		pathEnv = filepath.Join(home, ".local/bin") + ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}

	args := []string{
		"--ro-bind", "/usr", "/usr",
		"--ro-bind", "/bin", "/bin",
		"--ro-bind", "/sbin", "/sbin",
		"--ro-bind", "/etc", "/etc",
	}
	// /lib, /lib64 may or may not exist; bind those that do.
	for _, p := range []string{"/lib", "/lib64", "/usr/lib"} {
		if _, err := os.Stat(p); err == nil {
			args = append(args, "--ro-bind", p, p)
		}
	}
	// Read-only binaries/toolchain.
	for _, p := range c.ROBinaries {
		if _, err := os.Stat(p); err == nil {
			args = append(args, "--ro-bind", p, p)
		}
	}
	// Reference repositories (read-only).
	for host, mount := range c.ReadOnly {
		args = append(args, "--ro-bind", host, mount)
	}
	// Dedicated HOME (writable, persistent) and the writable work copy.
	args = append(args,
		"--bind", c.DedicatedHome, c.DedicatedHome,
		"--bind", c.WorkCopy, workMount,
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--chdir", workMount,
		// Clear ALL host environment, then set only what is needed. This prevents
		// host API keys / tokens from being inherited by the agent.
		"--clearenv",
		"--setenv", "HOME", c.DedicatedHome,
		"--setenv", "PATH", pathEnv,
		"--share-net",       // Kiro CLI is a cloud agent (documented)
		"--die-with-parent", // child dies if the bwrap parent is killed
	)
	// Explicit, caller-provided passthrough variables (allowlist). Values come
	// from the caller, never implicitly from the host environment.
	for k, v := range c.ExtraEnv {
		args = append(args, "--setenv", k, v)
	}
	args = append(args, argv...)

	cmd := exec.CommandContext(ctx, "bwrap", args...)
	// Minimise bwrap's own environment too (defense in depth). --clearenv already
	// clears the environment seen inside the sandbox; this ensures the bwrap
	// parent process does not carry host secrets either.
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	return cmd, nil
}

// Describe returns a human-readable, secret-free summary of the sandbox mounts
// for logging/approval display.
func (c Config) Describe() string {
	var b strings.Builder
	workMount := c.WorkMount
	if workMount == "" {
		workMount = "/work"
	}
	fmt.Fprintf(&b, "writable work copy: %s -> %s\n", c.WorkCopy, workMount)
	fmt.Fprintf(&b, "dedicated HOME (auth/state, writable): %s\n", c.DedicatedHome)
	fmt.Fprintf(&b, "tmpfs: /tmp (host /tmp NOT shared)\n")
	fmt.Fprintf(&b, "network: shared (cloud agent; destinations/content NOT restricted)\n")
	if len(c.ReadOnly) > 0 {
		b.WriteString("read-only references:\n")
		for host, mount := range c.ReadOnly {
			fmt.Fprintf(&b, "  %s -> %s (ro)\n", host, mount)
		}
	}
	return b.String()
}
