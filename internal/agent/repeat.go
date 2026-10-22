package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// Repeat-call guard (ported from deepseek-harness
// `guard/repeat-tool-reminder/src/index.ts:191-211`, read at `21638c56`).
//
// A model that has stopped making progress usually looks like this: the same
// tool, the same arguments, three turns running — each costing a full model
// round trip to re-derive a conclusion it already had. dsh counts it and
// injects a notice; it never vetoes, because a guard that blocks a call the
// model genuinely needs is worse than the loop it was meant to break.
//
// Two details are worth keeping from dsh's version, because both are the
// difference between a useful signal and noise:
//
//   - Counting happens where the result is built, so a DENIED call counts too.
//     A model hammering a denied call is the loop most worth breaking, and a
//     counter that only saw successes would miss it.
//   - A user interjection ends the run's chain. Repetition across a new
//     instruction is not a loop, it is compliance.
//
// The identity key is the tool name plus its arguments, canonicalized so that
// two calls differing only in key order compare equal. Go's json.Marshal sorts
// map keys on its own, so the deep key-sort dsh hand-writes
// (`sortJsonValue`) is the standard library here. The one thing stdlib does
// NOT do is preserve numeric fidelity, hence UseNumber: a plain decode turns
// 12345678901234567890 into 1.2345678901234567e+19, and two calls a model made
// with the same large id would then canonicalize to the same string for the
// wrong reason.

// repeatThresholds are the consecutive-repeat counts that produce a notice:
// the first is a gentle nudge, the rest name the tool and quote the arguments.
// dsh's defaults; the shape (escalate, don't nag) is the point.
var repeatThresholds = []int{3, 5, 8}

// repeatPreviewChars caps the argument quote in a detailed notice. A `write`
// body is a plausible repeat argument, and quoting it whole would spend the
// context the notice exists to protect. Detection always uses the FULL
// canonical string — the cap is on what the model reads, never on what is
// compared.
const repeatPreviewChars = 500

// repeatChain is one run's consecutive-repeat state: the last call's identity
// key and how many times it has now run in a row.
type repeatChain struct {
	key   string
	count int
}

// coarseChain is one run's REPEATED-WORK state: a per-key TOTAL over the
// whole run, not a consecutive chain.
//
// It exists because the chain cannot see a re-derivation. Live 2026-10-22
// (session adf5029b): a model answering "list me all pr urls opening xdev"
// asked the same question in ten spellings — `gh pr list`, `gh api
// repos/…/pulls`, `gh api graphql`, `gh search prs`, each with a different
// projection, a different page size and a different pipe. Every spelling is
// a different EXACT key, and the chain resets on each one, so 28 tool calls
// looked like 28 first attempts.
//
// The key is the coarse one (tool.CanonicalCommand): the program's path and
// its targets, with a projection's value dropped and a magnitude normalized.
// It is coarse ON PURPOSE and it is used for the NOTICE only, never for the
// hard break and never for the tool-result cache: a notice is text, and a
// text notice cannot hand the model a wrong answer.
type coarseChain struct {
	counts map[string]int
}

// observe advances the chain for one completed call and returns the
// consecutive count of the identical call, or 0 when the call does not
// participate (a transparent call neither counts nor resets).
func (c *repeatChain) observe(name string, args json.RawMessage) int {
	canonical := canonicalArgs(args)
	key := name + "\x00" + canonical
	if c.key != key {
		c.key, c.count = key, 1
		return 0
	}
	c.count++
	return c.count
}

// coarseKey is the identity of a call's WORK, as opposed to its exact
// arguments.
//
// A bash call is judged by its command's canonical form. Every other tool
// keeps the exact key: their arguments are structured already, and there is
// no shell to re-spell.
//
// An EMPTY command is not work — the bash tool refuses it ("command is
// required") — so it is keyed by the call's own id and never groups with the
// other refusals. Counting them would put "you have done the same thing
// five times" in front of a model that only ever asked for nothing.
func coarseKey(call ai.ToolCallBlock) string {
	if call.Name == "bash" {
		var a struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(call.Arguments, &a) == nil {
			c := tool.CanonicalCommand(a.Command)
			if c == "" {
				return "bash\x00empty\x00" + call.ID
			}
			return "bash\x00" + c
		}
	}
	return call.Name + "\x00" + canonicalArgs(call.Arguments)
}

// observe counts one call's work and returns the run's TOTAL for that key.
//
// It is a total, not a chain: the model's re-derivations were interleaved
// with other work, and a chain would have been reset by every one of them.
func (c *coarseChain) observe(call ai.ToolCallBlock) int {
	if c.counts == nil {
		c.counts = map[string]int{}
	}
	key := coarseKey(call)
	c.counts[key]++
	return c.counts[key]
}

// reset clears the per-run totals. A user interjection ends them: work that
// was asked for again is compliance, not a loop.
func (c *coarseChain) reset() {
	c.counts = map[string]int{}
}

