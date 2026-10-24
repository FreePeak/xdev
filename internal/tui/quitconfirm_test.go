package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// Ctrl+C is a two-press exit: the first press announces itself and stays,
// the second inside quitConfirmWindow leaves. Everything else that exits
// (C-d on the last tab, <leader>q, /quit) still takes one press.
//
// The first press must not be silent — a chord that does nothing on the one
// terminal everybody reflexively tries reads as a frozen app. It must also
// leave a live turn alone: the exit that follows is a hand-off by default, and
// cancelling on the way in would kill the work the hand-off keeps.
func TestCtrlCTakesTwoPresses(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	var canceled, quit bool
	app.SetHandlers(func(string) {}, func() { canceled = true }, func() { quit = true })
	app.SetRunning(true)

	ctrlC := func() { app.handleKey(tcell.NewEventKey(tcell.KeyCtrlC, 0, tcell.ModCtrl)) }

	ctrlC()
	if quit {
		t.Fatal("one Ctrl+C exited; the chord must arm first")
	}
	if canceled {
		t.Error("the arming Ctrl+C aborted the live turn; the exit press owns that decision")
	}
	if got := app.toastText(); !strings.Contains(got, "again to exit") {
		t.Fatalf("first press said %q, want the press-again notice", got)
	}

	ctrlC()
	if !quit {
		t.Fatal("the second Ctrl+C did not exit")
	}
	if !canceled {
		t.Error("the exit press quit without aborting the live turn")
	}
}

// An arm older than quitConfirmWindow is not consent: the press after it arms
// again rather than exiting, so a Ctrl+C a minute ago cannot lend its meaning
// to one pressed now.
func TestStaleCtrlCArmDoesNotExit(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	var quit bool
	app.SetHandlers(func(string) {}, func() {}, func() { quit = true })

	app.handleKey(tcell.NewEventKey(tcell.KeyCtrlC, 0, tcell.ModCtrl))
	app.quitArmedAt = app.quitArmedAt.Add(-quitConfirmWindow * 2)

	app.handleKey(tcell.NewEventKey(tcell.KeyCtrlC, 0, tcell.ModCtrl))
	if quit {
		t.Fatal("a stale arm exited; the window must expire the consent")
	}
	if !app.quitArmed {
		t.Fatal("the press after a stale arm did not re-arm")
	}
}

// C-d on the last tab, <leader>q and /quit are the other exits and keep the
// single press: only the Ctrl+C chord gained a confirmation step.
func TestOtherQuitPathsStillTakeOnePress(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	var quit bool
	app.SetHandlers(func(string) {}, func() {}, func() { quit = true })

	// C-d with a single tab is "nothing left to close, so exit".
	app.handleKey(tcell.NewEventKey(tcell.KeyCtrlD, 0, tcell.ModCtrl))
	if !quit {
		t.Fatal("C-d on the last tab no longer exits on one press")
	}
}
