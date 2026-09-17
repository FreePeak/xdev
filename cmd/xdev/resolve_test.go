package main

import (
	"testing"

	"github.com/FreePeak/xdev/internal/config"
	"strings"
)

func TestResolveModelPrecedence(t *testing.T) {
	resetProviderModelCache()
	cfg := &config.Config{Providers: map[string]*config.ProviderConfig{
		// "flag" is in the catalog: resolveModel now validates literal refs
		// against the merged catalog so a typo fails here instead of 404ing
		// on the next turn.
		"onegw": {Models: []config.ModelConfig{{ID: "yml-default"}, {ID: "flag"}}},
	}}
	s := &config.Settings{DefaultModel: "onegw/from-settings"}
	t.Setenv("XDEV_MODEL", "onegw/from-env")
	// Explicit (flag/settings-merged) beats env.
	if got, _, err := resolveModel("onegw/flag", cfg, s); err != nil || got != "onegw/flag" {
		t.Fatalf("explicit = %q err=%v", got, err)
	}
	// Empty explicit falls through to XDEV_MODEL.
	if got, _, err := resolveModel("", cfg, s); err != nil || got != "onegw/from-env" {
		t.Fatalf("env = %q err=%v", got, err)
	}
	// No env: settings.defaultModel wins over models.yml.
	t.Setenv("XDEV_MODEL", "")
	if got, _, err := resolveModel("", cfg, s); err != nil || got != "onegw/from-settings" {
		t.Fatalf("settings default = %q err=%v", got, err)
	}
}

// TestResolveModelAcceptance pins what the -model flag and /model accept:
// bare model ids resolve against the merged catalogs, a literal ":effort"
// suffix is split off the id (it used to reach the wire as part of the model
// name and 404 one turn later), and an id the provider does not declare is
// rejected up front instead of silently accepted.
func TestResolveModelAcceptance(t *testing.T) {
	// providerModelCache is keyed by provider name for the whole process:
	// another test's fixture for the same name must not leak in (and these
	// discoveries must not leak out).
	resetProviderModelCache()
	cfg := &config.Config{Providers: map[string]*config.ProviderConfig{
		"onegw": {Models: []config.ModelConfig{{ID: "free"}, {ID: "dev"}}},
		"empty": {}, // override-only provider: no catalog to validate against
		"a":     {Models: []config.ModelConfig{{ID: "dup"}}},
		"b":     {Models: []config.ModelConfig{{ID: "dup"}}},
	}}
	tests := []struct {
		name       string
		in         string
		wantRef    string
		wantEffort string
		wantErr    string
	}{
		{name: "concrete ref", in: "onegw/dev", wantRef: "onegw/dev"},
		{name: "bare id", in: "dev", wantRef: "onegw/dev"},
		{name: "bare id case-insensitive", in: "DEV", wantRef: "onegw/dev"},
		{name: "effort suffix", in: "onegw/dev:high", wantRef: "onegw/dev", wantEffort: "high"},
		{name: "bare id with effort", in: "dev:low", wantRef: "onegw/dev", wantEffort: "low"},
		// The catalog check is advisory for typed refs (gateways serve more
		// than models.yml pins); the hard guards are bare-id expansion below.
		{name: "unknown suffix stays in the id", in: "onegw/dev:turbo", wantRef: "onegw/dev:turbo"},
		{name: "unknown id for a known provider", in: "onegw/nope", wantRef: "onegw/nope"},
		{name: "unknown bare id", in: "nope", wantErr: "unknown model"},
		{name: "ambiguous bare id", in: "dup", wantErr: "matches a/dup, b/dup"},
		{name: "override-only provider skips validation", in: "empty/anything", wantRef: "empty/anything"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ref, effort, err := resolveModel(tc.in, cfg, &config.Settings{})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ref != tc.wantRef || effort != tc.wantEffort {
				t.Fatalf("got %q/%q, want %q/%q", ref, effort, tc.wantRef, tc.wantEffort)
			}
		})
	}
}

// TestTitleFromPrompt pins the session-title derivation: first line only,
// whitespace collapsed, capped so a pasted paragraph cannot become a title.
func TestTitleFromPrompt(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "first line wins", in: "fix the picker\nand the rest", want: "fix the picker"},
		{name: "whitespace collapsed", in: "  fix   the\tpicker ", want: "fix the picker"},
		{name: "capped", in: strings.Repeat("x", 60), want: strings.Repeat("x", 40) + "…"},
		{name: "empty", in: "   \n  ", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := titleFromPrompt(tc.in); got != tc.want {
				t.Fatalf("titleFromPrompt(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestEffortReachesTheBudget pins that ":effort" resolves to a concrete
// reasoning budget instead of being discarded at the cmd boundary (an
// earlier revision bound effortRef and then dropped it with `_ =`).
func TestEffortReachesTheBudget(t *testing.T) {
	_, effort, err := resolveModel("onegw/dev:high", &config.Config{}, &config.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	if effort != "high" {
		t.Fatalf("inline effort lost: %q", effort)
	}
	bud := effortBudget(effort)
	if bud == nil || bud.Tokens != config.EffortTokens["high"] {
		t.Fatalf("budget = %+v", bud)
	}
	if effortBudget("") != nil || effortBudget("nonsense") != nil {
		t.Fatal("no/unknown effort must mean no thinking requested")
	}
	// "minimal" is documented as the off switch (config.EffortTokens: 0), and a
	// 0-token budget is not "off" to any adapter — Anthropic rejects
	// max_tokens<=0, the OpenAI adapters read effort "low". It must fold to nil
	// here, the one function every run mode and adapter reads.
	if bud := effortBudget("minimal"); bud != nil {
		t.Fatalf("minimal must mean no thinking requested, got %+v", bud)
	}
}

// TestModelSwitchIsSticky: /model used to exist only inside the process that
// ran it — the next xdev resolved the model from settings.defaultModel and
// snapped back, so a user who picked onegw/xdev had to pick it again every
// launch. persistDefaultModel writes the resolved ref to the global layer and
// updates the in-memory settings, so a fresh resolveModel (what a new process
// does on startup) lands on the last-chosen model.
func TestModelSwitchIsSticky(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	resetProviderModelCache()
	prev := loadedSettings
	loadedSettings = &config.Settings{DefaultModel: "onegw/free"}
	t.Cleanup(func() { loadedSettings = prev })
	cfg := &config.Config{Providers: map[string]*config.ProviderConfig{
		"onegw": {Models: []config.ModelConfig{{ID: "free"}, {ID: "xdev"}}},
	}}

	persistDefaultModel("onegw/xdev")

	if got, _ := config.Get(config.GlobalSettingsPath(), "defaultModel"); got != "onegw/xdev" {
		t.Fatalf("defaultModel on disk = %q, want onegw/xdev", got)
	}
	if _, err := config.LoadSettings(t.TempDir(), nil); err != nil {
		t.Fatalf("the file a switch wrote must load: %v", err)
	}
	// Startup resolution: no flag, no env — the persisted value must win.
	t.Setenv("XDEV_MODEL", "")
	got, _, err := resolveModel("", cfg, lastSettings())
	if err != nil {
		t.Fatal(err)
	}
	if got != "onegw/xdev" {
		t.Fatalf("next start resolved %q, want the last selected onegw/xdev", got)
	}
}
