package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// A run ends when the MODEL stops calling tools, or when the turn cap is
// reached — never on a stopwatch.
//
// The shipped 6-minute DefaultRunBudget used to end productive runs: 97 wrap-ups
// across 80 sessions in this repo's own transcript store, 48 of them answered by
// a model that still wanted tools, so the run died at the next step boundary
// with no error, no notice and no exit code (2026-10-24). Every guard against
// it coming back is structural, so this pins the observable shape: a long run
// of tool turns executes EVERY turn's call and ends on the model's own prose,
// with no harness-authored user message anywhere in the middle.
func TestARunRunsUntilTheModelStops(t *testing.T) {
	const turns = 20
	p := &fakeProvider{}
	for i := 0; i < turns; i++ {
		// Distinct args per turn: the repeat guard ends a run that issues the
		// SAME call again, and that guard is not what this test is about.
		p.calls = append(p.calls, toolScript("echo", fmt.Sprintf(`{"text":"step-%d"}`, i)))
	}
	p.calls = append(p.calls, quietScript("done"))
	a, s, _ := storeAgent(t, p, CompactionConfig{})
	a.Retry = fastRetry()

	msg, err := a.Run(context.Background(), "sys", submitHistory(t, s, "keep going"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if msg == nil || msg.Text() != "done" {
		t.Fatalf("final message = %v, want the model's own answer", msg)
	}
	// Every turn's tool call got a result. A run cut short would leave the
	// batch it fired on unanswered.
	toolResults := 0
	for _, e := range s.Entries() {
		if me, ok := e.(*session.MessageEntry); ok && me.Message.Role == ai.RoleToolResult {
			toolResults++
		}
	}
	if toolResults < turns {
		t.Fatalf("%d tool results for %d turns — the run ended before the model did", toolResults, turns)
	}
	// No synthetic wrap-up: the only user-role messages are the one the person
	// typed and any keep-going nudge, which is a prompt continuation. A
	// budget wrap-up would show up here with its own attribution.
	for _, e := range s.Entries() {
		me, ok := e.(*session.MessageEntry)
		if !ok || me.Message.Role != ai.RoleUser {
			continue
		}
		switch me.Message.Attribution {
		case "", "user", PromptContinuationAttribution:
		default:
			t.Fatalf("a harness message %q reached the transcript — the run was ended by the harness, not the model",
				me.Message.Attribution)
		}
	}
}

// toolScript is one turn that asks for a tool call with the given arguments.
func toolScript(name, args string) fakeScript {
	return fakeScript{events: toolCallEvents(name, args)}
}

// The keep-going gate still knows about its own nudge, and a real user turn
// clears it. (The run-budget arm this used to carry is gone with the budget.)
func TestTheKeepGoingGateDoesNotNudgeTwice(t *testing.T) {
	history := []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "do it"}}},
		{Role: ai.RoleAssistant, Content: []ai.Block{ai.ToolCallBlock{
			ID: "e1", Name: "echo", Arguments: json.RawMessage(`{"text":"x"}`)}}},
		{Role: ai.RoleUser, Attribution: PromptContinuationAttribution, Content: []ai.Block{ai.TextBlock{Text: PromptContinuationPrompt}}},
	}
	if keepGoingNudge(history, 0, true) {
		t.Fatal("the run was nudged twice")
	}
	// A real user turn is a new task boundary, so a run that mutated inside it
	// can be nudged again.
	history = append(history, ai.Message{Role: ai.RoleUser, Attribution: "user",
		Content: []ai.Block{ai.TextBlock{Text: "now merge them"}}})
	if !keepGoingNudge(history, 0, true) {
		t.Fatal("a new real ask after the nudge got no keep-going nudge")
	}
}
