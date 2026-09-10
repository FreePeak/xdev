package main

import (
	"testing"

	"github.com/FreePeak/xdev/internal/config"
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
	cfg := &config.Config{Providers: map[string]*config.ProviderConfig{
		"onegw": {Models: []config.ModelConfig{{ID: "yml-default"}}},
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
