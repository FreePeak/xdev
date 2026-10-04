package tui

import (
	"path/filepath"
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

// The written spelling and the delivered chord must be the SAME string. A
// keybindings.yml line that normalizes to something no event can render loads
// without complaint and binds nothing — which is how opencode's own spelling
// of "alt+shift+down" and "ctrl+shift+tab" was silently dead. These are the
// bytes a terminal actually sends, fed through tcell's own input processor
// rather than hand-built events: what a chord renders is a fact about tcell,
// not about this table.
func TestWrittenChordsReachTheDeliveredChord(t *testing.T) {
	for _, tc := range []struct {
		written string
		bytes   string
	}{
		{"ctrl+tab", "\x1b[9;5u"},
		{"ctrl+shift+tab", "\x1b[9;6u"},
		{"shift+tab", "\x1b[Z"},
		{"alt+shift+down", "\x1b[1;4B"},
		{"alt+shift+up", "\x1b[1;4A"},
		{"alt+down", "\x1b[1;3B"},
		{"alt+up", "\x1b[1;3A"},
		{"shift+up", "\x1b[1;2A"},
		{"shift+ctrl+down", "\x1b[1;6B"},
		{"alt+ctrl+down", "\x1b[1;7B"},
		{"ctrl+3", "\x1b[51;5u"},
		{"ctrl+w", "\x17"},
		{"alt+,", "\x1b,"},
		{"f5", "\x1b[15~"},
	} {
		ch := make(chan tcell.Event, 4)
		tcell.NewInputProcessor(ch).(interface{ ScanUTF8([]byte) }).ScanUTF8([]byte(tc.bytes))
		ev, ok := <-ch
		if !ok {
			t.Errorf("%s: tcell dropped %q", tc.written, tc.bytes)
			continue
		}
		key, isKey := ev.(*tcell.EventKey)
		if !isKey {
			t.Errorf("%s: %q decoded to %T, not a key", tc.written, tc.bytes, ev)
			continue
		}
		if want, got := normalizeChord(tc.written), chordOf(key); want != got {
			t.Errorf("normalizeChord(%q) = %q, but the terminal's bytes render %q",
				tc.written, want, got)
		}
	}
}

// …and the end of that chain: a config pasted from an opencode tui.json has
// to reach the ACTION, not merely a table string that happens to match.
func TestOpencodeSpellingInKeybindingsReachesTheAction(t *testing.T) {
	home := t.TempDir()
	mk(t, home, "keybindings.yml", `
session.tab.next: ["alt+shift+down"]
session.tab.previous: ["ctrl+shift+tab"]
retry: ["f5"]
thinking-toggle: ["shift+tab"]
`)
	t.Setenv("HOME", home)
	t.Setenv("XDEV_AGENT_DIR", filepath.Join(home, ".xdev", "agent"))
	m, err := LoadKeyMap()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		written string
		bytes   string
		want    string
	}{
		{"alt+shift+down", "\x1b[1;4B", "session.tab.next"},
		{"ctrl+shift+tab", "\x1b[9;6u", "session.tab.previous"},
		{"f5", "\x1b[15~", "retry"},
		{"shift+tab", "\x1b[Z", "thinking-toggle"},
	} {
		ch := make(chan tcell.Event, 4)
		tcell.NewInputProcessor(ch).(interface{ ScanUTF8([]byte) }).ScanUTF8([]byte(tc.bytes))
		key := (<-ch).(*tcell.EventKey)
		if got := m.Resolve(key); got != tc.want {
			t.Errorf("%q resolved to %q, want %q", tc.written, got, tc.want)
		}
	}
}
