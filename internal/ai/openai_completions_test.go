package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// completionsFrames is a happy-path stream: text then a tool call, usage on
// the final chunk with empty choices.
var completionsFrames = [][2]string{
	{"", `{"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":"He"}}]}`},
	{"", `{"id":"c1","choices":[{"index":0,"delta":{"content":"llo"}}]}`},
	{"", `{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read","arguments":"{\"path\":"}}]}}]}`},
	{"", `{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"/tmp/x\"}"}}]}}]}`},
	{"", `{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`},
	{"", `{"id":"c1","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30},"completion_tokens_details":{"reasoning_tokens":7}}}`},
	{"", `[DONE]`},
}

func TestOpenAICompletionsHappyPath(t *testing.T) {
	srv, body := newStreamServer(t, completionsFrames...)
	p := NewOpenAICompletionsProvider("router", srv.URL, "sk-test", nil, nil)
	ch, err := p.Stream(context.Background(), StreamRequest{
		System:   "be brief",
		Model:    "free",
		Messages: []Message{{Role: RoleUser, Content: []Block{TextBlock{Text: "hi"}}}},
		Tools:    []ToolDef{{Name: "read", Description: "read a file", Parameters: []byte(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	evs := collectEvents(t, ch)
	assertOrder(t, evs,
		EventStart, EventTextStart, EventTextDelta, EventTextDelta, EventTextEnd,
		EventToolcallStart, EventToolcallDelta, EventToolcallDelta, EventToolcallEnd,
		EventDone,
	)
	if evs[0].Provider != "router" || evs[0].API != APIOpenAICompletions || evs[0].Model != "free" {
		t.Fatalf("start event = %+v", evs[0])
	}
	if d := evs[3].Delta; d != "llo" {
		t.Fatalf("text delta = %q", d)
	}
	if s := evs[3].Snapshot; s != "Hello" {
		t.Fatalf("text snapshot = %q", s)
	}

	done := evs[len(evs)-1]
	if done.Type != EventDone || done.StopReason != StopReasonStop {
		t.Fatalf("done = %+v", done)
	}
	// Usage numbers: input excludes cacheRead, total includes it.
	if done.Usage == nil || done.Usage.Input != 70 || done.Usage.Output != 20 ||
		done.Usage.CacheRead != 30 || done.Usage.TotalTokens != 120 ||
		done.Usage.ReasoningTokens != 7 {
		t.Fatalf("usage = %+v", done.Usage)
	}
	msg := done.Message
	if msg == nil || msg.Role != RoleAssistant {
		t.Fatalf("message = %+v", msg)
	}
	if msg.Provider != "router" || msg.API != APIOpenAICompletions || msg.Model != "free" {
		t.Fatalf("message meta = %v/%v/%v", msg.Provider, msg.API, msg.Model)
	}
	if msg.ResponseID != "c1" || msg.StopReason != StopReasonStop {
		t.Fatalf("message id/stop = %v/%v", msg.ResponseID, msg.StopReason)
	}
	if len(msg.Content) != 2 {
		t.Fatalf("content blocks = %d", len(msg.Content))
	}
	if txt, ok := msg.Content[0].(TextBlock); !ok || txt.Text != "Hello" {
		t.Fatalf("block 0 = %#v", msg.Content[0])
	}
	tc, ok := msg.Content[1].(ToolCallBlock)
	if !ok || tc.ID != "call_1" || tc.Name != "read" || tc.StreamIndex != 0 {
		t.Fatalf("block 1 = %#v", msg.Content[1])
	}
	if string(tc.Arguments) != `{"path":"/tmp/x"}` {
		t.Fatalf("arguments = %s", tc.Arguments)
	}
	if tc.PartialArgs != `{"path":"/tmp/x"}` {
		t.Fatalf("partial args = %s", tc.PartialArgs)
	}
	if msg.TTFTMS < 0 || msg.DurationMS < 0 {
		t.Fatalf("timings = %v/%v", msg.TTFTMS, msg.DurationMS)
	}

	// Request body: system message, string contents, tool shape, options.
	req := decodeJSON(t, body.get(t))
	if jstr(t, req, "model") != "free" || jpath(req, "stream") != true {
		t.Fatalf("model/stream = %v", req)
	}
	if jpath(req, "stream_options", "include_usage") != true {
		t.Fatalf("stream_options = %v", jpath(req, "stream_options"))
	}
	if jstr(t, req, "messages", 0, "role") != "system" ||
		jstr(t, req, "messages", 0, "content") != "be brief" {
		t.Fatalf("system message = %v", jpath(req, "messages", 0))
	}
	if jstr(t, req, "messages", 1, "role") != "user" ||
		jstr(t, req, "messages", 1, "content") != "hi" {
		t.Fatalf("user message = %v", jpath(req, "messages", 1))
	}
	if jstr(t, req, "tools", 0, "type") != "function" ||
		jstr(t, req, "tools", 0, "function", "name") != "read" ||
		jstr(t, req, "tools", 0, "function", "parameters", "type") != "object" {
		t.Fatalf("tools = %v", req["tools"])
	}
}

// TestOpenAICompletionsRetypesAskRootUnion pins that the chat/completions
// wire runs the OpenAI sanitizer: the ask tool's root anyOf used to leave
// the gateway as untyped branches and xAI 400'd
// "ask: tool parameter root must be an object type". Retrying the same
// request cannot succeed — the schema has to be rewritten before POST.
func TestOpenAICompletionsRetypesAskRootUnion(t *testing.T) {
	srv, body := newStreamServer(t, completionsFrames...)
	p := NewOpenAICompletionsProvider("router", srv.URL, "sk-test", nil, nil)
	ask := json.RawMessage(`{
		"type":"object",
		"properties":{
			"question":{"type":"string"},
			"options":{"type":"array"},
			"questions":{"type":"array"}
		},
		"anyOf":[
			{"required":["question","options"]},
			{"required":["questions"]}
		]
	}`)
	_, err := p.Stream(context.Background(), StreamRequest{
		Model:    "free",
		Messages: []Message{{Role: RoleUser, Content: []Block{TextBlock{Text: "hi"}}}},
		Tools:    []ToolDef{{Name: "ask", Description: "ask", Parameters: ask}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	req := decodeJSON(t, body.get(t))
	params, _ := jpath(req, "tools", 0, "function", "parameters").(map[string]any)
	branches, _ := params["anyOf"].([]any)
	if len(branches) != 2 {
		t.Fatalf("anyOf = %v, want both required-alternatives kept", params["anyOf"])
	}
	for i, b := range branches {
		obj, _ := b.(map[string]any)
		if obj["type"] != "object" {
			t.Fatalf("branch %d = %v, want type object so the 400 cannot fire", i, b)
		}
	}
}

func TestOpenAICompletionsThinking(t *testing.T) {
	frames := [][2]string{
		{"", `{"id":"c2","choices":[{"index":0,"delta":{"reasoning_content":"let me"}}]}`},
		{"", `{"id":"c2","choices":[{"index":0,"delta":{"reasoning":" think"}}]}`},
		{"", `{"id":"c2","choices":[{"index":0,"delta":{"content":"answer"}}]}`},
		{"", `{"id":"c2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`},
		{"", `[DONE]`},
	}
	srv, body := newStreamServer(t, frames...)
	p := NewOpenAICompletionsProvider("router", srv.URL, "", nil, nil)
	ch, err := p.Stream(context.Background(), StreamRequest{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: []Block{TextBlock{Text: "q"}}}},
		Thinking: &ThinkingBudget{Tokens: 4096},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	evs := collectEvents(t, ch)
	assertOrder(t, evs,
		EventStart, EventThinkingStart, EventThinkingDelta, EventThinkingDelta,
		EventThinkingEnd, EventTextStart, EventTextDelta, EventTextEnd, EventDone,
	)
	done := evs[len(evs)-1]
	th, ok := done.Message.Content[0].(ThinkingBlock)
	if !ok || th.Thinking != "let me think" || th.ThinkingSignature != "reasoning_content" {
		t.Fatalf("thinking block = %#v", done.Message.Content[0])
	}
	if jstr(t, decodeJSON(t, body.get(t)), "reasoning_effort") != "medium" {
		t.Fatalf("reasoning_effort mismatch")
	}
}

// TestOpenAICompletionsReasoningOnlyStops pins the wire shape behind #331:
// a thinking-mode upstream can finish a turn having streamed reasoning and
// nothing else. The adapter must report that honestly — done/stop with a lone
// thinking block — rather than inventing empty text, because the agent loop
// (not the adapter) is what decides a blank turn needs another round.
func TestOpenAICompletionsReasoningOnlyStops(t *testing.T) {
	frames := [][2]string{
		{"", `{"id":"c3","choices":[{"index":0,"delta":{"reasoning_content":"(context elided)"}}]}`},
		{"", `{"id":"c3","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`},
		{"", `[DONE]`},
	}
	srv, _ := newStreamServer(t, frames...)
	p := NewOpenAICompletionsProvider("router", srv.URL, "", nil, nil)
	ch, err := p.Stream(context.Background(), StreamRequest{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: []Block{TextBlock{Text: "q"}}}},
		Thinking: &ThinkingBudget{Tokens: 4096},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	evs := collectEvents(t, ch)
	done := evs[len(evs)-1]
	if done.Type != EventDone || done.StopReason != StopReasonStop {
		t.Fatalf("terminal event = %+v, want done/stop", done)
	}
	if got := done.Message.Text(); got != "" {
		t.Fatalf("reasoning-only turn produced text %q", got)
	}
	if len(done.Message.Content) != 1 {
		t.Fatalf("content = %#v, want the thinking block alone", done.Message.Content)
	}
	if th, ok := done.Message.Content[0].(ThinkingBlock); !ok || th.Thinking != "(context elided)" {
		t.Fatalf("content[0] = %#v, want the reasoning as a thinking block", done.Message.Content[0])
	}
}

func TestOpenAICompletionsToolResultRoundTrip(t *testing.T) {
	srv, body := newStreamServer(t, completionsFrames...)
	p := NewOpenAICompletionsProvider("router", srv.URL, "", nil, nil)
	_, err := p.Stream(context.Background(), StreamRequest{
		Model: "m",
		Messages: []Message{
			{Role: RoleAssistant, Content: []Block{
				TextBlock{Text: "on it"},
				ToolCallBlock{ID: "call_1", Name: "read", Arguments: json.RawMessage(`{"path":"/tmp/x"}`)},
			}},
			{Role: RoleToolResult, ToolCallID: "call_1", ToolName: "read", Content: []Block{TextBlock{Text: "1:x"}}},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	req := decodeJSON(t, body.get(t))
	// Assistant: content null, tool_calls with raw arguments string.
	if jpath(req, "messages", 0, "content") != nil {
		t.Fatalf("assistant content = %v, want null", jpath(req, "messages", 0, "content"))
	}
	if jstr(t, req, "messages", 0, "tool_calls", 0, "id") != "call_1" ||
		jstr(t, req, "messages", 0, "tool_calls", 0, "type") != "function" ||
		jstr(t, req, "messages", 0, "tool_calls", 0, "function", "name") != "read" ||
		jstr(t, req, "messages", 0, "tool_calls", 0, "function", "arguments") != `{"path":"/tmp/x"}` {
		t.Fatalf("tool_calls = %v", jpath(req, "messages", 0, "tool_calls"))
	}
	// Tool result: role tool with tool_call_id.
	if jstr(t, req, "messages", 1, "role") != "tool" ||
		jstr(t, req, "messages", 1, "tool_call_id") != "call_1" ||
		jstr(t, req, "messages", 1, "content") != "1:x" {
		t.Fatalf("tool message = %v", jpath(req, "messages", 1))
	}
}

func TestOpenAICompletionsNoFinishReason(t *testing.T) {
	srv, _ := newStreamServer(t,
		[2]string{"", `{"id":"c3","choices":[{"index":0,"delta":{"content":"partial"}}]}`},
		[2]string{"", `[DONE]`},
	)
	p := NewOpenAICompletionsProvider("router", srv.URL, "", nil, nil)
	ch, err := p.Stream(context.Background(), StreamRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	evs := collectEvents(t, ch)
	if evs[len(evs)-1].Type != EventError {
		t.Fatalf("last event = %v, want error", evs[len(evs)-1].Type)
	}
}

func TestOpenAICompletionsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()
	p := NewOpenAICompletionsProvider("router", srv.URL, "", nil, nil)
	ch, err := p.Stream(context.Background(), StreamRequest{Model: "m"})
	if err == nil || ch != nil {
		t.Fatalf("Stream err = %v, ch = %v; want error + nil channel", err, ch)
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "bad key") {
		t.Fatalf("error = %v, want status + snippet", err)
	}
}

// TestOpenAICompletionsTTFTIncludesGatewayQueue pins the #283 metric fix:
// the clock starts at fetch, so the queue + prefill wait before response
// headers is inside the recorded ttft. If start were stamped after the
// headers, this number would sit near zero while users wait seconds —
// the blind spot that hid the UI-stall episode (RCA §3).
func TestOpenAICompletionsTTFTIncludesGatewayQueue(t *testing.T) {
	const hold = 300 * time.Millisecond
	frames := [][2]string{
		{"", `{"id":"q","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`},
		{"", `{"id":"q","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`},
		{"", `{"id":"q","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":1}}`},
		{"", `[DONE]`},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(hold) // gateway queue + prefill before headers
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f[1])
			fl.Flush()
		}
	}))
	defer srv.Close()
	p := NewOpenAICompletionsProvider("router", srv.URL, "sk-test", nil, nil)
	ch, err := p.Stream(context.Background(), StreamRequest{
		Model:    "free",
		Messages: []Message{{Role: RoleUser, Content: []Block{TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	evs := collectEvents(t, ch)
	done := evs[len(evs)-1]
	if done.Message == nil {
		t.Fatalf("no message on done: %+v", done)
	}
	if done.Message.TTFTMS < hold.Milliseconds() {
		t.Fatalf("ttft %dms omits the %dms queue — start stamped after headers again", done.Message.TTFTMS, hold.Milliseconds())
	}
	if done.Message.DurationMS < done.Message.TTFTMS {
		t.Fatalf("duration %dms < ttft %dms", done.Message.DurationMS, done.Message.TTFTMS)
	}
}
