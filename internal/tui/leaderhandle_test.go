package tui

import (
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// The unit tests in leaderkeys_test.go call KeyMap.resolve directly. These go
// through App.handleKey instead — the same entry the Run loop uses — because
// that is where the leader actually broke: handleKey called Resolve twice on
// one event, and the second call disarmed the prefix the first had just armed.
// Every pair was dead in the real UI while the map looked perfect in a test.

// Ctrl+X then L opens the session list.
func TestHandleKeyLeaderPairOpensSessionList(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	app.SetTabs([]TabInfo{
		{ID: "aaaa1111", Title: "first", Current: true},
		{ID: "bbbb2222", Title: "second"},
	})
	app.SetTabPick(func(id string) error { return nil })

	app.handleKey(tcell.NewEventKey(tcell.KeyCtrlX, 0, tcell.ModCtrl))
	app.handleKey(tcell.NewEventKey(tcell.KeyRune, 'l', tcell.ModNone))

	if !app.PickerOpen() {
		t.Fatal("Ctrl+X L did not open the session list")
	}
}

// Ctrl+X then W closes the current tab, through the same path.
func TestHandleKeyLeaderWClosesTab(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	app.SetTabs([]TabInfo{
		{ID: "aaaa1111", Title: "first"},
		{ID: "bbbb2222", Title: "second", Current: true},
	})
	closed := ""
	app.SetTabClose(func(id string) error { closed = id; return nil })

	app.handleKey(tcell.NewEventKey(tcell.KeyCtrlX, 0, tcell.ModCtrl))
	app.handleKey(tcell.NewEventKey(tcell.KeyRune, 'w', tcell.ModNone))

	if closed != "bbbb2222" {
		t.Fatalf("Ctrl+X W closed %q, want the current tab", closed)
	}
}

// The pair typed past the timeout must not fire: a user who pauses mid-chord
// and then gives up does not get the action they abandoned.
func TestHandleKeyLeaderPairExpires(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	app.SetTabs([]TabInfo{{ID: "a", Current: true}, {ID: "b"}})
	app.SetTabPick(func(id string) error { return nil })

	app.handleKey(tcell.NewEventKey(tcell.KeyCtrlX, 0, tcell.ModCtrl))
	if !app.keyMap.ExpirePending(time.Now().Add(leaderTimeout + time.Second)) {
		t.Fatal("the armed prefix should expire on the UI tick")
	}
	app.handleKey(tcell.NewEventKey(tcell.KeyRune, 'l', tcell.ModNone))
	if app.PickerOpen() {
		t.Fatal("an expired leader pair fired its action")
	}
}

// A follow-up that is not a pair must do its OWN job. Ctrl+X Ctrl+P is
// model-cycle, not a swallowed key and not a literal "^P" in the composer.
func TestHandleKeyLeaderFallsThroughToItsOwnBinding(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	app.handleKey(tcell.NewEventKey(tcell.KeyCtrlX, 0, tcell.ModCtrl))
	app.handleKey(tcell.NewEventKey(tcell.KeyCtrlP, 0, tcell.ModCtrl))
	if got := app.ed.Text(); got != "" {
		t.Fatalf("Ctrl+X Ctrl+P reached the editor as %q", got)
	}
}

// A bare Ctrl+X must not leave a stray glyph in the composer either: it is a
// prefix, and a prefix that types a character is worse than one that does
// nothing.
func TestHandleKeyLeaderPrefixTypesNothing(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	app.handleKey(tcell.NewEventKey(tcell.KeyCtrlX, 0, tcell.ModCtrl))
	if got := app.ed.Text(); got != "" {
		t.Fatalf("Ctrl+X typed %q into the composer", got)
	}
}
