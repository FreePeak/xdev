package config

import (
	"reflect"
	"testing"
)

func TestNormalizeSessionEffort(t *testing.T) {
	cases := map[string]string{
		"":       SessionEffortDefault,
		"medium": SessionEffortMedium,
		"LOW":    SessionEffortLow,
		"high":   SessionEffortHigh,
		"xhigh":  SessionEffortXHigh,
		"max":    SessionEffortMax,
		"nope":   SessionEffortDefault,
		// The pre-ladder spellings fold onto their nearest rung so a stored
		// `effort: full` keeps resolving after the ladder change.
		"lean":     SessionEffortLow,
		"simple":   SessionEffortLow,
		"minimal":  SessionEffortLow,
		"standard": SessionEffortMedium,
		"full":     SessionEffortHigh,
		"omp":      SessionEffortHigh,
		"complex":  SessionEffortHigh,
	}
	for in, want := range cases {
		if got := NormalizeSessionEffort(in); got != want {
			t.Errorf("NormalizeSessionEffort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsSessionEffort(t *testing.T) {
	if !IsSessionEffort("low") || !IsSessionEffort("MAX") {
		t.Fatal("known rungs must pass")
	}
	if IsSessionEffort("") || IsSessionEffort("nope") {
		t.Fatal("empty and unknown must fail the closed check used by /effort set")
	}
	// Aliases normalize but are NOT the closed write vocabulary: /effort and
	// --effort take the ladder spelling, so a stored value and a typed value
	// cannot drift apart.
	for _, alias := range []string{"lean", "simple", "standard", "full", "omp", "complex"} {
		if IsSessionEffort(alias) {
			t.Errorf("%q is an alias, not a writeable rung", alias)
		}
	}
}

func TestSessionEffortThinking(t *testing.T) {
	want := map[string]string{
		SessionEffortLow:    "low",
		SessionEffortMedium: "auto",
		SessionEffortHigh:   "high",
		SessionEffortXHigh:  "xhigh",
		SessionEffortMax:    "max",
	}
	for rung, level := range want {
		if got := SessionEffortThinking(rung); got != level {
			t.Errorf("SessionEffortThinking(%q) = %q, want %q", rung, got, level)
		}
	}
	// Every level the effort rung implies must be one /thinking accepts, or
	// the fold would hand applyThinkingFlag a value it rejects.
	for _, rung := range SessionEffortLevels {
		if lv := SessionEffortThinking(rung); lv != "auto" && !isThinkingLevel(lv) {
			t.Errorf("rung %q implies %q, which /thinking does not accept", rung, lv)
		}
	}
}

func isThinkingLevel(v string) bool {
	for _, l := range ThinkingLevels {
		if l == v {
			return true
		}
	}
	return false
}

func TestSessionEffortCycleCoversEveryRung(t *testing.T) {
	if !reflect.DeepEqual(SessionEffortCycle, SessionEffortLevels) {
		t.Fatalf("cycle %v must walk the whole ladder %v", SessionEffortCycle, SessionEffortLevels)
	}
	for _, e := range SessionEffortLevels {
		if !IsSessionEffort(e) {
			t.Fatalf("cycle names %q, which is not a writeable rung", e)
		}
	}
}

func TestSessionEffortBlurbEveryRung(t *testing.T) {
	for _, e := range SessionEffortLevels {
		if SessionEffortBlurb(e) == "" {
			t.Errorf("rung %q has no blurb", e)
		}
	}
}
