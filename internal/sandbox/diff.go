package sandbox

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FileChange describes one change between the original source tree and the
// agent's working copy.
type FileChange struct {
	Path   string // path relative to the tree root
	Status string // "added" | "modified" | "deleted"
}

// DiffWorkCopy compares the original source tree (origSrc, .git ignored) with
// the agent's working copy (workCopy) and returns the list of changes. It is
// used to let a human review what the agent did before result approval. It does
// not modify either tree.
func DiffWorkCopy(origSrc, workCopy string) ([]FileChange, error) {
	orig, err := snapshot(origSrc)
	if err != nil {
		return nil, fmt.Errorf("sandbox.DiffWorkCopy: snapshot orig: %w", err)
	}
	work, err := snapshot(workCopy)
	if err != nil {
		return nil, fmt.Errorf("sandbox.DiffWorkCopy: snapshot work: %w", err)
	}

	var changes []FileChange
	// Added / modified.
	for path, wsum := range work {
		osum, ok := orig[path]
		switch {
		case !ok:
			changes = append(changes, FileChange{Path: path, Status: "added"})
		case !bytes.Equal(osum, wsum):
			changes = append(changes, FileChange{Path: path, Status: "modified"})
		}
	}
	// Deleted.
	for path := range orig {
		if _, ok := work[path]; !ok {
			changes = append(changes, FileChange{Path: path, Status: "deleted"})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes, nil
}

// RenderChanges formats changes for human review (secret-free: paths + status).
func RenderChanges(changes []FileChange) string {
	if len(changes) == 0 {
		return "(no file changes in the working copy)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d changed file(s):\n", len(changes))
	for _, c := range changes {
		fmt.Fprintf(&b, "  [%s] %s\n", c.Status, c.Path)
	}
	return b.String()
}

// snapshot returns a map of relative path -> content fingerprint for all regular
// files under root, excluding the same VCS/sensitive dirs and files that
// PrepareWorkCopy excludes. This keeps diffs/fingerprints consistent with what
// is actually placed in the work copy (so excluded files do not appear as
// spurious additions/deletions).
func snapshot(root string) (map[string][]byte, error) {
	out := make(map[string][]byte)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if excludedDirNames[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if isExcludedFile(info.Name()) {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		// Store the raw bytes as the fingerprint. Trees here are small (PoC
		// throwaway projects); for larger trees a hash would replace this.
		out[rel] = data
		return nil
	})
	return out, err
}
