package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// The reported bug, in one line: paste a large block and the composer shows
// NOTHING — yet Enter still sends all of it. Root cause: the box grew with the
// draft's wrapped rows without a ceiling, so its top edge climbed above row 0
// and drawComposer bailed on yTop < 1 — the composer was not painted at all
// while every byte of the draft sat in the buffer. The fix pages the box to
// the room the screen can give it (composerBudget) and scrolls that window
// with the cursor.

func TestLargePasteStaysOnScreen(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	app.AddSystemBlock("ready") // a transcript: the real session layout
	draft := strings.Repeat("this is a long pasted line that has to wrap on screen\n", 40)
	app.handlePaste(draft)
	app.draw()

	if app.ed.Text() != draft {
		t.Fatalf("buffer lost paste bytes: %d vs %d runes",
			len([]rune(app.ed.Text())), len([]rune(draft)))
	}
	lines, curRow, _ := app.composerInputLines()
	if len(lines) == 0 {
		t.Fatal("composer painted no input rows")
	}
	if curRow < 0 || curRow >= len(lines) {
		t.Fatalf("cursor row %d is outside the painted window (%d rows)", curRow, len(lines))
	}
	// A paste lands the cursor at the buffer end, so the window must show the
	// draft's TAIL — the text the user is about to send.
	if !strings.Contains(strings.Join(lines, "\n"), "wrap on screen") {
		t.Fatalf("painted rows do not reach the pasted tail:\n%q", lines)
	}
	text := screenText(scr)
	if !strings.Contains(text, "pasted line") {
		t.Fatalf("pasted draft is not on screen:\n%s", text)
	}
	if !strings.Contains(text, "draft ▲") {
		t.Fatalf("a paged composer must say rows are hidden:\n%s", text)
	}
}

// The invariant the bug broke: the box's top border always sits on screen, for
// any draft size. This held at 90x30 for a few rows and failed at ~28.
func TestComposerTopNeverLeavesTheScreen(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	app.AddSystemBlock("ready")
	max := app.composerBudget() + 2 // painted rows + top border + divider
	for _, n := range []int{1, 2, max - 3, max - 2, max - 1, max, max + 1, 40, 200, 5000} {
		setDraft(&app.ed, strings.Repeat("l\n", n), 2*n)
		if got := app.composerRows(); got > max {
			t.Fatalf("n=%d: composerRows = %d, want ≤ %d", n, got, max)
		}
		if yTop := app.height - 1 - app.composerRows(); yTop < 1 {
			t.Fatalf("n=%d: composer top at y=%d leaves the screen", n, yTop)
		}
	}
}

// Arrows walk the whole draft while the painted window scrolls with the
// cursor: walking off either edge never puts the cursor in a row the box does
// not paint, and history recall still happens only at the FIRST visual row.
// (A long single line is used because recall refuses a draft holding hard
// newlines — that contract is TestArrowsWalkHardNewlineRows' job.)
func TestArrowsScrollThePagedComposer(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	app.AddSystemBlock("ready")
	long := strings.Repeat("x", 3000) // ~33 visual rows against a 24-row budget
	setDraft(&app.ed, long, len([]rune(long)))
	app.ed.PushHistory("older prompt")

	_, startRow, _ := app.composerInputLines()
	if startRow < 23 {
		t.Fatalf("cursor at the buffer end should sit on the window's last row, got %d", startRow)
	}
	for i := range 32 {
		app.handleKey(tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone))
		lines, curRow, _ := app.composerInputLines()
		if curRow < 0 || curRow >= len(lines) {
			t.Fatalf("Up #%d put the cursor outside the window: row %d of %d", i, curRow, len(lines))
		}
		if app.ed.Text() != long {
			t.Fatalf("Up #%d clobbered the draft", i)
		}
	}
	if _, row, _ := app.composerInputLines(); row != 0 {
		t.Fatalf("after walking to the top, cursor row = %d, want 0", row)
	}
	app.handleKey(tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone))
	if app.ed.Text() != "older prompt" {
		t.Fatalf("Up at the first visual row should recall history, got %q", app.ed.Text())
	}
	// Down past the newest entry returns the whole draft, and the window
	// follows it back to the painted tail.
	app.handleKey(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	if app.ed.Text() != long {
		t.Fatalf("Down lost the draft: %d runes", len([]rune(app.ed.Text())))
	}
	lines, curRow, _ := app.composerInputLines()
	if curRow != len(lines)-1 {
		t.Fatalf("back at the tail, cursor row = %d of %d painted", curRow, len(lines))
	}
}

// rowWindow geometry: everything fits ⇒ everything paints; otherwise the
// window holds the cursor row and never runs off either end of the draft.
func TestRowWindowGeometry(t *testing.T) {
	cases := []struct{ n, cur, budget, lo, hi int }{
		{3, 1, 10, 0, 3},     // fits
		{30, 29, 10, 20, 30}, // cursor at the end: paint its tail
		{30, 0, 10, 0, 10},   // cursor at the start: paint the head
		{30, 14, 10, 5, 15},  // middle: cursor on the last painted row
		{30, 14, 1, 14, 15},  // a one-row budget still follows the cursor
		{30, 14, 0, 14, 15},  // a nonsense budget clamps to one row
	}
	for _, c := range cases {
		lo, hi := rowWindow(c.n, c.cur, c.budget)
		if lo != c.lo || hi != c.hi {
			t.Errorf("rowWindow(%d,%d,%d) = (%d,%d), want (%d,%d)",
				c.n, c.cur, c.budget, lo, hi, c.lo, c.hi)
		}
		if hi-lo > max(c.budget, 1) || lo < 0 || hi > c.n {
			t.Errorf("rowWindow(%d,%d,%d) = (%d,%d): window out of bounds",
				c.n, c.cur, c.budget, lo, hi)
		}
	}
}

// draftHint is the only proof a paged box holds more than it paints.
func TestDraftHint(t *testing.T) {
	for c, want := range map[[2]int]string{
		{0, 0}: "",
		{3, 0}: "draft ▲3",
		{0, 7}: "draft ▼7",
		{3, 2}: "draft ▲3 ▼2",
	} {
		if got := draftHint(c[0], c[1]); got != want {
			t.Errorf("draftHint(%d,%d) = %q, want %q", c[0], c[1], got, want)
		}
	}
}
