package sandbox

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Default limits and exclusions for work-copy preparation.
const (
	// DefaultMaxCopyBytes caps the total bytes copied into a work copy to avoid
	// unbounded copies. 0 in Options means use this default.
	DefaultMaxCopyBytes = 256 << 20 // 256 MiB
	// DefaultMaxFileBytes caps any single file copied.
	DefaultMaxFileBytes = 32 << 20 // 32 MiB
)

// excludedDirNames are directory names never copied into a work copy.
var excludedDirNames = map[string]bool{
	".git": true, ".hg": true, ".svn": true,
	"node_modules": true, ".ssh": true, ".gnupg": true, ".aws": true,
}

// excludedFileNames are sensitive file names never copied into a work copy.
var excludedFileNames = map[string]bool{
	".env": true, ".netrc": true, ".npmrc": true, ".pypirc": true,
	"id_rsa": true, "id_ed25519": true, "credentials": true,
}

// excludedFileSuffixes are sensitive file suffixes never copied.
var excludedFileSuffixes = []string{
	".pem", ".key", ".p12", ".pfx", ".keystore",
}

// Options tune work-copy preparation.
type Options struct {
	MaxTotalBytes int64
	MaxFileBytes  int64
}

func (o Options) withDefaults() Options {
	if o.MaxTotalBytes <= 0 {
		o.MaxTotalBytes = DefaultMaxCopyBytes
	}
	if o.MaxFileBytes <= 0 {
		o.MaxFileBytes = DefaultMaxFileBytes
	}
	return o
}

// isExcludedFile reports whether a file should be excluded as sensitive.
func isExcludedFile(name string) bool {
	if excludedFileNames[name] {
		return true
	}
	for _, suf := range excludedFileSuffixes {
		if strings.HasSuffix(name, suf) {
			return true
		}
	}
	return false
}

// PrepareWorkCopy creates a disposable copy of srcDir under a fresh temporary
// directory, EXCLUDING .git (and other VCS/sensitive dirs) and sensitive files
// (keys, .env, credentials). The returned path is the writable working copy the
// agent may modify; the real repository is never exposed writable. The caller is
// responsible for removing the copy (RemoveWorkCopy) when the run completes.
//
// Fail-closed: any error (including exceeding the total-size budget) means no
// usable work copy exists and the caller must not run the agent against the real
// repository.
func PrepareWorkCopy(srcDir string) (string, error) {
	return PrepareWorkCopyOpts(srcDir, Options{})
}

// PrepareWorkCopyOpts is PrepareWorkCopy with explicit size limits.
func PrepareWorkCopyOpts(srcDir string, opts Options) (string, error) {
	opts = opts.withDefaults()
	info, err := os.Stat(srcDir)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("sandbox.PrepareWorkCopy: source %q not a directory: %w", srcDir, err)
	}
	dst, err := os.MkdirTemp("", "navette-workcopy-")
	if err != nil {
		return "", fmt.Errorf("sandbox.PrepareWorkCopy: mktemp: %w", err)
	}
	if err := copyTreeFiltered(srcDir, dst, opts); err != nil {
		_ = os.RemoveAll(dst)
		return "", err
	}
	return dst, nil
}

// workCopyPrefix is the mandatory base-name prefix for directories created by
// PrepareWorkCopy. RemoveWorkCopy refuses to delete anything else.
const workCopyPrefix = "navette-workcopy-"

// RemoveWorkCopy deletes a working copy created by PrepareWorkCopy. It applies
// strict validation to avoid deleting anything NAVETTE did not create:
//   - the path must be non-empty and absolute,
//   - it must reside directly within the OS temporary directory,
//   - its base name must start with the exact workCopyPrefix,
//   - it must be a directory (not a symlink to elsewhere).
//
// Any violation returns an error without deleting.
func RemoveWorkCopy(path string) error {
	if path == "" {
		return fmt.Errorf("sandbox.RemoveWorkCopy: empty path")
	}
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return fmt.Errorf("sandbox.RemoveWorkCopy: refusing non-absolute path %q", path)
	}
	base := filepath.Base(clean)
	// Base name must START WITH the prefix (not merely contain it).
	if !strings.HasPrefix(base, workCopyPrefix) {
		return fmt.Errorf("sandbox.RemoveWorkCopy: refusing path without %q prefix: %q", workCopyPrefix, path)
	}
	// Must be located directly under the OS temp dir.
	tmp := filepath.Clean(os.TempDir())
	if filepath.Dir(clean) != tmp {
		return fmt.Errorf("sandbox.RemoveWorkCopy: refusing path outside temp dir %q: %q", tmp, path)
	}
	// Must be a real directory, not a symlink pointing elsewhere.
	fi, err := os.Lstat(clean)
	if err != nil {
		return fmt.Errorf("sandbox.RemoveWorkCopy: stat %q: %w", path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("sandbox.RemoveWorkCopy: refusing to remove a symlink %q", path)
	}
	if !fi.IsDir() {
		return fmt.Errorf("sandbox.RemoveWorkCopy: refusing to remove non-directory %q", path)
	}
	return os.RemoveAll(clean)
}

func copyTreeFiltered(src, dst string, opts Options) error {
	var total int64
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		if info.IsDir() {
			if excludedDirNames[info.Name()] {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		if !info.Mode().IsRegular() {
			// Skip symlinks/devices/etc. to avoid escaping the copy.
			return nil
		}
		if isExcludedFile(info.Name()) {
			return nil // sensitive file: never copy
		}
		if info.Size() > opts.MaxFileBytes {
			return fmt.Errorf("sandbox.PrepareWorkCopy: file %q exceeds per-file limit (%d > %d)", rel, info.Size(), opts.MaxFileBytes)
		}
		total += info.Size()
		if total > opts.MaxTotalBytes {
			return fmt.Errorf("sandbox.PrepareWorkCopy: total copy size exceeds limit (%d bytes)", opts.MaxTotalBytes)
		}
		return copyFile(path, filepath.Join(dst, rel), info.Mode())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
