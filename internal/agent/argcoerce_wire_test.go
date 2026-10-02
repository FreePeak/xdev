package agent

// End-to-end pin for the tool-argument repair (M14 follow-up, 2026-10-02).
// The unit tests in internal/tool pin the coercion itself; this one proves the
// agent loop actually calls it, using the exact arguments that failed seven
// times in the field against the hub tool.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// waitSpy is the hub tool's schema with a recording Execute, so the test needs
// no live hub and no subagent.
type waitSpy struct{ got *json.RawMessage }

func (w waitSpy) Name() string        { return "hub" }
func (w waitSpy) Description() string { return "spy" }
func (w waitSpy) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
		"op":{"type":"string"},
		"ids":{"type":"array","items":{"type":"string"}},
		"timeout":{"type":"number"}}}`)
}

func (w waitSpy) Execute(_ context.Context, args json.RawMessage) (tool.Result, error) {
	*w.got = args
	return tool.Result{Text: "wait ran"}, nil
}

// TestRunOneToolRepairsLiveFailureShapes fails on stock main: the spy refuses
// the flattened array and the quoted number with the same "malformed
// arguments" the hub tool reported in the field.
func TestRunOneToolRepairsLiveFailureShapes(t *testing.T) {
	var got json.RawMessage
	reg := tool.NewRegistry()
	reg.Register(waitSpy{got: &got})
	a := &Agent{Tools: reg, Hooks: &hookLog{}, Model: "m"}

	msg := a.runOneTool(context.Background(), ai.ToolCallBlock{
		Name:      "hub",
		Arguments: json.RawMessage(`{"op":"wait","ids":{"item":["hub-1","hub-2"]},"timeout":"600"}`),
	})
	if msg.IsError {
		t.Fatalf("live failure shape still refused: %s", msg.Text())
	}
	var back struct {
		Op      string   `json:"op"`
		IDs     []string `json:"ids"`
		Timeout float64  `json:"timeout"`
	}
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("tool received undecodable args %s: %v", got, err)
	}
	if len(back.IDs) != 2 || back.IDs[1] != "hub-2" || back.Timeout != 600 {
		t.Fatalf("tool received %s", got)
	}
}

// echoInterceptor is what the real harness wires: hooks.Bus.ToolCall returns
// call.Arguments unchanged when no hook revises, and ext.Manager.ToolCall
// returns the same bytes when no extension is subscribed. The chain then
// hands those bytes back as the "revised" payload.
type echoInterceptor struct{}

func (echoInterceptor) ToolCall(_ context.Context, call ai.ToolCallBlock) (json.RawMessage, error) {
	return call.Arguments, nil
}
func (echoInterceptor) ToolResult(_ context.Context, _ ai.ToolCallBlock, r json.RawMessage, _ bool) json.RawMessage {
	return r
}
func (echoInterceptor) Emit(context.Context, string, any) {}

// TestRunOneToolCoercesPastAnEchoingInterceptor is the pin for the real wired
// path. The repair happens, but every interceptor echoes call.Arguments back,
// so unless the repaired bytes also land on the call block the tool still
// receives the shape the model sent — which is exactly what happened in the
// field: the coercion ran, reported changed=true, and the hub tool refused
// the call anyway.
func TestRunOneToolCoercesPastAnEchoingInterceptor(t *testing.T) {
	var got json.RawMessage
	reg := tool.NewRegistry()
	reg.Register(waitSpy{got: &got})
	a := &Agent{Tools: reg, Hooks: &hookLog{}, Model: "m", Intercept: echoInterceptor{}}

	msg := a.runOneTool(context.Background(), ai.ToolCallBlock{
		Name:      "hub",
		Arguments: json.RawMessage(`{"op":"wait","ids":{"item":["hub-1","hub-2"]},"timeout":"600"}`),
	})
	if msg.IsError {
		t.Fatalf("live failure shape still refused: %s", msg.Text())
	}
	var back struct {
		IDs     []string `json:"ids"`
		Timeout float64  `json:"timeout"`
	}
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("tool received undecodable args %s: %v", got, err)
	}
	if len(back.IDs) != 2 || back.IDs[1] != "hub-2" || back.Timeout != 600 {
		t.Fatalf("tool received %s", got)
	}
}
