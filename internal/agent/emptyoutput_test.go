package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// TestToolResultMsgNeverCarriesEmptyText pins the invariant the openai-responses
// encoder depends on: a toolResult the model is shown always has text, so the
// function_call_output item it becomes always carries the required `output`.
//
// Live through onegw (Console Go) on 2026-09-17 a silent tool serialized to
// {"type":"function_call_output","call_id":"call_1"} and the upstream answered
// the turn — and every later turn, because the item was persisted in history —
// with:
//
//	HTTP 400 {"error":{"code":"400","message":"Error","type":"invalid_request_error"}}
//	`input[185]` missing required field `output`
//
// A 400 classifies as ClassBadRequest in ai.Classify, which the retry ladder
// does not retry, so the run ended instead of recovering. toolResultMsg is the
// one place every tool result is built, so the guard there covers all callers.
func TestToolResultMsgNeverCarriesEmptyText(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  tool.Result
	}{
		{"empty", tool.Result{}},
		{"whitespace", tool.Result{Text: "  \n\t "}},
		{"error with no message", tool.Result{IsError: true}},
		{"details only", tool.Result{Details: map[string]any{"exitCode": 0}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := toolResultMsg(ai.ToolCallBlock{ID: "call_1", Name: "glob"}, tc.res, 0)
			if strings.TrimSpace(m.Text()) != ai.ToolOutputPlaceholder {
				t.Fatalf("text = %q, want the placeholder", m.Text())
			}
			if m.ToolCallID != "call_1" || m.ToolName != "glob" {
				t.Fatalf("call identity lost: %#v", m)
			}
			if m.DurationMS != 0 {
				t.Fatalf("zero duration leaked onto the message: %d", m.DurationMS)
			}
		})
	}
}

// TestToolResultMsgKeepsRealText guards the other direction: the placeholder
// must not overwrite an ordinary result, and error/details survive the guard.
// DurationMS is stamped from the call's wall time so /trajectory and resume
// can show the same latency the live tool box already painted.
func TestToolResultMsgKeepsRealText(t *testing.T) {
	m := toolResultMsg(ai.ToolCallBlock{ID: "call_2", Name: "bash"},
		tool.Result{Text: "hello", IsError: true, Details: map[string]any{"exitCode": 1}},
		70*time.Millisecond)
	if m.Text() != "hello" || !m.IsError || m.ToolCallID != "call_2" || m.ToolName != "bash" {
		t.Fatalf("toolResult mutated: %#v", m)
	}
	if m.Details == nil {
		t.Fatal("details lost")
	}
	if m.DurationMS != 70 {
		t.Fatalf("DurationMS = %d, want 70 (persisted for /trajectory + resume)", m.DurationMS)
	}
}

// TestBadRequestMissingOutputRetriesInsteadOfEndingTheRun is the regression for
// the reported failure end to end at the recovery layer: the provider answers
// the turn with the 400 that killed it, and the ladder must retry rather than
// hand the error back.
//
// ai.Classify calls a plain 400 ClassBadRequest, and the recovery switch has no
// case for it, so the run ended on the first try. A 400 that names a missing
// required field is a shape the rebuild can fix — the tool result the agent
// posts is never empty (ai.EnsureToolOutput) — so Classify now calls it
// transient and this ladder retries it.
func TestBadRequestMissingOutputRetriesInsteadOfEndingTheRun(t *testing.T) {
	bad := &ai.HTTPError{API: "openai-completions", Status: 400,
		Body: "HTTP 400: {\"error\":{\"code\":\"400\",\"message\":\"Error\",\"type\":\"invalid_request_error\"}} " +
			"`input[185]` missing required field `output`"}
	p := &fakeProvider{calls: []fakeScript{
		{err: bad},
		{events: []ai.Event{{Type: ai.EventStart}, textEvent("recovered"), doneEvent("recovered")}},
	}}
	a, _, p := storeAgent(t, p, CompactionConfig{})
	a.Retry = fastRetry()
	final, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}})
	if err != nil {
		t.Fatalf("Run: the marked 400 must be retried, got %v", err)
	}
	if final == nil || final.Text() != "recovered" {
		t.Fatalf("final = %#v", final)
	}
	if len(p.gotReqs) != 2 {
		t.Fatalf("stream calls = %d, want 2 (the 400 then the retry)", len(p.gotReqs))
	}

	// The other direction: a plain 400 (ClassBadRequest) is retried only
	// within the bounded escalation ladder; retry.infinite does not turn a
	// provider-rejected request into an outage replay.
	plain := &ai.HTTPError{API: "openai-completions", Status: 400, Body: `{"error":{"message":"bad model"}}`}
	p2 := &fakeProvider{calls: []fakeScript{{err: plain}, {err: plain}, {err: plain}}}
	a2, _, p2 := storeAgent(t, p2, CompactionConfig{})
	a2.Retry = fastRetry()
	_, err = a2.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}})
	if err == nil {
		t.Fatal("a plain 400 must surface after bounded retries")
	}
	want := 1 + maxEscalationRounds
	if len(p2.gotReqs) != want {
		t.Fatalf("stream calls = %d, want %d (initial + escalation retries)", len(p2.gotReqs), want)
	}
}

// TestOpaqueBadRequestDoesNotUseAllTargetsDownNotice pins the incident from
// 2026-09-24: onegw returned a gateway-shaped 400 with no field detail, so
// ClassBadRequest fell into the default recovery branch and infinite retry
// kept replaying it. A bad request must surface, not masquerade as an outage.
func TestOpaqueBadRequestDoesNotUseAllTargetsDownNotice(t *testing.T) {
	bad := &ai.HTTPError{API: "openai-completions", Status: 400,
		Body: `{"error":{"code":"400","message":"Upstream request failed: [invalid_request_error] invalid request","type":"invalid_request_error"}}`}
	p := &fakeProvider{calls: []fakeScript{{err: bad}, {err: bad}, {err: bad}}}
	a, _, p := storeAgent(t, p, CompactionConfig{})
	a.Retry = RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Infinite: true}
	var notices []error
	a.Hooks = TurnHooksFunc{OnEventF: func(ev ai.Event) {
		if ev.Type == ai.EventError && strings.Contains(ev.Err.Error(), "all targets down") {
			notices = append(notices, ev.Err)
		}
	}}
	_, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}})
	var httpErr *ai.HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error = %v, want the provider HTTP error", err)
	}
	if httpErr.Status != 400 || !strings.Contains(httpErr.Body, "invalid request") {
		t.Fatalf("HTTP error = %+v, want the opaque 400", httpErr)
	}
	want := 1 + maxEscalationRounds
	if len(p.gotReqs) != want {
		t.Fatalf("stream calls = %d, want %d bounded retries", len(p.gotReqs), want)
	}
	if len(notices) != 0 {
		t.Fatalf("all-targets-down notices = %v, want none for a bad request", notices)
	}
}
