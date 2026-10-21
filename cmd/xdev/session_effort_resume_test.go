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
	appendThinkingLevel(s, "high", "high", "high")

	if got := sessionLevel(s, "low"); got != "high" {
		t.Fatalf("level = %q, want high (the session's own record wins over the global setting)", got)
	}
	if got := sessionRung(s, "low"); got != "high" {
		t.Fatalf("rung = %q, want high", got)
	}

	reg := effortStubRegistry()
	tool.ApplySessionEffort(reg, sessionRung(s, "low"))
	deferred := effortDeferred(reg)
	if !deferred["computer"] {
		t.Fatal("the resumed session must keep the high rung's surface: computer stays deferred")
	}
	if deferred["ast_grep"] {
		t.Fatal("the resumed session must not fall back to another rung's surface: ast_grep stays eager")
	}
}

// The RUNG and the LEVEL are two different facts, and the transcript records
// both. A session that pinned /thinking high on the low rung ran with the
// LOW tool surface and the high level: resuming it must restore the low
// surface, not the high one the level names.
//
// This is the mismatch a level-only record cannot see — and it is the
// difference between "a resumed session keeps its own effort rung" and a
// claim that a rung is a level.
func TestAResumedSessionKeepsTheRungAndNotTheLevel(t *testing.T) {
	s := session.OpenMem("test", "t")
	appendThinkingLevel(s, "high", "high", "low")

	if got := sessionLevel(s, "medium"); got != "high" {
		t.Fatalf("level = %q, want high", got)
	}
	if got := sessionRung(s, "medium"); got != "low" {
		t.Fatalf("rung = %q, want low (the rung the record names, not the level)", got)
	}

	reg := effortStubRegistry()
	tool.ApplySessionEffort(reg, sessionRung(s, "medium"))
	deferred := effortDeferred(reg)
	if !deferred["ast_grep"] {
		t.Fatal("the resumed session must keep the low rung's surface: ast_grep stays deferred")
	}
	if deferred["computer"] != true {
		// The low rung defers everything, computer included.
		t.Fatal("the low rung defers computer too")
	}
}

// A session that never changed its level records nothing, and falls back to
// the machine-wide setting — the same value a fresh session starts with.
func TestASessionThatRecordsNoLevelFallsBackToTheGlobalRung(t *testing.T) {
	s := session.OpenMem("test", "t")
	if err := s.Append(&session.MessageEntry{}); err != nil {
		t.Fatal(err)
	}
	if got := sessionLevel(s, "high"); got != "high" {
		t.Fatalf("level = %q, want the global fallback high", got)
	}
	if got := sessionRung(s, "high"); got != "high" {
		t.Fatalf("rung = %q, want the global fallback high", got)
	}
}

// A record that pins reasoning OFF is a record, not an absence: the level
// it names is "" and the session resumes with no reasoning — not with
// whatever the machine-wide setting says.
func TestASessionThatPinnedReasoningOffResumesOff(t *testing.T) {
	s := session.OpenMem("test", "t")
	appendThinkingLevel(s, "", "off", "medium")

	if got := sessionLevel(s, "high"); got != "" {
		t.Fatalf("level = %q, want empty (the session pinned reasoning off)", got)
	}
	// The rung is still the record's: /thinking off moves the level, not the
	// tool surface.
	if got := sessionRung(s, "low"); got != "medium" {
		t.Fatalf("rung = %q, want medium (the record's rung)", got)
	}
}

// An INHERITED level ("auto") is not a rung: the level followed the model
// role's ":effort" and the tool surface stayed the default one. It is the
// fallback for a record written before the rung field existed.
func TestAnInheritedLevelRestoresTheDefaultRung(t *testing.T) {
	s := session.OpenMem("test", "t")
	appendThinkingLevel(s, "high", session.ConfiguredAuto, "")
	if got := sessionRung(s, "low"); got != config.SessionEffortDefault {
		t.Fatalf("rung = %q, want the default rung %q", got, config.SessionEffortDefault)
	}
	reg := effortStubRegistry()
	tool.ApplySessionEffort(reg, sessionRung(s, "low"))
	deferred := effortDeferred(reg)
	if !deferred["ast_grep"] {
		t.Fatal("the default rung defers ast_grep")
	}
	if deferred["computer"] {
		t.Fatal("the default rung keeps computer eager")
	}
}

