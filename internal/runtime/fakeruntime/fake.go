// Package fakeruntime provides an AgentRuntime implementation that uses no LLM
// and no external process. It is used to validate the Control Plane (state
// machine, approval gates, persistence, timeouts) without launching a real
// agent such as the Kiro CLI.
package fakeruntime

import (
	"context"
	"time"

	"github.com/habuka036/navette/internal/runtime"
)

// Fake is a deterministic AgentRuntime for tests and Control Plane validation.
type Fake struct {
	// Steps is the number of Execute calls before reporting Done. Default 1.
	Steps int
	// Final is the final text reported when Done.
	Final string
	// Err, when non-nil, is returned from Execute to simulate a runtime failure.
	Err error
	// Delay, when > 0, makes each Execute block for this duration (respecting
	// context cancellation) to exercise timeout/cancel handling.
	Delay time.Duration

	calls int
}

// compile-time assertion.
var _ runtime.AgentRuntime = (*Fake)(nil)

// Name identifies the fake runtime.
func (f *Fake) Name() string { return "fake" }

// Execute performs one simulated step.
func (f *Fake) Execute(ctx context.Context, req runtime.RunRequest) (runtime.RunResult, error) {
	if f.Delay > 0 {
		select {
		case <-time.After(f.Delay):
		case <-ctx.Done():
			return runtime.RunResult{}, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return runtime.RunResult{}, err
	}
	if f.Err != nil {
		return runtime.RunResult{}, f.Err
	}

	f.calls++
	steps := f.Steps
	if steps <= 0 {
		steps = 1
	}
	done := f.calls >= steps
	final := ""
	if done {
		final = f.Final
		if final == "" {
			final = "fake run complete"
		}
	}
	return runtime.RunResult{
		Done:      done,
		FinalText: final,
		Events: []runtime.OutputEvent{
			{Kind: "message", Text: "fake step"},
		},
	}, nil
}
