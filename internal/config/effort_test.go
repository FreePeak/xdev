package config

import "testing"

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

func TestIsEffort(t *testing.T) {
	for _, e := range EffortLevels {
		if !IsEffort(e) {
			t.Fatalf("%q must be a known effort", e)
		}
	}
	if IsEffort("ultra") {
		t.Fatal("unknown effort accepted")
	}
}
