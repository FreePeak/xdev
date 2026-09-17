package ai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// A tool that produced no text still answers the call, so the wire MUST carry
// a function_call_output item. `output` was omitempty, so an empty result
// serialized to {"type":"function_call_output","call_id":"call_1"} — and the
// upstream rejects the whole turn:
//
//	HTTP 400 {"error":{"type":"invalid_request_error", ...}}
//	`input[185]` missing required field `output`
//
// Live through onegw (Console Go) on 2026-09-17: a 400 the retry ladder does
// not classify as retryable, so the session died with no way back.
func TestOpenAIResponsesEmptyToolResultCarriesOutput(t *testing.T) {
	srv, body := newStreamServer(t, responsesFrames...)
	p := NewOpenAIResponsesProvider("codex", srv.URL, "", nil, nil)
	_, err := p.Stream(context.Background(), StreamRequest{
		Model: "m",
		Messages: []Message{
			{Role: RoleAssistant, Content: []Block{
				ToolCallBlock{ID: "call_1", Name: "glob", Arguments: json.RawMessage(`{"pattern":"*.nope"}`)},
			}},
			// An empty result: the tool ran, wrote nothing to the transcript.
			{Role: RoleToolResult, ToolCallID: "call_1", ToolName: "glob", Content: []Block{TextBlock{Text: ""}}},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	req := decodeJSON(t, body.get(t))
	item, ok := jpath(req, "input", 1).(map[string]any)
	if !ok {
		t.Fatalf("input[1] = %v, want an object", jpath(req, "input", 1))
	}
	if jstr(t, req, "input", 1, "type") != "function_call_output" {
		t.Fatalf("input[1].type = %v", item["type"])
	}
	if _, present := item["output"]; !present {
		t.Fatalf("input[1] dropped the required `output` field: %v", item)
	}
}

// TestEnsureToolOutput pins the shared guard: a toolResult the model is shown
// always has text (so the Answers wire always has `output`), a real result is
// left exactly as it was, and details/error flags survive.
func TestEnsureToolOutput(t *testing.T) {
	for name, in := range map[string]Message{
		"no blocks":    {Role: RoleToolResult, ToolCallID: "c"},
		"empty text":   {Role: RoleToolResult, ToolCallID: "c", Content: []Block{TextBlock{Text: ""}}},
		"blank text":   {Role: RoleToolResult, ToolCallID: "c", Content: []Block{TextBlock{Text: "  \n"}}},
		"details only": {Role: RoleToolResult, ToolCallID: "c", Details: map[string]any{"exitCode": 0}},
	} {
		got := in.EnsureToolOutput()
		if strings.TrimSpace(got.Text()) == "" {
			t.Errorf("%s: still empty: %#v", name, got)
		}
		if name == "details only" && got.Details == nil {
			t.Errorf("%s: details dropped", name)
		}
	}

	real := Message{Role: RoleToolResult, ToolCallID: "c", IsError: true,
		Content: []Block{TextBlock{Text: "1:x"}}}
	if got := real.EnsureToolOutput(); got.Text() != "1:x" || !got.IsError {
		t.Fatalf("a real result was rewritten: %#v", got)
	}
}
