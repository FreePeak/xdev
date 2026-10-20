package tui

import (
	"runtime"
	"strings"
	"testing"
)

// The row cap is a budget on what the READER scrolls. The render work that
// fills the window has to be bounded by it too.
//
// Rendering every block of a long session and then dropping the ones the cap
// retires is what put a 19 MB session's first paint at 1.5 GB of allocations
// (measured 2026-10-20 with a pprof build of the shipped binary): the Go heap
// ballooned to ~450 MB between GCs and RSS to ~500 MB. The window has to be
// decided BEFORE the render, so the blocks the cap would drop are never
// rendered at all.
func TestSyncBoundsTheRenderWork(t *testing.T) {
	app := idxApp(120, 40)
	// 200 tool results of 400 lines each: 80 000 source rows, far past the
	// 10 000-row cap, and every one of them a body the renderer would style.
	body := strings.Repeat("output line of a command result 1234567890\n", 400)
	for range 200 {
		app.AddToolBlock("", "bash", `{"command":"go test ./..."}`)
		app.FinishTool("", "bash", false, body, ToolOutcome{Dur: "70ms"})
	}
	w := app.contentWidth()

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	app.sync(w)
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc

	// The window is 10 000 rows; a row costs a few hundred bytes to render, so
	// a bounded sync allocates tens of megabytes, not gigabytes.
	const budget = 64 << 20
	if allocated > budget {
		t.Fatalf("one sync allocated %d MB for a %d-block transcript, budget is %d MB",
			allocated>>20, len(app.blocks), budget>>20)
	}
	t.Logf("sync allocated %d MB, transcript kept %d blocks", allocated>>20, len(app.blocks))
}

// The bound must not cost the reader anything: the window that survives is
// the newest one, and the rows the cap retires are gone from the view.
func TestSyncWindowKeepsTheNewestBlocks(t *testing.T) {
	app := idxApp(120, 40)
	capFill(app, 400, 60)
	app.sync(app.contentWidth())

	app.mu.Lock()
	defer app.mu.Unlock()
	if total := app.rowIdx.total(); total > transcriptCap {
		t.Fatalf("transcript kept %d rows, cap is %d", total, transcriptCap)
	}
	if len(app.blocks) == 0 {
		t.Fatal("the window is empty")
	}
	last := app.blocks[len(app.blocks)-1]
	if !strings.Contains(last.Text, "L") {
		t.Fatalf("the newest block was dropped: %q", last.Text)
	}
	if app.droppedUserRows == 0 {
		t.Fatal("no prompt was retired, so the ordinals cannot stay absolute")
	}
}
