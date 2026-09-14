package tui

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
)

// rowsOf splits the drawn screen into lines, so a test can name a row by what
// it shows instead of by a layout constant.
func rowsOf(scr tcell.SimulationScreen) []string {
	return strings.Split(strings.TrimRight(screenText(scr), "\n"), "\n")
}

// rowWith returns the first drawn row containing needle, and the SCREEN COLUMN
// of its first cell (rowsOf lines are strings, so a byte index would land past
// any box-drawing rune before it).
func rowWith(t *testing.T, scr tcell.SimulationScreen, needle string) (y, x int) {
	t.Helper()
	for i, ln := range rowsOf(scr) {
		if j := strings.Index(ln, needle); j >= 0 {
			return i, utf8.RuneCountInString(ln[:j])
		}
	}
	t.Fatalf("%q not drawn on any row", needle)
	return -1, -1
}

// rowContaining returns the first drawn line carrying needle, or "" when no
// row does.
func rowContaining(scr tcell.SimulationScreen, needle string) string {
	for _, ln := range rowsOf(scr) {
		if strings.Contains(ln, needle) {
			return ln
		}
	}
	return ""
}

// drag runs the press → drag → release lifecycle over the given cells. The
// caller holds a.mu; draw() is a separate step because it takes the lock
// itself.
func drag(app *App, x0, y0, x1, y1 int) {
	app.handleMouse(tcell.NewEventMouse(x0, y0, tcell.Button1, tcell.ModNone))
	app.handleMouse(tcell.NewEventMouse(x1, y1, tcell.Button1, tcell.ModNone))
	app.handleMouse(tcell.NewEventMouse(x1, y1, tcell.ButtonNone, tcell.ModNone))
}

// contentRow returns the transcript row that rendered text sits on.
func contentRow(t *testing.T, app *App, text string) int {
	t.Helper()
	for i, sr := range app.selRows {
		if sr.text == text {
			return i
		}
	}
	t.Fatalf("row %q not captured in selRows (%v)", text, app.selRows)
	return -1
}

// TestSelectionCopyOnRelease pins the auto-copy contract end to end: press →
// drag → release over transcript rows puts the covered text on the clipboard
// (via tcell's OSC 52 SetClipboard, recorded by the simulation screen) and
// leaves out the accent rail.
func TestSelectionCopyOnRelease(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.AddUserBlock("hello world")
	app.AddSystemBlock("copy me please")
	app.draw()
	y := contentRow(t, app, "copy me please")
	app.mu.Lock()
	drag(app, 3, y, 3+len("copy me")-1, y)
	app.mu.Unlock()

	if got := string(scr.GetClipboardData()); got != "copy me" {
		t.Fatalf("clipboard = %q, want %q", got, "copy me")
	}
}

// TestSelectionSpansRows pins the multi-row shape: a drag from mid-row down
// one line yields the first row's tail, a newline, the next row's head
// (terminal-style linear selection).
func TestSelectionSpansRows(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.AddSystemBlock("alpha\nbravo")
	app.draw()

	y0 := contentRow(t, app, "alpha")
	if app.selRows[y0+1].text != "bravo" {
		t.Fatalf("expected alpha/bravo on consecutive rows, got %v", app.selRows)
	}
	app.mu.Lock()
	drag(app, 5, y0, 5, y0+1) // press on "pha", release on the last "bra" cell
	app.mu.Unlock()

	if got := string(scr.GetClipboardData()); got != "pha\nbra" {
		t.Fatalf("clipboard = %q, want %q", got, "pha\nbra")
	}
}

// TestSelectionCoversRowsOutsideTheTranscript pins the omp-visible contract
// that whatever is on screen can be selected: the composer's info divider is
// chrome, not a transcript row, so its text reaches the clipboard through the
// painted grid.
func TestSelectionCoversRowsOutsideTheTranscript(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.AddSystemBlock("copy me please")
	app.draw()

	y, x := rowWith(t, scr, "test/free")
	app.mu.Lock()
	drag(app, x, y, x+len("test/free")-1, y)
	app.mu.Unlock()

	if got := string(scr.GetClipboardData()); !strings.Contains(got, "test/free") {
		t.Fatalf("clipboard = %q, want the divider's model name", got)
	}
}

// TestSelectionStaysHighlightedAfterRelease pins that the highlight survives
// the release — a copy the user cannot see might as well not have happened —
// and that the next press takes the rectangle over instead of wedging.
func TestSelectionStaysHighlightedAfterRelease(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.AddSystemBlock("copy me please")
	app.draw()

	y := contentRow(t, app, "copy me please")
	app.mu.Lock()
	drag(app, 3, y, 9, y)
	shown := app.selShown
	down := app.selDown
	app.mu.Unlock()
	app.draw()

	if !shown || down {
		t.Fatalf("selShown=%v selDown=%v, want the release held up the highlight", shown, down)
	}
	if _, _, attr := cellStyle(scr, 5, y).Decompose(); attr&tcell.AttrReverse == 0 {
		t.Fatal("released selection is no longer painted")
	}

	// The next press replaces the selection, and the old cells go back to
	// normal video: exactly one highlight is ever on screen.
	app.mu.Lock()
	app.handleMouse(tcell.NewEventMouse(40, y, tcell.Button1, tcell.ModNone))
	app.mu.Unlock()
	app.draw()
	if _, _, attr := cellStyle(scr, 5, y).Decompose(); attr&tcell.AttrReverse != 0 {
		t.Fatal("stale highlight still painted after a new press")
	}
}

