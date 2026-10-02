package tui

import (
	"testing"

	"github.com/gdamore/tcell/v2"
)

// TestDiffOverlayEscClose verifies the diff overlay can be dismissed
// with Esc while open: handleDiffOverlayKey must consume Esc and call
// closeDiffOverlay, so the double-Esc rewind block in handleKey never
// intercepts it (previously "Esc close" in the footer was a lie).
func TestDiffOverlayEscClose(t *testing.T) {
	app, scr := newTestApp(t, 120, 30)
	defer scr.Fini()

	app.mu.Lock()
	app.diffOv = &diffOverlay{path: "internal/tui/dock.go", diff: "+added\n-removed", width: app.width}
	app.mu.Unlock()
	app.poke()

	if !app.diffOverlayOpen() {
		t.Fatal("diff overlay must be open to start the test")
	}

	app.handleKey(tcell.NewEventKey(tcell.KeyEsc, 0, tcell.ModNone))

	app.mu.Lock()
	closed := app.diffOv == nil
	app.mu.Unlock()
	if !closed {
		t.Fatal("Esc must close the diff overlay")
	}
}