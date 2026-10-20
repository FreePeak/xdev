package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// dedupEchoTool answers with a constant payload; only the call identity
// matters for the test below.
type dedupEchoTool struct{ calls int }

func (*dedupEchoTool) Name() string        { return "leankg_status" }
func (*dedupEchoTool) Description() string { return "test double" }
func (*dedupEchoTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"project":{"type":"string"}}}`)
}
func (d *dedupEchoTool) Execute(_ context.Context, _ json.RawMessage) (tool.Result, error) {
	d.calls++
	return tool.Result{Text: `{"elements":1}`}, nil
}

// TestRunOneToolCacheHitKeepsTheCurrentCallID pins the identity contract of
// the per-run dedup cache: a cached RESULT may be reused, but the result
// message must still answer the call the model just made.
//
// The cache is keyed by name+args (loop.go), so a model that repeats an
// identical call hits it. The cached ai.Message carries the ToolCallID of
// the EARLIER call, and returning it verbatim answers a call id the current
// assistant turn never issued — and re-uses an id an earlier turn already
// answered. Strict tool-calling upstreams reject that history
// ("invalid params, tool call id is invalid", MiniMax 2013), and because the
// poisoned turn is already in the session, every retry replays it.
func TestRunOneToolCacheHitKeepsTheCurrentCallID(t *testing.T) {
	tt := &dedupEchoTool{}
	reg := tool.NewRegistry()
	reg.Register(tt)
	ag := &Agent{Tools: reg, Hooks: TurnHooksFunc{}, Model: "m"}

	args := json.RawMessage(`{"project":"/tmp/x"}`)
	first := ag.runOneTool(context.Background(), ai.ToolCallBlock{
		ID: "call_first_1", Name: "leankg_status", Arguments: args,
	})
	second := ag.runOneTool(context.Background(), ai.ToolCallBlock{
		ID: "call_second_1", Name: "leankg_status", Arguments: args,
	})

	if first.ToolCallID != "call_first_1" {
		t.Fatalf("first result answered %q, want call_first_1", first.ToolCallID)
	}
	if second.ToolCallID != "call_second_1" {
		t.Fatalf("second result answered %q, want call_second_1 (the call it answers)", second.ToolCallID)
	}
}
