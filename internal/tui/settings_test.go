package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// TestSettingsOverlayToggleAppliesImmediately: the overlay's whole point is
// that a toggle takes effect the moment it is selected — the App state flips
// (SetShowThinking / SetDockMode) AND the value is persisted through the ops.
// Without this the panel would be a viewer of the settings file, not a
// settings panel.
func TestSettingsOverlayToggleAppliesImmediately(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)

	var writes [][2]string
	app.SetSettingsOverlayOps(&SettingsOverlayOps{
		Path: "/tmp/xdev-config.yml",
		Read: func() []SettingsRow {
			return []SettingsRow{
				{Key: "showThinking", Label: "Show thinking", Value: "true", Editable: true, Kind: "toggle"},
				{Key: "sidebarMode", Label: "Sidebar", Value: "auto", Editable: true, Kind: "select", Options: []string{"auto", "show", "hide"}},
				{Key: "theme", Label: "Theme", Value: "groknight", Editable: false, Kind: "text"},
			}
		},
		Write: func(key, value string) error {
			writes = append(writes, [2]string{key, value})
			return nil
		},
	})

	if !app.OpenSettingsOverlay() {
		t.Fatal("overlay did not open")
	}
	if !app.SettingsOverlayOpen() {
		t.Fatal("overlay reports closed right after opening")
	}

	// Enter on row 0 (showThinking, true) flips the display off and persists.
	app.handleSettingsOverlayKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if app.Thinking() {
		t.Fatal("showThinking toggle did not apply immediately")
	}
	if len(writes) != 1 || writes[0][0] != "showThinking" || writes[0][1] != "false" {
		t.Fatalf("writes = %v, want showThinking=false", writes)
	}

	// Down to sidebarMode (a select) cycles auto → show.
	app.handleSettingsOverlayKey(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	app.handleSettingsOverlayKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if got := app.DockMode(); got != "show" {
		t.Fatalf("sidebarMode cycle = %q, want show", got)
	}
	if len(writes) != 2 || writes[1][0] != "sidebarMode" || writes[1][1] != "show" {
		t.Fatalf("writes = %v, want sidebarMode=show", writes)
	}

	// A non-editable row must not write anything.
	app.handleSettingsOverlayKey(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	app.handleSettingsOverlayKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if len(writes) != 2 {
		t.Fatalf("editable=false row wrote: %v", writes)
	}

	// Esc closes.
	app.handleSettingsOverlayKey(tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone))
	if app.SettingsOverlayOpen() {
		t.Fatal("Esc did not close the overlay")
	}
}

// TestSettingsOverlayDrawsRows: the panel must actually paint the settings it
// was given — a state that opens but renders nothing is an invisible modal,
// and an invisible modal that owns the keyboard is the worst of both.
func TestSettingsOverlayDrawsRows(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	app.SetSettingsOverlayOps(&SettingsOverlayOps{
		Read: func() []SettingsRow {
			return []SettingsRow{
				{Key: "showThinking", Label: "Show thinking", Value: "true", Editable: true, Kind: "toggle"},
				{Key: "theme", Label: "Theme", Value: "ocean", Editable: false, Kind: "text"},
			}
		},
		Write: func(key, value string) error { return nil },
	})
	app.OpenSettingsOverlay()
	app.paint()
	app.scr.Show()

	got := screenText(scr)
	for _, want := range []string{"SETTINGS", "Show thinking", "Theme", "ocean"} {
		if !strings.Contains(got, want) {
			t.Fatalf("overlay frame missing %q:\n%s", want, got)
		}
	}
}

// TestSettingsOverlayCategoryFilterSelectsTheShownRow: the selection is an
// index into the VISIBLE rows, so with a category tab active the row the
// highlight sits on must be the row Enter acts on. The first cut indexed the
// unfiltered set — the highlight said one thing and the toggle did another —
// which is exactly the kind of bug a panel that writes config cannot have.
func TestSettingsOverlayCategoryFilterSelectsTheShownRow(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)

	var writes [][2]string
	app.SetSettingsOverlayOps(&SettingsOverlayOps{
		Read: func() []SettingsRow {
			return []SettingsRow{
				{Key: "sidebarMode", Label: "Sidebar", Value: "auto", Editable: true, Kind: "select", Options: []string{"auto", "show", "hide"}},
				{Key: "showThinking", Label: "Show thinking", Value: "true", Editable: true, Kind: "toggle"},
			}
		},
		Write: func(key, value string) error {
			writes = append(writes, [2]string{key, value})
			return nil
		},
	})
	app.OpenSettingsOverlay()

	// Tab once: category 1 is the first non-"all" group. Rows are declared
	// ui(sidebarMode) then reasoning(showThinking), so the tab order is
	// all, ui, reasoning and Tab lands on "ui" — one visible row.
	app.handleSettingsOverlayKey(tcell.NewEventKey(tcell.KeyTab, 0, tcell.ModNone))
	app.handleSettingsOverlayKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if len(writes) != 1 || writes[0][0] != "sidebarMode" {
		t.Fatalf("filtered Enter acted on the wrong row: %v", writes)
	}
	if got := app.DockMode(); got != "show" {
		t.Fatalf("sidebarMode = %q after filtered toggle, want show", got)
	}

	// The displayed value must follow too: a panel that toggles but still
	// draws the old value reads as broken.
	app.paint()
	app.scr.Show()
	if !strings.Contains(screenText(scr), "show") {
		t.Fatal("overlay still paints the pre-toggle value")
	}
}

