package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// anthropicFrames is a happy-path stream: text then a tool call.
var anthropicFrames = [][2]string{
	{"message_start", `{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":10,"cache_read_input_tokens":5,"cache_creation_input_tokens":2}}}`},
	{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"He"}}`},
	{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"llo"}}`},
	{"content_block_stop", `{"type":"content_block_stop","index":0}`},
	{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"read"}}`},
	{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`},
	{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"/tmp/x\"}"}}`},
	{"content_block_stop", `{"type":"content_block_stop","index":1}`},
	{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}`},
	{"message_stop", `{"type":"message_stop"}`},
}

func TestAnthropicProviderHappyPath(t *testing.T) {
	// Base URL without /v1: the adapter must hit /v1/messages.
	srv, body := newStreamServer(t, anthropicFrames...)
	p := NewAnthropicProvider("claude", srv.URL, "sk-test", nil, nil)
	ch, err := p.Stream(context.Background(), StreamRequest{
		System:    "be brief",
		Model:     "claude-sonnet",
		MaxTokens: 1024,
		Messages:  []Message{{Role: RoleUser, Content: []Block{TextBlock{Text: "hi"}}}},
		Tools:     []ToolDef{{Name: "read", Description: "read a file", Parameters: []byte(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	evs := collectEvents(t, ch)

	// Event order: start, text block, toolcall block, done.
	assertOrder(t, evs,
		EventStart, EventTextStart, EventTextDelta, EventTextEnd,
		EventToolcallStart, EventToolcallDelta, EventToolcallDelta, EventToolcallEnd,
		EventDone,
	)
	if evs[0].Provider != "claude" || evs[0].API != APIAnthropicMessages || evs[0].Model != "claude-sonnet" {
		t.Fatalf("start event = %+v", evs[0])
	}
	if d := evs[2].Delta; d != "llo" {
		t.Fatalf("text delta = %q", d)
	}

	done := evs[len(evs)-1]
	if done.Type != EventDone || done.StopReason != StopReasonStop {
		t.Fatalf("done = %+v", done)
	}
	if done.Usage == nil || done.Usage.Input != 10 || done.Usage.Output != 42 ||
		done.Usage.CacheRead != 5 || done.Usage.CacheWrite != 2 || done.Usage.TotalTokens != 59 {
		t.Fatalf("usage = %+v", done.Usage)
	}
	msg := done.Message
	if msg == nil || msg.Role != RoleAssistant {
		t.Fatalf("message = %+v", msg)
	}
	if msg.Provider != "claude" || msg.API != APIAnthropicMessages || msg.Model != "claude-sonnet" {
		t.Fatalf("message meta = %v/%v/%v", msg.Provider, msg.API, msg.Model)
	}
	if msg.ResponseID != "msg_1" || msg.StopReason != StopReasonStop {
		t.Fatalf("message id/stop = %v/%v", msg.ResponseID, msg.StopReason)
	}
	if len(msg.Content) != 2 {
		t.Fatalf("content blocks = %d", len(msg.Content))
	}
	if txt, ok := msg.Content[0].(TextBlock); !ok || txt.Text != "Hello" {
		t.Fatalf("block 0 = %#v", msg.Content[0])
	}
	tc, ok := msg.Content[1].(ToolCallBlock)
	if !ok || tc.ID != "toolu_1" || tc.Name != "read" || tc.StreamIndex != 1 {
		t.Fatalf("block 1 = %#v", msg.Content[1])
	}
	if string(tc.Arguments) != `{"path":"/tmp/x"}` {
		t.Fatalf("arguments = %s", tc.Arguments)
	}
	if msg.Text() != "Hello" {
		t.Fatalf("text = %q", msg.Text())
	}
	if msg.TTFTMS < 0 || msg.DurationMS < 0 {
		t.Fatalf("timings = %v/%v", msg.TTFTMS, msg.DurationMS)
	}

	// Request body: model, max_tokens, system, user text, tool schema.
	req := decodeJSON(t, body.get(t))
	if jstr(t, req, "model") != "claude-sonnet" || jnum(t, req, "max_tokens") != 1024 {
		t.Fatalf("model/max_tokens = %v", req)
	}
	if jstr(t, req, "system") != "be brief" || jpath(req, "stream") != true {
	}
	if jstr(t, req, "messages", 0, "role") != "user" {
		t.Fatalf("messages[0].role = %v", req["messages"])
	}
	if jstr(t, req, "messages", 0, "content", 0, "type") != "text" ||
		jstr(t, req, "messages", 0, "content", 0, "text") != "hi" {
		t.Fatalf("messages[0].content = %v", jpath(req, "messages", 0, "content"))
	}
	if jstr(t, req, "tools", 0, "name") != "read" ||
		jstr(t, req, "tools", 0, "input_schema", "type") != "object" {
		t.Fatalf("tools = %v", req["tools"])
	}
}

func TestAnthropicProviderBaseURLWithV1(t *testing.T) {
	srv, _ := newStreamServer(t, anthropicFrames...)
	p := NewAnthropicProvider("claude", srv.URL+"/v1", "", nil, nil)
	_, err := p.Stream(context.Background(), StreamRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	// The handler accepted the request; a request to the wrong path would
	// have hit the default mux (404) and failed pre-flight.
}

func TestAnthropicProviderToolResultRoundTrip(t *testing.T) {
	srv, body := newStreamServer(t, anthropicFrames...)
	p := NewAnthropicProvider("claude", srv.URL, "", nil, nil)
	_, err := p.Stream(context.Background(), StreamRequest{
		Model: "m",
		Messages: []Message{
			{Role: RoleUser, Content: []Block{TextBlock{Text: "run it"}}},
			{Role: RoleAssistant, Content: []Block{
				TextBlock{Text: "on it"},
				ToolCallBlock{ID: "toolu_1", Name: "read", Arguments: json.RawMessage(`{"path":"/tmp/x"}`)},
			}},
			{Role: RoleToolResult, ToolCallID: "toolu_1", ToolName: "read", Content: []Block{TextBlock{Text: "1:x"}}},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	req := decodeJSON(t, body.get(t))
	msgs := jpath(req, "messages").([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3 (assistant must not merge into user)", len(msgs))
	}
	if jstr(t, req, "messages", 1, "role") != "assistant" {
		t.Fatalf("messages[1].role = %v", jpath(req, "messages", 1, "role"))
	}
	// tool_result rides in a user-role message.
	if jstr(t, req, "messages", 2, "role") != "user" {
		t.Fatalf("messages[2].role = %v, want user (tool_result)", jpath(req, "messages", 2, "role"))
	}
	if jstr(t, req, "messages", 2, "content", 0, "type") != "tool_result" ||
		jstr(t, req, "messages", 2, "content", 0, "tool_use_id") != "toolu_1" ||
		jstr(t, req, "messages", 2, "content", 0, "content", 0, "text") != "1:x" {
		t.Fatalf("tool_result block = %v", jpath(req, "messages", 2, "content", 0))
	}
	// Assistant tool_use keeps its parsed arguments object.
	if jstr(t, req, "messages", 1, "content", 1, "type") != "tool_use" ||
		jstr(t, req, "messages", 1, "content", 1, "input", "path") != "/tmp/x" {
		t.Fatalf("tool_use block = %v", jpath(req, "messages", 1, "content", 1))
	}
}

func TestAnthropicProviderToolResultsMerge(t *testing.T) {
	srv, body := newStreamServer(t, anthropicFrames...)
	p := NewAnthropicProvider("claude", srv.URL, "", nil, nil)
	_, err := p.Stream(context.Background(), StreamRequest{
		Model: "m",
		Messages: []Message{
			{Role: RoleToolResult, ToolCallID: "a", Content: []Block{TextBlock{Text: "one"}}},
			{Role: RoleToolResult, ToolCallID: "b", IsError: true, Content: []Block{TextBlock{Text: "two"}}},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	req := decodeJSON(t, body.get(t))
	msgs := jpath(req, "messages").([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1 (consecutive tool_results merge)", len(msgs))
	}
	if jstr(t, req, "messages", 0, "content", 0, "tool_use_id") != "a" ||
		jstr(t, req, "messages", 0, "content", 1, "tool_use_id") != "b" ||
		jpath(req, "messages", 0, "content", 1, "is_error") != true {
		t.Fatalf("merged content = %v", jpath(req, "messages", 0, "content"))
	}
}

func TestAnthropicProviderThinking(t *testing.T) {
	frames := [][2]string{
		{"message_start", `{"type":"message_start","message":{"id":"msg_t","usage":{"input_tokens":8}}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let me"}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig1"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text"}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":1}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`},
		{"message_stop", `{"type":"message_stop"}`},
	}
	srv, body := newStreamServer(t, frames...)
	p := NewAnthropicProvider("claude", srv.URL, "", nil, nil)
	ch, err := p.Stream(context.Background(), StreamRequest{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: []Block{TextBlock{Text: "q"}}}},
		Thinking: &ThinkingBudget{Tokens: 1024},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	evs := collectEvents(t, ch)
	assertOrder(t, evs,
		EventStart, EventThinkingStart, EventThinkingDelta, EventThinkingEnd,
		EventTextStart, EventTextDelta, EventTextEnd, EventDone,
	)
	done := evs[len(evs)-1]
	th, ok := done.Message.Content[0].(ThinkingBlock)
	if !ok || th.Thinking != "let me" || th.ThinkingSignature != "sig1" {
		t.Fatalf("thinking block = %#v", done.Message.Content[0])
	}

	req := decodeJSON(t, body.get(t))
	if jstr(t, req, "thinking", "type") != "enabled" || jnum(t, req, "thinking", "budget_tokens") != 1024 {
		t.Fatalf("thinking = %v", req["thinking"])
	}
	// max_tokens must exceed budget_tokens.
	if jnum(t, req, "max_tokens") <= 1024 {
		t.Fatalf("max_tokens = %v, want > budget", jpath(req, "max_tokens"))
	}
}

func TestAnthropicProviderStopReasons(t *testing.T) {
	cases := []struct {
		wire string
		want StopReason
	}{
		{"end_turn", StopReasonStop},
		{"stop_sequence", StopReasonStop},
		{"tool_use", StopReasonStop},
		{"max_tokens", StopReasonLength},
	}
	for _, tc := range cases {
		srv, _ := newStreamServer(t,
			[2]string{"message_start", `{"type":"message_start","message":{"id":"m","usage":{"input_tokens":1}}}`},
			[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"` + tc.wire + `"},"usage":{"output_tokens":1}}`},
			[2]string{"message_stop", `{"type":"message_stop"}`},
		)
		p := NewAnthropicProvider("claude", srv.URL, "", nil, nil)
		ch, err := p.Stream(context.Background(), StreamRequest{Model: "m"})
		if err != nil {
			t.Fatalf("%s: Stream: %v", tc.wire, err)
		}
		evs := collectEvents(t, ch)
		last := evs[len(evs)-1]
		if last.Type != EventDone || last.StopReason != tc.want {
			t.Fatalf("%s: done = %+v, want stop %s", tc.wire, last, tc.want)
		}
	}
}

func TestAnthropicProviderHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":{"message":"overloaded"}}`))
	}))
	defer srv.Close()
	p := NewAnthropicProvider("claude", srv.URL, "", nil, nil)
	ch, err := p.Stream(context.Background(), StreamRequest{Model: "m"})
	if err == nil {
		t.Fatal("Stream: want error on 500")
	}
	if ch != nil {
		t.Fatal("Stream: channel must be nil on pre-flight error")
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "overloaded") {
		t.Fatalf("error = %v, want status + snippet", err)
	}
}

func TestAnthropicProviderErrorEvent(t *testing.T) {
	srv, _ := newStreamServer(t,
		[2]string{"error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`},
	)
	p := NewAnthropicProvider("claude", srv.URL, "", nil, nil)
	ch, err := p.Stream(context.Background(), StreamRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	evs := collectEvents(t, ch)
	if len(evs) != 1 || evs[0].Type != EventError {
		t.Fatalf("events = %v, want single error", eventTypes(evs))
	}
	if !strings.Contains(evs[0].Err.Error(), "Overloaded") {
		t.Fatalf("err = %v", evs[0].Err)
	}
}

func TestAnthropicProviderContextCancel(t *testing.T) {
	// A handler that never finishes the stream: cancellation must terminate it.
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fl.Flush()
		<-block
	}))
	defer srv.Close()
	defer close(block)

	p := NewAnthropicProvider("claude", srv.URL, "", nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := p.Stream(ctx, StreamRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	cancel()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("channel closed without error event")
		}
		if ev.Type != EventError {
			t.Fatalf("event = %v, want error", ev.Type)
		}
		if !errors.Is(ev.Err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", ev.Err)
		}
		if _, ok := <-ch; ok {
			t.Fatal("channel must close after error event")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not terminate after cancel")
	}
}
