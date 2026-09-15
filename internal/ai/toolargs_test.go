package ai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// A sloppy model (GLM-family in the field, 2026-09-14) emits tool arguments
// with a missing comma before an unquoted key; a response cut off at
// max_tokens leaves them truncated the same way. Both used to be a fatal
// stream error that ai.Classify could not name, so the retry ladder never ran
// and the turn died with the call unpaired. The contract now: the stream
// always reaches done, the call keeps its identity, and arguments that cannot
// be parsed strictly become {} — the tool's own "argument is required" error
// is what tells the model to re-issue. No prefix is salvaged on purpose:
// guessing {"path":"/x"} from `{"path":"/x" content:"..."}` would let a tool
// run on arguments the model never finished writing.
func TestOpenAICompletionsMalformedToolArgs(t *testing.T) {
	cases := []struct {
		name string
		args string // "%" splits the two streamed deltas
	}{
		{"missing comma before unquoted key", `{"path":"/tmp/x"` + "%" + ` content:"hello"}`},
		{"truncated by max_tokens", `{"path":"/tmp/x",` + "%" + `"content":"half a fi`},
		{"not json at all", `I will read` + "%" + ` that file now`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			part := strings.SplitN(tc.args, "%", 2)
			if len(part) != 2 {
				t.Fatalf("case args must hold one %%: %q", tc.args)
			}
			frames := [][2]string{
				{"", `{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read","arguments":` + jsonString(t, part[0]) + `}}]}}]}`},
				{"", `{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":` + jsonString(t, part[1]) + `}}]}}]}`},
				{"", `{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`},
				{"", `[DONE]`},
			}
			srv, _ := newStreamServer(t, frames...)
			p := NewOpenAICompletionsProvider("router", srv.URL, "", nil, nil)
			ch, err := p.Stream(context.Background(), StreamRequest{
				Model:    "free",
				Messages: []Message{{Role: RoleUser, Content: []Block{TextBlock{Text: "hi"}}}},
			})
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			evs := collectEvents(t, ch)
			for _, ev := range evs {
				if ev.Type == EventError {
					t.Fatalf("stream errored instead of degrading: %v", ev.Err)
				}
			}
			done := evs[len(evs)-1]
			if done.Type != EventDone || done.Message == nil {
				t.Fatalf("last event = %v, want done", done.Type)
			}
			tcs := done.Message.ToolCalls()
			if len(tcs) != 1 {
				t.Fatalf("tool calls = %d, want 1", len(tcs))
			}
			if tcs[0].ID != "call_1" || tcs[0].Name != "read" {
				t.Fatalf("call identity lost: %#v", tcs[0])
			}
			if string(tcs[0].Arguments) != "{}" {
				t.Fatalf("arguments = %s, want {} (never a guessed prefix)", tcs[0].Arguments)
			}
			// The raw blob still rides along for diagnostics; only the
			// parsed form is neutered.
			if !strings.Contains(tcs[0].PartialArgs, part[0]) {
				t.Fatalf("partial args lost the model's bytes: %q", tcs[0].PartialArgs)
			}
		})
	}
}

// jsonString encodes s as a JSON string literal for embedding in a frame.
func jsonString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal %q: %v", s, err)
	}
	return string(b)
}

// A stored call whose arguments no longer parse (an imported or hand-edited
// log, a dialect decoder that gave up) is read as {} by every consumer — the
// guard lives in Message.ToolCalls, which the encoders, ACP and tool
// execution all go through. An empty blob keeps its PartialArgs fallback:
// those are calls whose arguments never closed, and the bytes are the point.
func TestMessageToolCallsNeutersInvalidStoredArgs(t *testing.T) {
	msg := Message{Role: RoleAssistant, API: APIOpenAICompletions, Content: []Block{
		ToolCallBlock{ID: "a", Name: "bash", Arguments: json.RawMessage(`{"command":"ls" cwd:"/tmp"}`)},
		ToolCallBlock{ID: "b", Name: "read", Arguments: json.RawMessage(`{"path":"/tmp/x"}`)},
		ToolCallBlock{ID: "c", Name: "bash", PartialArgs: `{"command":"ls`},
	}}
	calls := msg.ToolCalls()
	if len(calls) != 3 {
		t.Fatalf("calls = %d, want 3", len(calls))
	}
	if got := string(calls[0].Arguments); got != "{}" {
		t.Fatalf("invalid args survived as %q, want {}", got)
	}
	if got := string(calls[1].Arguments); got != `{"path":"/tmp/x"}` {
		t.Fatalf("valid args were rewritten: %s", got)
	}
	if len(calls[2].Arguments) != 0 || calls[2].PartialArgs == "" {
		t.Fatalf("empty arguments must keep the PartialArgs fallback: %#v", calls[2])
	}
}
