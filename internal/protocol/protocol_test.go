package protocol

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
)

// TestEventFromAIMapping pins the RPC contract's event translation: each
// stream event must reach the embedder with the fields it needs — and the
// done event must carry the materialized message (tool calls, usage), the
// one thing that made the wire useless for embedders when it was missing.
func TestEventFromAIMapping(t *testing.T) {
	tests := []struct {
		name string
		in   ai.Event
		want func(t *testing.T, ev Event)
	}{
		{
			name: "start advertises the resolved model",
			in:   ai.Event{Type: ai.EventStart, Provider: "onegw", API: "openai-completions", Model: "free"},
			want: func(t *testing.T, ev Event) {
				t.Helper()
				if ev.Type != "start" || ev.Provider != "onegw" || ev.API != "openai-completions" || ev.Model != "free" {
					t.Fatalf("start = %+v", ev)
				}
			},
		},
		{
			name: "text delta carries incremental and snapshot",
			in:   ai.Event{Type: ai.EventTextDelta, Delta: "hel", Snapshot: "hello world"},
			want: func(t *testing.T, ev Event) {
				t.Helper()
				if ev.Delta != "hel" || ev.Snapshot != "hello world" {
					t.Fatalf("delta = %+v", ev)
				}
			},
		},
		{
			name: "thinking delta uses the same fields",
			in:   ai.Event{Type: ai.EventThinkingDelta, Delta: "mm"},
			want: func(t *testing.T, ev Event) {
				t.Helper()
				if ev.Type != "thinking_delta" || ev.Delta != "mm" {
					t.Fatalf("thinking = %+v", ev)
				}
			},
		},
		{
			name: "toolcall start names the tool",
			in:   ai.Event{Type: ai.EventToolcallStart, ToolCallID: "c1", ToolName: "bash", StreamIndex: 2},
			want: func(t *testing.T, ev Event) {
				t.Helper()
				if ev.ToolCallID != "c1" || ev.ToolName != "bash" || ev.StreamIndex != 2 {
					t.Fatalf("toolcall_start = %+v", ev)
				}
			},
		},
		{
			name: "toolcall end carries accumulated args",
			in:   ai.Event{Type: ai.EventToolcallEnd, ToolCallID: "c1", StreamIndex: 2, PartialJSON: `{"command":"ls"}`},
			want: func(t *testing.T, ev Event) {
				t.Helper()
				if ev.PartialJSON != `{"command":"ls"}` {
					t.Fatalf("toolcall_end = %+v", ev)
				}
			},
		},
		{
			name: "done carries the full assistant message",
			in: ai.Donef(ai.StopReasonStop, &ai.Usage{Input: 10, Output: 4, TotalTokens: 14}, &ai.Message{
				Role:    ai.RoleAssistant,
				Content: []ai.Block{ai.ToolCallBlock{ID: "c1", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)}},
			}),
			want: func(t *testing.T, ev Event) {
				t.Helper()
				if ev.StopReason != "stop" {
					t.Fatalf("stopReason = %q", ev.StopReason)
				}
				if ev.Message == nil || len(ev.Message.ToolCalls()) != 1 {
					t.Fatalf("done event lost the materialized message: %+v", ev.Message)
				}
			},
		},
		{
			name: "error renders the cause as text",
			in:   ai.Errorf(errors.New("stream stalled")),
			want: func(t *testing.T, ev Event) {
				t.Helper()
				if ev.Type != "error" || ev.MessageErr != "stream stalled" {
					t.Fatalf("error = %+v", ev)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := EventFromAI(tc.in)
			tc.want(t, ev)
			// A bare `error` field must never serialize as {} (the reason
			// the wire type keeps its own text field).
			raw, err := json.Marshal(ev)
			if err != nil {
				t.Fatal(err)
			}
			if ev.Type == "error" && !strings.Contains(string(raw), "stream stalled") {
				t.Fatalf("error text not on the wire: %s", raw)
			}
			if ev.Type == "done" && !strings.Contains(string(raw), `"message"`) {
				t.Fatalf("done message not on the wire: %s", raw)
			}
		})
	}
}
