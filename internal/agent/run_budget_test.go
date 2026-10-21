package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// Live 2026-10-21 (session 8efefc9e): one "tell me where we are" question
// ran 113 provider round trips and 182 tool calls over 17 minutes and
// produced no answer at all — the session ended in a provider failure, not a
// report. Every guard the loop had was satisfied the whole way: the turn cap
// (200) was not reached, the repeat guard never fired (no two calls were
// byte-identical), and the compaction ladder never fired (the window had
// room). Only a clock says "this has taken too long, say what you have".
func TestRunBudgetWrapsUpAndEndsTheRun(t *testing.T) {
	// Every turn asks for a tool call, so the run would never end on its own.
	p := &fakeProvider{}
	for i := 0; i < 8; i++ {
		p.calls = append(p.calls, fakeScript{events: toolCallEvents("echo", `{"text":"x"}`), delay: 5 * time.Millisecond})
	}
	a, s, _ := storeAgent(t, p, CompactionConfig{})
	a.Retry = fastRetry()
	a.RunBudget = time.Millisecond

	if _, err := a.Run(context.Background(), "sys", submitHistory(t, s, "tell me where we are")); err != nil {
		t.Fatalf("run: %v", err)
	}
	// The wrap-up must have reached the provider.
	sawWrap := false
	for _, req := range p.gotReqs {
		for _, m := range req.Messages {
			if m.Role == ai.RoleUser && strings.Contains(m.Text(), "time budget") {
				sawWrap = true
			}
		}
	}
	if !sawWrap {
		t.Fatalf("the budget wrap-up never reached the provider (%d requests)", len(p.gotReqs))
	}
}

// A budget that can be spent again is not a budget: the wrap-up goes out
// once, and a model that answers it with another tool call ends the run.
func TestRunBudgetIsNotSpentTwice(t *testing.T) {
	p := &fakeProvider{}
	for i := 0; i < 8; i++ {
		p.calls = append(p.calls, fakeScript{events: toolCallEvents("echo", `{"text":"x"}`), delay: 5 * time.Millisecond})
	}
	a, s, _ := storeAgent(t, p, CompactionConfig{})
	a.Retry = fastRetry()
	a.RunBudget = time.Millisecond

	if _, err := a.Run(context.Background(), "sys", submitHistory(t, s, "go")); err != nil {
		t.Fatalf("run: %v", err)
	}
	wraps := 0
	for _, e := range s.Entries() {
		m, ok := e.(*session.MessageEntry)
		if !ok {
			continue
		}
		if m.Message.Attribution == RunBudgetAttribution {
			wraps++
		}
	}
	if wraps != 1 {
		t.Fatalf("the wrap-up was injected %d times, want exactly 1", wraps)
	}
	// The run must have ended: the script had eight turns and the budget is
	// spent after the first.
	if len(p.gotReqs) > 4 {
		t.Fatalf("provider calls = %d, want the run to have ended on the budget", len(p.gotReqs))
	}
}

// A negative budget is an explicit opt-out: a host that knows what it is
// doing gets an unbounded run.
func TestRunBudgetNegativeIsUnbounded(t *testing.T) {
	a := &Agent{RunBudget: -1}
	if got := a.runBudget(); got != 0 {
		t.Fatalf("runBudget() = %v, want 0 (unbounded)", got)
	}
	a.RunBudget = 0
	if got := a.runBudget(); got != DefaultRunBudget {
		t.Fatalf("runBudget() = %v, want the default %v", got, DefaultRunBudget)
	}
}
