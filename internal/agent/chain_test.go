package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/hooks"
	"github.com/FreePeak/xdev/internal/tool"
)

// chain_test: the agent's Interceptor composition (hooks + extension
// shape) and the lifecycle emits must fire through Agent.Run end to end.

// echoRecorder is a tiny interceptor that records Emit calls.
type echoRecorder struct {
	events []string
}

func (r *echoRecorder) ToolCall(_ context.Context, call ai.ToolCallBlock) (json.RawMessage, error) {
	args := call.Arguments
	return args, nil
}
func (r *echoRecorder) ToolResult(_ context.Context, _ ai.ToolCallBlock, result json.RawMessage, _ bool) json.RawMessage {
	return result
}
func (r *echoRecorder) Emit(_ context.Context, event string, _ any) {
	r.events = append(r.events, event)
}

func TestInterceptorChainComposes(t *testing.T) {
	rec := &echoRecorder{}
	c := NewChain(nil, rec) // nil parts must be dropped
	if c == nil {
		t.Fatal("chain with one live part must not be nil")
	}
	c.Emit(context.Background(), "tool_result", nil)
	c.Emit(context.Background(), "agent_end", nil)
	if len(rec.events) != 2 || rec.events[0] != "tool_result" {
		t.Fatalf("events = %v", rec.events)
	}
	if NewChain(nil, nil) != nil {
		t.Fatal("all-nil chain must be nil (agent nil-check contract)")
	}
}

// A configured tool_call hook must block the tool at the agent level,
// end to end through Agent.Run.
func TestAgentRunFiresHooksToolCall(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "blocked-marker")
	bus := hooks.FromSettings(map[string]any{
		"tool_call": "echo '{\"block\":true,\"reason\":\"policy: no writes\"}' && touch " + marker,
	})
	p := &fakeProvider{calls: []fakeScript{
		{events: toolCallEvents("write", `{"path":"x.txt","content":"y"}`)},
		{events: doneEvents("aborted by policy")},
	}}
	reg := tool.NewRegistry()
	reg.Register(&noopWriteTool{})
	ag := &Agent{Provider: p, Tools: reg, Model: "m", MaxTurns: 3, Intercept: NewChain(bus)}
	_, err := ag.Run(context.Background(), "sys", []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "write x.txt"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The hook command ran (marker created) and the tool never executed
	// (noopWriteTool would have recorded a call).
	if _, serr := os.Stat(marker); serr != nil {
		t.Fatal("tool_call hook did not run")
	}
	if noopWriteCalls != 0 {
		t.Fatalf("blocked tool still executed %d times", noopWriteCalls)
	}
	// The blocked reason must reach the model as the tool result.
	if len(p.gotReqs) < 2 {
		t.Fatalf("model never saw the blocked result (%d requests)", len(p.gotReqs))
	}
	last := p.gotReqs[len(p.gotReqs)-1].Messages
	found := false
	for _, m := range last {
		if m.Role == ai.RoleToolResult && m.IsError {
			for _, b := range m.Content {
				if tb, ok := b.(ai.TextBlock); ok && tb.Text != "" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("no error toolResult reached the model")
	}
}

var noopWriteCalls int

type noopWriteTool struct{}

func (noopWriteTool) Name() string { return "write" }
func (noopWriteTool) Description() string {
	return "test double"
}
func (noopWriteTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (noopWriteTool) Execute(_ context.Context, _ json.RawMessage) (tool.Result, error) {
	noopWriteCalls++
	return tool.Result{Text: "should never run"}, nil
}

// agent_start and agent_end must bracket every Run.
func TestAgentRunEmitsLifecycle(t *testing.T) {
	rec := &echoRecorder{}
	p := &fakeProvider{calls: []fakeScript{{events: doneEvents("all good")}}}
	ag := &Agent{Provider: p, Tools: tool.NewRegistry(), Model: "m", Intercept: rec}
	if _, err := ag.Run(context.Background(), "sys", []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}},
	}); err != nil {
		t.Fatal(err)
	}
	var start, end bool
	for _, ev := range rec.events {
		switch ev {
		case "session_start":
			start = true
		case "agent_end":
			end = true
		}
	}
	if !start || !end {
		t.Fatalf("events = %v", rec.events)
	}
}
