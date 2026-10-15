// Package approval defines the human approval gate. The Control Plane requests a
// decision through the Gate interface; the CLI implementation prompts a human
// over stdin. Tests use a programmable gate.
package approval

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/habuka036/navette/internal/task"
)

// Request describes an approval the Control Plane needs.
type Request struct {
	TaskID string
	Kind   task.ApprovalKind // pre_exec or result
	Goal   string
	// Summary is extra human-facing context (for result approval: a digest of
	// produced artifacts).
	Summary string
}

// Gate obtains an approval decision. Implementations must respect context
// cancellation (so a blocked prompt can be interrupted by shutdown).
type Gate interface {
	Decide(ctx context.Context, req Request) (task.Decision, error)
}

// CLIGate prompts a human over an input stream and writes prompts to an output
// stream. It is the default PoC approval mechanism.
type CLIGate struct {
	In  io.Reader
	Out io.Writer
	By  string // identity recorded with the decision (e.g. "cli-user")
}

// Decide prompts for y/n and returns the decision.
//
// Cancellation note: Go's standard input Read cannot be unblocked by context
// alone. We run the blocking read in a goroutine and return on ctx.Done. The
// send channel is buffered (size 1) so that goroutine never blocks on send even
// after we have returned — it exits as soon as the pending Read yields a line or
// EOF. Thus at most one short-lived reader goroutine lingers per cancelled
// prompt, and it is reclaimed when stdin next produces input or closes. For a
// single-user CLI PoC this is safe and bounded; a future daemon front-end would
// use a cancellable input source instead.
func (g *CLIGate) Decide(ctx context.Context, req Request) (task.Decision, error) {
	label := "pre-execution"
	if req.Kind == task.ApprovalResult {
		label = "result"
	}
	fmt.Fprintf(g.Out, "\n[APPROVAL] task=%s kind=%s\n  goal: %s\n", req.TaskID, label, req.Goal)
	if req.Summary != "" {
		fmt.Fprintf(g.Out, "  summary: %s\n", req.Summary)
	}
	fmt.Fprintf(g.Out, "  approve? [y/N]: ")

	// Read a line, but abort if the context is cancelled.
	type result struct {
		text string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		r := bufio.NewReader(g.In)
		line, err := r.ReadString('\n')
		ch <- result{text: line, err: err}
	}()

	select {
	case <-ctx.Done():
		return task.Decision{}, ctx.Err()
	case res := <-ch:
		if res.err != nil && res.text == "" {
			return task.Decision{}, fmt.Errorf("approval: read input: %w", res.err)
		}
		approved := strings.EqualFold(strings.TrimSpace(res.text), "y")
		by := g.By
		if by == "" {
			by = "cli-user"
		}
		return task.Decision{Kind: req.Kind, Approved: approved, By: by}, nil
	}
}
