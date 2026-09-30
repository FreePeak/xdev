package tui

import (
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// A repeated quit chord is a valid terminal event burst: the first Ctrl+C
// closes quitCh, then drainKeys applies the second before Run gets another
// turn. Quit must be idempotent, or that harmless repeat panics the TUI with
// "close of closed channel" and exits the process.
func TestRepeatedQuitChordDoesNotPanic(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.SetHandlers(func(string) {}, func() {}, func() { app.Quit() })

	done := make(chan struct{})
	go func() { app.Run(); close(done) }()
	scr.InjectKey(tcell.KeyCtrlC, 0, tcell.ModCtrl)
	scr.InjectKey(tcell.KeyCtrlC, 0, tcell.ModCtrl)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit after repeated quit chords")
	}
}

// The reported bug (session 2750b48c): the quit chord did nothing for the whole
// of an unbroken turn. `quit` cancelled a running turn and returned, so C-c and
// C-d only ever asked the turn to stop — and a turn that is slow to unwind (a
// recovery ladder, an in-flight tool, an MCP call still draining) never left
// the user with a way out. The chord is the exit chord: it must quit, and take
// the turn with it.
func TestQuitChordExitsWhileATurnRuns(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	var canceled, quit bool
	app.SetHandlers(func(string) {}, func() { canceled = true }, func() { quit = true })
	app.SetRunning(true) // the turn is live: `running` is what used to swallow the chord

	for _, k := range []tcell.Key{tcell.KeyCtrlC, tcell.KeyCtrlD} {
		canceled, quit = false, false
		app.handleKey(tcell.NewEventKey(k, 0, tcell.ModCtrl))
		if !quit {
			t.Fatalf("chord %v did not quit while a turn was running", k)
		}
		if !canceled {
			t.Fatalf("chord %v quit without aborting the live turn", k)
		}
	}
}

// The same chord while idle still quits and cancels nothing: aborting with no
// turn in flight would print a "turn canceled" nobody asked for.
func TestQuitChordWhileIdleOnlyQuits(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	var canceled, quit bool
	app.SetHandlers(func(string) {}, func() { canceled = true }, func() { quit = true })

	app.handleKey(tcell.NewEventKey(tcell.KeyCtrlC, 0, tcell.ModCtrl))
	if !quit || canceled {
		t.Fatalf("idle quit chord: quit=%v canceled=%v", quit, canceled)
	}
}

// Esc is cancel-only and must not take the session down: "stop this turn" and
// "exit" are different requests, and the fix for the chord above is not a
// blanket every-control-key-exits.
func TestEscCancelsWithoutQuitting(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	var canceled, quit bool
	app.SetHandlers(func(string) {}, func() { canceled = true }, func() { quit = true })
	app.SetRunning(true)

	app.handleKey(tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone))
	if !canceled {
		t.Fatal("Esc did not cancel the live turn")
	}
	if quit {
		t.Fatal("Esc quit the session; cancel and quit are different requests")
	}
}

// Detach-on-quit (settings tui.exitDetach, default on): when a turn is
// running and onQuitRunning returns true, the chord must quit WITHOUT
// cancelling — the turn is being handed off, not killed.
func TestQuitChordDetachesWhenConfigured(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	var canceled, quit bool
	app.SetHandlers(func(string) {}, func() { canceled = true }, func() { quit = true })
	app.SetQuitRunning(func() bool { return true })
	app.SetRunning(true)

	app.handleKey(tcell.NewEventKey(tcell.KeyCtrlC, 0, tcell.ModCtrl))
	if !quit {
		t.Fatal("detach quit chord did not quit")
	}
	if canceled {
		t.Fatal("detach quit chord cancelled the turn — the handoff owns it")
	}
}

// When detach is offered but declines (settings off, or handoff failed),
// the chord falls through to cancel-then-quit — the old behaviour.
func TestQuitChordFallsThroughWhenDetachDeclines(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	var canceled, quit bool
	app.SetHandlers(func(string) {}, func() { canceled = true }, func() { quit = true })
	app.SetQuitRunning(func() bool { return false })
	app.SetRunning(true)

	app.handleKey(tcell.NewEventKey(tcell.KeyCtrlC, 0, tcell.ModCtrl))
	if !quit || !canceled {
		t.Fatalf("decline detach: quit=%v canceled=%v", quit, canceled)
	}
}
