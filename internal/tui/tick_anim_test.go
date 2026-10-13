package tui

import "testing"

// The CPU floor the report was about: an xdev left open must be able to reach
// zero. Two things used to deny it — an empty session repainting 30 times a
// second for the logo's sheen, and a session parked mid-turn repainting 30
// times a second for the spinner. Both numbers are asserted here, so a future
// edit that ties the animations back to the tick has to say so out loud.

// TestTickAnimateIsIdleFPS: one animation frame per animEvery ticks, and no
// more. The loop's own cadence is unchanged; only the animations are divided.
func TestTickAnimateIsIdleFPS(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.AddUserBlock("hi") // a real turn: one block, so the sheen is not the mover
	app.SetRunning(true)   // takes a.mu itself, so it goes before the lock below
	app.mu.Lock()
	defer app.mu.Unlock()

	moved := 0
	const ticks = 100
	for range ticks {
		if app.tickAnimate() {
			moved++
		}
	}
	// One frame per animEvery tick, allowing for the phase the counter starts
	// in: the ratio is what matters, not the offset.
	want := ticks / animEvery
	if moved != want {
		t.Fatalf("ticks moved %d of %d frames, want %d (animEvery=%d)", moved, ticks, want, animEvery)
	}
	if moved == ticks {
		t.Fatal("every tick moved a frame: the animations are back on the loop's own cadence")
	}
}

// TestSheenIsBounded: the welcome sheen sweeps once and settles, so a session
// nobody touches on the start screen reaches true idle. A spinner may not be
// bounded — an open turn is working — but a logo forever animating is the cost
// the report named.
func TestSheenIsBounded(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.blocks) != 0 {
		t.Fatal("the welcome screen is the empty transcript")
	}

	// The phase advances once per animEvery ticks, so the sweep needs that
	// many ticks per column plus slack to prove it stopped.
	frames := 0
	ticks := sheenSweep()*animEvery + 10*animEvery
	for range ticks {
		if app.tickAnimate() {
			frames++
		}
	}
	if got, want := app.sheenPhase, sheenSweep(); got != want {
		t.Fatalf("sheen phase settled at %d, want the sweep's end %d", got, want)
	}
	if frames > sheenSweep() {
		t.Fatalf("the sheen moved on %d frames, more than one column per animation frame (%d)", frames, sheenSweep())
	}

	// Past the sweep, an empty session moves nothing at all: this is the zero
	// the report asked for.
	for range 20 * animEvery {
		if app.tickAnimate() {
			t.Fatal("an idle welcome screen still animates after the sweep")
		}
	}
}

// TestTickAnimatePrefersTheSpinner: while a turn is open the spinner is the
// mover even with an empty transcript, so a session that starts running before
// its first block still shows the turn.
func TestTickAnimatePrefersTheSpinner(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.SetRunning(true)
	app.mu.Lock()
	defer app.mu.Unlock()
	before := app.st.spinnerIdx
	for range animEvery {
		app.tickAnimate()
	}
	if app.st.spinnerIdx == before {
		t.Fatal("a running session must advance its spinner")
	}
	if app.sheenPhase != 0 {
		t.Fatalf("the sheen advanced (%d) while a turn was running", app.sheenPhase)
	}
}
