package agent

import (
	"encoding/json"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
)

// Live 2026-10-22, session adf5029b: a TUI run of "list me all pr urls
// opening xdev" went on for 22 assistant turns and 28 tool calls over 6
// minutes 19 seconds, and the answer arrived only when the run-budget
// wrap-up forced it.
//
// The wrap-up worked: the model answered in prose. And then the keep-going
// gate fired, because the gate's guard knew about the gate's OWN nudge
// (lastWasPromptCont) and about nothing else — so the turn's earlier `git
// fetch`, a genuine mutation, armed the gate, and the run continued for one
// more turn AFTER the clock had already ended it.
//
// The budget is the run's STOP. A stop that something else can override is
// not a stop.
func TestTheRunBudgetsStopIsAuthoritative(t *testing.T) {
	history := []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "list me all pr urls"}}},
		// The turn that mutated: `git fetch` is a write.
		{Role: ai.RoleAssistant, Content: []ai.Block{ai.ToolCallBlock{
			ID: "m1", Name: "echo", Arguments: json.RawMessage(`{"text":"x"}`)}}},
		// The budget fired, and the model answered in prose.
		{Role: ai.RoleUser, Attribution: RunBudgetAttribution, Content: []ai.Block{ai.TextBlock{Text: RunBudgetPrompt}}},
		{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "here is the report"}}},
	}
	if keepGoingNudge(history, 0, true) {
		t.Fatal("a keep-going nudge fired AFTER the run budget ended the run — the budget's stop must be authoritative")
	}
}

// The other direction, unchanged: a run that mutated and has NOT stopped
// still gets the nudge. That is the feature the gate exists for.
func TestTheKeepGoingGateStillFiresForUnfinishedWork(t *testing.T) {
	history := []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "do it"}}},
		{Role: ai.RoleAssistant, Content: []ai.Block{ai.ToolCallBlock{
			ID: "e1", Name: "echo", Arguments: json.RawMessage(`{"text":"x"}`)}}},
	}
	if !keepGoingNudge(history, 0, true) {
		t.Fatal("a run that mutated inside the current task got no keep-going nudge")
	}
	// And a read-only run never does.
	if keepGoingNudge(history, 0, false) {
		t.Fatal("a read-only run got a keep-going nudge")
	}
}

// The gate's own nudge stops it too: a run that already nudged once and
// then yielded again does not nudge a second time.
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
}

// A REAL user turn after the budget ends the budget's authority: the person
// asked for something new, and that new work can be nudged.
func TestARealUserTurnEndsTheBudgetsAuthority(t *testing.T) {
	history := []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "list me all pr urls"}}},
		{Role: ai.RoleUser, Attribution: RunBudgetAttribution, Content: []ai.Block{ai.TextBlock{Text: RunBudgetPrompt}}},
		{Role: ai.RoleUser, Attribution: "user", Content: []ai.Block{ai.TextBlock{Text: "now merge them"}}},
	}
	if !keepGoingNudge(history, 0, true) {
		t.Fatal("a new real ask after the budget got no keep-going nudge")
	}
}
