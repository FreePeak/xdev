package config

import (
	"strings"
	"testing"
)

func roleSettings() *Settings {
	return &Settings{
		DefaultModel: "onegw/free",
		ModelRoles: map[string]string{
			"default": "onegw/free",
			"smol":    "onegw/tiny",
			"slow":    "onegw/dev",
		},
		ModelRolesEffort: map[string]string{"slow": "medium"},
	}
}

func TestResolveModelRef(t *testing.T) {
	s := roleSettings()
	tests := []struct {
		in     string
		want   RoleRef
		errSub string
	}{
		{in: "onegw/other", want: RoleRef{Ref: "onegw/other"}},
		{in: "", want: RoleRef{Ref: "onegw/free"}},
		{in: "@smol", want: RoleRef{Ref: "onegw/tiny", Role: "smol"}},
		{in: "@slow:high", want: RoleRef{Ref: "onegw/dev", Role: "slow", Effort: "high"}},
		// A pinned per-role effort applies when no suffix overrides it.
		{in: "@slow", want: RoleRef{Ref: "onegw/dev", Role: "slow", Effort: "medium"}},
		{in: "@vision", errSub: "unknown role @vision"},
		{in: "@slow:ultra", errSub: "unknown effort"},
		{in: "@", errSub: "empty role"},
	}
	for _, tc := range tests {
		got, err := ResolveModelRef(s, tc.in)
		if tc.errSub != "" {
			if err == nil || !strings.Contains(err.Error(), tc.errSub) {
				t.Errorf("ResolveModelRef(%q): err=%v, want %q", tc.in, err, tc.errSub)
			}
			continue
		}
		if err != nil {
			t.Errorf("ResolveModelRef(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ResolveModelRef(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

// TestResolveUnknownRoleIsNotSilent: a typo must not quietly run on the
// default model — that is the whole reason the error names what IS
// configured.
func TestResolveUnknownRoleIsNotSilent(t *testing.T) {
	_, err := ResolveModelRef(roleSettings(), "@nope")
	if err == nil {
		t.Fatal("unknown role must fail")
	}
	for _, want := range []string{"smol", "slow", "default"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error must list configured roles, missing %q: %v", want, err)
		}
	}
}

func TestResolveNoModelAnywhere(t *testing.T) {
	_, err := ResolveModelRef(&Settings{}, "")
	if err == nil || !strings.Contains(err.Error(), "no model configured") {
		t.Fatalf("bare install must explain itself, got %v", err)
	}
}

func TestEffortBudget(t *testing.T) {
	if n, ok := EffortBudget("high"); !ok || n != EffortTokens["high"] {
		t.Fatalf("high = %d ok=%v", n, ok)
	}
	if n, ok := EffortBudget("minimal"); !ok || n != 0 {
		t.Fatalf("minimal must resolve to a zero budget, got %d ok=%v", n, ok)
	}
	if _, ok := EffortBudget(""); ok {
		t.Fatal("empty effort must mean no thinking requested")
	}
	if _, ok := EffortBudget("bogus"); ok {
		t.Fatal("unknown effort must not resolve")
	}
}

func TestRoleNamesAreTheCanonicalSet(t *testing.T) {
	want := []string{"default", "smol", "slow", "vision", "plan", "commit", "tiny", "task", "advisor"}
	if len(RoleNames) != len(want) {
		t.Fatalf("RoleNames = %v", RoleNames)
	}
	for i, r := range want {
		if RoleNames[i] != r || !IsKnownRole(r) {
			t.Fatalf("role %q missing from %v", r, RoleNames)
		}
	}
	if IsKnownRole("bogus") {
		t.Fatal("unknown role accepted")
	}
}

// TestResolveChainedAliases pins the role grammar: a role may point at
// another role, effort follows the chain, and cycles are reported rather
// than resolved by iteration luck.
func TestResolveChainedAliases(t *testing.T) {
	s := &Settings{ModelRoles: map[string]string{
		"plan":    "@slow",
		"slow":    "onegw/dev",
		"loop":    "@loop",
		"mutual":  "@pair",
		"pair":    "@mutual",
		"deep":    "@deeper:high",
		"deeper":  "@deepest",
		"deepest": "onegw/final",
	}}
	if got, err := ResolveModelRef(s, "@plan"); err != nil || got.Ref != "onegw/dev" || got.Role != "plan" {
		t.Fatalf("chained alias = %+v err=%v", got, err)
	}
	// A :effort mid-chain applies.
	if got, err := ResolveModelRef(s, "@deep"); err != nil || got.Ref != "onegw/final" || got.Effort != "high" {
		t.Fatalf("chained effort = %+v err=%v", got, err)
	}
	for _, bad := range []string{"@loop", "@mutual"} {
		if _, err := ResolveModelRef(s, bad); err == nil || !strings.Contains(err.Error(), "cycle") {
			t.Errorf("%s: cycle must be reported, got %v", bad, err)
		}
	}
}

func TestResolveRoleWithInlineEffortBeatsPinned(t *testing.T) {
	s := &Settings{
		ModelRoles:       map[string]string{"slow": "onegw/dev"},
		ModelRolesEffort: map[string]string{"slow": "medium"},
	}
	if got, _ := ResolveModelRef(s, "@slow:low"); got.Effort != "low" {
		t.Fatalf("explicit :effort must override the pinned one, got %q", got.Effort)
	}
}
