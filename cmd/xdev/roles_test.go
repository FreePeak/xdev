package main

import (
	"testing"

	"github.com/FreePeak/xdev/internal/config"
	"strings"
)

func TestChildModelRole(t *testing.T) {
	same := &config.Settings{ModelRoles: map[string]string{"task": "onegw/dev"}}
	if got := childModel(same, "onegw", "free"); got != "dev" {
		t.Fatalf("@task same-provider = %q, want dev", got)
	}
	// Cross-provider roles would need a different client; keep the parent's.
	cross := &config.Settings{ModelRoles: map[string]string{"task": "other/dev"}}
	if got := childModel(cross, "onegw", "free"); got != "free" {
		t.Fatalf("@task cross-provider = %q, want free", got)
	}
	// No role configured at all → parent model, no error.
	if got := childModel(&config.Settings{}, "onegw", "free"); got != "free" {
		t.Fatalf("no roles = %q, want free", got)
	}
	if got := childModel(nil, "onegw", "free"); got != "free" {
		t.Fatalf("nil settings = %q, want free", got)
	}
}

func TestResolveModelPrecedence(t *testing.T) {
	providerModelCache = map[string][]config.ModelConfig{}
	cfg := &config.Config{Providers: map[string]*config.ProviderConfig{
		// "flag" is in the catalog: resolveModel now validates literal refs
		// against the merged catalog so a typo fails here instead of 404ing
		// on the next turn.
		"onegw": {Models: []config.ModelConfig{{ID: "yml-default"}, {ID: "flag"}}},
	}}
	s := &config.Settings{
		DefaultModel: "onegw/from-settings",
		ModelRoles:   map[string]string{"smol": "onegw/tiny"},
	}
	t.Setenv("XDEV_MODEL", "onegw/from-env")
	// Explicit (flag/settings-merged) beats env.
	if got, _, err := resolveModel("onegw/flag", cfg, s); err != nil || got != "onegw/flag" {
		t.Fatalf("explicit = %q err=%v", got, err)
	}
	// Empty explicit still resolves the role form.
	if got, eff, err := resolveModel("@smol:low", cfg, s); err != nil || got != "onegw/tiny" || eff != "low" {
		t.Fatalf("role = %q/%q err=%v", got, eff, err)
	}
	// Bogus role surfaces rather than silently using the default.
	if _, _, err := resolveModel("@nope", cfg, s); err == nil {
		t.Fatal("unknown role must error")
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
	providerModelCache = map[string][]config.ModelConfig{}
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
		{name: "unknown suffix stays in the id", in: "onegw/dev:turbo", wantErr: "unknown model"},
		{name: "unknown bare id", in: "nope", wantErr: "unknown model"},
		{name: "ambiguous bare id", in: "dup", wantErr: "matches a/dup, b/dup"},
		{name: "unknown id for a known provider", in: "onegw/nope", wantErr: "configured: free, dev"},
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
	s := &config.Settings{ModelRoles: map[string]string{"smol": "onegw/tiny"}, ModelRolesEffort: map[string]string{"smol": "high"}}
	_, effort, err := resolveModel("@smol", &config.Config{}, s)
	if err != nil {
		t.Fatal(err)
	}
	if effort != "high" {
		t.Fatalf("pinned role effort lost: %q", effort)
	}
	bud := effortBudget(effort)
	if bud == nil || bud.Tokens != config.EffortTokens["high"] {
		t.Fatalf("budget = %+v", bud)
	}
	if effortBudget("") != nil || effortBudget("nonsense") != nil {
		t.Fatal("no/unknown effort must mean no thinking requested")
	}
}
