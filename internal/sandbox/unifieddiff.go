package sandbox

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FileDiff is a per-file change with a rendered unified diff (for text files).
type FileDiff struct {
	Path    string
	Status  string // "added" | "modified" | "deleted"
	Binary  bool   // true if either side looks binary; no text diff is rendered
	Unified string // unified diff text (empty for binary)
}

// UnifiedDiff compares origSrc and workCopy (both with .git/VCS excluded via the
// same snapshot rules) and returns per-file diffs. Text files get a unified
// diff; binary files are flagged without content. Added/deleted files are
// rendered as all-added / all-removed hunks (or flagged binary).
func UnifiedDiff(origSrc, workCopy string) ([]FileDiff, error) {
	orig, err := snapshot(origSrc)
	if err != nil {
		return nil, fmt.Errorf("sandbox.UnifiedDiff: snapshot orig: %w", err)
	}
	work, err := snapshot(workCopy)
	if err != nil {
		return nil, fmt.Errorf("sandbox.UnifiedDiff: snapshot work: %w", err)
	}

	seen := map[string]bool{}
	var out []FileDiff
	for path, w := range work {
		seen[path] = true
		o, existed := orig[path]
		if existed && bytes.Equal(o, w) {
			continue
		}
		status := "modified"
		if !existed {
			status = "added"
		}
		out = append(out, buildFileDiff(path, status, o, w))
	}
	for path, o := range orig {
		if seen[path] {
			continue
		}
		out = append(out, buildFileDiff(path, "deleted", o, nil))
	}
	// Deterministic order.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Path < out[i].Path {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

func buildFileDiff(path, status string, oldb, newb []byte) FileDiff {
	fd := FileDiff{Path: path, Status: status}
	if isBinary(oldb) || isBinary(newb) {
		fd.Binary = true
		return fd
	}
	fd.Unified = renderUnified(path, splitLines(string(oldb)), splitLines(string(newb)))
	return fd
}

// RenderUnifiedDiffs formats a set of FileDiffs for human review.
func RenderUnifiedDiffs(diffs []FileDiff) string {
	if len(diffs) == 0 {
		return "(no file changes in the working copy)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d changed file(s):\n", len(diffs))
	for _, d := range diffs {
		if d.Binary {
			fmt.Fprintf(&b, "  [%s, binary] %s\n", d.Status, d.Path)
			continue
		}
		fmt.Fprintf(&b, "  [%s] %s\n", d.Status, d.Path)
		b.WriteString(d.Unified)
	}
	return b.String()
}

func isBinary(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	// NUL byte within the first 8000 bytes => treat as binary.
	n := len(b)
	if n > 8000 {
		n = 8000
	}
	return bytes.IndexByte(b[:n], 0) >= 0
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	// SplitAfter keeps trailing "" when s ends with \n; drop it.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// renderUnified produces a minimal unified diff using an LCS of lines. For PoC
// scale (small throwaway projects) a single hunk covering the whole file is
// acceptable and keeps the implementation dependency-free.
func renderUnified(path string, a, b []string) string {
	lcs := lcsMatrix(a, b)
	var ops []string
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i] == b[j] {
			ops = append(ops, " "+trimNL(a[i]))
			i++
			j++
		} else if lcs[i+1][j] >= lcs[i][j+1] {
			ops = append(ops, "-"+trimNL(a[i]))
			i++
		} else {
			ops = append(ops, "+"+trimNL(b[j]))
			j++
		}
	}
	for ; i < len(a); i++ {
		ops = append(ops, "-"+trimNL(a[i]))
	}
	for ; j < len(b); j++ {
		ops = append(ops, "+"+trimNL(b[j]))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "    --- a/%s\n    +++ b/%s\n", path, path)
	for _, op := range ops {
		fmt.Fprintf(&sb, "    %s\n", op)
	}
	return sb.String()
}

func trimNL(s string) string { return strings.TrimRight(s, "\n") }

func lcsMatrix(a, b []string) [][]int {
	m, n := len(a), len(b)
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := m - 1; i >= 0; i-- {
		for j := n - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	return dp
}

// SaveArtifacts copies the work copy (which already excludes .git/sensitive
// files) into destDir for durable, auditable retention. It returns a content
// fingerprint over all files so the caller can verify that what was approved
// matches what was saved. destDir must not already exist.
func SaveArtifacts(workCopy, destDir string) (fingerprint string, err error) {
	if _, statErr := os.Stat(destDir); statErr == nil {
		return "", fmt.Errorf("sandbox.SaveArtifacts: destination %q already exists", destDir)
	}
	if err := os.MkdirAll(filepath.Dir(destDir), 0o755); err != nil {
		return "", fmt.Errorf("sandbox.SaveArtifacts: mkdir parent: %w", err)
	}
	// Reuse the filtered copy (it re-applies exclusions; the work copy is already
	// filtered, so this is a straight copy of regular files).
	if err := copyTreeFiltered(workCopy, destDir, Options{}.withDefaults()); err != nil {
		_ = os.RemoveAll(destDir)
		return "", err
	}
	fp, err := Fingerprint(destDir)
	if err != nil {
		return "", err
	}
	return fp, nil
}

// Fingerprint returns a stable content hash over all regular files under root
// (path + bytes), excluding .git/VCS. Used to verify approved == saved.
func Fingerprint(root string) (string, error) {
	snap, err := snapshot(root)
	if err != nil {
		return "", err
	}
	paths := make([]string, 0, len(snap))
	for p := range snap {
		paths = append(paths, p)
	}
	// sort for determinism
	for i := 0; i < len(paths); i++ {
		for j := i + 1; j < len(paths); j++ {
			if paths[j] < paths[i] {
				paths[i], paths[j] = paths[j], paths[i]
			}
		}
	}
	h := newHasher()
	for _, p := range paths {
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write(snap[p])
		h.Write([]byte{0})
	}
	return h.Sum(), nil
}
