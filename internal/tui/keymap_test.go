package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

func mk(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".xdev", "agent", name), []byte(body), 0o644); err != nil {
		// create the parent dir
		os.MkdirAll(filepath.Dir(filepath.Join(dir, ".xdev", "agent", name)), 0o755)
		os.WriteFile(filepath.Join(dir, ".xdev", "agent", name), []byte(body), 0o644)
	}
}

func TestDefaultKeyMapResolvesAllActions(t *testing.T) {
	m := DefaultKeyMap()
	// Actions intentionally without a default chord (context disambiguates
	// or the binding is user-supplied only) still resolve; every other
	// builtin must have a factory chord.
	unbound := map[string]bool{
		"history-next": true, "abort": true, "complete": true,
		"dismiss-menu": true, "newline": true,
	}
	for _, action := range BuiltinActions {
		if unbound[action] {
			continue
		}
		if m.Chord(action) == "" {
			t.Errorf("default map has no chord for %q", action)
		}
	}
}

func TestChordOf(t *testing.T) {
	tests := []struct {
		name string
		ev   *tcell.EventKey
		want string
	}{
		{"Enter", tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone), "Enter"},
		{"Ctrl+q", tcell.NewEventKey(tcell.KeyRune, 'q', tcell.ModCtrl), "C-q"},
		{"Escape", tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone), "Escape"},
		{"Shift+Up", tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModShift), "S-Up"},
		{"Alt+Backspace", tcell.NewEventKey(tcell.KeyBackspace, 0, tcell.ModAlt), "A-Backspace"},
		{"plain rune", tcell.NewEventKey(tcell.KeyRune, 'a', tcell.ModNone), "a"},
		{"unknown", tcell.NewEventKey(tcell.KeyF1, 0, tcell.ModNone), ""},
	}
	for _, tc := range tests {
		if got := chordOf(tc.ev); got != tc.want {
			t.Errorf("chordOf(%s) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestResolveMenuNavigation(t *testing.T) {
	m := DefaultKeyMap()
	// Tab accepts, Up/Down navigate, Escape dismisses.
	tests := []struct {
		ev   *tcell.EventKey
		want string
	}{
		{tcell.NewEventKey(tcell.KeyTab, 0, tcell.ModNone), "menu-accept"},
		{tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone), "menu-prev"},
		{tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone), "menu-next"},
		{tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone), "cancel"},
		{tcell.NewEventKey(tcell.KeyRune, 'x', tcell.ModNone), ""}, // unbound rune
	}
	for i, tc := range tests {
		if got := m.Resolve(tc.ev); got != tc.want {
			t.Errorf("test %d: Resolve = %q, want %q", i, got, tc.want)
		}
	}
}

func TestLoadKeyMapMissingFileIsDefaults(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m, err := LoadKeyMap()
	if err != nil {
		t.Fatal(err)
	}
	if m.Chord("submit") != "Enter" {
		t.Fatalf("submit chord = %q", m.Chord("submit"))
	}
}

func TestLoadKeyMapCustomBinding(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	mk(t, home, "keybindings.yml", `
submit:
  - "C-Enter"
newline:
  - "S-Enter"
quit: []
`)
	m, err := LoadKeyMap()
	if err != nil {
		t.Fatal(err)
	}
	if m.Chord("submit") != "C-Enter" {
		t.Fatalf("submit = %q, want C-Enter", m.Chord("submit"))
	}
	if m.Chord("newline") != "S-Enter" {
		t.Fatalf("newline = %q, want S-Enter", m.Chord("newline"))
	}
	if m.Chord("quit") != "" {
		t.Fatalf("quit should be disabled: %q", m.Chord("quit"))
	}
	// Ctrl+C should resolve to no action.
	if got := m.Resolve(tcell.NewEventKey(tcell.KeyRune, 'c', tcell.ModCtrl)); got != "" {
		t.Fatalf("disabled action still resolves: %q", got)
	}
}

func TestLoadKeyMapRejectsMalformed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	mk(t, home, "keybindings.yml", `submit: "Enter"`) // string, not a list
	if _, err := LoadKeyMap(); err == nil || !strings.Contains(err.Error(), "expected a list") {
		t.Fatalf("malformed config must error, got %v", err)
	}
}

func TestUnknownActionIgnored(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	mk(t, home, "keybindings.yml", `
futureAction:
  - "C-Shift-F12"
`)
	m, err := LoadKeyMap()
	if err != nil {
		t.Fatalf("unknown actions must not error: %v", err)
	}
	// All defaults survive.
	if m.Chord("submit") != "Enter" {
		t.Fatalf("known actions lost")
	}
}

func TestHotkeysRendersEveryAction(t *testing.T) {
	m := DefaultKeyMap()
	got := m.Hotkeys()
	for _, a := range BuiltinActions {
		if !strings.Contains(got, a) {
			t.Errorf("Hotkeys() missing %q:\n%s", a, got)
		}
	}
}

// The newline action must advertise the portable chord first: Ctrl+J
// (0x0A) reaches every terminal; Alt-Enter is the only terminal-reported
// alias (Shift-Enter never resolves through chordOf, so the default table
// no longer lists a chord the runtime would ignore).
func TestChordsPreferPortableNewline(t *testing.T) {
	m := DefaultKeyMap()
	chords := m.Chords("newline")
	if len(chords) < 2 {
		t.Fatalf("newline chords = %v, want Ctrl+J plus the Alt-Enter alias", chords)
	}
	if chords[0] != "C-j" {
		t.Fatalf("first newline chord = %q, want C-j", chords[0])
	}
	// Every listed chord must actually resolve to the action (a table
	// that lists a chord the runtime ignores is the bug this guards).
	for _, c := range chords {
		if m.bindings[c] != "newline" {
			t.Fatalf("listed chord %q does not resolve to newline", c)
		}
	}
	if got := m.Chords("submit"); len(got) == 0 || got[0] != "Enter" {
		t.Fatalf("submit chords = %v", got)
	}
}

// A remapped chord in keybindings.yml must drive the action: the table
// is not decorative.
func TestRemapDrivesAction(t *testing.T) {
	m := DefaultKeyMap()
	m.bindings["C-y"] = "newline"
	m.clearAction("newline")
	m.bindings["C-y"] = "newline"
	ev := tcell.NewEventKey(tcell.KeyCtrlY, 0, tcell.ModCtrl)
	if got := m.Resolve(ev); got != "newline" {
		t.Fatalf("remap Resolve = %q, want newline", got)
	}
	if got := m.Chords("newline"); len(got) != 1 || got[0] != "C-y" {
		t.Fatalf("after remap, chords = %v", got)
	}
}
