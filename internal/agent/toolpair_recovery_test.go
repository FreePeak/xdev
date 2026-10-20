package agent

import (
	"context"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// TestToolPairing400RebuildsTheHistoryAndRecovers is the regression for the
// reported failure end to end at the recovery layer.
//
// Live 2026-10-20: xdev → onegw → OpenCode Zen (MiniMax M3.1 Flash Free)
// answered every turn with
//
//	HTTP 400 {"error":{"code":"400","message":"Upstream request failed: [bad_request_error] invalid params, tool call id is invalid (2013)","type":"server_error"}}
//
// The request carried a tool result whose id answered an EARLIER turn's call
// (the agent's dedup cache hands a repeated call the earlier call's result,
// id included), so the upstream paired nothing with it. The body matches the
// generic "upstream request failed" relay, so it classified transient and the
// ladder replayed the same rejected request forever ("all targets down",
// every target healthy).
//
// The fix has two halves and this test needs both: the 400 classifies as a
// bad request (so the ladder rebuilds the history instead of replaying it),
// and the rebuild drops the result that answers no live call (so the rebuilt
// request is one the upstream accepts).
func TestToolPairing400RebuildsTheHistoryAndRecovers(t *testing.T) {
	bad := &ai.HTTPError{API: "openai-completions", Status: 400,
		Body: `{"error":{"code":"400","message":"Upstream request failed: [bad_request_error] invalid params, tool call id is invalid (2013)","type":"server_error"}}`}
	p := &fakeProvider{calls: []fakeScript{
		{err: bad},
		{events: []ai.Event{{Type: ai.EventStart}, textEvent("recovered"), doneEvent("recovered")}},
	}}
	a, s, p := storeAgent(t, p, CompactionConfig{})
	a.Retry = fastRetry()

	// The poisoned history: call_x asked and answered, then a second
	// assistant turn asks call_y — and the result that follows carries
	// call_x's id, so call_y is never answered and call_x is answered
	// twice. This is what the dedup cache wrote into the session.
	seed := []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}},
		{Role: ai.RoleAssistant, Content: []ai.Block{ai.ToolCallBlock{ID: "call_x", Name: "echo", Arguments: []byte(`{}`)}}},
		{Role: ai.RoleToolResult, ToolCallID: "call_x", ToolName: "echo", Content: []ai.Block{ai.TextBlock{Text: "first"}}},
		{Role: ai.RoleAssistant, Content: []ai.Block{ai.ToolCallBlock{ID: "call_y", Name: "echo", Arguments: []byte(`{}`)}}},
		{Role: ai.RoleToolResult, ToolCallID: "call_x", ToolName: "echo", Content: []ai.Block{ai.TextBlock{Text: "stale"}}},
	}
	for _, m := range seed {
		if err := s.Append(&session.MessageEntry{Message: m}); err != nil {
			t.Fatal(err)
		}
	}

	final, err := a.Run(context.Background(), "sys", seed[:1])
	if err != nil {
		t.Fatalf("Run: a tool-pairing 400 must be retried on a rebuilt history, got %v", err)
	}
	if final == nil || final.Text() != "recovered" {
		t.Fatalf("final = %#v", final)
	}
	if len(p.gotReqs) != 2 {
		t.Fatalf("stream calls = %d, want 2 (the 400 then the rebuild-and-retry)", len(p.gotReqs))
	}

	// The retried request must carry no result that answers no live call:
	// the stale one is gone, and the call it never answered is gone too.
	var toolMsgs []string
	for _, m := range p.gotReqs[1].Messages {
		if m.Role == ai.RoleToolResult {
			toolMsgs = append(toolMsgs, m.ToolCallID)
		}
	}
	if len(toolMsgs) != 1 || toolMsgs[0] != "call_x" {
		t.Fatalf("retried request carries %v, want only [call_x] (the stale result must be dropped)", toolMsgs)
	}
}
