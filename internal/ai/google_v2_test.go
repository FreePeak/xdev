package ai

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestGoogleVertexEndpoint pins the Vertex project/location URL, Bearer auth
// and the Gemini schema normalization of the tool parameters.
func TestGoogleVertexEndpoint(t *testing.T) {
	srv, cap := newGoogleServer(t, googleChunks)
	rec := &recordingTransport{}
	p := NewGoogleVertexProvider("vertex", srv.URL, "access-token", nil, GoogleVertexOptions{
		Project:  "my-project",
		Location: "europe-west4",
	}, &http.Client{Transport: rec})
	if p.API() != APIGoogleVertex {
		t.Fatalf("API() = %q", p.API())
	}
	ch, err := p.Stream(context.Background(), StreamRequest{
		System:   "be brief",
		Model:    "gemini-2.5-pro",
		Messages: []Message{{Role: RoleUser, Content: []Block{TextBlock{Text: "hi"}}}},
		Tools: []ToolDef{{
			Name: "read",
			Parameters: []byte(`{"type":"object","additionalProperties":false,"properties":{
				"path":{"type":"string","description":"a file","pattern":"^(?=.*x)foo$","default":"/tmp/x"},
				"mode":{"type":["string","null"]},
				"tags":{"type":"array"}},
				"required":["path"]}`),
		}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	evs := collectEvents(t, ch)
	if evs[0].API != APIGoogleVertex {
		t.Fatalf("start API = %q, want the Vertex label", evs[0].API)
	}
	if !strings.Contains(rec.path, "/projects/my-project/locations/europe-west4/publishers/google/models/gemini-2.5-pro:streamGenerateContent") {
		t.Fatalf("path = %q, want the project/location route", rec.path)
	}
	if rec.query != "alt=sse" {
		t.Fatalf("query = %q, want alt=sse and no key parameter", rec.query)
	}
	if rec.auth != "Bearer access-token" {
		t.Fatalf("Authorization = %q, want the Bearer token", rec.auth)
	}
	body := decodeJSON(t, []byte(cap.body))
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", body["tools"])
	}
	decl, _ := tools[0].(map[string]any)
	params, _ := decl["parameters"].(map[string]any)
	if _, has := params["additionalProperties"]; has {
		t.Fatalf("parameters = %v, want additionalProperties dropped for Gemini", params)
	}
	props, _ := params["properties"].(map[string]any)
	path, _ := props["path"].(map[string]any)
	if pat, _ := path["pattern"].(string); pat != "^foo$" {
		t.Fatalf("pattern = %q, want the lookaround group removed", path["pattern"])
	}
	// Gemini's Schema carries `default` itself, so it is kept verbatim.
	if path["default"] != "/tmp/x" || path["description"] != "a file" {
		t.Fatalf("path = %v, want the default and description preserved", path)
	}
	mode, _ := props["mode"].(map[string]any)
	if mode["nullable"] != true || mode["type"] != "string" {
		t.Fatalf("mode = %v, want type string + nullable", mode)
	}
	tags, _ := props["tags"].(map[string]any)
	if _, has := tags["items"]; !has {
		t.Fatalf("tags = %v, want items required for arrays", tags)
	}
}

// TestGoogleVertexRequiresProject pins the loud failure on a missing project.
func TestGoogleVertexRequiresProject(t *testing.T) {
	p := NewGoogleVertexProvider("vertex", "http://127.0.0.1:1", "", nil, GoogleVertexOptions{}, nil)
	if _, err := p.Stream(context.Background(), StreamRequest{Model: "m"}); err == nil ||
		!strings.Contains(err.Error(), "project is required") {
		t.Fatalf("err = %v, want a clear project error", err)
	}
}

// TestGeminiCLIWire pins the Code Assist envelope: the body nests the
// GenerateContent request under `request`, the model rides the body, the URL is
// the v1internal method, and the SSE frames unwrap from `response`.
func TestGeminiCLIWire(t *testing.T) {
	chunks := []string{
		`{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"Hel"}]}}]}}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"lo"}]}}]}`,
		`{"response":{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2,"totalTokenCount":9}}}`,
	}
	srv, cap := newGoogleServer(t, chunks)
	rec := &recordingTransport{}
	p := NewGeminiCLIProvider("gca", srv.URL, "oauth-token", nil, GeminiCLIOptions{Project: "cloud-project"}, &http.Client{Transport: rec})
	if p.API() != APIGeminiCLI {
		t.Fatalf("API() = %q", p.API())
	}
	ch, err := p.Stream(context.Background(), StreamRequest{
		System:   "be brief",
		Model:    "gemini-2.5-pro",
		Messages: []Message{{Role: RoleUser, Content: []Block{TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	evs := collectEvents(t, ch)
	assertOrder(t, evs, EventStart, EventTextStart, EventTextDelta, EventTextDelta, EventTextEnd, EventDone)
	if got := evs[3].Snapshot; got != "Hello" {
		t.Fatalf("snapshot = %q, want the wrapped and bare chunks merged", got)
	}
	if rec.path != "/v1internal:streamGenerateContent" {
		t.Fatalf("path = %q, want the Code Assist method", rec.path)
	}
	if rec.auth != "Bearer oauth-token" {
		t.Fatalf("Authorization = %q", rec.auth)
	}
	body := decodeJSON(t, []byte(cap.body))
	if body["model"] != "gemini-2.5-pro" || body["project"] != "cloud-project" {
		t.Fatalf("envelope = %v, want model + project", body)
	}
	inner, _ := body["request"].(map[string]any)
	contents, _ := inner["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("request.contents = %v, want the nested GenerateContent request", inner)
	}
	if si, _ := inner["systemInstruction"].(map[string]any); si == nil {
		t.Fatalf("request = %v, want the system instruction nested", inner)
	}
}

// TestGeminiCLIWrapsToolCalls pins that a functionCall in the envelope becomes
// a toolcall event triplet.
func TestGeminiCLIWrapsToolCalls(t *testing.T) {
	srv, cap := newGoogleServer(t, []string{
		`{"response":{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"fc_1","name":"bash","args":{"command":"ls"}}}]}}]}}`,
		`{"response":{"candidates":[{"finishReason":"STOP"}]}}`,
	})
	p := NewGeminiCLIProvider("gca", srv.URL, "", nil, GeminiCLIOptions{}, nil)
	ch, err := p.Stream(context.Background(), StreamRequest{
		Model: "gemini-2.5-pro",
		Tools: []ToolDef{{Name: "bash", Parameters: []byte(`{"type":"object","properties":{"command":{"type":"string"}}}`)}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	evs := collectEvents(t, ch)
	assertOrder(t, evs, EventStart, EventToolcallStart, EventToolcallDelta, EventToolcallEnd, EventDone)
	if evs[1].ToolName != "bash" || evs[1].ToolCallID != "fc_1" {
		t.Fatalf("toolcall_start = %+v", evs[1])
	}
	body := decodeJSON(t, []byte(cap.body))
	tools, _ := body["request"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("request.tools = %v, want the normalized declaration", body["request"])
	}
}
