package tui

import (
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// The registry lock guards a process-wide map, so one leaked unlock is not a
// stalled feature — it is the entire UI thread: draw() calls hubState() every
// frame, and the next frame after the leak never comes (no keys, no Ctrl+C,
// and only a kill clears it). Four empty-roster paths returned while holding
// it. The stall watchdog proved it from the field: goroutine 1 parked in
// hubState (hubroster.go:73) under draw.

func TestHubRosterEmptyRowKeysReleaseTheRegistry(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	app.SetHubOps(&HubOps{Roster: func() []HubAgent { return nil }})
	if err := app.HubRoster(); err != nil {
		t.Fatal(err)
	}
	// Every key that has no row to act on: Enter, and the k/r/p chords.
	for _, ev := range []*tcell.EventKey{
		tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone),
		tcell.NewEventKey(tcell.KeyRune, 'k', tcell.ModNone),
		tcell.NewEventKey(tcell.KeyRune, 'r', tcell.ModNone),
		tcell.NewEventKey(tcell.KeyRune, 'p', tcell.ModNone),
	} {
		if !app.handleHubRosterKey(ev) {
			t.Fatalf("key %v not claimed by the open roster", ev)
		}
		if !hubRegMu.TryLock() {
			t.Fatalf("key %v leaked hubRegMu — the next draw() would freeze the TUI", ev)
		}
		hubRegMu.Unlock()
		// And the frame that follows must actually complete.
		drawn := make(chan struct{})
		go func() { app.draw(); close(drawn) }()
		select {
		case <-drawn:
		case <-time.After(5 * time.Second):
			t.Fatalf("draw() hung after key %v: frozen UI thread", ev)
		}
	}
	scr.Fini()
}

// The defer-based unlock replaced a manual one at the tail; a key that falls
// through normally must release exactly once (a double unlock panics the UI
// thread, which is the same outage by a different route).
func TestHubRosterNormalKeyReleasesOnce(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	f := &hubFixture{rows: rosterRows()}
	app.SetHubOps(f.ops())
	if err := app.HubRoster(); err != nil {
		t.Fatal(err)
	}
	for _, k := range []tcell.Key{tcell.KeyDown, tcell.KeyUp, tcell.KeyEsc} {
		if !pressRosterKey(app, k, 0) {
			t.Fatalf("key %v not handled by the open roster", k)
		}
	}
	if !hubRegMu.TryLock() {
		t.Fatal("hubRegMu still held after normal roster keys")
	}
	hubRegMu.Unlock()
}
