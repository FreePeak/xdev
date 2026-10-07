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
	cur := config.SessionEffortStandard
	app.SetEffortOps(effortWired(&cur))
	app.AddSystemBlock("ready")
	app.draw()

	if err := app.Effort(""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lastBlock(t, app), "effort: standard") {
		t.Fatalf("bare /effort = %q", lastBlock(t, app))
	}

	if err := app.Effort("lean"); err != nil {
		t.Fatal(err)
	}
	if cur != config.SessionEffortLean {
		t.Fatalf("effort = %q, want lean", cur)
	}
	if b := lastBlock(t, app); !strings.Contains(b, "effort: lean") {
		t.Fatalf("/effort lean = %q", b)
	}

	if err := app.Effort("nope"); err == nil {
		t.Fatal("unknown effort must error")
	}
	if cur != config.SessionEffortLean {
		t.Fatalf("rejected effort changed live to %q", cur)
	}

	app.CycleEffort()
	if cur != config.SessionEffortStandard {
		t.Fatalf("cycle from lean = %q, want standard", cur)
	}
	app.CycleEffort()
	if cur != config.SessionEffortFull {
		t.Fatalf("cycle from standard = %q, want full", cur)
	}
	app.CycleEffort()
	if cur != config.SessionEffortLean {
		t.Fatalf("cycle from full = %q, want lean", cur)
	}
	app.draw()
	if d := dividerRow(t, scr); !strings.Contains(d, "lean") {
		t.Fatalf("divider %q missing effort", d)
	}
}

func TestEffortCycleCoversAllRungs(t *testing.T) {
	if len(config.SessionEffortCycle) != 3 {
		t.Fatalf("cycle = %v", config.SessionEffortCycle)
	}
}
