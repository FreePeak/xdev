package tui

import (
	"testing"

	"github.com/gdamore/tcell/v2"
)

// The sidebar is a window of its own: a press inside its columns belongs to the
// panel, a press in the TRANSCRIPT columns stays the transcript's even when the
// two share a screen row. dockRowAt is the panel's only click seam, so the
// column test lives there — before this, a click at x=40 on a row the panel
// painted opened that file's diff (the 2026-09-28 interactive audit).
func TestDockRowAtAnswersOnlyInsideThePanel(t *testing.T) {
	app, _ := sidebarApp(t)
	app.mu.Lock()
	app.blocks = append(app.blocks, &Block{Kind: KindToolDone,
		Diff: "--- a/foo.txt\n+++ b/foo.txt\n@@ -0,0 +1 @@\n+hello", Text: "+hello"})
	app.mu.Unlock()
	app.draw()

	app.mu.Lock()
	d := app.ensureDock()
	d.lines = []dockRow{{text: "foo.txt", path: "foo.txt", add: "+1", del: "-0"}}
	top, _ := app.dockGrid()
	y := top + d.titleRows()
	inPanelX := app.width - dockCols + 4
	outsideX := inPanelX - dockCols - 5
	app.mu.Unlock()

	// the hit table itself
	app.mu.Lock()
	if p, _ := app.dockRowAt(outsideX, y); p != "" {
		t.Errorf("dockRowAt(x=%d) outside the panel returned %q", outsideX, p)
	}
	if p, a := app.dockRowAt(inPanelX, y); p != "foo.txt" || a != "" {
		t.Errorf("dockRowAt(x=%d) inside the panel returned path=%q act=%q, want foo.txt", inPanelX, p, a)
	}
	app.mu.Unlock()

	// and the gesture: a press in the transcript columns must not open the diff
	app.mu.Lock()
	openAtOutside := func() bool {
		app.handleMouse(tcell.NewEventMouse(outsideX, y, tcell.Button1, tcell.ModNone), true)
		ov := app.diffOv != nil
		app.closeDiffOverlayLocked()
		return ov
	}
	app.mu.Unlock()
	if openAtOutside() {
		t.Errorf("a press at x=%d (transcript columns) opened the panel's file diff", outsideX)
	}

	// the panel's own click still works
	app.mu.Lock()
	app.handleMouse(tcell.NewEventMouse(inPanelX, y, tcell.Button1, tcell.ModNone), true)
	ov, path := app.diffOv != nil, ""
	if app.diffOv != nil {
		path = app.diffOv.path
	}
	app.mu.Unlock()
	if !ov || path != "foo.txt" {
		t.Errorf("a press inside the panel (x=%d) did not open the diff: open=%v path=%q", inPanelX, ov, path)
	}
}
