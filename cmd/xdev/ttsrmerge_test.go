package main

import (
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/rules"
)

// ttsrConfig merges discovered rule-file conditions into the stream-rule set
// (parity T3 #39: the rules parser stored `condition` verbatim and nothing
// consumed it, so a rulebook rule was dead config). NewTTSR returns nil when
// no rule has a usable condition, so a non-nil engine is the proof the
// discovered condition reached the matcher.

func TestTTSRConfigMergesDiscoveredConditions(t *testing.T) {
	prev := rules.Active()
	t.Cleanup(func() { rules.Set(prev) })
	rules.Set([]rules.Rule{
		{Name: "secret-leak", Condition: `SK-[A-Z0-9]{8}`, Description: "stop echoing secrets", Source: "cursor"},
		{Name: "prose-only", Condition: ""}, // a rulebook without a condition is not a stream rule
	})
	got := ttsrConfig(&config.Settings{})
	if got == nil || len(got.Rules) != 1 {
		t.Fatalf("the discovered condition must become exactly one rule: %+v", got)
	}
	r := got.Rules[0]
	if r.Name != "secret-leak" || !strings.Contains(r.Condition, "SK-") {
		t.Fatalf("wrong rule merged: %+v", r)
	}
	if r.Message != "stop echoing secrets" {
		t.Fatalf("the rulebook description should become the notice: %+v", r)
	}
	if agent.NewTTSR(got) == nil {
		t.Fatal("merged config produced no engine — the condition is still dead config")
	}
}

func TestTTSRConfigSettingsRuleWinsByName(t *testing.T) {
	prev := rules.Active()
	t.Cleanup(func() { rules.Set(prev) })
	rules.Set([]rules.Rule{{Name: "dup", Condition: "FROM-RULEBOOK", InterruptMode: "bogus"}})
	s := &config.Settings{TTSR: &config.TTSRSettings{Rules: []config.TTSRRule{{Name: "dup", Condition: "FROM-SETTINGS"}}}}
	got := ttsrConfig(s)
	if len(got.Rules) != 1 || got.Rules[0].Condition != "FROM-SETTINGS" {
		t.Fatalf("a declared rule must shadow the discovered one: %+v", got.Rules)
	}
}

// A discovered rule with no settings group still arms the engine (the group
// ships enabled by default), and an unknown interrupt mode inherits the group
// policy rather than reaching the matcher raw.
func TestTTSRConfigDefaultsAndModeSanitizing(t *testing.T) {
	prev := rules.Active()
	t.Cleanup(func() { rules.Set(prev) })
	rules.Set([]rules.Rule{{Name: "only", Condition: "X", InterruptMode: "sometimes"}})
	got := ttsrConfig(&config.Settings{})
	if got.Enabled == nil || !*got.Enabled {
		t.Fatal("a rulebook-only merge must leave the engine enabled")
	}
	if got.Rules[0].InterruptMode != "" {
		t.Fatalf("unknown mode must fall back to inherit, got %q", got.Rules[0].InterruptMode)
	}
	if ttsrModeOrInherit("tool-only") != "tool-only" {
		t.Fatal("valid modes pass through")
	}
}

// The loaded settings object is never mutated by the merge: /settings and any
// later read must still see the declared rules only.
func TestTTSRConfigDoesNotMutateSettings(t *testing.T) {
	prev := rules.Active()
	t.Cleanup(func() { rules.Set(prev) })
	rules.Set([]rules.Rule{{Name: "extra", Condition: "Y"}})
	s := &config.Settings{TTSR: &config.TTSRSettings{Rules: []config.TTSRRule{{Name: "declared", Condition: "Z"}}}}
	_ = ttsrConfig(s)
	if len(s.TTSR.Rules) != 1 || s.TTSR.Rules[0].Name != "declared" {
		t.Fatalf("settings were mutated by the merge: %+v", s.TTSR.Rules)
	}
}
