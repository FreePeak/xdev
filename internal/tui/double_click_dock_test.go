// Regression test: a double-click on a dock FILES row must not deadlock.
//
// The deadlock was: handleMouse holds a.mu (app.go:1841) and calls
// dockClick→openDiffOverlay, which tried to take a.mu again (dock.go:859).
// The fix removes the redundant lock from openDiffOverlay/closeDiffOverlay
// and adds it to the callers that don't already hold it.
//
// This test exercises handleMouse (which holds a.mu) calling
// dockClick→openDiffOverlay — the exact path that deadlocked.
package tui

import (
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// withDockRows sets up the dock state manually (no locking helpers)
// and returns a y where dockClick finds a path and a click opens
// the diff overlay. Caller must hold app.mu.
func withDockRows(app *App) (left int, y int, path string) {
	app.dock = &dockState{mode: DockShow}
	app.dock.ops = DockOps{
		Session: func() (string, string) { return "opencode sidebar", "sess1234" },
		Tasks:   func() string { return "TASKS · 0/0 done" },
		Agents:  func() string { return "AGENTS · 0 running" },
	}
	// A tool block with a real-looking Diff gives dockChanges() a FILES row.
	app.blocks = append(app.blocks, &Block{
		Kind: KindToolDone, ToolName: "edit",
		Diff: "---\n+++ b/internal/tui/dock.go\n@@ -1,5 +1,3 @@\n-old line\n+new line",
	})
	// Bump the version so dockBuild rebuilds instead of skipping.
	app.DockBump()
	app.dockBuild()

	_, dg := app.dockGrid()
	if dg < 2 {
		return 0, 0, ""
	}
	left = app.width - dockCols
	// Scan every y until dockClick resolves a path.
	for y = app.transcriptTop() + 1; y < dg; y++ {
		p := app.dockClick(left+5, y)
		if p != "" {
			path = p
			return left, y, path
		}
	}
	return 0, 0, ""
}

// TestDoubleClickDockCannotDeadlock pins the reported crash: a double-click
// (press+release+press+release within clickWordWindow) on a FILES row in the
// dock must open the diff overlay, not deadlock.
func TestDoubleClickDockCannotDeadlock(t *testing.T) {
	app, scr := newTestApp(t, 120, 30)
	defer scr.Fini()

	app.mu.Lock()
	left, y, path := withDockRows(app)
	app.mu.Unlock()
	if path == "" {
		t.Skip("dock layout doesn't expose a clickable row")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		app.handleMouse(tcell.NewEventMouse(left+5, y, tcell.Button1, tcell.ModNone), true)
		app.handleMouse(tcell.NewEventMouse(left+5, y, tcell.ButtonNone, tcell.ModNone), false)
		time.Sleep(100 * time.Millisecond)
		app.handleMouse(tcell.NewEventMouse(left+5, y, tcell.Button1, tcell.ModNone), true)
		app.handleMouse(tcell.NewEventMouse(left+5, y, tcell.ButtonNone, tcell.ModNone), false)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("double-click on dock deadlocked — the overlay opener held a.mu")
	}

	app.mu.Lock()
	ov := app.diffOv
	app.mu.Unlock()
	if ov == nil {
		t.Fatal("double-click did not open the diff overlay")
	}
	if ov.path != path {
		t.Fatalf("overlay path = %q, want %q", ov.path, path)
	}
}
