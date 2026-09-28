package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// The sidebar is a window of its own (user-reported "the sidebar works as new
// TUI windows"): a click anywhere in the panel's columns focuses THAT window, so
// the transcript's hit-tests must not reach across and claim a cell the panel
// painted, and a drag inside it still copies the panel's own text. Three things
// follow, and each is pinned below: no stray message menu from a press that
// lands over a user row the panel covers, the trajectory button row opens its
// ledger again (a panel row that names a surface must reach it), and a drag
// over the panel copies the panel.

// sidebarApp is a drawn app with the panel pinned open, a transcript that runs
// under it, and a ledger wired so the trajectory row has something to open.
func sidebarApp(t *testing.T) (*App, tcell.SimulationScreen) {
	t.Helper()
	app, scr := newTestApp(t, 160, 40)
	app.SetDockMode(DockShow)
	app.SetDockOps(DockOps{
		Session:    func() (string, string) { return "sidebar window", "sess1234" },
		Tasks:      func() string { return "TASKS · 1/2 done\n[x] wire the panel\n[ ] write tests" },
		Trajectory: func() string { return "TRAJECTORY · 2 records" },
	})
	app.SetTrajectoryOps(&TrajectoryOps{
		Records: func() []TrajectoryRecord {
			return []TrajectoryRecord{
				{Index: 1, Kind: "user", Text: "make a file", Detail: "make a file"},
				{Index: 2, Kind: "tool", Text: "edit foo.txt", Detail: "+hello"},
			}
		},
	})
	// A transcript tall enough to run under the panel, so a press on the panel
	// can land on a row that also exists in the document.
	for i := 0; i < 6; i++ {
		app.AddSystemBlock("transcript row " + string(rune('A'+i)))
	}
	app.AddUserBlock("a user prompt the panel can cover")
	app.AddSystemBlock("more transcript below the panel")
	app.draw()
	return app, scr
}

// dockCell is a screen cell on the panel row carrying want: two cells into the
// row's text, so a click there cannot be read as a press on the gutter.
func dockCell(t *testing.T, app *App, want string) (x, y int, ok bool) {
	t.Helper()
	app.mu.Lock()
	rows := append([]selRow(nil), app.selDockRows...)
	app.mu.Unlock()
	if len(rows) == 0 {
		t.Fatal("the panel painted no selectable rows")
	}
	for _, r := range rows {
		if strings.Contains(r.text, want) {
			return r.x0 + 2, r.y, true
		}
	}
	return 0, 0, false
}

// TestSidebarClickOverAUserRowDoesNotOpenTheTranscriptMenu is the headline of
// "the panel is its own window": a click on the panel's columns focuses the
// panel. The panel is painted OVER the transcript, so its cells sit on
// document rows that have their own affordances — the user-message menu, a
// reasoning box. A press that lands inside the panel must not arm either, or a
// human reaching for the sidebar gets a popup anchored over the sidebar.
func TestSidebarClickOverAUserRowDoesNotOpenTheTranscriptMenu(t *testing.T) {
	app, _ := sidebarApp(t)
	x, _, ok := dockCell(t, app, "")
	if !ok {
		t.Skip("panel layout exposed no text row")
	}
	// The screen row the user prompt actually painted on, so the press lands on
	// a cell the transcript claims AND the panel covers.
	y := userRow(t, app, 0)
	app.mu.Lock()
	covered := app.dockAt(x, y)
	_, bi := app.userRowAt(y)
	app.mu.Unlock()
	if !covered {
		t.Skipf("the panel does not cover row %d; nothing to prove here", y)
	}
	if bi < 0 {
		t.Fatalf("row %d is not a user row; the test setup moved the transcript", y)
	}

	app.mu.Lock()
	press(app, x, y)
	release(app, x, y)
	armed, focus, menu := app.msgArmed, app.thinkFocus, app.msgm != nil
	app.mu.Unlock()

	if menu {
		t.Fatal("a click inside the sidebar opened the user-message menu")
	}
	if armed {
		t.Fatal("a click inside the sidebar armed the user-message menu")
	}
	if focus >= 0 {
		t.Fatalf("a click inside the sidebar focused transcript block %d through the panel", focus)
	}
}

// TestSidebarDragCopiesThePanelText pins that focusing the panel did not cost
// the copy: a drag across a panel row puts the panel's own text on the
// clipboard, not the transcript row that happens to share the screen row.
func TestSidebarDragCopiesThePanelText(t *testing.T) {
	app, scr := sidebarApp(t)
	x, y, ok := dockCell(t, app, "wire the panel")
	if !ok {
		t.Skip("panel layout exposed no task row")
	}

	app.mu.Lock()
	drag(app, x, y, x+4, y)
	app.mu.Unlock()

	got := string(scr.GetClipboardData())
	// The row paints as "[x] wire the panel", and the copy is measured in
	// screen cells, so two cells in is "]" and the drag covers "] wir".
	if !strings.Contains(got, "wir") {
		t.Fatalf("clipboard = %q, want the panel's own row text", got)
	}
	if strings.Contains(got, "transcript") {
		t.Fatalf("a drag over the panel copied the transcript beneath it: %q", got)
	}
}

