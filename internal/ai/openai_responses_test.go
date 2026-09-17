package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// responsesFrames is a happy-path stream: text then a function call.
var responsesFrames = [][2]string{
	{"response.created", `{"type":"response.created","response":{"id":"resp_1"}}`},
	{"response.output_text.delta", `{"type":"response.output_text.delta","delta":"He","snapshot":"He"}`},
	{"response.output_text.delta", `{"type":"response.output_text.delta","delta":"llo","snapshot":"Hello"}`},
	{"response.output_item.added", `{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"read"}}`},
	{"response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"path\":"}`},
	{"response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","output_index":1,"delta":"\"/tmp/x\"}"}`},
	{"response.function_call_arguments.done", `{"type":"response.function_call_arguments.done","output_index":1,"arguments":"{\"path\":\"/tmp/x\"}"}`},
	{"response.completed", `{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":50,"output_tokens":10,"input_tokens_details":{"cached_tokens":20},"output_tokens_details":{"reasoning_tokens":3}}}}`},
}

func TestOpenAIResponsesHappyPath(t *testing.T) {
	srv, body := newStreamServer(t, responsesFrames...)
	p := NewOpenAIResponsesProvider("codex", srv.URL, "sk-test", nil, nil)
	ch, err := p.Stream(context.Background(), StreamRequest{
		System:   "be brief",
		Model:    "gpt-5",
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
	if evs[0].Provider != "codex" || evs[0].API != APIOpenAIResponses || evs[0].Model != "gpt-5" {
		t.Fatalf("start event = %+v", evs[0])
	}
	if d := evs[3].Delta; d != "llo" {
		t.Fatalf("text delta = %q", d)
	}
	if s := evs[3].Snapshot; s != "Hello" {
		t.Fatalf("text snapshot = %q, want server-provided snapshot", s)
	}
	tcStart := evs[5]
	if tcStart.ToolCallID != "call_1" || tcStart.ToolName != "read" || tcStart.StreamIndex != 1 {
		t.Fatalf("toolcall_start = %+v", tcStart)
	}

	done := evs[len(evs)-1]
	if done.Type != EventDone || done.StopReason != StopReasonStop {
		t.Fatalf("done = %+v", done)
	}
	if done.Usage == nil || done.Usage.Input != 30 || done.Usage.Output != 10 ||
		done.Usage.CacheRead != 20 || done.Usage.TotalTokens != 60 ||
		done.Usage.ReasoningTokens != 3 {
		t.Fatalf("usage = %+v", done.Usage)
	}
	msg := done.Message
	if msg == nil || msg.Role != RoleAssistant {
		t.Fatalf("message = %+v", msg)
	}
	if msg.Provider != "codex" || msg.API != APIOpenAIResponses || msg.Model != "gpt-5" {
		t.Fatalf("message meta = %v/%v/%v", msg.Provider, msg.API, msg.Model)
	}
	if msg.ResponseID != "resp_1" || msg.StopReason != StopReasonStop {
		t.Fatalf("message id/stop = %v/%v", msg.ResponseID, msg.StopReason)
	}
	if len(msg.Content) != 2 {
		t.Fatalf("content blocks = %d", len(msg.Content))
	}
	if txt, ok := msg.Content[0].(TextBlock); !ok || txt.Text != "Hello" {
		t.Fatalf("block 0 = %#v", msg.Content[0])
	}
	tc, ok := msg.Content[1].(ToolCallBlock)
	if !ok || tc.ID != "call_1" || tc.Name != "read" || tc.StreamIndex != 1 {
		t.Fatalf("block 1 = %#v", msg.Content[1])
	}
	if string(tc.Arguments) != `{"path":"/tmp/x"}` {
		t.Fatalf("arguments = %s", tc.Arguments)
	}
	if msg.TTFTMS < 0 || msg.DurationMS < 0 {
		t.Fatalf("timings = %v/%v", msg.TTFTMS, msg.DurationMS)
	}

	// Request body: instructions, flat function tool shape, user input_text.
	req := decodeJSON(t, body.get(t))
	if jstr(t, req, "model") != "gpt-5" || jpath(req, "stream") != true {
		t.Fatalf("model/stream = %v", req)
	}
	if jstr(t, req, "instructions") != "be brief" {
		t.Fatalf("instructions = %v", jpath(req, "instructions"))
	}
	if jstr(t, req, "input", 0, "type") != "message" ||
		jstr(t, req, "input", 0, "role") != "user" ||
		jstr(t, req, "input", 0, "content", 0, "type") != "input_text" ||
		jstr(t, req, "input", 0, "content", 0, "text") != "hi" {
		t.Fatalf("input[0] = %v", jpath(req, "input", 0))
	}
	if jstr(t, req, "tools", 0, "type") != "function" ||
		jstr(t, req, "tools", 0, "name") != "read" ||
		jstr(t, req, "tools", 0, "parameters", "type") != "object" {
		t.Fatalf("tools = %v", req["tools"])
	}
}

func TestOpenAIResponsesThinking(t *testing.T) {
	frames := [][2]string{
		{"response.created", `{"type":"response.created","response":{"id":"resp_t"}}`},
		{"response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","delta":"let me"}`},
		{"response.reasoning_text.delta", `{"type":"response.reasoning_text.delta","delta":" think"}`},
		{"response.output_text.delta", `{"type":"response.output_text.delta","delta":"answer"}`},
		{"response.completed", `{"type":"response.completed","response":{"id":"resp_t","status":"completed","usage":{"input_tokens":5,"output_tokens":5}}}`},
	}
	srv, body := newStreamServer(t, frames...)
	p := NewOpenAIResponsesProvider("codex", srv.URL, "", nil, nil)
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
		EventStart, EventThinkingStart, EventThinkingDelta, EventThinkingDelta,
		EventThinkingEnd, EventTextStart, EventTextDelta, EventTextEnd, EventDone,
	)
	done := evs[len(evs)-1]
	th, ok := done.Message.Content[0].(ThinkingBlock)
	if !ok || th.Thinking != "let me think" {
		t.Fatalf("thinking block = %#v", done.Message.Content[0])
	}
	if jstr(t, decodeJSON(t, body.get(t)), "reasoning", "effort") != "low" {
		t.Fatalf("reasoning effort mismatch")
	}
}

func TestOpenAIResponsesToolResultRoundTrip(t *testing.T) {
	srv, body := newStreamServer(t, responsesFrames...)
	p := NewOpenAIResponsesProvider("codex", srv.URL, "", nil, nil)
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
	items := jpath(req, "input").([]any)
	if len(items) != 3 {
		t.Fatalf("input items = %d, want 3", len(items))
	}
	// Assistant text message with output_text.
	if jstr(t, req, "input", 0, "type") != "message" ||
		jstr(t, req, "input", 0, "role") != "assistant" ||
		jstr(t, req, "input", 0, "content", 0, "type") != "output_text" ||
		jstr(t, req, "input", 0, "content", 0, "text") != "on it" {
		t.Fatalf("assistant message = %v", jpath(req, "input", 0))
	}
	// function_call with the arguments round-tripped.
	if jstr(t, req, "input", 1, "type") != "function_call" ||
		jstr(t, req, "input", 1, "call_id") != "call_1" ||
		jstr(t, req, "input", 1, "name") != "read" ||
		jstr(t, req, "input", 1, "arguments") != `{"path":"/tmp/x"}` {
		t.Fatalf("function_call = %v", jpath(req, "input", 1))
	}
	// function_call_output.
	if jstr(t, req, "input", 2, "type") != "function_call_output" ||
		jstr(t, req, "input", 2, "call_id") != "call_1" ||
		jstr(t, req, "input", 2, "output") != "1:x" {
		t.Fatalf("function_call_output = %v", jpath(req, "input", 2))
	}
}

func TestOpenAIResponsesIncomplete(t *testing.T) {
	srv, _ := newStreamServer(t,
		[2]string{"response.created", `{"type":"response.created","response":{"id":"resp_i"}}`},
		[2]string{"response.output_text.delta", `{"type":"response.output_text.delta","delta":"par"}`},
		[2]string{"response.completed", `{"type":"response.completed","response":{"id":"resp_i","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":5,"output_tokens":5}}}`},
	)
	p := NewOpenAIResponsesProvider("codex", srv.URL, "", nil, nil)
	ch, err := p.Stream(context.Background(), StreamRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	evs := collectEvents(t, ch)
	last := evs[len(evs)-1]
	if last.Type != EventDone || last.StopReason != StopReasonLength {
		t.Fatalf("done = %+v, want length", last)
	}
}

func TestOpenAIResponsesFailed(t *testing.T) {
	srv, _ := newStreamServer(t,
		[2]string{"response.created", `{"type":"response.created","response":{"id":"resp_f"}}`},
		[2]string{"response.failed", `{"type":"response.failed","response":{"id":"resp_f","status":"failed","error":{"message":"server exploded"}}}`},
	)
	p := NewOpenAIResponsesProvider("codex", srv.URL, "", nil, nil)
	ch, err := p.Stream(context.Background(), StreamRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	evs := collectEvents(t, ch)
	if len(evs) != 1 || evs[0].Type != EventError {
		t.Fatalf("events = %v, want single error", eventTypes(evs))
	}
	if !strings.Contains(evs[0].Err.Error(), "server exploded") {
		t.Fatalf("err = %v", evs[0].Err)
	}
}

func TestOpenAIResponsesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"bad input"}}`))
	}))
	defer srv.Close()
	p := NewOpenAIResponsesProvider("codex", srv.URL, "", nil, nil)
	ch, err := p.Stream(context.Background(), StreamRequest{Model: "m"})
	if err == nil || ch != nil {
		t.Fatalf("Stream err = %v, ch = %v; want error + nil channel", err, ch)
	}
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "bad input") {
		t.Fatalf("error = %v, want status + snippet", err)
	}
}