// TestSettingsOverlayRefreshPicksUpExternalWrites: the overlay is not the only
// writer of the config file, so 'r' must re-read rather than keep a snapshot
// taken when the panel opened.
func TestSettingsOverlayRefreshPicksUpExternalWrites(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	theme := "groknight"
	app.SetSettingsOverlayOps(&SettingsOverlayOps{
		Read: func() []SettingsRow {
			return []SettingsRow{{Key: "theme", Label: "Theme", Value: theme, Editable: false, Kind: "text"}}
		},
		Write: func(key, value string) error { return nil },
	})
	app.OpenSettingsOverlay()

	theme = "ocean" // written by `xdev config set` in another process
	app.handleSettingsOverlayKey(tcell.NewEventKey(tcell.KeyRune, 'r', tcell.ModNone))

	app.paint()
	app.scr.Show()
	if !strings.Contains(screenText(scr), "ocean") {
		t.Fatal("refresh did not pick up the external write")
	}
}

// TestSettingsOverlayChordResolves: the overlay's default chord lives in the
// keymap table, so the chord the panel advertises must actually resolve to the
// action — a table that lists a chord the runtime ignores is the classic
// keybinding bug this guards (Alt+, arrives as ESC + "," which chordOf has to
// be able to name).
func TestSettingsOverlayChordResolves(t *testing.T) {
	m := DefaultKeyMap()
	if got := m.Chord("app.settings"); got != "A-," {
		t.Fatalf("app.settings chord = %q, want A-,", got)
	}
	ev := tcell.NewEventKey(tcell.KeyRune, ',', tcell.ModAlt)
	if got := m.Resolve(ev); got != "app.settings" {
		t.Fatalf("Alt+, resolves to %q, want app.settings", got)
	}
}

// TestSettingsOverlayUnknownValueNotPaintedOff: the panel must not claim a
// setting is off when it cannot read the value. A hand-edited `showThinking:
// yes` reaches the row as the string "yes" in a test seam, and painting that
// as "○ off" would put a wrong state in front of a user about to write over it.
func TestSettingsOverlayUnknownValueNotPaintedOff(t *testing.T) {
	if on, known := settingsTruthy("yes"); known {
		t.Fatalf("settingsTruthy(yes) = %v, %v — must be unknown", on, known)
	}
	if got := toggleBoolValue("yes"); got != "true" {
		t.Fatalf("toggleBoolValue(yes) = %q, want true (turn it on, never assume off)", got)
	}

	app, scr := newTestApp(t, 100, 30)
	app.SetSettingsOverlayOps(&SettingsOverlayOps{
		Read: func() []SettingsRow {
			return []SettingsRow{{Key: "showThinking", Label: "Show thinking", Value: "yes", Editable: true, Kind: "toggle"}}
		},
		Write: func(key, value string) error { return nil },
	})
	app.OpenSettingsOverlay()
	app.paint()
	app.scr.Show()
	if got := screenText(scr); strings.Contains(got, offMark) {
		t.Fatalf("unreadable value painted as off:\n%s", got)
	}
}

// TestSettingsOverlaySqueezedWindowKeepsSelectionVisible: on a short screen the
// painted body is smaller than the cap, so the selection must clamp against
// what was actually painted. A key path that still clamped against the cap
// (12) would let the highlight scroll several rows below the panel — the user
// presses Enter on a row they cannot see.
func TestSettingsOverlaySqueezedWindowKeepsSelectionVisible(t *testing.T) {
	app, scr := newTestApp(t, 100, 14) // 14 rows: the panel gets squeezed
	var rows []SettingsRow
	for i := 0; i < 20; i++ {
		rows = append(rows, SettingsRow{
			Key: fmt.Sprintf("k%02d", i), Label: fmt.Sprintf("Row %02d", i),
			Value: "true", Editable: true, Kind: "toggle",
		})
	}
	app.SetSettingsOverlayOps(&SettingsOverlayOps{
		Read:  func() []SettingsRow { return rows },
		Write: func(key, value string) error { return nil },
	})
	app.OpenSettingsOverlay()
	app.paint()
	app.scr.Show()

	st := settingsStateOf(app)
	settingsRegMu.Lock()
	painted, top := st.bodyRows, st.top
	settingsRegMu.Unlock()
	if painted <= 0 || painted >= len(rows) {
		t.Fatalf("painted body = %d rows, want a squeezed window", painted)
	}

	// Walk the selection to the end, then assert it is inside the painted
	// window rather than merely inside the cap.
	for i := 0; i < len(rows)+2; i++ {
		app.handleSettingsOverlayKey(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	}
	settingsRegMu.Lock()
	sel, top2 := st.sel, st.top
	settingsRegMu.Unlock()
	if sel >= top2+painted {
		t.Fatalf("selection %d outside painted window [%d,%d)", sel, top2, top2+painted)
	}
	_ = top
	_ = scr
}
