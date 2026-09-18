package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/websearch"
)

// TestWebSearchToolContract pins the registry-facing contract: the name the
// model calls, and a schema that requires the query argument.
func TestWebSearchToolContract(t *testing.T) {
	ws := NewWebSearchTool(websearch.Settings{})
	if ws.Name() != "web_search" {
		t.Fatalf("name = %q", ws.Name())
	}
	var schema struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(ws.Parameters(), &schema); err != nil {
		t.Fatalf("parameters are not JSON: %v", err)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "query" {
		t.Fatalf("required = %v", schema.Required)
	}
}

func TestWebSearchToolArgumentHandling(t *testing.T) {
	ws := NewWebSearchTool(websearch.Settings{})
	if res, err := ws.Execute(context.Background(), json.RawMessage(`{bad`)); err != nil || !res.IsError {
		t.Fatalf("malformed args: err=%v res=%+v", err, res)
	}
	if res, err := ws.Execute(context.Background(), json.RawMessage(`{"query":"  "}`)); err != nil || !res.IsError {
		t.Fatalf("blank query: err=%v res=%+v", err, res)
	}
}

// TestWebSearchToolRejectsBadMaxResults: a negative or over-ceiling
// max_results is an argument error, not a silent clamp — the engine's limit()
// would otherwise happily honor 21, and a negative would underflow the
// per-provider request count.
func TestWebSearchToolRejectsBadMaxResults(t *testing.T) {
	ws := NewWebSearchTool(websearch.Settings{})
	for _, tc := range []struct {
		args string
		want string
	}{
		{`{"query":"x","max_results":-1}`, "must be non-negative"},
		{`{"query":"x","max_results":21}`, "exceeds the ceiling"},
	} {
		res, err := ws.Execute(context.Background(), json.RawMessage(tc.args))
		if err != nil || !res.IsError || !strings.Contains(res.Text, tc.want) {
			t.Fatalf("%s: err=%v res=%+v", tc.args, err, res)
		}
	}
}

// TestWebSearchToolDelegatesToSearcher proves the wrapper wires the engine's
// answer through unchanged: with a keyless tavily-only chain the engine
// reports the skip, and the wrapper must surface it as a failed result with
// the structured details attached.
func TestWebSearchToolDelegatesToSearcher(t *testing.T) {
	t.Setenv("TAVILY_API_KEY", "")
	t.Setenv("BRAVE_API_KEY", "")
	ws := NewWebSearchTool(websearch.Settings{Providers: []string{"tavily"}})

	res, err := ws.Execute(context.Background(), json.RawMessage(`{"query":"golang generics"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("no provider ran; expected a failed result: %+v", res)
	}
	if !strings.Contains(res.Text, "tavily: skipped: no API key") {
		t.Fatalf("engine note lost: %q", res.Text)
	}
	details, ok := res.Details.(map[string]any)
	if !ok {
		t.Fatalf("details not machine-readable: %T", res.Details)
	}
	results, ok := details["results"].([]map[string]string)
	if !ok || len(results) != 0 {
		t.Fatalf("results = %#v", details["results"])
	}
}
