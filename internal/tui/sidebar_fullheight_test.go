package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// The sidebar is a window of its own, not a band inside the transcript's rows
// (user-reported: "the sidebar should be like opencode's — full height, like 2
// windows, the main and the sidebar; right now it is not 100% and the textbox
// is still 100% width"). Four rows of the old layout are the whole defect, and
// each is pinned below: the panel's own band is every row of the terminal, the
// prompt box and its text stop at the panel's left edge, the status row's
// metrics stay in the main pane, and no main-pane row runs under the panel.

// twoWindowApp is a drawn app with the panel pinned open, a transcript, a draft
// in the composer and metrics on the status row — the four surfaces the split
// has to move at once.
func twoWindowApp(t *testing.T, w, h int) (*App, tcell.SimulationScreen) {
	t.Helper()
	app, scr := newTestApp(t, w, h)
	app.SetDockMode(DockShow)
	app.SetDockOps(DockOps{
		Session: func() (string, string) { return "two windows", "sess1234" },
		Tasks:   func() string { return "TASKS · 1/2 done\n[x] split the layout" },
	})
	app.AddUserBlock("the prompt box is still full width")
	setDraft(&app.ed, "draft in the main pane", 0)
	app.AddUsage(1200, 340, 0, 0, 1540)
	app.draw()
	return app, scr
}

// TestSidebarIsFullHeight is the headline: with the panel open it owns every
// row of the terminal, so its background is unbroken from the top down to the
// status row. Before this the band stopped above the composer, which read as a
// box floating in the transcript.
func TestSidebarIsFullHeight(t *testing.T) {
	app, scr := twoWindowApp(t, 160, 40)
	app.mu.Lock()
	top, h := app.dockGrid()
	edge := app.width - dockCols
	app.mu.Unlock()
	if top != 0 || h != 40 {
		t.Fatalf("the panel's band is %d..%d, want the whole 0..40", top, top+h)
	}
	if edge != 160-dockCols {
		t.Fatalf("the panel starts at column %d, want %d", edge, 160-dockCols)
	}
	// The surface, not a box: no cell of the panel's columns carries box chrome,
	// and row 0 — the row the pane's transcript shares with the panel's title
	// slot — is the panel's, which its own title is the witness for. (Its bg can
	// no longer witness it: the panel's field is the terminal's own.)
	for y := range 40 {
		for x := edge; x < 160; x++ {
			ch, _, _, _ := scr.GetContent(x, y)
			if ch == '│' || ch == '─' {
				t.Fatalf("the panel drew box chrome at x=%d y=%d", x, y)
			}
		}
	}
	if row := dockRowText(scr, 0, edge); !strings.Contains(row, "two windows") {
		t.Fatalf("row 0 column %d carries no panel content: %q, the band stops short", edge, row)
	}
	// And the last row of the terminal is inside it: the status row's cells
	// under the panel are the panel's, not the status row's.
	if ch, _, _, _ := scr.GetContent(159, 39); ch != ' ' {
		t.Fatalf("the bottom-right cell is %q, want the panel's own surface", ch)
	}
}

// TestComposerStopsAtTheSidebar: "the textbox still 100% width" — the box, its
// borders and the text's wrap width are the main pane's. A box that runs under
// the panel is the symptom, so the assertion is on the painted row.
func TestComposerStopsAtTheSidebar(t *testing.T) {
	app, scr := twoWindowApp(t, 160, 40)
	edge := app.width - dockCols
	app.mu.Lock()
	avail, cRows := app.composerAvail(), app.composerRows()
	app.mu.Unlock()
	if avail != edge-8 {
		t.Fatalf("the editor wraps at %d, want the pane's %d", avail, edge-8)
	}
	// The box's top border, its right edge and its divider all stop inside the
	// pane (the border sits two cells in); nothing paints a composer border
	// under the panel.
	yTop := 40 - 1 - cRows
	row := dockRowText(scr, yTop-1, 0)
	if !strings.Contains(row, "╮") {
		t.Fatalf("the composer lost its right border: %q", row)
	}
	if ch, _, _, _ := scr.GetContent(edge-2, yTop-1); ch != '╮' {
		t.Fatalf("the box's right edge is %q at column %d, want the border at %d", ch, edge-2, edge-2)
	}
	for _, x := range []int{edge - 1, edge} {
		if ch, _, _, _ := scr.GetContent(x, yTop-1); ch != ' ' {
			t.Fatalf("the box paints %q at column %d, at or under the panel", ch, x)
		}
	}
}

// TestStatusRowAndTopRowStayInTheMainPane: the two remaining full-width rows.
// The metrics are the loudest failure (they right-align to the terminal's
// edge, so with the panel open they slid under it), and the top transcript row
// takes w for the same reason.
func TestStatusRowAndTopRowStayInTheMainPane(t *testing.T) {
	app, scr := twoWindowApp(t, 160, 40)
	edge := app.width - dockCols
	row := lastRow(screenText(scr))
	if !strings.Contains(row, "1.5k") {
		t.Fatalf("the metrics vanished from the status row: %q", row)
	}
	// Nothing of the status row may reach into the panel's columns: the row is
	// the pane's row, and the metrics right-align to the pane's edge.
	for x := edge; x < 160; x++ {
		if ch, _, _, _ := scr.GetContent(x, 39); ch != ' ' {
			t.Fatalf("the status row painted %q at x=%d, inside the panel", ch, x)
		}
	}
	// Row 0 is the one row both windows share a surface on — the pane's
	// transcript on the left, the panel's title on the right — so the check is
	// the panel's own gutter: the pane's text may not cross the boundary to
	// reach the panel's text.
	first := strings.SplitN(screenText(scr), "\n", 2)[0]
	if strings.TrimSpace(first[edge:edge+dockPad]) != "" {
		t.Fatalf("the transcript runs into the panel's gutter: %q", first[edge:edge+dockPad])
	}
}

// TestTwoWindowShapeUnchangedWhenClosed: the layout is the shipped one whenever
// the panel is shut — rightEdge is the whole terminal, so the box, the status
// row are exactly as wide as they were before the split.
func TestTwoWindowShapeUnchangedWhenClosed(t *testing.T) {
	app, scr := newTestApp(t, 160, 40)
	app.SetDockMode(DockHide)
	app.AddUserBlock("the prompt box")
	setDraft(&app.ed, "draft", 0)
	app.AddUsage(1200, 340, 0, 0, 1540)
	app.draw()
	app.mu.Lock()
	edge, avail := app.rightEdge(), app.composerAvail()
	app.mu.Unlock()
	if edge != 160 || avail != 160-8 {
		t.Fatalf("closed: rightEdge=%d avail=%d, want the whole terminal", edge, avail)
	}
	if !strings.Contains(lastRow(screenText(scr)), "1.5k") {
		t.Fatal("the status row lost its metrics with the panel shut")
	}
}
