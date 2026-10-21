package tool

import (
	"context"
	"encoding/json"
	"testing"
)

type namedStub struct{ n, d string }

func (s namedStub) Name() string                { return s.n }
func (s namedStub) Description() string         { return s.d }
func (s namedStub) Parameters() json.RawMessage { return json.RawMessage(`{}`) }
func (s namedStub) Execute(context.Context, json.RawMessage) (Result, error) {
	return Result{}, nil
}

func TestApplySessionEffortLowDefersLongTail(t *testing.T) {
	r := NewRegistry()
	for _, n := range []string{"read", "write", "edit", "bash", "grep", "glob", "ask", "todo", "lsp", "task", "browser", "web_search"} {
		r.Register(namedStub{n: n, d: n + " tool"})
	}
	r.Register(NewToolSearchTool(r.Catalog()))
	ApplySessionEffort(r, "low")
	eager := map[string]bool{}
	for _, d := range r.Defs() {
		eager[d.Name] = true
	}
	for _, must := range []string{"read", "write", "edit", "bash", "grep", "glob", "ask", "todo", "tool_search"} {
		if !eager[must] {
			t.Fatalf("low must keep %s eager", must)
		}
	}
	for _, gone := range []string{"lsp", "task", "browser", "web_search"} {
		if eager[gone] {
			t.Fatalf("low must defer %s", gone)
		}
	}
}

func TestApplySessionEffortMediumMatchesLegacySet(t *testing.T) {
	r := NewRegistry()
	for _, n := range []string{"read", "ast_grep", "ast_edit", "github", "gmail", "hub", "send_message", "inbox", CheckpointToolName, RewindToolName, "regression_tests", "lsp"} {
		r.Register(namedStub{n: n, d: n})
	}
	ApplySessionEffort(r, "medium")
	deferred := map[string]bool{}
	for _, e := range r.Deferred() {
		deferred[e.Name] = true
	}
	for _, want := range []string{"ast_grep", "ast_edit", "github", "gmail", "hub", "send_message", "inbox", CheckpointToolName, RewindToolName, "regression_tests"} {
		if !deferred[want] {
			t.Fatalf("medium must defer %s", want)
		}
	}
	if deferred["lsp"] || deferred["read"] {
		t.Fatal("medium must leave lsp and read eager")
	}
}

func TestApplySessionEffortHighKeepsCodingToolsEager(t *testing.T) {
	r := NewRegistry()
	for _, n := range []string{"read", "lsp", "task", "browser", "web_search", "computer", "tts", "generate_image"} {
		r.Register(namedStub{n: n, d: n})
	}
	ApplySessionEffort(r, "high")
	eager := map[string]bool{}
	for _, d := range r.Defs() {
		eager[d.Name] = true
	}
	for _, must := range []string{"read", "lsp", "task", "browser", "web_search"} {
		if !eager[must] {
			t.Fatalf("high must keep %s eager", must)
		}
	}
	if eager["computer"] {
		t.Fatal("high still defers computer")
	}
}

// TestApplySessionEffortTopRungsShareTheHighSurface: xhigh and max buy a
// wider reasoning budget, not a fourth catalog. They must defer EXACTLY what
// high defers — a rung that quietly kept computer/tts eager would be a
// different surface, not a higher effort.
func TestApplySessionEffortTopRungsShareTheHighSurface(t *testing.T) {
	names := []string{"read", "lsp", "task", "browser", "web_search", "computer", "tts", "generate_image", "security_scan"}
	deferredFor := func(rung string) map[string]bool {
		r := NewRegistry()
		for _, n := range names {
			r.Register(namedStub{n: n, d: n})
		}
		ApplySessionEffort(r, rung)
		out := map[string]bool{}
		for _, e := range r.Deferred() {
			out[e.Name] = true
		}
		return out
	}
	high := deferredFor("high")
	for _, rung := range []string{"xhigh", "max"} {
		got := deferredFor(rung)
		if len(got) != len(high) {
			t.Fatalf("%s defers %v, high defers %v", rung, got, high)
		}
		for name := range high {
			if !got[name] {
				t.Fatalf("%s must defer %s like high does", rung, name)
			}
		}
	}
}

// TestApplySessionEffortFoldsPreLadderSpellings: the aliases a stored value or
// an old session may carry resolve to the same three surfaces, so upgrading
// cannot silently change a user's tool set.
func TestApplySessionEffortFoldsPreLadderSpellings(t *testing.T) {
	names := []string{"read", "lsp", "task", "browser", "computer", "tts"}
	deferredFor := func(rung string) map[string]bool {
		r := NewRegistry()
		for _, n := range names {
			r.Register(namedStub{n: n, d: n})
		}
		ApplySessionEffort(r, rung)
		out := map[string]bool{}
		for _, e := range r.Deferred() {
			out[e.Name] = true
		}
		return out
	}
	for alias, rung := range map[string]string{
		"lean": "low", "simple": "low", "minimal": "low",
		"standard": "medium", "": "medium", "nonsense": "medium",
		"full": "high", "omp": "high", "complex": "high",
	} {
		got, want := deferredFor(alias), deferredFor(rung)
		if len(got) != len(want) {
			t.Fatalf("effort %q defers %v, want %q's %v", alias, got, rung, want)
		}
		for name := range want {
			if !got[name] {
				t.Fatalf("effort %q must defer %s like %q does", alias, name, rung)
			}
		}
	}
}

func TestApplySessionEffortReapplyIsIdempotent(t *testing.T) {
	r := NewRegistry()
	r.Register(namedStub{n: "read", d: "r"})
	r.Register(namedStub{n: "lsp", d: "l"})
	ApplySessionEffort(r, "low")
	ApplySessionEffort(r, "high")
	ApplySessionEffort(r, "low")
	for _, d := range r.Defs() {
		if d.Name == "lsp" {
			t.Fatal("second low must defer lsp again")
		}
	}
}

// A view is a WINDOW onto the tool set, not a copy of it: a tool registered
// after the view was made — an MCP server that connects late, an extension
// attached later — is visible in the view at once.
//
// A copy would hide it, and the model would never be offered the tool: the
// view is built once per turn, and the extension tools land after it.
func TestTheViewSeesAToolRegisteredAfterItWasMade(t *testing.T) {
	r := NewRegistry()
	r.Register(namedStub{n: "read"})
	view := r.View("medium")
	if !viewHas(view, "read") {
		t.Fatal("the view must carry the tools the registry had")
	}
	r.Register(namedStub{n: "late_extension_tool"})
	if !viewHas(view, "late_extension_tool") {
		t.Fatal("a tool registered after the view was made must be visible in it")
	}
	// The shared registry keeps its own set: the view's rung never deferred
	// anything on it.
	if r.deferred("read") {
		t.Fatal("the view must not defer anything on the shared registry")
	}
	// The view's own bridge is over the view's own catalog.
	if view.Catalog() == r.Catalog() {
		t.Fatal("the view must carry its own deferred set")
	}
}

// viewHas reports whether the view advertises the named tool.
func viewHas(r *Registry, name string) bool {
	for _, d := range r.Defs() {
		if d.Name == name {
			return true
		}
	}
	return false
}
