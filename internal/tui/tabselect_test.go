package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// opencode's tab chords, as its own keybind table spells them: Ctrl+Tab
// / Ctrl+Shift+Tab cycle, Ctrl+1..9 and <leader>1..9 jump to tab N (0 is the
// tenth). xdev had only the Alt+[ / Alt+] pair, so a user arriving from
// opencode pressed chords that resolved to nothing at all — and Ctrl+1..9
// could not even be NAMED, because keyName returned "" for a digit, which
// made every one of those bindings a listed chord the runtime ignored.
func TestOpencodeTabChordsResolve(t *testing.T) {
	m := DefaultKeyMap()
	for _, tc := range []struct {
		ev   *tcell.EventKey
		want string
	}{
		{tcell.NewEventKey(tcell.KeyTab, 0, tcell.ModCtrl), "session.tab.next"},
		{tcell.NewEventKey(tcell.KeyTab, 0, tcell.ModCtrl|tcell.ModShift), "session.tab.previous"},
		{tcell.NewEventKey(tcell.KeyRune, '2', tcell.ModCtrl), tabSelectAction(2)},
		{tcell.NewEventKey(tcell.KeyRune, '0', tcell.ModCtrl), tabSelectAction(10)},
	} {
		if got := m.Resolve(tc.ev); got != tc.want {
			t.Errorf("chordOf(%s) resolved to %q, want %q", chordOf(tc.ev), got, tc.want)
		}
	}
	// The <leader> spelling of the same jumps — opencode binds both.
	now := time.Now()
	m.resolve("C-x", now)
	if got := m.resolve("3", now); got != tabSelectAction(3) {
		t.Fatalf("C-x 3 = %q, want %s", got, tabSelectAction(3))
	}
}

// The tenth tab is on 0, exactly as opencode spells it, and every id exists
// in BuiltinActions so /hotkeys lists it and keybindings.yml can name it.
func TestTabSelectIdsAreKnownActions(t *testing.T) {
	for n := 1; n <= 10; n++ {
		if !isKnownAction(tabSelectAction(n)) {
			t.Errorf("%s is not a known action, so keybindings.yml cannot rebind it", tabSelectAction(n))
		}
	}
	if got := DefaultKeyMap().Chords(tabSelectAction(10)); len(got) == 0 {
		t.Fatal("the tenth tab has no chord")
	}
}

// Ctrl+3 on a host with two sessions open must say so, not no-op — and a
// slot that IS open must focus that session through the same onTabPick the
// strip's click and /tabs use, so the three paths cannot diverge.
func TestSelectTabJumpsThroughTheSharedCallback(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	picked := make(chan string, 4)
	app.SetTabPick(func(id string) error { picked <- id; return nil })
	app.SetTabs([]TabInfo{
		{ID: "aaa", Title: "first", Current: true},
		{ID: "bbb", Title: "second"},
	})

	app.handleKey(tcell.NewEventKey(tcell.KeyRune, '2', tcell.ModCtrl))
	select {
	case got := <-picked:
		if got != "bbb" {
			t.Fatalf("Ctrl+2 focused %q, want bbb", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ctrl+2 focused nothing")
	}

	app.handleKey(tcell.NewEventKey(tcell.KeyRune, '9', tcell.ModCtrl))
	deadline := time.After(2 * time.Second)
	for !hasNotice(app, "only 2 sessions open") {
		select {
		case got := <-picked:
			t.Fatalf("Ctrl+9 focused %q with two sessions open; it must say the slot is closed", got)
		case <-deadline:
			t.Fatal("Ctrl+9 with two sessions open was a silent no-op")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// hasNotice reports whether the transcript carries a system block with want
// in it — the "this chord did nothing" path's only evidence.
func hasNotice(app *App, want string) bool {
	blocks := app.Blocks()
	if len(blocks) == 0 {
		return false
	}
	last := blocks[len(blocks)-1]
	return last.Kind == KindSystem && strings.Contains(last.Text, want)
}
