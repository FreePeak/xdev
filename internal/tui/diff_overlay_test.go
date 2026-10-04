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
	// A blank interior cell: inside the border, below the fixture's five diff
	// rows. It cannot be a row a diff band owns — a changed row's band runs to
	// the panel's right border, so "well clear of the diff text" horizontally
	// no longer clears it.
	r, _, st, _ := scr.GetContent(100, 9)
	if _, bg, _ := st.Decompose(); bg != tcell.ColorDefault {
		t.Fatalf("overlay interior cell %q has background %v, want the terminal default", r, bg)
	}
}

// TestDiffOverlayIgnoresANamedBackground is the other half: the panel's field
// is the terminal's, whatever bg_base names. A themed fill painted the blank
// interior one colour and left every text cell on the terminal's own, so a
// viewer that named bg_base came up striped — black text bars on a grey band.
func TestDiffOverlayIgnoresANamedBackground(t *testing.T) {
	t.Setenv("NO_COLOR", "")
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

	// The panel's FIELD is the terminal's, whatever bg_base names: a themed
	// fill painted the blank interior one colour while every text cell kept
	// the terminal's own, so a viewer that named bg_base came up striped — black
	// text bars on a grey band. A changed row is the one exception and it is
	// not a field: a diff band is the claim on that row, so a cell a changed
	// row painted carries the band's tint and never the themed fill. Its band
	// runs the whole interior, so the probe for "blank" is a row below the
	// diff, not a cell beside it.
	_, _, blank, _ := scr.GetContent(100, 9)
	if _, bg, _ := blank.Decompose(); bg != tcell.ColorDefault {
		t.Fatalf("overlay blank interior has background %v, want the terminal default", bg)
	}
	// A changed row carries the claim ON that row — the row's tint, or the
	// stronger one under its changed words — and never the panel's fill.
	remRow, _ := app.th.Slot(theme.ToolDiffRemovedBg)
	remWord, _ := app.th.Slot(theme.ToolDiffRemovedWordBg)
	rowY, rowX := -1, -1
	for y := 0; y < app.height && rowY < 0; y++ {
		for x := 0; x < app.width; x++ {
			r, _, st, _ := scr.GetContent(x, y)
			_, bg, _ := st.Decompose()
			switch bg {
			case app.cellColor(remRow), app.cellColor(remWord):
				rowX, rowY = x, y
			case app.cellColor(c):
				t.Fatalf("overlay cell %d,%d %q has the themed fill %v; only a changed row may paint", x, y, r, bg)
			}
		}
	}
	if rowY < 0 {
		t.Fatalf("no removed band anywhere in the overlay: the diff rows lost their band (%+v / %+v)", remRow, remWord)
	}
	// The band is a stripe, not a glyph: it runs to the panel's right border.
	if r, _, st, _ := scr.GetContent(rowX+1, rowY); func() bool {
		_, bg, _ := st.Decompose()
		return bg != tcell.ColorDefault
	}() {
		t.Fatalf("cell %d,%d %q painted no band where the row does", rowX+1, rowY, r)
	}
}

// TestSidebarIsTheTerminalBackground pins the sidebar's field to the terminal's
// own background (#466 for the diff popup, the same call for the panel beside
// it). A themed fill here painted bg_base — #141414 — beside a transcript that
// is the terminal's black, so the two windows read as two different themes. The
// panel's text cells resolve to the default whatever the fill says, so a
// surviving fill is not a cosmetic difference: it is a grey band with black
// text bars in it, the same striping #454 built into the popup.
func TestSidebarIsTheTerminalBackground(t *testing.T) {
	app, scr, _ := dockTestApp(t, 160, 40)
	app.SetDockMode(DockShow)
	app.draw()

	base, ok := app.th.Slot(theme.BgBase)
	if !ok {
		t.Fatal("the built-in theme must name bg_base, or this test proves nothing")
	}
	edge := app.width - dockCols
	for _, at := range []struct{ x, y int }{{edge, 0}, {edge + dockPad, 1}, {edge + 1, 39}} {
		r, _, st, _ := scr.GetContent(at.x, at.y)
		if _, bg, _ := st.Decompose(); bg != tcell.ColorDefault {
			t.Fatalf("sidebar cell (%d,%d) %q has background %v, want the terminal default (bg_base is %v)", at.x, at.y, r, bg, app.cellColor(base))
		}
	}
}
