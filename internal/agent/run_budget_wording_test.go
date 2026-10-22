package agent

import (
	"strings"
	"testing"
)

// The budget's wrap-up used to forbid tool calls outright: "answer now, in
// prose, with what you have found so far, and do not call any more tools".
//
// That is right for a run that OWES NOTHING — a question ends at the report
// — and wrong for a run that still owes a change: the clock turned a slow
// run into a run that reported and changed nothing.
//
// Measured 2026-10-22, a ladder of eight asks from "what does this project
// do?" to "the tests do not pass, find the cause and fix it": every step
// that had to CHANGE something ended with an untouched tree, and every one
// of them hit the budget. The read steps were lean (12-29 s).
func TestTheRunBudgetWrapUpStopsExploringNotActing(t *testing.T) {
	p := strings.ToLower(RunBudgetPrompt)
	for _, want := range []struct {
		needle string
		why    string
	}{
		{"do not explore further", "the wrap-up must end the SEARCH"},
		{"if your task was to make a change", "a run that owes a change must be allowed to make it"},
		{"make it now", "the change is the deliverable, and the wrap-up must not forbid it"},
		{"report in prose", "the report is still the deliverable for a read-only run"},
	} {
		if !strings.Contains(p, want.needle) {
			t.Errorf("RunBudgetPrompt is missing %q — %s", want.needle, want.why)
		}
	}
	// And the old wording is the thing that broke it: a run that owes a
	// change must not be told to stop calling tools.
	if strings.Contains(p, "do not call any more tools") {
		t.Error("RunBudgetPrompt still forbids tool calls — a run that owes a change cannot make it")
	}
}
