package tui

import (
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
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

// TestDiffOverlayInsideIsTheTerminalBackground pins that the diff viewer's
// inside bar is the terminal's own background, not a colour the app picked.
// The overlay resolved it with Get, which cannot say "the terminal decides": a
// theme that leaves bg_base to the terminal answers with an explicit black, so
// the overlay painted #000000 over whatever scheme the human was running.
func TestDiffOverlayInsideIsTheTerminalBackground(t *testing.T) {
	app, scr := newTestApp(t, 120, 30)
	defer scr.Fini()

	// A theme that hands bg_base to the terminal — the one case Get reads as
	// black. Load builds a fresh map per call, so marking the slot here stays
	// local to this test.
	th := theme.Load("groknight")
	th.Slots[theme.BgBase] = theme.Color{}
	th.Defaults[theme.BgBase] = true
	app.SetTheme(th)

	app.mu.Lock()
	_, _, path := withDockRows(app)
	app.mu.Unlock()
	if path == "" {
		t.Skip("dock layout doesn't expose a clickable row")
	}
	app.mu.Lock()
	opened := app.openDiffOverlay(path)
	app.mu.Unlock()
	if !opened {
		t.Fatal("no diff overlay for the changed file")
	}
	app.draw()

	// A blank interior cell: inside the border, well clear of the diff text.
	r, _, st, _ := scr.GetContent(100, 6)
	if _, bg, _ := st.Decompose(); bg != tcell.ColorDefault {
		t.Fatalf("overlay interior cell %q has background %v, want the terminal default", r, bg)
	}
}

// TestDiffOverlayIgnoresANamedBackground is the other half: the panel's field
// is the terminal's, whatever bg_base names. A themed fill painted the blank
// interior one colour and left every text cell on the terminal's own, so a
// viewer that named bg_base came up striped — black text bars on a grey band.
func TestDiffOverlayIgnoresANamedBackground(t *testing.T) {
	app, scr := newTestApp(t, 120, 30)
	defer scr.Fini()

	app.mu.Lock()
	_, _, path := withDockRows(app)
	app.mu.Unlock()
	if path == "" {
		t.Skip("dock layout doesn't expose a clickable row")
	}
	app.mu.Lock()
	opened := app.openDiffOverlay(path)
	c, ok := app.th.Slot(theme.BgBase)
	app.mu.Unlock()
	if !ok {
		t.Fatal("the built-in theme must name bg_base")
	}
	if !opened {
		t.Fatal("no diff overlay for the changed file")
	}
	app.draw()

	// A blank interior cell and a cell the diff text painted: one background.
	for _, x := range []int{100, 8} {
		r, _, st, _ := scr.GetContent(x, 6)
		if _, bg, _ := st.Decompose(); bg != tcell.ColorDefault {
			t.Fatalf("overlay cell %d %q has background %v, want the terminal default (bg_base is %v)", x, r, bg, app.cellColor(c))
		}
	}
}