// cellStyle is the drawn style of one cell.
func cellStyle(scr tcell.SimulationScreen, x, y int) tcell.Style {
	_, _, st, _ := scr.GetContent(x, y)
	return st
}

// TestClickWithoutDragKeepsClipboard pins the terminal's rule that a click
// clears the selection rather than copying the single cell under it: an empty
// gesture must never overwrite what the user copied before.
func TestClickWithoutDragKeepsClipboard(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.AddSystemBlock("copy me please")
	app.draw()

	y := contentRow(t, app, "copy me please")
	app.mu.Lock()
	drag(app, 3, y, 9, y)
	app.mu.Unlock()
	if got := string(scr.GetClipboardData()); got != "copy me" {
		t.Fatalf("drag clipboard = %q, want %q", got, "copy me")
	}

	// A click, with no drag: nothing copied, nothing highlighted.
	app.mu.Lock()
	drag(app, 40, y, 40, y)
	shown := app.selShown
	app.mu.Unlock()

	if shown {
		t.Fatal("click left a highlight on screen")
	}
	if got := string(scr.GetClipboardData()); got != "copy me" {
		t.Fatalf("click clobbered the clipboard to %q", got)
	}
}

// TestCopyConfirmationRidesTheDivider pins the feedback omp gives for every
// copy path (showStatus "… copied to clipboard"): a gesture that copies says
// so on the composer's info divider, and the message expires on its own.
func TestCopyConfirmationRidesTheDivider(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.AddSystemBlock("copy me please")
	app.draw()

	y := contentRow(t, app, "copy me please")
	app.mu.Lock()
	drag(app, 3, y, 9, y)
	app.mu.Unlock()
	app.draw()

	line := rowContaining(scr, "Copied 7 chars")
	if line == "" {
		t.Fatal("no row carried the copy confirmation")
	}
	if !strings.Contains(line, "test/free") {
		t.Fatalf("confirmation row = %q, want it on the model's divider", line)
	}

	// Past its deadline the notice is gone, and the viewport hint takes the
	// slot back.
	app.mu.Lock()
	app.selNoticeUntil = time.Now().Add(-time.Millisecond)
	expired := app.copyHint()
	app.mu.Unlock()
	if expired != "" {
		t.Fatalf("copyHint = %q after the deadline", expired)
	}
	app.draw()
	if line := rowContaining(scr, "Copied"); line != "" {
		t.Fatalf("confirmation row = %q, want the expired notice gone", line)
	}
}

// TestShiftedDragIsLeftToTheTerminal pins the omp / Claude Code contract:
// while the app holds the mouse, a Shift-dragged selection is the terminal's —
// its native selection and its copy. The app must not start, extend, or finish
// a selection of its own, because its release-time write would clobber the
// terminal's clipboard copy.
func TestShiftedDragIsLeftToTheTerminal(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.AddSystemBlock("copy me please")
	app.draw()

	y := contentRow(t, app, "copy me please")
	app.mu.Lock()
	app.handleMouse(tcell.NewEventMouse(3, y, tcell.Button1, tcell.ModShift))
	app.handleMouse(tcell.NewEventMouse(10, y, tcell.Button1, tcell.ModShift))
	app.handleMouse(tcell.NewEventMouse(10, y, tcell.ButtonNone, tcell.ModShift))
	shown, down := app.selShown, app.selDown
	app.mu.Unlock()

	if shown || down {
		t.Fatal("shifted press started an app selection")
	}
	if got := string(scr.GetClipboardData()); got != "" {
		t.Fatalf("shifted drag wrote %q to the clipboard; the terminal owns it", got)
	}
}

// TestShiftCancelsAnInFlightSelection: holding Shift partway through a plain
// drag hands the gesture to the terminal, so the app drops its highlight
// instead of leaving it stuck on screen (tcell reports the release with
// ModShift too, so the release arm never runs).
func TestShiftCancelsAnInFlightSelection(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.AddSystemBlock("copy me please")
	app.draw()

	y := contentRow(t, app, "copy me please")
	app.mu.Lock()
	app.handleMouse(tcell.NewEventMouse(3, y, tcell.Button1, tcell.ModNone))
	app.handleMouse(tcell.NewEventMouse(10, y, tcell.Button1, tcell.ModNone))
	if !app.selDown {
		app.mu.Unlock()
		t.Fatal("plain drag did not start a selection")
	}
	// Shift arrives mid-gesture (terminal-native selection takes over).
	app.handleMouse(tcell.NewEventMouse(10, y, tcell.Button1, tcell.ModShift))
	app.handleMouse(tcell.NewEventMouse(10, y, tcell.ButtonNone, tcell.ModShift))
	shown, down := app.selShown, app.selDown
	app.mu.Unlock()

	if shown || down {
		t.Fatal("selection still active after the gesture went native")
	}
	if got := string(scr.GetClipboardData()); got != "" {
		t.Fatalf("clipboard = %q, want untouched (the terminal owns the copy)", got)
	}
}
