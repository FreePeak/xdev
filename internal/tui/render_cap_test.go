package tui

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// capBodies is a tool result far longer than the head+tail render window
// (toolRecentHead + toolRecentTail), so every block frames 250 rows and the
// pass below is measured on the window, not on a short result.
const capBodies = 400

// capTranscript fills the transcript with n tool results, each one a result
// long enough to be windowed.
func capTranscript(app *App, n int) {
	body := strings.Repeat("output line of command result 1234567890\n", capBodies)
	for range n {
		app.AddToolBlock("", "bash", `{"command":"go test ./..."}`)
		app.FinishTool("", "bash", false, body, ToolOutcome{Dur: "70ms"})
	}
}

// The row cap is the whole bound on the transcript's renders — the PRD says so
// (§3.7: "Transcript renders (row index) | 2–6 MB … the 10 000-row cap is the
// whole bound"). So the pass that RENDERS must not hold more renders than the
// cap keeps, even for the frames between "the cap has decided" and "the trim
// ran".
//
// It used to: the pass rendered every block in the session and the trim
// dropped the old ones afterwards, so a first paint of a long session held
// the whole session's renders at once — 204 000 lines for this 800-block
// transcript, against a cap that keeps 40 of those blocks.
func TestRenderPassHoldsOnlyTheWindowWorthOfRenders(t *testing.T) {
	app := idxApp(120, 40)
	capTranscript(app, 800)

	// reconcile is the pass that renders every block; the trim that follows it
	// in sync() is what drops the old ones. The bound belongs to the pass.
	// The setup mirrors sync()'s, so the pass runs on a grown index.
	w := app.contentWidth()
	app.rowIdx.w = w
	app.rowIdx.reset()
	app.rowIdx.grow(len(app.blocks))
	app.reconcile(w, 0)

	held := 0
	for i := range app.rowIdx.rend {
		held += len(app.rowIdx.rend[i].lines)
	}
	t.Logf("the pass held %d lines", held)
	// The cap is a ROW budget, and a block's rows are its lines plus the
	// separator row, so the held lines are within one block of the cap.
	const slack = 4 * (toolRecentHead + toolRecentTail)
	if held > transcriptCap+slack {
		t.Fatalf("the render pass held %d lines, the cap is %d rows", held, transcriptCap)
	}
}

// The same bound, seen the way the user sees it: the live heap during a first
// paint of a long transcript must stay inside the PRD's <100 MB RSS budget.
//
// This is the symptom the row cap was supposed to prevent — a resumed session
// that took the process to 500 MB — so it is pinned on the live heap rather
// than on the internal table.
func TestFirstPaintOfALongTranscriptStaysInsideTheMemoryBudget(t *testing.T) {
	app := idxApp(120, 40)
	capTranscript(app, 800)

	peak := peakHeapDuring(t, func() { app.sync(app.contentWidth()) })
	// The fixture's own bodies are ~12 MB and the app is a few MB more, so a
	// pass that holds the window's renders and nothing more lands far below
	// the budget. Before the fix this measured 197 MB.
	t.Logf("the first paint peaked at %d MB", peak>>20)
	const budget = 100 << 20
	if peak > budget {
		t.Fatalf("the first paint peaked at %d MB, the budget is %d MB", peak>>20, budget>>20)
	}
}

// peakHeapDuring runs fn and returns the highest live heap it reached.
func peakHeapDuring(t *testing.T, fn func()) uint64 {
	t.Helper()
	done := make(chan struct{})
	peak := make(chan uint64, 1)
	go func() {
		var high uint64
		for {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > high {
				high = m.HeapAlloc
			}
			select {
			case <-done:
				peak <- high
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	fn()
	close(done)
	return <-peak
}
