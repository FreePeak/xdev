package ai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// inbandTextFrames streams a Hermes-dialect assistant turn: visible prose, then
// a <tool_call> block in the text.
var inbandTextFrames = [][2]string{
	{"", `{"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":"Let me check.\n"}}]}`},
	{"", `{"id":"c1","choices":[{"index":0,"delta":{"content":"<tool_call>\n{\"name\": \"read\", \"arguments\": {\"path\":\"/tmp/x\"}}\n</tool_call>"}}]}`},
	{"", `{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`},
	{"", `[DONE]`},
}

// TestInBandProviderEncodesAndDecodes pins both halves of the wrapper against a
// real wire adapter: the request carries no native tools and replays history as
// dialect text, the response's text call becomes toolcall events, and the final
// message carries the decoded call.
func TestInBandProviderEncodesAndDecodes(t *testing.T) {
	srv, body := newStreamServer(t, inbandTextFrames...)
	inner := NewOpenAICompletionsProvider("local", srv.URL, "", nil, nil)
	p := NewInBandProvider(inner, ToolFormatHermes)
	ch, err := p.Stream(context.Background(), StreamRequest{
		System: "be brief",
		Model:  "deepseek-r1",
		Messages: []Message{
			{Role: RoleUser, Content: []Block{TextBlock{Text: "list files"}}},
			{Role: RoleAssistant, Content: []Block{
				TextBlock{Text: "checking"},
				ToolCallBlock{ID: "call_prev", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`), StreamIndex: 0},
			}},
			{Role: RoleToolResult, ToolCallID: "call_prev", ToolName: "bash", Content: []Block{TextBlock{Text: "a.go"}}},
		},
		Tools: []ToolDef{{Name: "read", Description: "read a file", Parameters: []byte(`{"type":"object","properties":{"path":{"type":"string"}}}`)}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	evs := collectEvents(t, ch)
	assertOrder(t, evs, EventStart, EventTextStart, EventTextDelta, EventTextEnd,
		EventToolcallStart, EventToolcallDelta, EventToolcallEnd, EventDone)

	req := decodeJSON(t, body.get(t))
	if _, has := req["tools"]; has {
		t.Fatalf("request tools = %v, want the native tools field dropped", req["tools"])
	}
	msgs, _ := req["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages = %v, want system + user + assistant + result", msgs)
	}
	system, _ := msgs[0].(map[string]any)
	systemText, _ := system["content"].(string)
	if !strings.Contains(systemText, "be brief") || !strings.Contains(systemText, "<tools>") {
		t.Fatalf("system = %q, want the prompt plus the inlined catalog", systemText)
	}
	if !strings.Contains(systemText, `"name":"read"`) || !strings.Contains(systemText, "Emit each call as") {
		t.Fatalf("system = %q, want the catalog entries and the dialect guide", systemText)
	}
	assistant, _ := msgs[2].(map[string]any)
	if _, has := assistant["tool_calls"]; has {
		t.Fatalf("assistant = %v, want no native tool_calls on replay", assistant)
	}
	assistantText, _ := assistant["content"].(string)
	if !strings.Contains(assistantText, "checking") || !strings.Contains(assistantText, "<tool_call>") {
		t.Fatalf("assistant = %q, want the replayed call as dialect text", assistantText)
	}
	result, _ := msgs[3].(map[string]any)
	if result["role"] != "user" {
		t.Fatalf("result message = %v, want it folded into a user message", result)
	}
	resultText, _ := result["content"].(string)
	if !strings.Contains(resultText, "a.go") || !strings.Contains(resultText, "<tool_response>") {
		t.Fatalf("result = %q, want the dialect result envelope", resultText)
	}

	text := evs[2].Delta
	if strings.Contains(text, "<tool_call>") {
		t.Fatalf("text delta = %q, want the call markup out of the visible text", text)
	}
	if evs[4].ToolName != "read" || evs[4].ToolCallID == "" {
		t.Fatalf("toolcall_start = %+v", evs[4])
	}
	if evs[5].PartialJSON != `{"path":"/tmp/x"}` {
		t.Fatalf("toolcall_delta = %+v", evs[5])
	}
	done := evs[len(evs)-1]
	if done.Message == nil || len(done.Message.Content) != 2 {
		t.Fatalf("done message = %+v", done.Message)
	}
	if tb, ok := done.Message.Content[0].(TextBlock); !ok || strings.Contains(tb.Text, "<tool_call>") {
		t.Fatalf("done text block = %#v, want the visible text only", done.Message.Content[0])
	}
	tc, ok := done.Message.Content[1].(ToolCallBlock)
	if !ok || tc.Name != "read" || string(tc.Arguments) != `{"path":"/tmp/x"}` {
		t.Fatalf("done call block = %#v, want the decoded call", done.Message.Content[1])
	}
}

// TestInBandProviderResultTextWithoutEnvelope pins the no-op path: a dialect
// miss (plain text) leaves the stream untouched.
func TestInBandProviderPlainText(t *testing.T) {
	srv, _ := newStreamServer(t, [][2]string{
		{"", `{"id":"c2","choices":[{"index":0,"delta":{"role":"assistant","content":"just prose"}}]}`},
		{"", `{"id":"c2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`},
		{"", `[DONE]`},
	}...)
	p := NewInBandProvider(NewOpenAICompletionsProvider("local", srv.URL, "", nil, nil), ToolFormatHermes)
	ch, err := p.Stream(context.Background(), StreamRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	evs := collectEvents(t, ch)
	assertOrder(t, evs, EventStart, EventTextStart, EventTextDelta, EventTextEnd, EventDone)
	if evs[2].Delta != "just prose" {
		t.Fatalf("delta = %q", evs[2].Delta)
	}
	msg := evs[len(evs)-1].Message
	if msg == nil || len(msg.Content) != 1 {
		t.Fatalf("message = %+v", msg)
	}
	if tb, ok := msg.Content[0].(TextBlock); !ok || tb.Text != "just prose" {
		t.Fatalf("block = %#v", msg.Content[0])
	}
}

// TestNewInBandProviderNativeIsPassthrough pins that a native dialect leaves the
// provider untouched (no wrapper, no request rewriting).
func TestNewInBandProviderNativeIsPassthrough(t *testing.T) {
	inner := NewOpenAICompletionsProvider("local", "http://127.0.0.1:1", "", nil, nil)
	if got := NewInBandProvider(inner, ToolFormatNative); got != Provider(inner) {
		t.Fatalf("native wrap = %T, want the unchanged provider", got)
	}
	if got := NewInBandProvider(nil, ToolFormatHermes); got != nil {
		t.Fatal("nil provider must stay nil")
	}
	if got := NewResolvedInBandProvider(inner, "gpt-5"); got != Provider(inner) {
		t.Fatalf("unknown model = %T, want the unchanged provider", got)
	}
	if got := NewResolvedInBandProvider(inner, "deepseek-v3"); got == Provider(inner) {
		t.Fatal("deepseek model must be wrapped for in-band tool calling")
	}
}