// sidebarTitleCell is two cells into the panel's title slot, and the row's
// text: the copy is measured in screen cells, so the expectation is the slice
// of the title between the two drag corners rather than the whole string.
func sidebarTitleCell(t *testing.T, app *App, want string) (x, y int, row selRow, ok bool) {
	t.Helper()
	app.mu.Lock()
	rows := append([]selRow(nil), app.selDockRows...)
	app.mu.Unlock()
	for _, r := range rows {
		if strings.Contains(r.text, want) {
			return r.x0 + 2, r.y, r, true
		}
	}
	return 0, 0, selRow{}, false
}

// TestSidebarDragCopiesTheTitleSlot pins the one panel row the copy table used
// to leave out. The title slot — the session's name, or its first prompt when
// the session has no title of its own — is the string a human reaches the panel
// to copy, and it sits above the built rows. Not being in selDockRows meant a
// drag across it fell through to the transcript row behind the panel and, worse,
// clipped the cell range to nothing (spanRow against an empty row), so the
// copy came out blank.
func TestSidebarDragCopiesTheTitleSlot(t *testing.T) {
	app, scr := sidebarApp(t)
	x, y, row, ok := sidebarTitleCell(t, app, "sidebar window")
	if !ok {
		t.Skip("panel layout exposed no title row")
	}
	want := cellSlice(row.text, 2, 9)

	app.mu.Lock()
	drag(app, x, y, x+6, y)
	app.mu.Unlock()

	if got := string(scr.GetClipboardData()); got != want {
		t.Fatalf("clipboard = %q, want the panel's title slot text %q", got, want)
	}
}

// The same slot with no session title of its own: the panel falls back to the
// session's first prompt, and that string must be copyable too — it is the task
// name, and it is usually too long for the slot, so it wraps onto a second row
// that has to be in the table as well.
func TestSidebarTitleSlotCopiesTheFirstPrompt(t *testing.T) {
	app, scr := newTestApp(t, 160, 40)
	app.SetDockMode(DockShow)
	app.SetDockOps(DockOps{
		Session: func() (string, string) { return "", "sess1234" },
		Tasks:   func() string { return "TASKS · 0/0 done" },
	})
	for i := 0; i < 6; i++ {
		app.AddSystemBlock("transcript row " + string(rune('A'+i)))
	}
	app.AddUserBlock("port the first prompt to the sidebar title slot")
	app.AddSystemBlock("more transcript below the panel")
	app.draw()

	x, y, row, ok := sidebarTitleCell(t, app, "port the first prompt")
	if !ok {
		t.Skip("panel layout exposed no title row")
	}
	want := cellSlice(row.text, 2, 10)

	app.mu.Lock()
	drag(app, x, y, x+7, y)
	app.mu.Unlock()

	if got := string(scr.GetClipboardData()); got != want {
		t.Fatalf("clipboard = %q, want the wrapped first prompt's first row %q", got, want)
	}
}

// TestSidebarTrajectoryRowOpensTheLedger pins the click that was dead: the
// panel's TRAJECTORY row is a button whose action dockRowAt reported and nothing
// ran, so clicking the only panel row that names a surface did nothing the eye
// could see. A press on it must open the ledger.
func TestSidebarTrajectoryRowOpensTheLedger(t *testing.T) {
	app, _ := sidebarApp(t)
	x, y, ok := dockCell(t, app, "click to open the ledger")
	if !ok {
		t.Skip("panel layout exposed no trajectory row")
	}

	app.mu.Lock()
	press(app, x, y)
	release(app, x, y)
	app.mu.Unlock()

	if !app.TrajectoryOpen() {
		t.Fatal("clicking the panel's trajectory row did not open the ledger")
	}
}

// TestSidebarClickStillOpensTheFileDiff is the other half of the same branch:
// the panel's FILES rows keep their own click, so "the panel owns its clicks"
// cannot quietly break the diff surface.
func TestSidebarClickStillOpensTheFileDiff(t *testing.T) {
	app, _ := newTestApp(t, 120, 30)

	app.mu.Lock()
	left, y, path := withDockRows(app)
	app.mu.Unlock()
	if path == "" {
		t.Skip("dock layout doesn't expose a clickable row")
	}

	app.handleMouse(tcell.NewEventMouse(left+5, y, tcell.Button1, tcell.ModNone), true)
	app.handleMouse(tcell.NewEventMouse(left+5, y, tcell.ButtonNone, tcell.ModNone), false)

	app.mu.Lock()
	ov := app.diffOv
	app.mu.Unlock()
	if ov == nil {
		t.Fatal("clicking a panel FILES row no longer opens the diff overlay")
	}
}
