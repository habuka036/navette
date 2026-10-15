package task_test

import (
	"testing"

	"github.com/habuka036/navette/internal/task"
)

func TestCanTransition_Allowed(t *testing.T) {
	allowed := []struct{ from, to task.State }{
		{task.Pending, task.AwaitingPreApproval},
		{task.AwaitingPreApproval, task.Running},
		{task.AwaitingPreApproval, task.Rejected},
		{task.Running, task.AwaitingResultApproval},
		{task.Running, task.TimedOut},
		{task.Running, task.Cancelled},
		{task.Running, task.Indeterminate},
		{task.AwaitingResultApproval, task.Finalizing},
		{task.AwaitingResultApproval, task.Rejected},
		{task.Finalizing, task.Succeeded},
		{task.Finalizing, task.Failed},
	}
	for _, c := range allowed {
		if !task.CanTransition(c.from, c.to) {
			t.Errorf("expected %s -> %s to be allowed", c.from, c.to)
		}
	}
}

func TestCanTransition_Disallowed(t *testing.T) {
	disallowed := []struct{ from, to task.State }{
		{task.Pending, task.Running},                  // must go through pre-approval
		{task.Pending, task.Succeeded},                // cannot skip
		{task.AwaitingPreApproval, task.Succeeded},    // pre-approval is not result
		{task.AwaitingResultApproval, task.Running},   // no going back
		{task.AwaitingResultApproval, task.Succeeded}, // must go through Finalizing
		{task.Running, task.Succeeded},                // needs result approval
		{task.Succeeded, task.Running},                // terminal
		{task.Rejected, task.Running},                 // terminal
		{task.Indeterminate, task.Running},            // never auto-resume
		{task.Indeterminate, task.Succeeded},          // never auto-succeed
	}
	for _, c := range disallowed {
		if task.CanTransition(c.from, c.to) {
			t.Errorf("expected %s -> %s to be DISALLOWED", c.from, c.to)
		}
	}
}

func TestIsTerminal(t *testing.T) {
	terminal := []task.State{task.Succeeded, task.Rejected, task.Failed, task.Cancelled, task.TimedOut}
	for _, s := range terminal {
		if !s.IsTerminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
	nonTerminal := []task.State{task.Pending, task.AwaitingPreApproval, task.Running, task.AwaitingResultApproval, task.Indeterminate}
	for _, s := range nonTerminal {
		if s.IsTerminal() {
			t.Errorf("%s should NOT be terminal", s)
		}
	}
}
