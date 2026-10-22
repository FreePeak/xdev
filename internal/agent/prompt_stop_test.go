package agent

import (
	"strings"
	"testing"
)

// Live 2026-10-22, session adf5029b: a model answering "list me all pr urls
// opening xdev" had the answer at turn 5 and said so in its own reasoning —
//
//	"So: the answer is clear. There are ZERO open PRs in FreePeak/xdev. Let me
//	 double-check that the PR numbers against branch names…"
//
// — and then spent 15 more turns corroborating it, because it decided the
// answer was "too thin" and went looking for adjacent material to offer
// instead.
//
// The prompt already said "a question gets an answer … do the reading, give
// the report and stop". That is about not OVER-delivering by starting to
// implement. It says nothing about the other half: the answer was IN HAND
// and the model kept going anyway.
//
// This is the second half, added to the rule that already owns the territory
// (the prompt has a hard budget, so a rule REPLACES prose; it never
// appends). The prose it replaced was "Take the smaller reading, and say
// what you skipped" — the smaller reading is covered by "a question or a
// one-liner gets an answer" and by the "ask when the request is ambiguous"
// rule, and the prompt's budget had no room for both.
func TestBasePromptCarriesTheStopOnceYouHaveTheAnswer(t *testing.T) {
	p := strings.ToLower(SystemPromptBase)
	for _, want := range []struct {
		needle string
		why    string
	}{
		{"don't keep gathering once you have it", "the prompt never says a finished answer ends the run"},
		{"give the report and stop", "the report is the deliverable, and that is not stated"},
		{"don't start implementing", "a question must not turn into an implementation"},
	} {
		if !strings.Contains(p, want.needle) {
			t.Errorf("SystemPromptBase is missing %q — %s", want.needle, want.why)
		}
	}
}
