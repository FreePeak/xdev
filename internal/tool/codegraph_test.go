package tool

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/codegraph"
)

// The impact tool's whole value is turning "I am about to edit shared code"
// into "here is what else you touch". These tests pin that it does that, and
// -- more importantly -- that when it cannot, it says so and points at the
// tools that still work instead of going quiet.

func impactServer(t *testing.T, hit bool, calls *[]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if r.URL.Path == "/api/v1/status" {
			_ = json.NewEncoder(w).Encode(map[string]any{"project_dir": "/repo"})
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		*calls = append(*calls, body)
		if action, _ := body["action"].(string); action == "" {
			if !hit {
				_ = json.NewEncoder(w).Encode(map[string]any{"freshness": "fresh", "hits": []any{}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"freshness": "fresh", "hits": []any{
				map[string]any{"name": "Target", "qualified_name": "pkg/target.go::Target",
					"element_type": "function", "file_path": "pkg/target.go", "line_start": 12},
			}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"freshness": "fresh", "callers": []any{
			map[string]any{"qualified_name": "pkg/a.go::callsTarget"},
			map[string]any{"qualified_name": "pkg/b.go::alsoCalls"},
		}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newImpactTool(t *testing.T, base string) *ImpactTool {
	t.Helper()
	return &ImpactTool{
		Graph: codegraph.New(codegraph.Config{BaseURL: base, Project: "xdev", ExpectDir: "/repo"}),
		CWD:   "/repo",
	}
}

func runImpact(t *testing.T, it *ImpactTool, args map[string]any) Result {
	t.Helper()
	return runTool(t, it, args)
}

func TestImpactResolvesThenAsksForCallers(t *testing.T) {
	var calls []any
	it := newImpactTool(t, impactServer(t, true, &calls).URL)

	res := runImpact(t, it, map[string]any{"symbol": "Target"})
	if res.IsError {
		t.Fatalf("impact failed: %s", res.Text)
	}
	if !strings.Contains(res.Text, "pkg/a.go::callsTarget") {
		t.Fatalf("the callers must be named, got:\n%s", res.Text)
	}
	// The graph verbs require a qualified name, so the tool must resolve the
	// bare symbol first rather than forwarding it and taking the refusal.
	if len(calls) != 2 {
		t.Fatalf("expected resolve-then-ask, got %d requests: %v", len(calls), calls)
	}
	second, _ := calls[1].(map[string]any)
	if second["query"] != "pkg/target.go::Target" {
		t.Fatalf("second request query = %v, want the resolved name", second["query"])
	}
}

func TestImpactSaysSoWhenTheSymbolIsUnknown(t *testing.T) {
	var calls []any
	it := newImpactTool(t, impactServer(t, false, &calls).URL)

	res := runImpact(t, it, map[string]any{"symbol": "Nonexistent"})
	// An empty graph answer is not a broken graph: the model must be told the
	// symbol is absent, not that the server is down.
	if res.IsError {
		t.Fatalf("a missing symbol is a normal answer, not a tool error: %s", res.Text)
	}
	if !strings.Contains(res.Text, "no element named") {
		t.Fatalf("got:\n%s", res.Text)
	}
}

// TestImpactWorksWithoutAGraphConfigured covers a machine with no graph at
// all. Unlike a dead server (which degrades to a soft answer, below), a tool
// the session never wired up is an honest refusal: the model asked for a
// capability this run does not have, and the answer names the fallback.
func TestImpactWorksWithoutAGraphConfigured(t *testing.T) {
	it := &ImpactTool{CWD: "/repo"}
	res := runImpact(t, it, map[string]any{"symbol": "Target"})
	if !res.IsError {
		t.Fatalf("an unwired graph must not be a silent success, got:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "not configured") || !strings.Contains(res.Text, "grep") {
		t.Fatalf("it must name the fallback, got:\n%s", res.Text)
	}
}

func TestImpactPathNeedsATarget(t *testing.T) {
	var calls []any
	it := newImpactTool(t, impactServer(t, true, &calls).URL)
	res := runImpact(t, it, map[string]any{"symbol": "Target", "relation": "path"})
	if !res.IsError || !strings.Contains(res.Text, "to") {
		t.Fatalf("path without a target must say what is missing, got:\n%s", res.Text)
	}
}

// TestImpactDeadGraphPointsAtTheFallback is the failure contract from the
// decision doc: a graph that cannot answer must be indistinguishable from a
// cost the model can avoid, and must name the tools that still work.
func TestImpactDeadGraphPointsAtTheFallback(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer dead.Close()
	it := newImpactTool(t, dead.URL)

	res := runImpact(t, it, map[string]any{"symbol": "Target"})
	if res.IsError {
		t.Fatalf("a dead graph must not fail the turn: %s", res.Text)
	}
	if !strings.Contains(res.Text, "unavailable") || !strings.Contains(res.Text, "grep") {
		t.Fatalf("got:\n%s", res.Text)
	}
}

func TestImpactFilePathAsksForReferences(t *testing.T) {
	var calls []any
	it := newImpactTool(t, impactServer(t, true, &calls).URL)
	res := runImpact(t, it, map[string]any{"symbol": "pkg/target.go"})
	if res.IsError {
		t.Fatalf("a file path must be answered: %s", res.Text)
	}
	if !strings.Contains(res.Text, "pkg/target.go") {
		t.Fatalf("got:\n%s", res.Text)
	}
}

func TestImpactRequiresASymbol(t *testing.T) {
	it := newImpactTool(t, impactServer(t, true, &[]any{}).URL)
	res := runImpact(t, it, map[string]any{})
	if !res.IsError || !strings.Contains(res.Text, "symbol is required") {
		t.Fatalf("got: %+v", res)
	}
}

// --- code_query ------------------------------------------------------------

func TestCodeQueryRendersRankedCandidates(t *testing.T) {
	var calls []any
	q := &CodeQueryTool{
		Graph: codegraph.New(codegraph.Config{BaseURL: impactServer(t, true, &calls).URL, Project: "xdev", ExpectDir: "/repo"}),
		CWD:   "/repo",
	}
	res := runTool(t, q, map[string]any{"query": "Target"})
	if res.IsError {
		t.Fatalf("code_query failed: %s", res.Text)
	}
	if !strings.Contains(res.Text, "pkg/target.go::Target") {
		t.Fatalf("the qualified name must be returned so impact can follow it:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "pkg/target.go:12") {
		t.Fatalf("the file position must be returned:\n%s", res.Text)
	}
}

func TestCodeQueryOnEmptyResultNamesTheFallback(t *testing.T) {
	var calls []any
	q := &CodeQueryTool{
		Graph: codegraph.New(codegraph.Config{BaseURL: impactServer(t, false, &calls).URL, Project: "xdev", ExpectDir: "/repo"}),
		CWD:   "/repo",
	}
	res := runTool(t, q, map[string]any{"query": "nothing-here"})
	if res.IsError {
		t.Fatalf("an empty answer is normal: %s", res.Text)
	}
	if !strings.Contains(res.Text, "grep") {
		t.Fatalf("it must name the fallback:\n%s", res.Text)
	}
}

// TestImpactRendersTheServerRowShapes is the guard for a bug the unit tests
// could not have found: the fake server answered `callers` with objects, and
// the real server answers with plain strings. The renderer printed the right
// COUNT and then a row of "?" for every real answer, which is worse than
// silence -- the model gets a confident summary with nothing in it.
//
// The three shapes are copied from a live leankg (2026-10-03):
//
//	callers / callees : ["pkg/x.go::f", ...]
//	impact            : [{"qn": "pkg/x.go::f", "depth": 1}, ...]
func TestImpactRendersTheServerRowShapes(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if r.URL.Path == "/api/v1/status" {
			_ = json.NewEncoder(w).Encode(map[string]any{"project_dir": "/repo"})
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		action, _ := body["action"].(string)
		switch action {
		case "":
			_ = json.NewEncoder(w).Encode(map[string]any{"hits": []any{
				map[string]any{"qualified_name": "pkg/x.go::Target"}},
			})
		case "callers":
			// strings, as the real server sends them
			_ = json.NewEncoder(w).Encode(map[string]any{"callers": []any{"pkg/a.go::c1"}})
		case "impact":
			// objects keyed qn+depth
			_ = json.NewEncoder(w).Encode(map[string]any{"hits": []any{
				map[string]any{"qn": "pkg/x.go::Target", "depth": float64(1)},
			}})
		}
	}))
	defer srv.Close()

	it := &ImpactTool{
		Graph: codegraph.New(codegraph.Config{BaseURL: srv.URL, Project: "xdev", ExpectDir: "/repo"}),
		CWD:   "/repo",
	}

	res := runTool(t, it, map[string]any{"symbol": "Target", "relation": "callers"})
	if !strings.Contains(res.Text, "pkg/a.go::c1") {
		t.Fatalf("a string-shaped caller row was dropped:\n%s", res.Text)
	}
	if strings.Contains(res.Text, "?") {
		t.Fatalf("an empty row was rendered as \"?\":\n%s", res.Text)
	}

	res = runTool(t, it, map[string]any{"symbol": "Target", "relation": "impact", "depth": 2})
	if !strings.Contains(res.Text, "pkg/x.go::Target") {
		t.Fatalf("a qn-shaped impact row was dropped:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "depth 1") {
		t.Fatalf("the impact depth must survive:\n%s", res.Text)
	}
}
