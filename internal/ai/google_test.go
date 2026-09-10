package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// googleChunks are streamGenerateContent SSE frames in Gemini's shape:
// text deltas, then a thought, then a function call, then final usage.
var googleChunks = []string{
	`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hel"}]}}]}`,
	`{"candidates":[{"content":{"role":"model","parts":[{"text":"lo"}]}}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2,"totalTokenCount":9}}`,
	`{"candidates":[{"content":{"role":"model","parts":[{"text":"counting","thought":true}]},"finishReason":"STOP"}]}`,
	`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"fc_1","name":"bash","args":{"command":"ls"}}}]}}]}`,
	`{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":20,"totalTokenCount":31,"thoughtsTokenCount":4}}`,
}

// googleCapture records what the provider sent so assertions run after the
// stream completes.
type googleCapture struct {
	body  string
	query string
}

func newGoogleServer(t *testing.T, chunks []string) (*httptest.Server, *googleCapture) {
	t.Helper()
	cap := &googleCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		cap.body = string(buf[:n])
		cap.query = r.URL.RawQuery
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("response writer cannot flush")
			return
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

func collectGoogle(t *testing.T, p *GoogleGenAIProvider, req StreamRequest) []Event {
	t.Helper()
	ch, err := p.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var out []Event
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

func TestGoogleGenAIStream(t *testing.T) {
	srv, cap := newGoogleServer(t, googleChunks)
	p := NewGoogleGenAIProvider("gemini", srv.URL, "AIza-test", nil, nil)
	evs := collectGoogle(t, p, StreamRequest{
		System:   "be brief",
		Model:    "gemini-2.5-pro",
		Messages: []Message{{Role: RoleUser, Content: []Block{TextBlock{Text: "hi"}}}},
		Tools:    []ToolDef{{Name: "bash", Description: "run", Parameters: json.RawMessage(`{"type":"object"}`)}},
	})

	var types []string
	for _, e := range evs {
		types = append(types, string(e.Type))
	}
	joined := strings.Join(types, " ")
	for _, want := range []string{"start", "text_delta", "thinking_delta", "toolcall_start", "toolcall_end", "done"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("event sequence missing %q: %s", want, joined)
		}
	}
	if types[len(types)-1] != "done" {
		t.Fatalf("stream must terminate with done: %s", joined)
	}
	last := evs[len(evs)-1]
	if last.Usage == nil || last.Usage.TotalTokens != 31 || last.Usage.ReasoningTokens != 4 {
		t.Fatalf("usage = %+v", last.Usage)
	}
	if last.Message == nil {
		t.Fatal("done must carry the materialized message")
	}
	calls := last.Message.ToolCalls()
	if len(calls) != 1 || calls[0].Name != "bash" || calls[0].ID != "fc_1" {
		t.Fatalf("tool calls = %+v", calls)
	}
	if !strings.Contains(last.Message.Text(), "Hello") {
		t.Fatalf("text = %q", last.Message.Text())
	}
	// Request shape: system instruction, tools declared, user role.
	if !strings.Contains(cap.body, "systemInstruction") || !strings.Contains(cap.body, `"role":"user"`) {
		t.Fatalf("request body wrong: %s", cap.body)
	}
	if !strings.Contains(cap.body, `"tools"`) {
		t.Fatalf("tools missing from body: %s", cap.body)
	}
	// The key rides the query string (Gemini's HTTP auth) — never the body.
	if strings.Contains(cap.body, "AIza-test") {
		t.Fatalf("api key leaked into the body: %s", cap.body)
	}
	if !strings.Contains(cap.query, "key=AIza-test") {
		t.Fatalf("api key missing from the query: %q", cap.query)
	}
}

func TestGoogleGenAIToolResultRoundTrip(t *testing.T) {
	srv, cap := newGoogleServer(t, googleChunks[:2])
	p := NewGoogleGenAIProvider("gemini", srv.URL, "k", nil, nil)
	collectGoogle(t, p, StreamRequest{
		Model: "gemini-2.5-pro",
		Messages: []Message{
			{Role: RoleUser, Content: []Block{TextBlock{Text: "run it"}}},
			{Role: RoleAssistant, Content: []Block{ToolCallBlock{ID: "fc_1", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)}}},
			{Role: RoleToolResult, ToolCallID: "fc_1", ToolName: "bash", Content: []Block{TextBlock{Text: "a.go"}}},
		},
	})
	var req googleRequest
	if err := json.Unmarshal([]byte(cap.body), &req); err != nil {
		t.Fatalf("our own request failed to parse: %v\n%s", err, cap.body)
	}
	// Gemini has no tool role: the result returns as a user turn carrying a
	// functionResponse named after the call it answers.
	var found bool
	for _, c := range req.Contents {
		for _, part := range c.Parts {
			if part.FunctionResult == nil {
				continue
			}
			found = true
			if part.FunctionResult.Name != "bash" {
				t.Fatalf("functionResponse name = %q", part.FunctionResult.Name)
			}
			if c.Role != "user" {
				t.Fatalf("functionResponse must ride a user turn, got %q", c.Role)
			}
			if out, _ := part.FunctionResult.Response["output"].(string); out != "a.go" {
				t.Fatalf("functionResponse output = %v", part.FunctionResult.Response)
			}
		}
	}
	if !found {
		t.Fatalf("no functionResponse part in the request: %s", cap.body)
	}
}

func TestGoogleGenAIFinishReasons(t *testing.T) {
	cases := []struct {
		reason string
		want   StopReason
	}{
		{"MAX_TOKENS", StopReasonLength},
		{"SAFETY", StopReasonAborted},
		{"STOP", StopReasonStop},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			srv, _ := newGoogleServer(t, []string{
				fmt.Sprintf(`{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},"finishReason":"%s"}]}`, tc.reason),
			})
			p := NewGoogleGenAIProvider("gemini", srv.URL, "k", nil, nil)
			evs := collectGoogle(t, p, StreamRequest{
				Model: "m", Messages: []Message{{Role: RoleUser, Content: []Block{TextBlock{Text: "x"}}}},
			})
			if got := evs[len(evs)-1].StopReason; got != tc.want {
				t.Fatalf("%s mapped to %q, want %q", tc.reason, got, tc.want)
			}
		})
	}
}

func TestGoogleGenAIErrorBodyClassifies(t *testing.T) {
	srv, _ := newGoogleServer(t, []string{`{"error":{"message":"API key not valid","status":"INVALID_ARGUMENT"}}`})
	p := NewGoogleGenAIProvider("gemini", srv.URL, "k", nil, nil)
	evs := collectGoogle(t, p, StreamRequest{
		Model: "m", Messages: []Message{{Role: RoleUser, Content: []Block{TextBlock{Text: "x"}}}},
	})
	last := evs[len(evs)-1]
	if last.Type != EventError {
		t.Fatalf("expected an error event, got %q", last.Type)
	}
	if got := Classify(last.Err); got != ClassBadRequest {
		t.Fatalf("provider error must classify as bad-request, got %v (%v)", got, last.Err)
	}
}

func TestGoogleGenAIRequiresModel(t *testing.T) {
	p := NewGoogleGenAIProvider("gemini", "https://example.invalid", "k", nil, nil)
	if _, err := p.Stream(context.Background(), StreamRequest{
		Messages: []Message{{Role: RoleUser, Content: []Block{TextBlock{Text: "x"}}}},
	}); err == nil || !strings.Contains(err.Error(), "model is required") {
		t.Fatalf("missing model must be a clear error, got %v", err)
	}
}
