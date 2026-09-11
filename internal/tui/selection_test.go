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
	app.handleMouse(tcell.NewEventMouse(3+2, y0, tcell.Button1, tcell.ModNone))   // press at "ph"
	app.handleMouse(tcell.NewEventMouse(3+3, y0+1, tcell.Button1, tcell.ModNone)) // drag to "bra"
	app.handleMouse(tcell.NewEventMouse(3+3, y0+1, tcell.ButtonNone, tcell.ModNone))
	app.mu.Unlock()

	if got := string(scr.GetClipboardData()); got != "pha\nbra" {
		t.Fatalf("clipboard = %q, want %q", got, "pha\nbra")
	}
}
