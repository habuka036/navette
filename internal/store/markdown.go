package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/habuka036/navette/internal/task"
)

// RenderTaskMarkdown produces a human-facing Markdown review view for a task,
// derived from the SQLite source of truth. The Markdown is a read-only
// projection for human review; it is NOT authoritative state.
func (s *Store) RenderTaskMarkdown(ctx context.Context, taskID string) (string, error) {
	t, err := s.GetTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	evs, err := s.Events(ctx, taskID)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Task %s\n\n", t.ID)
	fmt.Fprintf(&b, "- Goal: %s\n", t.Goal)
	fmt.Fprintf(&b, "- State: **%s**\n", t.State)
	fmt.Fprintf(&b, "- Created: %s\n", t.CreatedAt.Format("2006-01-02 15:04:05 MST"))
	fmt.Fprintf(&b, "- Updated: %s\n", t.UpdatedAt.Format("2006-01-02 15:04:05 MST"))
	if t.Constraints.WorkDir != "" {
		fmt.Fprintf(&b, "- WorkDir (temp copy): %s\n", t.Constraints.WorkDir)
	}
	fmt.Fprintf(&b, "- MaxIterations: %d\n", t.Constraints.MaxIterations)

	b.WriteString("\n> This Markdown is a read-only review view derived from SQLite ")
	b.WriteString("(the source of truth). Result approval accepts artifacts only; ")
	b.WriteString("it does not write back to any real repository or create a git commit.\n")

	b.WriteString("\n## History\n\n")
	if len(evs) == 0 {
		b.WriteString("_(no events)_\n")
		return b.String(), nil
	}
	b.WriteString("| Time | Kind | Transition | Reason |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, e := range evs {
		transition := ""
		if e.Kind == task.EventStateTransition {
			transition = fmt.Sprintf("%s → %s", e.From, e.To)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n",
			e.At.Format("15:04:05.000"), e.Kind, transition, sanitiseCell(e.Reason))
	}
	return b.String(), nil
}

// sanitiseCell keeps a reason on one Markdown table cell line.
func sanitiseCell(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", "\\|")
	return s
}
