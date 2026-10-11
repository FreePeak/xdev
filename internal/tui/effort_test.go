package tui

import (
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/config"
)

func effortWired(cur *string) *EffortOps {
	return &EffortOps{
		Current: func() string { return *cur },
		Set:     func(e string) error { *cur = e; return nil },
	}
}

func TestEffortCommandAndCycle(t *testing.T) {
	app, scr := newTestApp(t, 100, 24)
	cur := config.SessionEffortDefault
	app.SetEffortOps(effortWired(&cur))
	app.AddSystemBlock("ready")
	app.draw()

	if err := app.Effort(""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lastBlock(t, app), "effort: medium") {
		t.Fatalf("bare /effort = %q", lastBlock(t, app))
	}

	if err := app.Effort("low"); err != nil {
		t.Fatal(err)
	}
	if cur != config.SessionEffortLow {
		t.Fatalf("effort = %q, want low", cur)
	}
	if b := lastBlock(t, app); !strings.Contains(b, "effort: low") {
		t.Fatalf("/effort low = %q", b)
	}

	// The pre-ladder spelling is an alias, not a writeable rung: it normalizes
	// for a stored value but /effort rejects it, so the typed vocabulary and
	// the stored one cannot drift apart.
	if err := app.Effort("lean"); err == nil {
		t.Fatal("an alias must not be settable through /effort")
	}
	if err := app.Effort("nope"); err == nil {
		t.Fatal("unknown effort must error")
	}
	if cur != config.SessionEffortLow {
		t.Fatalf("rejected effort changed live to %q", cur)
	}

	for _, want := range []string{
		config.SessionEffortMedium, config.SessionEffortHigh,
		config.SessionEffortXHigh, config.SessionEffortMax, config.SessionEffortLow,
	} {
		app.CycleEffort()
		if cur != want {
			t.Fatalf("cycle from %q = %q, want %q", cur, want, cur)
		}
	}
	app.draw()
	if d := dividerRow(t, scr); !strings.Contains(d, "low") {
		t.Fatalf("divider %q missing effort", d)
	}
}

func TestEffortCycleCoversTheWholeLadder(t *testing.T) {
	if len(config.SessionEffortCycle) != 5 {
		t.Fatalf("cycle = %v", config.SessionEffortCycle)
	}
	for _, e := range config.SessionEffortLevels {
		found := false
		for _, c := range config.SessionEffortCycle {
			if c == e {
				found = true
			}
		}
		if !found {
			t.Fatalf("%q missing from cycle", e)
		}
	}
}

// TestEffortRungWearsItsOwnInk pins that the top rungs are visibly a rung of
// the same ladder /thinking paints: a rung whose token equals another rung's
// would read as the same amount of effort.
func TestEffortRungWearsItsOwnInk(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)
	cur := config.SessionEffortMedium
	app.SetEffortOps(effortWired(&cur))
	seen := map[string]string{}
	for _, rung := range config.SessionEffortLevels {
		cur = rung
		tok := app.effortToken()
		if tok == "" {
			t.Fatalf("rung %q paints nothing", rung)
		}
		if prev, ok := seen[tok]; ok {
			t.Fatalf("rungs %q and %q share the ink %q", prev, rung, tok)
		}
		seen[tok] = rung
	}
}

// TestDividerDropsADuplicateEffortRung pins the bug the CC ladder introduced
// on the composer divider: thinking and session effort share low|medium|high|
// xhigh|max, so painting both labels side by side produced "model · high · high"
// whenever the two dials agreed. One amount of effort is one word; the chip
// only ever appears once (TestDividerFoldsTheTwoDialsIntoOneRung covers the
// two rungs disagreeing).
func TestDividerDropsADuplicateEffortRung(t *testing.T) {
	app, scr := newTestApp(t, 100, 24)
	app.AddSystemBlock("ready")
	think := "high"
	effort := "high"
	app.SetThinkingOps(thinkWired(&think))
	app.SetEffortOps(effortWired(&effort))
	app.draw()
	div := dividerRow(t, scr)
	if !strings.Contains(div, "test/free · high") {
		t.Fatalf("divider %q must still carry the shared rung once", div)
	}
	if strings.Count(div, "high") != 1 {
		t.Fatalf("divider %q painted the same rung twice", div)
	}
}

// TestDividerFoldsTheTwoDialsIntoOneRung is the case #614 left open: with
// `thinking: high` pinned AND `effort: max`, the divider painted
// "test/free · high · max" — two rungs of the same ladder on one request.
// One request has one reasoning posture, so the higher rung wins and the
// lower word is gone.
func TestDividerFoldsTheTwoDialsIntoOneRung(t *testing.T) {
	app, scr := newTestApp(t, 100, 24)
	app.AddSystemBlock("ready")
	think := "high"
	effort := "max"
	app.SetThinkingOps(thinkWired(&think))
	app.SetEffortOps(effortWired(&effort))
	app.draw()
	div := dividerRow(t, scr)
	if !strings.Contains(div, "test/free · max") {
		t.Fatalf("divider %q must carry the higher rung once", div)
	}
	if strings.Count(div, "high") != 0 || strings.Count(div, "·") != 1 {
		t.Fatalf("divider %q painted two rungs of one ladder", div)
	}

	// The other direction folds the same way: an effort rung BELOW the pinned
	// thinking level still leaves one word — the one the request actually
	// spends, which is the pinned thinking level.
	effort = "low"
	app.draw()
	div = dividerRow(t, scr)
	if div != "test/free · high" && !strings.Contains(div, "test/free · high") {
		t.Fatalf("divider %q must keep only the higher rung", div)
	}
	if strings.Contains(div, "low") {
		t.Fatalf("divider %q kept a rung the request does not spend", div)
	}

	// "auto" and "off" are not rungs, so they are never folded away: auto
	// beside an effort rung still says which level the auto model will take,
	// and off is its own statement next to any chip.
	think = "auto"
	effort = "max"
	app.draw()
	div = dividerRow(t, scr)
	if !strings.Contains(div, "auto · max") {
		t.Fatalf("divider %q must keep auto beside the rung it pins", div)
	}
	think = "off"
	app.draw()
	div = dividerRow(t, scr)
	if !strings.Contains(div, "off · max") {
		t.Fatalf("divider %q must keep off beside the effort chip", div)
	}
}
