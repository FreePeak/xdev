package tui

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/theme"

	"github.com/gdamore/tcell/v2"
)

// flushScreen counts frames, charges each Show() what a real tty flush costs,
// and snapshots the composer from inside the flush. The simulation screen's own
// Show() is nearly free, so without an injected cost a frame-per-event bug reads
// as noise instead of the latency a terminal user actually feels. The snapshot
// runs on the UI goroutine (Show is called from the UI loop) because the editor
// is UI-thread-owned — reading it from the test goroutine would race the loop.
type flushScreen struct {
	tcell.SimulationScreen
	shows atomic.Int64
	flush time.Duration
	snap  func() string

	mu       sync.Mutex
	lastText string
}

func (f *flushScreen) Show() {
	if f.snap != nil {
		t := f.snap()
		f.mu.Lock()
		f.lastText = t
		f.mu.Unlock()
	}
	f.shows.Add(1)
	if f.flush > 0 {
		time.Sleep(f.flush)
	}
	f.SimulationScreen.Show()
}

func (f *flushScreen) composer() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastText
}

// TestScrollBurstCostsOneFrameNotOnePerEvent pins the fix for "scroll a lot,
// then type — the composer lags": the UI loop painted once per input event, so
// a wheel flick queued a whole burst of frames ahead of the keystroke typed
// behind it (600 notches ≈ 900 Show() calls, each a synchronous tty write).
// A burst must cost a frame per loop iteration, not a frame per event, and the
// keystroke behind it must land on the next frame — so with each flush charged
// 1ms, the frame count below is the wait the user feels (293 frames pre-fix).
func TestScrollBurstCostsOneFrameNotOnePerEvent(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatal(err)
	}
	sim.SetSize(120, 40)
	scr := &flushScreen{SimulationScreen: sim, flush: time.Millisecond}
	app := New(scr, theme.Load("groknight"), "test/free", "sess1234")
	app.SetHandlers(func(string) {}, func() {}, func() {})
	app.width, app.height = 120, 40
	idxFill(app, 200, 60) // a long transcript: scrolling it is the reported case
	scr.snap = app.ed.Text

	done := make(chan struct{})
	go func() { app.Run(); close(done) }()
	// Quit and wait for Run to return before touching the screen again: a flush
	// still in flight would race the Fini below.
	defer func() {
		app.Quit()
		<-done
		sim.Fini()
	}()

	// Wait for the loop's opening frame, then start counting.
	waitFor(t, "the UI loop to paint", func() bool { return scr.shows.Load() > 0 })
	frames := scr.shows.Load()

	const notches = 200
	for range notches {
		sim.InjectMouse(10, 20, tcell.WheelUp, tcell.ModNone)
	}
	// The keystroke typed straight after the flick. It queues behind the burst,
	// so it can only be echoed once the backlog is caught up.
	const typed = "ZQ9"
	for _, ch := range typed {
		sim.InjectKey(tcell.KeyRune, ch, tcell.ModNone)
	}
	waitFor(t, "the keystroke to reach the composer", func() bool {
		return strings.Contains(scr.composer(), typed)
	})

	// The burst must have scrolled — an unhandled wheel event would also be
	// cheap, and the test would pass while proving nothing.
	app.mu.Lock()
	follow, off := app.sm.Following(), app.sm.offset
	app.mu.Unlock()
	if follow || off == 0 {
		t.Fatalf("wheel burst must leave the live edge: follow=%v offset=%d", follow, off)
	}
	// The gesture plus the echo must fit in a handful of frames. Pre-fix this
	// was one frame per event; the budget leaves room for a burst split across
	// loop iterations without admitting that.
	if got := scr.shows.Load() - frames; got > 24 {
		t.Fatalf("%d wheel notches + typing cost %d frames, want a frame per burst (<= 24)", notches, got)
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Microsecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
