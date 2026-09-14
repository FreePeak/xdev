package tui

import (
	"testing"

	"github.com/gdamore/tcell/v2"
)

// TestSelectionCopyOnRelease pins the auto-copy contract end to end:
// press → drag → release over transcript rows puts the covered text on
// the clipboard (via tcell's OSC 52 SetClipboard, recorded by the
// simulation screen). This is the only reliably testable half of the
// feature — the mouse plumbing itself is exercised through handleKey.
func TestSelectionCopyOnRelease(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.AddUserBlock("hello world")
	app.AddSystemBlock("copy me please")
	app.draw()

	// Locate the system block's rendered row (content starts at col 3).
	y := -1
	for i, sr := range app.selRows {
		if sr.text == "copy me please" {
			y = i
			break
		}
	}
	if y < 0 {
		t.Fatal("system block row not captured in selRows")
	}

	app.mu.Lock()
	app.handleMouse(tcell.NewEventMouse(3, y, tcell.Button1, tcell.ModNone))
	app.handleMouse(tcell.NewEventMouse(3+len("copy me"), y, tcell.Button1, tcell.ModNone))
	app.handleMouse(tcell.NewEventMouse(3, y, tcell.ButtonNone, tcell.ModNone))
	app.mu.Unlock()

	if got := string(scr.GetClipboardData()); got != "copy me" {
		t.Fatalf("clipboard = %q, want %q", got, "copy me")
	}
}

// TestSelectionSpansRows pins the multi-row shape: a drag from mid-row
// down one line yields the first row's tail, a newline, the next row's
// head (terminal-style linear selection).
func TestSelectionSpansRows(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.AddSystemBlock("alpha\nbravo")
	app.draw()

	y0 := -1
	for i, sr := range app.selRows {
		if sr.text == "alpha" {
			y0 = i
			break
		}
	}
	if y0 < 0 || y0+1 >= len(app.selRows) || app.selRows[y0+1].text != "bravo" {
		t.Fatalf("expected alpha/bravo on consecutive rows, got %v", app.selRows)
	}

	app.mu.Lock()
	app.handleMouse(tcell.NewEventMouse(5, y0, tcell.Button1, tcell.ModNone))   // press at "pha"
	app.handleMouse(tcell.NewEventMouse(6, y0+1, tcell.Button1, tcell.ModNone)) // drag to "bra"
	app.handleMouse(tcell.NewEventMouse(6, y0+1, tcell.ButtonNone, tcell.ModNone))
	app.mu.Unlock()

	if got := string(scr.GetClipboardData()); got != "pha\nbra" {
		t.Fatalf("clipboard = %q, want %q", got, "pha\nbra")
	}
}

// TestShiftedDragIsLeftToTheTerminal pins the omp / Claude Code
// contract: while the app holds the mouse, a Shift-dragged selection is
// the terminal's — its native selection and its copy. The app must not
// start, extend, or finish a selection of its own, because its
// release-time write would clobber the terminal's clipboard copy.
func TestShiftedDragIsLeftToTheTerminal(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.AddSystemBlock("copy me please")
	app.draw()

	y := -1
	for i, sr := range app.selRows {
		if sr.text == "copy me please" {
			y = i
			break
		}
	}
	if y < 0 {
		t.Fatal("system block row not captured in selRows")
	}

	app.mu.Lock()
	app.handleMouse(tcell.NewEventMouse(3, y, tcell.Button1, tcell.ModShift))
	app.handleMouse(tcell.NewEventMouse(10, y, tcell.Button1, tcell.ModShift))
	app.handleMouse(tcell.NewEventMouse(10, y, tcell.ButtonNone, tcell.ModShift))
	app.mu.Unlock()

	if app.selActive {
		t.Fatal("shifted press started an app selection")
	}
	if got := string(scr.GetClipboardData()); got != "" {
		t.Fatalf("shifted drag wrote %q to the clipboard; the terminal owns it", got)
	}
}

// TestShiftCancelsAnInFlightSelection: holding Shift partway through a
// plain drag hands the gesture to the terminal, so the app drops its
// highlight instead of leaving it stuck on screen (tcell reports the
// release with ModShift too, so the release arm never runs).
func TestShiftCancelsAnInFlightSelection(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.AddSystemBlock("copy me please")
	app.draw()

	y := -1
	for i, sr := range app.selRows {
		if sr.text == "copy me please" {
			y = i
			break
		}
	}
	if y < 0 {
		t.Fatal("system block row not captured in selRows")
	}

	app.mu.Lock()
	app.handleMouse(tcell.NewEventMouse(3, y, tcell.Button1, tcell.ModNone))
	app.handleMouse(tcell.NewEventMouse(10, y, tcell.Button1, tcell.ModNone))
	if !app.selActive {
		app.mu.Unlock()
		t.Fatal("plain drag did not start a selection")
	}
	// Shift arrives mid-gesture (terminal-native selection takes over).
	app.handleMouse(tcell.NewEventMouse(10, y, tcell.Button1, tcell.ModShift))
	app.handleMouse(tcell.NewEventMouse(10, y, tcell.ButtonNone, tcell.ModShift))
	app.mu.Unlock()

	if app.selActive {
		t.Fatal("selection still active after the gesture went native")
	}
	if got := string(scr.GetClipboardData()); got != "" {
		t.Fatalf("clipboard = %q, want untouched (the terminal owns the copy)", got)
	}
}
