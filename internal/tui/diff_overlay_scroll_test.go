package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// longDiff makes enough unified-diff rows that the overlay must scroll.
func longDiff(n int) string {
	var b strings.Builder
	b.WriteString("--- a/f.go\n+++ b/f.go\n@@ -1,1 +1,1 @@\n")
	for i := 0; i < n; i++ {
		b.WriteString("-old line\n+new line\n")
	}
	return b.String()
}

// openLongDiffOverlay plants a long tool diff and opens the overlay for it.
func openLongDiffOverlay(t *testing.T, app *App) *diffOverlay {
	t.Helper()
	app.mu.Lock()
	defer app.mu.Unlock()
	app.blocks = append(app.blocks, &Block{
		Kind: KindToolDone, ToolName: "edit",
		Diff: longDiff(80),
	})
	if !app.openDiffOverlay("f.go") {
		t.Fatal("openDiffOverlay failed")
	}
	ov := app.diffOv
	if ov == nil {
		t.Fatal("diffOv nil after open")
	}
	// Mimic the first paint: body height is less than the line count.
	ov.scrollVp = 10
	ov.scrollOff = 0
	return ov
}

// TestDiffOverlayArrowKeysScroll pins the reported bug: ↑↓ on the open
// diff surface must move scrollOff, not fall through to the composer.
func TestDiffOverlayArrowKeysScroll(t *testing.T) {
	app, scr := newTestApp(t, 120, 40)
	defer scr.Fini()
	ov := openLongDiffOverlay(t, app)

	app.handleKey(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	app.mu.Lock()
	off := ov.scrollOff
	app.mu.Unlock()
	if off != 1 {
		t.Fatalf("Down: scrollOff=%d want 1", off)
	}

	app.handleKey(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	app.handleKey(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	app.mu.Lock()
	off = ov.scrollOff
	app.mu.Unlock()
	if off != 3 {
		t.Fatalf("3×Down: scrollOff=%d want 3", off)
	}

	app.handleKey(tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone))
	app.mu.Lock()
	off = ov.scrollOff
	app.mu.Unlock()
	if off != 2 {
		t.Fatalf("Up: scrollOff=%d want 2", off)
	}
}

// TestDiffOverlayMouseWheelScrolls pins the same bug for the wheel: a
// notch must move the overlay, not the transcript underneath it.
func TestDiffOverlayMouseWheelScrolls(t *testing.T) {
	app, scr := newTestApp(t, 120, 40)
	defer scr.Fini()
	ov := openLongDiffOverlay(t, app)

	app.handleKey(tcell.NewEventMouse(10, 10, tcell.WheelDown, tcell.ModNone))
	app.mu.Lock()
	off := ov.scrollOff
	app.mu.Unlock()
	if off != 3 {
		t.Fatalf("wheel down: scrollOff=%d want 3", off)
	}

	app.handleKey(tcell.NewEventMouse(10, 10, tcell.WheelUp, tcell.ModNone))
	app.mu.Lock()
	off = ov.scrollOff
	app.mu.Unlock()
	if off != 0 {
		t.Fatalf("wheel up: scrollOff=%d want 0", off)
	}
}

// TestDiffOverlayScrollVpIsViewportNotContent pins the root cause: open
// used to set scrollVp = len(lines), so maxOff was always 0.
func TestDiffOverlayScrollVpIsViewportNotContent(t *testing.T) {
	app, scr := newTestApp(t, 120, 40)
	defer scr.Fini()

	app.mu.Lock()
	app.blocks = append(app.blocks, &Block{
		Kind: KindToolDone, ToolName: "edit",
		Diff: longDiff(80),
	})
	if !app.openDiffOverlay("f.go") {
		app.mu.Unlock()
		t.Fatal("openDiffOverlay failed")
	}
	ov := app.diffOv
	nLines := len(ov.lines)
	vp := ov.scrollVp
	app.mu.Unlock()

	if nLines <= vp {
		t.Fatalf("need content taller than viewport: lines=%d vp=%d", nLines, vp)
	}
	// A single Down must be able to advance.
	app.diffBodyScroll(1, true)
	app.mu.Lock()
	off := ov.scrollOff
	app.mu.Unlock()
	if off != 1 {
		t.Fatalf("after Down with real vp: scrollOff=%d want 1 (vp was %d, lines %d)", off, vp, nLines)
	}
}
