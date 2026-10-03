package tui

import (
	"strings"
	"testing"
	"time"
)

// The whole point of the toast: a notice must be readable in the corner and
// nowhere near the prompt. This drives the real painter and reads the drawn
// screen, so a layout regression (a toast painted on the divider, or a level
// painted in the wrong place) fails here rather than in a screenshot.
func TestToastPaintsInTheCornerAndOffTheComposer(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.AddSystemBlock("hello")
	app.draw()

	const msg = "mcp: playwright unavailable — see `xdev mcps` for configured URLs"
	app.Toast(ToastError, msg, time.Minute)
	app.draw()

	// Row 0 is the first transcript row (there is no top bar) and the corner
	// is the right end of it — not the composer's row.
	rows := rowsOf(scr)
	if !strings.Contains(rows[0], "playwright unavailable") {
		t.Fatalf("row 0 = %q, want the toast in the corner of the first transcript row", rows[0])
	}
	if strings.Contains(rows[0], "test/free") {
		t.Fatalf("row 0 = %q, want the toast clear of the composer's divider", rows[0])
	}
	for i, ln := range rows {
		if strings.Contains(ln, "playwright") && strings.Contains(ln, "test/free") {
			t.Fatalf("row %d = %q, the toast must never share the divider", i, ln)
		}
	}
}

// Two levels stack, newest at the top, and the newest is the one on the first
// row — a config with two broken servers is the case this exists for.
func TestToastStackPutsTheNewestFirst(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.AddSystemBlock("hello")
	app.draw()

	app.Toast(ToastError, "oldest failure", time.Minute)
	app.Toast(ToastError, "newest failure", time.Minute)
	app.draw()

	rows := rowsOf(scr)
	if !strings.Contains(rows[0], "newest failure") {
		t.Fatalf("row 0 = %q, want the newest toast on top", rows[0])
	}
	if !strings.Contains(rows[1], "oldest failure") {
		t.Fatalf("row 1 = %q, want the older toast below it", rows[1])
	}
}

// The cap is a ceiling, not a queue: past toastMaxRows the oldest is dropped,
// because four notices stacked over the transcript is a wall, not a report.
func TestToastStackIsCapped(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	for i := range 5 {
		app.Toast(ToastInfo, "notice "+string(rune('a'+i)), time.Minute)
	}
	app.mu.Lock()
	n := len(app.toasts)
	app.mu.Unlock()
	if n != toastMaxRows {
		t.Fatalf("stack = %d toasts, want %d", n, toastMaxRows)
	}
}

// A multi-line error (an MCP stderr dump, a paste failure with a newline)
// must stay one row: the stack's geometry is a row per toast.
func TestToastIsOneRow(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.AddSystemBlock("hello")
	app.draw()

	app.Toast(ToastError, "first line\nsecond line\tand a tab", time.Minute)
	app.draw()

	rows := rowsOf(scr)
	if !strings.Contains(rows[0], "first line second line and a tab") {
		t.Fatalf("row 0 = %q, want the notice collapsed onto one row", rows[0])
	}
	if strings.Contains(strings.Join(rows[1:], "\n"), "second line") {
		t.Fatal("the notice wrapped onto a second row")
	}
}

// An expired toast is gone on the next frame and cannot come back: the frame
// that drops it is the one the tick asks for, and no later draw resurrects it.
func TestToastExpires(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.AddSystemBlock("hello")
	app.draw()

	app.Toast(ToastInfo, "transient", time.Minute)
	app.draw()
	if rowContaining(scr, "transient") == "" {
		t.Fatal("the toast never painted")
	}
	app.mu.Lock()
	app.expireToasts()
	app.mu.Unlock()
	app.draw()
	if ln := rowContaining(scr, "transient"); ln != "" {
		t.Fatalf("row = %q, want the expired toast gone", ln)
	}
}

// A pane narrower than the layout's own minimum has no corner to give, and a
// toast squeezed into a cell or two says nothing — it drops out rather than
// covering content with noise.
func TestToastSkipsATooNarrowTerminal(t *testing.T) {
	app, scr := newTestApp(t, 12, 10)
	app.AddSystemBlock("hello")
	app.draw()
	app.Toast(ToastInfo, "transient", time.Minute)
	app.draw()
	if ln := rowContaining(scr, "transient"); ln != "" {
		t.Fatalf("row = %q, want no toast on a pane with no room for one", ln)
	}
}

// The corner belongs to the main pane, not the terminal: with the context
// dock open a toast must stop at the panel's left edge and never paint over
// the sidebar, because the panel is a window of its own (#291).
func TestToastStopsAtTheDockEdge(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	app.SetDockOps(DockOps{
		Plan:    func() (string, bool) { return "1. do the thing", true },
		Session: func() (string, string) { return "dock", "sess1234" },
	})
	app.AddSystemBlock("hello")
	app.SetDockMode(DockShow) // auto would close the panel at 100 columns
	app.draw()
	if !app.dockOn() {
		t.Skip("the dock is not visible at this width; there is no edge to test")
	}

	app.Toast(ToastError, "mcp: be-kg unavailable", time.Minute)
	app.draw()

	// The dock's own rows carry its text, so the toast must end where the pane
	// ends: the drawn row may be full width, but the toast's last cell has to
	// sit left of the panel.
	_, x := rowWith(t, scr, "be-kg unavailable")
	if cell, _, _, _ := scr.GetContent(x+width("✗ mcp: be-kg unavailable")-1, 1); string(cell) != " " {
		t.Fatalf("the toast's last cell is %q, want it left of the dock (rightEdge %d)", cell, app.rightEdge())
	}
}
