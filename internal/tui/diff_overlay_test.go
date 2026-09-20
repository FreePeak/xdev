package tui

import (
	"testing"

	"github.com/gdamore/tcell/v2"
)

// TestDiffOverlayDoesNotAutoShow pins the reported regression: the diff
// overlay used to paint on every frame regardless of state, and clicking
// anywhere could not dismiss it. The overlay must only paint while diffOv
// != nil, and a click outside it must close it.
func TestDiffOverlayDoesNotAutoShow(t *testing.T) {
	app, scr := newTestApp(t, 120, 30)
	defer scr.Fini()

	app.mu.Lock()
	left, y, path := withDockRows(app)
	app.mu.Unlock()
	if path == "" {
		t.Skip("dock layout doesn't expose a clickable row")
	}

	// Open the overlay via a click on the dock FILES row.
	app.handleMouse(tcell.NewEventMouse(left+5, y, tcell.Button1, tcell.ModNone), true)
	app.handleMouse(tcell.NewEventMouse(left+5, y, tcell.ButtonNone, tcell.ModNone), false)

	app.mu.Lock()
	if app.diffOv == nil {
		app.mu.Unlock()
		t.Fatal("click on FILES row did not open the diff overlay")
	}
	app.mu.Unlock()

	// Click far outside the overlay (bottom-left corner). The overlay
	// must close — it must not stay pinned, and the click must not
	// panic or deadlock.
	app.handleMouse(tcell.NewEventMouse(2, app.height-3, tcell.Button1, tcell.ModNone), true)
	app.handleMouse(tcell.NewEventMouse(2, app.height-3, tcell.ButtonNone, tcell.ModNone), false)

	app.mu.Lock()
	ov := app.diffOv
	app.mu.Unlock()
	if ov != nil {
		t.Fatal("click outside the overlay did not close it")
	}
}

// TestDiffOverlayClosesOnClickOutside pins the "no way to close"
// symptom: with the overlay open, a click on the transcript area
// (outside the overlay rectangle) closes it rather than leaving it
// pinned for every subsequent frame.
func TestDiffOverlayClosesOnClickOutside(t *testing.T) {
	app, scr := newTestApp(t, 120, 30)
	defer scr.Fini()

	app.mu.Lock()
	left, y, path := withDockRows(app)
	app.mu.Unlock()
	if path == "" {
		t.Skip("dock layout doesn't expose a clickable row")
	}

	app.handleMouse(tcell.NewEventMouse(left+5, y, tcell.Button1, tcell.ModNone), true)
	app.handleMouse(tcell.NewEventMouse(left+5, y, tcell.ButtonNone, tcell.ModNone), false)

	app.mu.Lock()
	if app.diffOv == nil {
		app.mu.Unlock()
		t.Fatal("overlay not open before click-outside test")
	}
	app.mu.Unlock()

	// A click in the transcript band, well below the overlay's
	// bottom edge (y0=1, panelH grows with height but the overlay
	// never reaches row 20 on a 30-row terminal).
	app.handleMouse(tcell.NewEventMouse(left+20, 20, tcell.Button1, tcell.ModNone), true)
	app.handleMouse(tcell.NewEventMouse(left+20, 20, tcell.ButtonNone, tcell.ModNone), false)

	app.mu.Lock()
	ov := app.diffOv
	app.mu.Unlock()
	if ov != nil {
		t.Fatal("click outside overlay did not close it")
	}
}