// The resumed level goes through the SAME clamp every other path does, so a
// persisted level above what the model accepts resumes clamped — not
// dropped, and not sent as a level the model does not take.
func TestTheResumeClampsThePersistedLevel(t *testing.T) {
	cfg := &config.Config{Providers: map[string]*config.ProviderConfig{
		"openai": {BaseURL: "https://api.openai.com/v1", API: "openai-responses", Models: []config.ModelConfig{
			{ID: "gpt-6", Effort: []string{"low", "medium", "high"}},
		}},
	}}
	s := session.OpenMem("test", "t")
	appendThinkingLevel(s, "xhigh", "xhigh", "high")

	got := sessionLevelFor(s, "low", "openai", "gpt-6", cfg)
	if got != "high" {
		t.Fatalf("level = %q, want high (clamped to what the model accepts)", got)
	}
	// The clamp is the model's, not the session's: a model that takes
	// everything leaves the level alone.
	if got := sessionLevelFor(s, "low", "openai", "gpt-6-unknown", cfg); got != "xhigh" {
		t.Fatalf("level = %q, want xhigh (a model the catalog does not list has no opinion)", got)
	}
}

// The capability must not need a re-run of `xdev connect`: the SHIPPED
// catalog is a trusted source, and it is what connect copies from.
func TestTheEffortCapabilityFallsBackToTheShippedCatalog(t *testing.T) {
	// A model the user's models.yml says nothing about.
	cfg := &config.Config{Providers: map[string]*config.ProviderConfig{
		"onegw": {BaseURL: "http://127.0.0.1:20128/v1", API: "openai-completions", Models: []config.ModelConfig{
			{ID: "gpt-6-astra"},
		}},
	}}
	levels := modelEffortWire(cfg, "onegw", "gpt-6-astra")
	if len(levels) == 0 {
		t.Fatal("the shipped catalog must answer for a model the models.yml does not declare")
	}
	// A model nobody knows: no capability, and the request keeps its shape.
	if got := modelEffortWire(cfg, "onegw", "no-such-model"); len(got) != 0 {
		t.Fatalf("levels = %v, want none for a model the catalog does not name", got)
	}
	// The user's own declaration wins over the catalog.
	// A second provider name: the discovered-model cache is keyed by the
	// provider, so a reused name would answer from the first config.
	cfg2 := &config.Config{Providers: map[string]*config.ProviderConfig{
		"onegw2": {BaseURL: "http://127.0.0.1:20128/v1", API: "openai-completions", Models: []config.ModelConfig{
			{ID: "gpt-6-astra", Effort: []string{"low"}},
		}},
	}}
	if got := modelEffortWire(cfg2, "onegw2", "gpt-6-astra"); len(got) != 1 || got[0] != "low" {
		t.Fatalf("levels = %v, want the user's own declaration [low]", got)
	}
}

// The tool set follows the SESSION, not the shared registry: a view carries
// its own deferred set, so two sessions at different rungs do not fight
// over one catalog.
func TestTheToolViewLeavesTheSharedRegistryAlone(t *testing.T) {
	reg := effortStubRegistry()
	tool.ApplySessionEffort(reg, "medium")
	shared := effortDeferred(reg)
	if !shared["ast_grep"] {
		t.Fatal("the shared registry starts at the medium rung: ast_grep deferred")
	}

	view := reg.View("high")
	if view == nil {
		t.Fatal("View returned nil")
	}
	viewDeferred := effortDeferred(view)
	if !viewDeferred["computer"] {
		t.Fatal("the view carries the high rung's surface: computer deferred")
	}
	if viewDeferred["ast_grep"] {
		t.Fatal("the view carries the high rung's surface: ast_grep eager")
	}
	// The shared registry is untouched: the view is a window, not a rebuild.
	after := effortDeferred(reg)
	if !after["ast_grep"] || after["computer"] {
		t.Fatal("the view must not rebuild the shared registry's deferred set")
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
