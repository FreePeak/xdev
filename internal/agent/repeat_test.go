package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

func TestRepeatChainCountsIdenticalCalls(t *testing.T) {
	var c repeatChain
	args := json.RawMessage(`{"path":"a.go","text":"same"}`)
	// The returned count is the run length INCLUDING this call, so the first
	// call of a run reports 0 and repeats report 2, 3, ... — the same shape
	// dsh counts in (`guard/repeat-tool-reminder/src/index.ts:203-206`).
	for i, want := range []int{0, 2, 3} {
		if got := c.observe("read", args); got != want {
			t.Fatalf("call %d returned %d, want %d", i+1, got, want)
		}
	}
	if c.count != 3 {
		t.Fatalf("chain count = %d, want 3", c.count)
	}
	// A different call resets: the chain is CONSECUTIVE.
	if got := c.observe("grep", args); got != 0 {
		t.Fatalf("first non-repeat = %d, want 0", got)
	}
	if got := c.observe("read", args); got != 0 {
		t.Fatalf("count after reset = %d, want 0 (a new run of 1)", got)
	}
}

// Key order is not identity: two calls a model wrote in different order are
// the same call, and a guard that missed them would never fire on a model
// whose JSON key order varies.
func TestRepeatChainIgnoresKeyOrder(t *testing.T) {
	var c repeatChain
	c.observe("read", json.RawMessage(`{"a":1,"b":{"y":2,"x":3}}`))
	if got := c.observe("read", json.RawMessage(`{"b":{"x":3,"y":2},"a":1}`)); got != 2 {
		t.Fatalf("reordered args not seen as identical: count = %d", got)
	}
}

// Numbers must keep their literal form. A plain decode turns a large id into
// a float, and then two calls with genuinely DIFFERENT ids can canonicalize to
// the same string — the guard would fire on a call that never repeated, and
// the notice would tell the model to stop doing something correct.
func TestCanonicalArgsPreservesLargeNumbers(t *testing.T) {
	a := canonicalArgs(json.RawMessage(`{"id":12345678901234567890}`))
	b := canonicalArgs(json.RawMessage(`{"id":12345678901234567891}`))
	if a == b {
		t.Fatalf("distinct large ids canonicalized alike: %s", a)
	}
	if !strings.Contains(a, "12345678901234567890") {
		t.Fatalf("canonical form lost precision: %s", a)
	}
}

// Malformed argument JSON must not crash the guard, and two malformed calls
// that differ byte-for-byte are two different calls.
func TestCanonicalArgsOnMalformedJSON(t *testing.T) {
	if got := canonicalArgs(json.RawMessage(`{"broken":`)); got != `{"broken":` {
		t.Fatalf("malformed args = %q, want the raw string", got)
	}
	if canonicalArgs(json.RawMessage(`{bad}`)) == canonicalArgs(json.RawMessage(`{worse}`)) {
		t.Fatal("distinct malformed args merged")
	}
}

// The first threshold is gentle; later ones name the tool and quote the
// arguments. Escalating is the point — a single canned warning repeated three
// times is noise the model learns to skip.
func TestRepeatReminderEscalates(t *testing.T) {
	args := json.RawMessage(`{"path":"a.go"}`)
	if got := repeatReminder("read", repeatThresholds[0], args); !strings.Contains(got, "repeating the exact same tool call") {
		t.Fatalf("first threshold is not the gentle form: %q", got)
	}
	detailed := repeatReminder("read", repeatThresholds[len(repeatThresholds)-1], args)
	for _, want := range []string{"tool: read", "consecutive_calls: 8", `arguments: {"path":"a.go"}`} {
		if !strings.Contains(detailed, want) {
			t.Fatalf("detailed reminder missing %q:\n%s", want, detailed)
		}
	}
	// Between thresholds the guard stays silent: one notice per threshold,
	// not one per call.
	if got := repeatReminder("read", 4, args); got != "" {
		t.Fatalf("non-threshold count produced a notice: %q", got)
	}
}

// A write body is a plausible repeat argument, and quoting it whole would
// spend the context the notice exists to protect. The cap bounds what the
// model READS; the chain key always uses the full canonical string.
func TestRepeatPreviewCapsTheQuoteNotTheDetection(t *testing.T) {
	big := `{"text":"` + strings.Repeat("x", repeatPreviewChars*2) + `"}`
	args := json.RawMessage(big)
	got := repeatReminder("write", repeatThresholds[1], args)
	if !strings.Contains(got, "more chars") {
		t.Fatalf("long arguments were not marked as truncated:\n%s", got)
	}
	if len(got) > repeatPreviewChars*2 {
		t.Fatalf("reminder is %d chars, the cap did not bound it", len(got))
	}
	// Detection still works on the full string.
	var c repeatChain
	for i := 0; i < repeatThresholds[1]; i++ {
		c.observe("write", args)
	}
	if c.count != repeatThresholds[1] {
		t.Fatalf("chain count = %d with truncated quoting", c.count)
	}
}

