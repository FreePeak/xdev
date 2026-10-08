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

// TestEffortLadderIsStrictlyIncreasing pins the property the whole ladder
// rests on: a higher rung must never ask for a smaller budget. Without it
// "xhigh" could silently mean less reasoning than "high" and nothing would
// notice, because every consumer only ever reads one rung.
func TestEffortLadderIsStrictlyIncreasing(t *testing.T) {
	prev, prevName := -1, ""
	for _, e := range EffortLevels {
		n, ok := EffortBudget(e)
		if !ok {
			t.Fatalf("%q is a rung but has no budget", e)
		}
		if n <= prev && prevName != "" {
			t.Fatalf("%s (%d) must ask for more than %s (%d)", e, n, prevName, prev)
		}
		prev, prevName = n, e
	}
	// The top of the ladder is the point of the two extra rungs: if xhigh and
	// max carried high's budget they would be names for the same request.
	if EffortTokens["xhigh"] <= EffortTokens["high"] || EffortTokens["max"] <= EffortTokens["xhigh"] {
		t.Fatalf("xhigh/max must widen the budget: %v", EffortTokens)
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

func TestEffortRank(t *testing.T) {
	for i, e := range EffortLevels {
		if r, ok := EffortRank(e); !ok || r != i {
			t.Fatalf("EffortRank(%q) = (%d,%v), want (%d,true)", e, r, ok, i)
		}
	}
	// Case and padding are folded, because the value arrives from a flag, a
	// settings file and an env var and none of them is canonical.
	wantRank, _ := EffortRank("high")
	if r, ok := EffortRank("  HIGH "); !ok || r != wantRank {
		t.Fatalf("padded/cased rung did not fold: (%d,%v)", r, ok)
	}
	for _, v := range []string{"", "auto", "off", "ultra"} {
		if _, ok := EffortRank(v); ok {
			t.Errorf("EffortRank(%q) must report no rank", v)
		}
	}
}

// TestClampEffort is Claude Code's own rule: a level the active model does not
// support falls back to the highest supported level at or below it. The
// empty-list case is the important one — it is how "this model has no opinion"
// is expressed, and it must NOT clamp, or a gateway-served id would silently
// lose the rung the user chose.
func TestClampEffort(t *testing.T) {
	for _, tc := range []struct {
		name       string
		level      string
		advertised []string
		want       string
	}{
		{name: "no opinion clamps nothing", level: "xhigh", advertised: nil, want: "xhigh"},
		{name: "empty list clamps nothing", level: "max", advertised: []string{}, want: "max"},
		{name: "xhigh on a 4-rung model runs as high", level: "xhigh", advertised: []string{"low", "medium", "high"}, want: "high"},
		{name: "max on a 4-rung model runs as high", level: "max", advertised: []string{"low", "medium", "high"}, want: "high"},
		{name: "an advertised rung is kept", level: "medium", advertised: []string{"low", "medium", "high"}, want: "medium"},
		{name: "the full ladder changes nothing", level: "max", advertised: []string{"low", "medium", "high", "xhigh", "max"}, want: "max"},
		{name: "a one-rung model takes its only rung", level: "max", advertised: []string{"low"}, want: "low"},
		{name: "below the model's floor takes the floor", level: "low", advertised: []string{"medium", "high"}, want: "medium"},
		{name: "an unordered list still finds the highest at or below", level: "high", advertised: []string{"medium", "low"}, want: "medium"},
		{name: "names the ladder does not know are ignored", level: "high", advertised: []string{"ultra"}, want: "high"},
		{name: "off is not a rung — untouched", level: "off", advertised: []string{"low"}, want: "off"},
		{name: "auto is not a rung — untouched", level: "auto", advertised: []string{"low"}, want: "auto"},
		{name: "case is folded on both sides", level: "XHIGH", advertised: []string{"LOW", "HIGH"}, want: "high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClampEffort(tc.level, tc.advertised); got != tc.want {
				t.Fatalf("ClampEffort(%q, %v) = %q, want %q", tc.level, tc.advertised, got, tc.want)
			}
		})
	}
}
