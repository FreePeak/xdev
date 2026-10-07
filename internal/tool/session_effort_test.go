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

func TestApplySessionEffortLeanDefersLongTail(t *testing.T) {
	r := NewRegistry()
	for _, n := range []string{"read", "write", "edit", "bash", "grep", "glob", "ask", "todo", "lsp", "task", "browser", "web_search"} {
		r.Register(namedStub{n: n, d: n + " tool"})
	}
	r.Register(NewToolSearchTool(r.Catalog()))
	ApplySessionEffort(r, "lean")
	eager := map[string]bool{}
	for _, d := range r.Defs() {
		eager[d.Name] = true
	}
	for _, must := range []string{"read", "write", "edit", "bash", "grep", "glob", "ask", "todo", "tool_search"} {
		if !eager[must] {
			t.Fatalf("lean must keep %s eager", must)
		}
	}
	for _, gone := range []string{"lsp", "task", "browser", "web_search"} {
		if eager[gone] {
			t.Fatalf("lean must defer %s", gone)
		}
	}
}

func TestApplySessionEffortStandardMatchesLegacySet(t *testing.T) {
	r := NewRegistry()
	for _, n := range []string{"read", "ast_grep", "ast_edit", "github", "hub", "send_message", "inbox", CheckpointToolName, RewindToolName, "lsp"} {
		r.Register(namedStub{n: n, d: n})
	}
	ApplySessionEffort(r, "standard")
	deferred := map[string]bool{}
	for _, e := range r.Deferred() {
		deferred[e.Name] = true
	}
	for _, want := range []string{"ast_grep", "ast_edit", "github", "hub", "send_message", "inbox", CheckpointToolName, RewindToolName} {
		if !deferred[want] {
			t.Fatalf("standard must defer %s", want)
		}
	}
	if deferred["lsp"] || deferred["read"] {
		t.Fatal("standard must leave lsp and read eager")
	}
}

func TestApplySessionEffortFullKeepsCodingToolsEager(t *testing.T) {
	r := NewRegistry()
	for _, n := range []string{"read", "lsp", "task", "browser", "web_search", "computer", "tts", "generate_image"} {
		r.Register(namedStub{n: n, d: n})
	}
	ApplySessionEffort(r, "full")
	eager := map[string]bool{}
	for _, d := range r.Defs() {
		eager[d.Name] = true
	}
	for _, must := range []string{"read", "lsp", "task", "browser", "web_search"} {
		if !eager[must] {
			t.Fatalf("full must keep %s eager", must)
		}
	}
	if eager["computer"] {
		t.Fatal("full still defers computer")
	}
}

func TestApplySessionEffortReapplyIsIdempotent(t *testing.T) {
	r := NewRegistry()
	r.Register(namedStub{n: "read", d: "r"})
	r.Register(namedStub{n: "lsp", d: "l"})
	ApplySessionEffort(r, "lean")
	ApplySessionEffort(r, "full")
	ApplySessionEffort(r, "lean")
	for _, d := range r.Defs() {
		if d.Name == "lsp" {
			t.Fatal("second lean must defer lsp again")
		}
	}
}
