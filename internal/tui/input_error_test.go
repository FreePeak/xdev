package tui

import (
	"errors"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// A tty read error ends tcell's input loop forever: tscreen.go's inputLoop
// posts one EventError and returns, so PollEvent never yields another event
// again. App.Run's poll goroutine only exits on a nil event (tcell returns nil
// solely from StopQ), so it parked forever — and even if it had delivered the
// event, handleKey ignores everything that is not a key, a resize or a mouse
// (app.go), so the error would have been dropped on the floor.
//
// The result is the "the whole TUI froze, I had to kill it" report: the
// process is alive and painting, the terminal is still in raw mode and the
// alt screen, and no key, resize or quit chord does anything. Nothing in the
// process can recover, because the only thing that could read the terminal is
// the goroutine that just died.
//
// There is no partial credit here: once the input side is gone the UI is
// undrivable, and the honest end is to leave the loop so runTUI's defers put
// the terminal back and the user relaunches. Staying alive would be a zombie
// on a dead terminal.
func TestTtyReadErrorEndsTheUILoopInsteadOfWedgingIt(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.SetHandlers(func(string) {}, func() {}, func() { app.Quit() })

	done := make(chan struct{})
	go func() { app.Run(); close(done) }()

	// A read error on the tty: tcell's inputLoop posts exactly this and
	// then never reads again.
	if err := scr.PostEvent(tcell.NewEventError(errors.New("input/output error"))); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run kept running with a dead input loop: no key, resize or quit can ever arrive again")
	}
}

// The guard the other way: the error is the ONLY event that ends the loop.
// Every other event shape must still be delivered, or this fix would trade a
// wedge for a TUI that ignores its own resize events (or drops keystrokes).
func TestOnlyTheReadErrorEndsTheUILoop(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	var sent []string
	app.SetHandlers(func(text string) { sent = append(sent, text) }, func() {}, func() { app.Quit() })

	done := make(chan struct{})
	go func() { app.Run(); close(done) }()

	// A resize is an ordinary event, and tcell sends it constantly.
	scr.SetSize(100, 30)
	if err := scr.PostEvent(tcell.NewEventResize(100, 30)); err != nil {
		t.Fatal(err)
	}
	// ... and a keystroke still has to reach the composer.
	for _, r := range "hi" {
		if err := scr.PostEvent(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone)); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-done:
			t.Fatal("Run exited on a resize or a keystroke")
		case <-deadline:
			app.mu.Lock()
			text := app.ed.Text()
			app.mu.Unlock()
			if text != "hi" {
				t.Fatalf("composer = %q, want %q: an ordinary event was swallowed", text, "hi")
			}
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}
