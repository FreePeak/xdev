package tui

import "testing"

// The CPU floor the report was about: an xdev left open must be able to reach
// zero. A session parked mid-turn is the only thing left that repaints on its
// own, and it repaints on the animation cadence rather than on the loop's own.
// Both numbers are asserted here, so a future edit that ties the animation back
// to the tick has to say so out loud.

// TestTickAnimateIsIdleFPS: one animation frame per animEvery ticks, and no
// more. The loop's own cadence is unchanged; only the animation is divided.
func TestTickAnimateIsIdleFPS(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.SetRunning(true) // takes a.mu itself, so it goes before the lock below
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
		t.Fatal("every tick moved a frame: the animation is back on the loop's own cadence")
	}
}

// TestWelcomeScreenAnimatesNothing: the start screen is static. An empty
// transcript has nothing to spin and no logo sweep, so it moves no frame at
// all — this is the zero the report asked for, and the reason a session parked
// on the welcome reaches true idle instead of animating a logo forever.
func TestWelcomeScreenAnimatesNothing(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.blocks) != 0 {
		t.Fatal("the welcome screen is the empty transcript")
	}
	for range 20 * animEvery {
		if app.tickAnimate() {
			t.Fatal("the welcome screen moved a frame: it has no animation left to run")
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
}
