package config

import "testing"

func TestNormalizeSessionEffort(t *testing.T) {
	cases := map[string]string{
		"":         SessionEffortStandard,
		"standard": SessionEffortStandard,
		"LEAN":     SessionEffortLean,
		"simple":   SessionEffortLean,
		"full":     SessionEffortFull,
		"omp":      SessionEffortFull,
		"nope":     SessionEffortStandard,
	}
	for in, want := range cases {
		if got := NormalizeSessionEffort(in); got != want {
			t.Errorf("NormalizeSessionEffort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsSessionEffort(t *testing.T) {
	if !IsSessionEffort("lean") || !IsSessionEffort("FULL") {
		t.Fatal("known rungs must pass")
	}
	if IsSessionEffort("") || IsSessionEffort("nope") || IsSessionEffort("simple") {
		// aliases normalize but are not the closed write vocabulary
		t.Fatal("aliases and empty must fail the closed check used by /effort set")
	}
}

func TestSessionEffortThinking(t *testing.T) {
	if got := SessionEffortThinking("lean"); got != "low" {
		t.Fatalf("lean → %q, want low", got)
	}
	if got := SessionEffortThinking("full"); got != "high" {
		t.Fatalf("full → %q, want high", got)
	}
	if got := SessionEffortThinking("standard"); got != "auto" {
		t.Fatalf("standard → %q, want auto", got)
	}
}

func TestSessionEffortCycleCoversEveryRung(t *testing.T) {
	if len(SessionEffortCycle) != 3 {
		t.Fatalf("cycle = %v", SessionEffortCycle)
	}
	for _, e := range SessionEffortLevels {
		found := false
		for _, c := range SessionEffortCycle {
			if c == e {
				found = true
			}
		}
		if !found {
			t.Fatalf("%q missing from cycle", e)
		}
	}
}
