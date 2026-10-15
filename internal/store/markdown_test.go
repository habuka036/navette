package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/habuka036/navette/internal/task"
)

func TestRenderTaskMarkdown(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	seedTask(t, st, "md1")
	_ = st.Transition(ctx, "md1", task.AwaitingPreApproval, "awaiting", nil)

	md, err := st.RenderTaskMarkdown(ctx, "md1")
	if err != nil {
		t.Fatalf("RenderTaskMarkdown: %v", err)
	}
	for _, want := range []string{"# Task md1", "State:", "## History", "AwaitingPreApproval"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q\n---\n%s", want, md)
		}
	}
	// Must state it is a derived view, not authoritative.
	if !strings.Contains(md, "source of truth") {
		t.Error("markdown should note SQLite is the source of truth")
	}
	_ = time.Now
}
