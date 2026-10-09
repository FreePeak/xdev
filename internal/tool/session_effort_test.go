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
	for _, n := range []string{"read", "ast_grep", "ast_edit", "github", "gmail", "hub", "send_message", "inbox", CheckpointToolName, RewindToolName, "lsp"} {
		r.Register(namedStub{n: n, d: n})
	}
	ApplySessionEffort(r, "medium")
	deferred := map[string]bool{}
	for _, e := range r.Deferred() {
		deferred[e.Name] = true
	}
	for _, want := range []string{"ast_grep", "ast_edit", "github", "gmail", "hub", "send_message", "inbox", CheckpointToolName, RewindToolName} {
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