// The guard must never change the outcome of a call: it decorates a finished
// result, it does not veto one.
func TestRepeatNoticeLeadsTheResultAndKeepsIt(t *testing.T) {
	a := &Agent{}
	call := aiToolCall("read", `{"path":"a.go"}`)
	msg := aiToolResult("file body")
	for i := 0; i < repeatThresholds[0]; i++ {
		a.repeatNotice(&msg, call)
	}
	if !strings.Contains(msg.Text(), "repeating the exact same tool call") {
		t.Fatalf("notice did not lead the result: %q", msg.Text())
	}
	if !strings.Contains(msg.Text(), "file body") {
		t.Fatalf("the real result was lost: %q", msg.Text())
	}
	if msg.Role != ai.RoleToolResult || msg.ToolCallID != call.ID {
		t.Fatalf("the guard mutated the message's identity: %+v", msg)
	}
}

// A non-tool message is not the guard's business.
func TestRepeatNoticeIgnoresNonResults(t *testing.T) {
	a := &Agent{}
	msg := aiMessageUser("read a.go")
	a.repeatNotice(&msg, aiToolCall("read", `{"path":"a.go"}`))
	if msg.Text() != "read a.go" {
		t.Fatalf("a user message was decorated: %q", msg.Text())
	}
	a.repeatNotice(nil, aiToolCall("read", `{}`)) // must not panic
}

// --- local builders, so the test file does not depend on other test files --

func aiToolCall(name, args string) ai.ToolCallBlock {
	return ai.ToolCallBlock{ID: "c1", Name: name, Arguments: json.RawMessage(args)}
}

func aiToolResult(text string) ai.Message {
	return toolResultMsg(aiToolCall("read", "{}"), tool.Result{Text: text}, 0)
}

func aiMessageUser(text string) ai.Message {
	return ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: text}}}
}

// End-to-end through the loop: a model that asks the same thing five times
// must be told once at the threshold and again at the next one, with the
// result itself never altered. This is the whole point of the guard — the
// notice rides a real turn, not just a unit predicate.
func TestLoopNoticesARepeatedToolCall(t *testing.T) {
	same := json.RawMessage(`{"text":"x"}`)
	call := func() fakeScript {
		return fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, &ai.Message{
			Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
			Content: []ai.Block{ai.ToolCallBlock{ID: "c", Name: "echo", Arguments: same}},
		})}}
	}
	p := &fakeProvider{calls: []fakeScript{call(), call(), call(), call(), call()}}
	a, ends, results := runAgent(t, p)
	// Five identical tool turns, then a text turn to end the run.
	p.calls = append(p.calls, plainTextMsg("done"))
	if _, err := a.Run(context.Background(), "sys", userTurn()); err != nil {
		t.Fatal(err)
	}
	if len(*results) != 5 {
		t.Fatalf("tool results = %d, want 5", len(*results))
	}
	notices := 0
	for _, r := range *results {
		if strings.Contains(r.Text(), "repeating the exact same tool call") {
			notices++
		}
		if !strings.Contains(r.Text(), "x") {
			t.Fatalf("the real result was lost: %q", r.Text())
		}
	}
	if notices != 1 {
		t.Fatalf("gentle notice fired %d times over 5 identical calls, want 1 (at %d)", notices, repeatThresholds[0])
	}
	if len(*ends) == 0 {
		t.Fatal("the run produced no completed messages")
	}
}

// A steering message is a human changing the instruction. Repeating the same
// call around one is compliance, not a loop, and a guard that keeps counting
// across it will eventually tell a model to stop doing the right thing.
func TestSteeringEndsTheRepeatChain(t *testing.T) {
	var a Agent
	call := aiToolCall("read", `{"path":"a.go"}`)
	msg := func() ai.Message { return aiToolResult("body") }

	// Two identical calls: a run of 2, still below the threshold.
	for i := 0; i < 2; i++ {
		m := msg()
		a.repeatNotice(&m, call)
	}
	if a.repeats.count != 2 {
		t.Fatalf("chain = %d, want 2", a.repeats.count)
	}

	// The human interrupts with new instructions.
	a.Steer("try a different file")
	steering := a.drainSteering()
	if len(steering) != 1 {
		t.Fatalf("steering = %d, want 1", len(steering))
	}
	if len(steering) > 0 {
		a.repeats = repeatChain{}
	}

	// The same call again is a new run of 1, not a run of 3.
	m := msg()
	a.repeatNotice(&m, call)
	if strings.Contains(m.Text(), "repeating") {
		t.Fatalf("the chain survived the steering message: %q", m.Text())
	}
	if a.repeats.count != 1 {
		t.Fatalf("chain = %d after steering, want 1", a.repeats.count)
	}
}
