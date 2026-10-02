package tui

import (
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// TestDiffOverlayClickCannotDeadlock pins the reported freeze: a click on a
// dock FILES row stopped the UI thread dead.
//
// handleMouse's non-wheel branch in app.go runs the whole gesture with a.mu
// held; the diff-overlay helpers it reaches (openDiffOverlay /
// closeDiffOverlay) used to take a.mu again on the click path, so the first
// dock FILES click self-deadlocked. A non-reentrant goroutine is the only way
// to reproduce it for real — on the UI goroutine the whole test would simply
// hang — so the gesture runs here with no lock held from outside.
//
// It is driven through handleKey (the path the UI loop uses) rather than a
// direct handleMouse call, so the test fails should that wrapping lock ever
// go away and the helpers' own lock come back.
func TestDiffOverlayClickCannotDeadlock(t *testing.T) {
	app, scr := newTestApp(t, 120, 30)
	defer scr.Fini()

	app.mu.Lock()
	left, y, path := withDockRows(app)
	app.mu.Unlock()
	if path == "" {
		t.Skip("dock layout doesn't expose a clickable row")
	}

	// Open the overlay through the real key path, then click a dock row with
	// it up (which re-points it) and click outside (which closes it). If any
	// of those takes a.mu twice, the gesture never returns.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2; i++ {
			app.handleKey(tcell.NewEventKey(tcell.KeyRune, 'x', tcell.ModNone))
			press := tcell.NewEventMouse(left+5, y, tcell.Button1, tcell.ModNone)
			release := tcell.NewEventMouse(left+5, y, tcell.ButtonNone, tcell.ModNone)
			app.handleKey(press)
			app.handleKey(release)
		}
		app.handleKey(tcell.NewEventMouse(2, app.height-3, tcell.Button1, tcell.ModNone))
		app.handleKey(tcell.NewEventMouse(2, app.height-3, tcell.ButtonNone, tcell.ModNone))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("dock FILES click deadlocked: the UI thread never came back")
	}

	// The mouse path needs the overlay open first for the re-point/close
	// branches to run; open it directly (under the lock) and repeat the
	// gesture, so the closeDiffOverlayOnClick branch is exercised too.
	app.mu.Lock()
	app.openDiffOverlay(path)
	app.mu.Unlock()

	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		app.handleKey(tcell.NewEventMouse(left+5, y, tcell.Button1, tcell.ModNone))
		app.handleKey(tcell.NewEventMouse(left+5, y, tcell.ButtonNone, tcell.ModNone))
	}()
	select {
	case <-done2:
	case <-time.After(5 * time.Second):
		t.Fatal("dock FILES click with the overlay open deadlocked")
	}
	if !app.diffOverlayOpen() {
		t.Fatal("clicking the same dock row closed the overlay instead of re-pointing it")
	}
}

// TestDiffOverlayCloseIsSafeUnderTheLock pins the other direction: the key
// and click paths that dismiss the overlay already hold a.mu, so closing must
// not take it again — the same wedge, reached from Esc.
func TestDiffOverlayCloseIsSafeUnderTheLock(t *testing.T) {
	app, scr := newTestApp(t, 120, 30)
	defer scr.Fini()

	app.mu.Lock()
	_, _, path := withDockRows(app)
	if path == "" {
		app.mu.Unlock()
		t.Skip("dock layout doesn't expose a clickable row")
	}
	app.openDiffOverlay(path)
	app.mu.Unlock()
	if !app.diffOverlayOpen() {
		t.Fatal("overlay did not open")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		app.handleKey(tcell.NewEventKey(tcell.KeyEsc, 0, tcell.ModNone))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Esc on the diff overlay deadlocked")
	}
	if app.diffOverlayOpen() {
		t.Fatal("Esc did not close the overlay")
	}
}
