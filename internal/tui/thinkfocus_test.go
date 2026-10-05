package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// firstThinkBoxRow paints the transcript and returns the screen row the first
// reasoning box's top border lands on, so a test can press the real mouse at
// the real coordinate instead of poking thinkFocus directly — a press is
// armed in selection.go, and only a real press exercises that wiring.
func firstThinkBoxRow(t *testing.T, app *App, scr tcell.SimulationScreen) int {
	t.Helper()
	app.width, app.height = scr.Size() // paint reads the App's size, not the screen's
	app.paint()
	scr.Show() // Show flushes the back buffer into the simulation's cells
	cells, w, h := scr.GetContents()
	if cells == nil {
		t.Fatal("simulation screen has no cells")
	}
	for y := 0; y < h; y++ {
		var b strings.Builder
		for x := 0; x < w; x++ {
			if r := cells[y*w+x].Runes; len(r) > 0 {
				b.WriteString(string(r))
			} else {
				b.WriteByte(' ')
			}
		}
		// Match the state's own word, not the glyph, so the probe survives a
		// symbol preset that redraws the frame.
		if strings.Contains(b.String(), "Thought") {
			return y
		}
	}
	return -1
}

// TestMousePressFocusesTheBox drives the real press path: a click that names a
// reasoning box must move thinkFocus to it, which is the only wiring that
// gives the wheel a box to scroll. A unit test that assigns thinkFocus itself
// would stay green with selection.go never arming anything.
//
// The frame's INK is fixed (theme.ThinkFrame, one per polarity) and Bold is
// the whole focus mark, so there is no tween left to pin here — thinkframe_test
// .go owns the ink and the mark, this one owns the press.
func TestMousePressFocusesTheBox(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.SetHandlers(func(string) {}, func() {}, func() {})
	app.BeginThinking()
	app.AppendThinking(thinkLines(40))
	app.EndThinking()

	y := firstThinkBoxRow(t, app, scr)
	if y < 0 {
		t.Skip("no painted reasoning box in this geometry")
	}

	app.handleMouse(tcell.NewEventMouse(5, y, tcell.Button1, tcell.ModNone), true)

	app.mu.Lock()
	focus := app.thinkFocus
	app.mu.Unlock()
	if focus != 0 {
		t.Fatalf("press on the box's top border (row %d): thinkFocus = %d, want 0", y, focus)
	}
}

// TestMousePressOffBoxLeavesTheFocusAlone is the other side: a press that lands
// on plain transcript gives the wheel back to the transcript, so the next
// notch scrolls the session rather than a box.
func TestMousePressOffBoxLeavesTheFocusAlone(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.SetHandlers(func(string) {}, func() {}, func() {})
	app.width, app.height = scr.Size()
	app.AddSystemBlock("ready")
	app.paint()

	app.handleMouse(tcell.NewEventMouse(5, 2, tcell.Button1, tcell.ModNone), true)

	app.mu.Lock()
	focus := app.thinkFocus
	app.mu.Unlock()
	if focus != -1 {
		t.Errorf("press on plain transcript left thinkFocus = %d, want -1", focus)
	}
}
