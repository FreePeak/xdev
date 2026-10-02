package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/FreePeak/xdev/internal/ai"
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
	count := a.repeats.observe(call.Name, call.Arguments)
	if count == 0 {
		return
	}
	notice := repeatReminder(call.Name, count, call.Arguments)
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
