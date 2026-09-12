package ai

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// recordingTransport records the request line of the last call and forwards to
// the default transport, so tests can assert the variant's URL shape.
type recordingTransport struct {
	path   string
	query  string
	apiKey string
	auth   string
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.path, r.query = req.URL.Path, req.URL.RawQuery
	r.apiKey = req.Header.Get("api-key")
	r.auth = req.Header.Get("Authorization")
	return http.DefaultTransport.RoundTrip(req)
}

func TestCodexResponsesMapping(t *testing.T) {
	srv, body := newStreamServer(t, responsesFrames...)
	p := NewOpenAICodexResponsesProvider("codex", srv.URL, "oauth-token", nil, nil)
	if p.API() != APIOpenAICodexResponses {
		t.Fatalf("API() = %q", p.API())
	}
	ch, err := p.Stream(context.Background(), StreamRequest{
		Model:    "gpt-5-codex",
		Messages: []Message{{Role: RoleUser, Content: []Block{TextBlock{Text: "hi"}}}},
		Tools: []ToolDef{{
			Name:        "read",
			Description: "read a file",
			Parameters:  []byte(`{"type":"object","properties":{"path":{"type":"string","default":"/tmp/x"}}}`),
		}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	evs := collectEvents(t, ch)
	if evs[0].API != APIOpenAICodexResponses {
		t.Fatalf("start API = %q, want the variant label", evs[0].API)
	}
	req := decodeJSON(t, body.get(t))
	if store, ok := req["store"].(bool); !ok || store {
		t.Fatalf("store = %v, want false", req["store"])
	}
	if instr, _ := req["instructions"].(string); instr != CodexDefaultInstructions {
		t.Fatalf("instructions = %q, want the Codex default", instr)
	}
	tools, _ := req["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", req["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if strict, _ := tool["strict"].(bool); !strict {
		t.Fatalf("tool = %v, want strict:true after enforcement", tool)
	}
	params, _ := tool["parameters"].(map[string]any)
	if _, has := params["additionalProperties"]; !has {
		t.Fatalf("parameters = %v, want additionalProperties from strict enforcement", params)
	}
	required, _ := params["required"].([]any)
	if len(required) != 1 || required[0] != "path" {
		t.Fatalf("parameters.required = %v, want every property required", params["required"])
	}
}

// TestCodexResponsesSystemPromptWins pins that a caller system prompt is not
// replaced by the Codex default.
func TestCodexResponsesSystemPromptWins(t *testing.T) {
	srv, body := newStreamServer(t, responsesFrames...)
	p := NewOpenAICodexResponsesProvider("codex", srv.URL, "", nil, nil)
	ch, err := p.Stream(context.Background(), StreamRequest{System: "custom system", Model: "gpt-5"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	collectEvents(t, ch)
	if instr, _ := decodeJSON(t, body.get(t))["instructions"].(string); instr != "custom system" {
		t.Fatalf("instructions = %q", instr)
	}
}

// TestAzureResponsesEndpoint pins the Azure deployment URL, the api-version
// query and the api-key header, plus the strict:false rule for that surface.
func TestAzureResponsesEndpoint(t *testing.T) {
	srv, body := newStreamServer(t, responsesFrames...)
	rec := &recordingTransport{}
	p := NewAzureResponsesProvider("azure", srv.URL, "az-secret", nil, AzureResponsesOptions{
		Deployment: "my-deployment",
		APIVersion: "2025-04-01-preview",
	}, &http.Client{Transport: rec})
	ch, err := p.Stream(context.Background(), StreamRequest{
		Model: "gpt-5",
		Tools: []ToolDef{{Name: "read", Parameters: []byte(`{"type":"object","properties":{"path":{"type":"string"}}}`)}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	evs := collectEvents(t, ch)
	if evs[0].API != APIAzureOpenAIResponses {
		t.Fatalf("start API = %q, want the Azure label", evs[0].API)
	}
	if !strings.Contains(rec.path, "/openai/deployments/my-deployment/responses") {
		t.Fatalf("path = %q, want the deployment route", rec.path)
	}
	if rec.query != "api-version=2025-04-01-preview" {
		t.Fatalf("query = %q", rec.query)
	}
	if rec.apiKey != "az-secret" {
		t.Fatalf("api-key header = %q, want the Azure key header", rec.apiKey)
	}
	req := decodeJSON(t, body.get(t))
	tools, _ := req["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", req["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if _, has := tool["strict"]; has {
		t.Fatalf("tool = %v, want no strict field on Azure", tool)
	}
	if _, has := tool["parameters"].(map[string]any)["properties"]; !has {
		t.Fatalf("parameters = %v, want the sanitizer's properties map", tool["parameters"])
	}
}

// TestAzureResponsesDeploymentFallsBackToModel pins the deployment default and
// the pinned api-version default.
func TestAzureResponsesDeploymentFallsBackToModel(t *testing.T) {
	srv, body := newStreamServer(t, responsesFrames...)
	rec := &recordingTransport{}
	p := NewAzureResponsesProvider("azure", srv.URL, "", nil, AzureResponsesOptions{}, &http.Client{Transport: rec})
	ch, err := p.Stream(context.Background(), StreamRequest{Model: "gpt-4o-mini"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	collectEvents(t, ch)
	body.get(t)
	if !strings.Contains(rec.path, "/deployments/gpt-4o-mini/responses") {
		t.Fatalf("path = %q, want the model-derived deployment", rec.path)
	}
	if rec.query != "api-version="+DefaultAzureAPIVersion {
		t.Fatalf("query = %q, want the pinned default version", rec.query)
	}
}

// TestAzureResponsesRequiresDeployment pins the loud failure on a missing
// deployment.
func TestAzureResponsesRequiresDeployment(t *testing.T) {
	p := NewAzureResponsesProvider("azure", "http://127.0.0.1:1", "", nil, AzureResponsesOptions{}, nil)
	if _, err := p.Stream(context.Background(), StreamRequest{}); err == nil ||
		!strings.Contains(err.Error(), "deployment") {
		t.Fatalf("err = %v, want a clear deployment error", err)
	}
}

// TestAzureResponsesStrictBypass pins the PI_NO_STRICT escape hatch: Azure
// never asks for strict, and the bypass keeps Codex from claiming it either.
func TestAzureResponsesStrictBypass(t *testing.T) {
	t.Setenv("PI_NO_STRICT", "1")
	srv, body := newStreamServer(t, responsesFrames...)
	p := NewOpenAICodexResponsesProvider("codex", srv.URL, "", nil, nil)
	ch, err := p.Stream(context.Background(), StreamRequest{
		Model: "gpt-5",
		Tools: []ToolDef{{Name: "read", Parameters: []byte(`{"type":"object","properties":{"path":{"type":"string"}}}`)}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	collectEvents(t, ch)
	req := decodeJSON(t, body.get(t))
	tools, _ := req["tools"].([]any)
	tool, _ := tools[0].(map[string]any)
	if _, has := tool["strict"]; has {
		t.Fatalf("tool = %v, want no strict field when PI_NO_STRICT is set", tool)
	}
	if _, has := tool["parameters"].(map[string]any)["additionalProperties"]; has {
		t.Fatalf("parameters = %v, want the sanitized (not enforced) schema", tool["parameters"])
	}
}
