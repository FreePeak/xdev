package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/tool"
)

// A resumed session continues at the rung its own transcript records.
//
// The machine-wide setting is a different value from what the run that
// produced the transcript used, and the rung is not only a reasoning budget
// in xdev: it decides the TOOL SURFACE (tool.ApplySessionEffort). A resumed
// session that came back at the global rung would offer a different set of
// tools than the run that wrote the transcript — and the model would be
// reasoning at a level the transcript never saw.
func TestAResumedSessionKeepsItsOwnEffortRung(t *testing.T) {
	s := session.OpenMem("test", "t")
	appendThinkingLevel(s, "high", "high")

	if got := sessionThinkingLevel(s, "low"); got != "high" {
		t.Fatalf("level = %q, want high (the session's own record wins over the global setting)", got)
	}
	if got := sessionEffortRung(s, "low"); got != "high" {
		t.Fatalf("rung = %q, want high", got)
	}

	reg := effortStubRegistry()
	tool.ApplySessionEffort(reg, sessionEffortRung(s, "low"))
	deferred := effortDeferred(reg)
	if !deferred["computer"] {
		t.Fatal("the resumed session must keep the high rung's surface: computer stays deferred")
	}
	if deferred["ast_grep"] {
		t.Fatal("the resumed session must not fall back to another rung's surface: ast_grep stays eager")
	}
}

// A session that never changed its level records nothing, and falls back to
// the machine-wide setting — the same value a fresh session starts with.
func TestASessionThatRecordsNoLevelFallsBackToTheGlobalRung(t *testing.T) {
	s := session.OpenMem("test", "t")
	if err := s.Append(&session.MessageEntry{}); err != nil {
		t.Fatal(err)
	}
	if got := sessionThinkingLevel(s, "high"); got != "high" {
		t.Fatalf("level = %q, want the global fallback high", got)
	}
	if got := sessionEffortRung(s, "high"); got != "high" {
		t.Fatalf("rung = %q, want the global fallback high", got)
	}
}

// An INHERITED level ("auto") is not a rung: the level followed the model
// role's ":effort" and the tool surface stayed the default one.
func TestAnInheritedLevelRestoresTheDefaultRung(t *testing.T) {
	s := session.OpenMem("test", "t")
	appendThinkingLevel(s, "high", session.ConfiguredAuto)
	if got := sessionEffortRung(s, "low"); got != config.SessionEffortDefault {
		t.Fatalf("rung = %q, want the default rung %q", got, config.SessionEffortDefault)
	}
	reg := effortStubRegistry()
	tool.ApplySessionEffort(reg, sessionEffortRung(s, "low"))
	deferred := effortDeferred(reg)
	if !deferred["ast_grep"] {
		t.Fatal("the default rung defers ast_grep")
	}
	if deferred["computer"] {
		t.Fatal("the default rung keeps computer eager")
	}
}

// The wire request carries the level the rung names and the levels the
// model accepts. A model that declares no capability gets no level at all,
// so its request stays the one it always sent.
func TestEffortThinkingCarriesTheLevelAndTheCapability(t *testing.T) {
	cfg := &config.Config{Providers: map[string]*config.ProviderConfig{
		"openai": {BaseURL: "https://api.openai.com/v1", API: "openai-responses", Models: []config.ModelConfig{
			{ID: "gpt-6", Effort: []string{"low", "medium", "high", "xhigh", "max"}},
			{ID: "gpt-6-plain"},
		}},
	}}

	b := effortThinking("xhigh", "openai", "gpt-6", cfg)
	if b == nil {
		t.Fatal("budget = nil, want the xhigh budget")
	}
	if b.Tokens != 32768 {
		t.Errorf("tokens = %d, want 32768 (the budget is unchanged)", b.Tokens)
	}
	if b.Level != "xhigh" {
		t.Errorf("level = %q, want xhigh", b.Level)
	}
	if len(b.Supported) != 5 {
		t.Errorf("supported = %v, want the five declared levels", b.Supported)
	}

	plain := effortThinking("xhigh", "openai", "gpt-6-plain", cfg)
	if plain == nil || plain.Tokens != 32768 {
		t.Fatalf("budget = %v, want the xhigh budget", plain)
	}
	if plain.Level != "" || len(plain.Supported) != 0 {
		t.Fatalf("level/supported = %q/%v, want none for a model that declares no capability",
			plain.Level, plain.Supported)
	}
}

// effortStub is a tool that only has a name: the ladder is about WHICH
// tools are deferred, not what they do.
type effortStub struct{ n string }

func (s effortStub) Name() string                { return s.n }
func (s effortStub) Description() string         { return s.n + " tool" }
func (s effortStub) Parameters() json.RawMessage { return json.RawMessage(`{}`) }
func (s effortStub) Execute(context.Context, json.RawMessage) (tool.Result, error) {
	return tool.Result{}, nil
}

// effortStubRegistry is a registry holding the tools the ladder defers, one
// per rung shape.
func effortStubRegistry() *tool.Registry {
	r := tool.NewRegistry()
	for _, n := range []string{
		"read", "write", "edit", "bash", "grep", "glob", "ask", "todo",
		"ast_grep", "ast_edit", "github", "gmail", "hub", "send_message", "inbox",
		"checkpoint", "rewind", "regression_tests",
		"computer", "tts", "generate_image", "security_scan",
		"schedule_create", "schedule_list", "schedule_delete",
	} {
		r.Register(effortStub{n: n})
	}
	return r
}

// effortDeferred is the set of tools the registry keeps behind the bridge.
func effortDeferred(r *tool.Registry) map[string]bool {
	out := map[string]bool{}
	for _, e := range r.Deferred() {
		out[e.Name] = true
	}
	return out
}
