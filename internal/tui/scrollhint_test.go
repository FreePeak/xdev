package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// dividerRow returns the composer's info divider (the ╰─ model ─╯ row) from a
// rendered screen: the bottom rows are the status row, the divider, and the
// prompt above it — so the divider is the second line counting back from the
// end of what was actually drawn; TrimRight drops blank rows, so instead of a
// fixed index the divider is the last row that opens a box corner.
func dividerRow(t *testing.T, scr tcell.SimulationScreen) string {
	t.Helper()
	rows := strings.Split(strings.TrimRight(screenText(scr), "\n"), "\n")
	if len(rows) < 3 {
		t.Fatalf("screen too short to have a composer: %q", strings.Join(rows, "|"))
	}
	for i := len(rows) - 1; i >= 0; i-- {
		if strings.Contains(rows[i], "╰") {
			return rows[i]
		}
	}
	t.Fatalf("no composer divider on screen: %q", strings.Join(rows, "|"))
	return ""
}

// TestNoScrollCountOnTheDivider pins where the scroll position is reported: the
// transcript's own "↓ n new" chip and nothing else. The ▲n▼n hint that used to
// ride the composer's divider was a second copy of the same number — in the
// one box the user is reading their own words in — and the "draft ▲n" beside it
// was a third, for a box that has always windowed itself silently. Content rows
// and chrome each keep their own job: the chip is the position report, the
// divider is the model line.
func TestNoScrollCountOnTheDivider(t *testing.T) {
	app, scr := newTestApp(t, 100, 24)
	app.AddSystemBlock(strings.Repeat("line\n", 40)) // guarantees rows hidden above
	app.draw()

	rows := strings.Split(strings.TrimRight(screenText(scr), "\n"), "\n")
	if len(rows) == 0 {
		t.Fatal("nothing drawn")
	}
	// Row 0 is transcript content, never chrome: neither the old y=0 hint nor
	// any replacement may eat it.
	if strings.Contains(rows[0], "▲") || strings.Contains(rows[0], "▼") {
		t.Fatalf("scroll hint overwrote the first transcript row: %q", rows[0])
	}
	if !strings.Contains(rows[0], "line") {
		t.Fatalf("the first transcript row lost its content: %q", rows[0])
	}
	divider := dividerRow(t, scr)
	if !strings.Contains(divider, "╰") {
		t.Fatalf("expected the info divider, got %q", divider)
	}
	if strings.Contains(divider, "▲") || strings.Contains(divider, "▼") {
		t.Fatalf("the divider carries a scroll count again: %q", divider)
	}
	// The model line keeps its own budget whole: dropping the hint must not
	// have been paid for out of the model name.
	if !strings.Contains(divider, "test/free") {
		t.Fatalf("model name lost when the hint went: %q", divider)
	}
}

// TestNoStaleCountAfterClear: /clear must not leave the last frame's count on
// screen. With the hint gone from the divider there is nothing to go stale, so
// this pins the chip instead — the one report left — clearing with the tail.
func TestNoStaleCountAfterClear(t *testing.T) {
	app, scr := scrolledUp(t) // a transcript with rows hidden below the tail
	app.mu.Lock()
	jumped := app.jump.w != 0
	app.mu.Unlock()
	if !jumped {
		t.Fatal("the fixture must hide rows below before it can clear them")
	}

	// Reset is the real /clear: it drops the blocks AND the scroll model's
	// position, so nothing is hidden and nothing has a count to show. The
	// welcome frame that follows is the screen a /clear leaves behind, and it
	// must carry no number from the transcript that was just discarded.
	app.Reset()
	app.draw()
	if strings.Contains(screenText(scr), "▲") || strings.Contains(screenText(scr), "▼") {
		t.Fatalf("stale count painted after /clear:\n%s", screenText(scr))
	}
}
