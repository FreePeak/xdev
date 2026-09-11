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
	if app.composerRows() != 4 { // 2 input rows + top border + divider
		t.Fatalf("composerRows = %d, want 4", app.composerRows())
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
	avail := app.width - 7
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
