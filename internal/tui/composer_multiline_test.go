package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// setDraft installs draft text + cursor directly (in-package test hook:
// the editor exposes no setter).
func setDraft(e *Editor, text string, cur int) {
	e.buf = []rune(text)
	e.cur = cur
}

// Ctrl+J / Alt+Enter insert a hard newline; the composer must render it
// as another input row. The bug the user hit: the buffer HAD the newline
// but the box drew a single line, so the text ran together on screen.
func TestCtrlJInsertsNewlineAndComposerRendersRows(t *testing.T) {
	app, _ := newTestApp(t, 90, 30)
	app.ed.Reset()
	for _, r := range "first line" {
		app.ed.HandleKey(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
	}
	if send := app.ed.HandleKey(tcell.NewEventKey(tcell.KeyCtrlJ, 0, tcell.ModNone)); send {
		t.Fatal("Ctrl+J must not send")
	}
	for _, r := range "second line" {
		app.ed.HandleKey(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
	}
	lines, curRow, curCol := app.composerInputLines()
	if len(lines) != 2 || lines[0] != "first line" || lines[1] != "second line" {
		t.Fatalf("lines = %q", lines)
	}
	if curRow != 1 || curCol != len("second line") {
		t.Fatalf("cursor = (%d,%d)", curRow, curCol)
	}
	if app.composerRows() != 3 { // 2 input rows + the info divider (no top border)
		t.Fatalf("composerRows = %d, want 3", app.composerRows())
	}
}

func TestComposerCursorMidSecondRow(t *testing.T) {
	app, _ := newTestApp(t, 90, 30)
	setDraft(&app.ed, "first line\nsecond line", len([]rune("first line\nsec")))
	lines, curRow, curCol := app.composerInputLines()
	if len(lines) != 2 || curRow != 1 || curCol != 3 {
		t.Fatalf("mid-row cursor = (%d,%d) lines=%q", curRow, curCol, lines)
	}
}

// A long single line wraps at the available width instead of running off
// the box.
func TestComposerWrapsLongLine(t *testing.T) {
	app, _ := newTestApp(t, 40, 24)
	long := strings.Repeat("x", 100)
	setDraft(&app.ed, long, 0)
	lines, _, _ := app.composerInputLines()
	if len(lines) < 3 {
		t.Fatalf("long line did not wrap: %d row(s)", len(lines))
	}
	avail := app.composerAvail()
	for i, l := range lines {
		if width(l) > avail {
			t.Fatalf("row %d width %d exceeds avail %d", i, width(l), avail)
		}
	}
}

// The transcript viewport shrinks by exactly the composer's growth, so a
// multi-line draft never overlaps the scrollback.
func TestViewportShrinksWithComposer(t *testing.T) {
	app, _ := newTestApp(t, 80, 30)
	setDraft(&app.ed, "", 0)
	one := app.viewportLinesLocked()
	setDraft(&app.ed, "a\nb\nc", 0)
	three := app.viewportLinesLocked()
	if one-three != 2 {
		t.Fatalf("viewport delta = %d, want 2 (two extra input rows)", one-three)
	}
}

// The reported bug: with a multi-line draft, Up/Down did nothing — recall()
// refuses newline-bearing buffers and there was no line-motion path. Now the
// arrows walk the painted rows; at the top/bottom row they stay put and the
// draft is never clobbered by history.
func TestArrowsWalkHardNewlineRows(t *testing.T) {
	app, _ := newTestApp(t, 90, 30)
	setDraft(&app.ed, "first line\nsecond line", 22) // cursor after "second line"
	app.handleKey(tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone))
	_, row, col := app.composerInputLines()
	if row != 0 || col != 10 {
		t.Fatalf("Up from row 1 col 11 = (%d,%d), want (0,10) (clamped to row 0 length)", row, col)
	}
	// At the first row Up must not recall: the draft holds newlines.
	app.ed.PushHistory("older prompt")
	app.handleKey(tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone))
	if app.ed.Text() != "first line\nsecond line" {
		t.Fatalf("Up at top row clobbered the multi-line draft: %q", app.ed.Text())
	}
	// Down walks back and stops at the last row for the same reason.
	app.handleKey(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	_, row, col = app.composerInputLines()
	if row != 1 || col != 11 {
		t.Fatalf("Down = (%d,%d), want (1,11)", row, col)
	}
	app.handleKey(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	if app.ed.Text() != "first line\nsecond line" || app.ed.cur != 22 {
		t.Fatalf("Down at bottom row moved the buffer: %q cur=%d", app.ed.Text(), app.ed.cur)
	}
}

// A long single line that WRAPS is multi-row too: the arrows walk the
// visual rows, preserving the cell column, and history recall returns only
// at the first/last row — never mid-paragraph.
func TestArrowsWalkWrappedRowsAndRecallAtEdge(t *testing.T) {
	app, _ := newTestApp(t, 40, 24) // avail = 36 → 100 x's cover 3 rows (36/36/28)
	long := strings.Repeat("x", 100)
	setDraft(&app.ed, long, 100)
	app.ed.PushHistory("older prompt")
	app.handleKey(tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone))
	_, row, col := app.composerInputLines()
	if row != 1 || col != 28 {
		t.Fatalf("Up from the last wrapped row = (%d,%d), want (1,28)", row, col)
	}
	// One more row to the top; the edge is the next Up.
	app.handleKey(tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone))
	if app.ed.Text() != long {
		t.Fatalf("recall fired before the edge: %q", app.ed.Text())
	}
	app.handleKey(tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone))
	if app.ed.Text() != "older prompt" {
		t.Fatalf("Up at first wrapped row should recall history, got %q", app.ed.Text())
	}
}
