package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// scrolledUp parks the viewport off the tail the way a wheel or PgUp does, and
// returns the app and screen with a chip-sized transcript behind it.
func scrolledUp(t *testing.T) (*App, tcell.SimulationScreen) {
	t.Helper()
	app, scr := newTestApp(t, 100, 24)
	longTranscript(t, app, 60)
	app.mu.Lock()
	app.sm.ScrollUp(12, app.totalLinesLocked(), app.viewportLinesLocked())
	app.mu.Unlock()
	app.draw()
	return app, scr
}

// TestJumpChipCountsTheHiddenRows: the button is the scroll hint's missing
// half — the "▲n▼n" says a jump is possible, the chip says how much and does it.
// It appears only while rows are hidden below, and it goes away at the tail.
func TestJumpChipCountsTheHiddenRows(t *testing.T) {
	app, scr := scrolledUp(t)

	app.mu.Lock()
	jump, down := app.jump, app.sm.offset
	app.mu.Unlock()
	if down <= 0 {
		t.Fatalf("fixture did not scroll off the tail (offset %d)", down)
	}
	if jump.w == 0 || jump.h == 0 {
		t.Fatal("no jump chip painted while scrolled up")
	}
	if !strings.Contains(screenText(scr), fmt.Sprintf("↓ %d new", down)) {
		t.Fatalf("the chip does not name the %d hidden rows:\n%s", down, screenText(scr))
	}

	// At the tail there is nothing to jump to, so no chip and no hit area.
	app.mu.Lock()
	app.sm.Bottom()
	app.mu.Unlock()
	app.draw()
	app.mu.Lock()
	jump = app.jump
	app.mu.Unlock()
	if jump.w != 0 {
		t.Fatalf("a chip stayed on screen at the tail: %+v", jump)
	}
	if strings.Contains(screenText(scr), "new") && strings.Contains(screenText(scr), "↓") {
		t.Fatalf("stale chip painted at the tail:\n%s", screenText(scr))
	}
}

// TestJumpChipClickReturnsToTheTail: the press spends itself on the button. It
// must not also anchor a selection on the transcript row the chip covers.
func TestJumpChipClickReturnsToTheTail(t *testing.T) {
	app, _ := scrolledUp(t)
	app.mu.Lock()
	jump := app.jump
	app.mu.Unlock()

	app.mu.Lock()
	press(app, jump.x+1, jump.y)
	app.mu.Unlock()

	app.mu.Lock()
	offset, following, down := app.sm.offset, app.sm.Following(), app.selDown
	app.mu.Unlock()
	if offset != 0 || !following {
		t.Fatalf("the click did not jump to the tail: offset=%d following=%v", offset, following)
	}
	if down {
		t.Fatal("the click anchored a selection behind the chip")
	}

	// The frame the click asked for must show the tail, chip gone.
	app.draw()
	app.mu.Lock()
	jump = app.jump
	app.mu.Unlock()
	if jump.w != 0 {
		t.Fatalf("the chip survived the jump: %+v", jump)
	}
}

// TestJumpChipNeverCoversTheTail: with nothing hidden below there is no button,
// so a click on the very row a chip would have used still reaches the text.
func TestJumpChipNeverCoversTheTail(t *testing.T) {
	app, _ := scrolledUp(t)
	app.mu.Lock()
	app.sm.Bottom()
	app.mu.Unlock()
	app.draw()

	app.mu.Lock()
	press(app, 10, app.height-3)
	app.mu.Unlock()

	app.mu.Lock()
	selDown := app.selDown
	app.mu.Unlock()
	if !selDown {
		t.Fatal("a click on the transcript below the tail did not start a selection")
	}
}