// canonicalArgs renders a call's arguments in a form two calls with the same
// meaning share. Numbers keep their literal form (see the file comment);
// malformed JSON falls back to the raw string, because two malformed calls
// that differ byte-for-byte are two different calls and must not merge.
func canonicalArgs(args json.RawMessage) string {
	if len(args) == 0 {
		return ""
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return string(args)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return string(args)
	}
	return string(out)
}

// repeatReminder is the notice to LEAD a repeated call's result with, or ""
// when this count is not a threshold. It leads rather than appends for the
// same reason the TTSR reminder leads (loop.go): in a long result, the first
// line is the one the model reads.
func repeatReminder(name string, count int, args json.RawMessage) string {
	if !repeatThresholdHit(count) {
		return ""
	}
	if count == repeatThresholds[0] {
		return "You are repeating the exact same tool call with identical arguments. " +
			"Read the previous result before calling again: if the task is not done, " +
			"use a different approach or different arguments instead of repeating the call."
	}
	canonical := canonicalArgs(args)
	return fmt.Sprintf(
		"Repeated tool call detected:\n- tool: %s\n- consecutive_calls: %d\n- arguments: %s\n"+
			"These calls are not making progress. Do not call this tool with these exact "+
			"arguments again. Read the latest result, then choose a different action, "+
			"different arguments, or finish if you already have enough evidence.",
		name, count, previewArgs(canonical))
}

func repeatThresholdHit(count int) bool {
	for _, t := range repeatThresholds {
		if count == t {
			return true
		}
	}
	return false
}

// coarseThresholds are the run-totals that produce the REPEATED-WORK notice.
//
// The threshold matches the exact chain's first one. A coarse key groups
// calls that are SIMILAR rather than identical, so it reaches 3 on more
// runs than the exact chain does (measured over 799 of this repo's own
// runs: 20.9% against 13.9%), and the extra runs it reaches are the ones
// re-deriving. The notice it produces is a nudge, not a break.
//
// There is no hard break on a coarse count. The break at 8 stays on the
// EXACT chain, where eight identical calls in a row really is a loop; a
// coarse count of 8 can be a model reading a file in chunks, and breaking a
// run for that would cost the user their work.
var coarseThresholds = []int{3}

// coarseThresholdHit reports whether a run-total is one of the notice
// thresholds.
func coarseThresholdHit(count int) bool {
	for _, t := range coarseThresholds {
		if count == t {
			return true
		}
	}
	return false
}

// coarseReminder is the notice for a call that re-derives work the run has
// already done.
//
// It says the one thing the model needs: REPORT WHAT YOU HAVE. The live
// failure this exists for is a model that had the answer at turn 5 and spent
// 15 more turns corroborating it.
func coarseReminder(name string, count int) string {
	if !coarseThresholdHit(count) {
		return ""
	}
	return fmt.Sprintf(
		"Repeated work detected: this run has now done the same thing %d times, in %d different spellings.\n"+
			"You are re-deriving something you already have. Stop gathering: if you have the answer, "+
			"report it now, in prose. If you do not, say what is still missing and ask for it.",
		count, count)
}

// previewArgs head-truncates the canonical arguments for quoting, marking what
// was omitted so the model knows the quote is partial rather than the whole
// call.
func previewArgs(canonical string) string {
	if len(canonical) <= repeatPreviewChars {
		return canonical
	}
	return fmt.Sprintf("%s… (+%d more chars)", canonical[:repeatPreviewChars], len(canonical)-repeatPreviewChars)
}

// repeatNotice decorates one finished call's result with the guard's notice.
// A pointer, so a nil or contentless message is a no-op rather than a copy
// nobody reads.
func (a *Agent) repeatNotice(msg *ai.Message, call ai.ToolCallBlock) {
	if msg == nil || msg.Role != ai.RoleToolResult || len(msg.Content) == 0 {
		return
	}
	// The COARSE count is observed first and independently: it is the one
	// that sees a re-derivation, and it must not be shadowed by the exact
	// chain's early return below.
	coarse := a.coarseRepeats.observe(call)
	coarseNotice := coarseReminder(call.Name, coarse)
	count := a.repeats.observe(call.Name, call.Arguments)
	if count == 0 && coarseNotice == "" {
		return
	}
	// Veto: once the guard's highest threshold is reached, stop the run
	// instead of only nudging. The notice alone is ignored by a model that
	// is already stuck, so a hard break is required to end a tool-call loop.
	// Retry/network-error handling lives in oneTurnWithRecovery and is
	// unaffected: this only stops an identical-call loop.
	//
	// The break stays on the EXACT count. A coarse count of 8 can be a model
	// reading a file in chunks, and breaking a run for that would cost the
	// user their work.
	if count >= repeatThresholds[len(repeatThresholds)-1] {
		a.repeatBreak = true
	}
	notice := repeatReminder(call.Name, count, call.Arguments)
	if coarseNotice != "" {
		if notice != "" {
			notice = notice + "\n\n"
		}
		notice = coarseNotice + notice
	}
	if notice == "" {
		return
	}
	for i, b := range msg.Content {
		if tb, ok := b.(ai.TextBlock); ok {
			msg.Content[i] = ai.TextBlock{Text: notice + "\n\n" + strings.TrimLeft(tb.Text, "\n")}
			return
		}
	}
}
