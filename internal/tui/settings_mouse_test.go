package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

func settingsMouseApp(t *testing.T) *App {
	t.Helper()
	app, _ := newTestApp(t, 120, 40)
	app.AddUserBlock("hello")
	app.SetSettingsOverlayOps(&SettingsOverlayOps{
		Path: "/tmp/x.yml",
		Read: func() []SettingsRow {
			return []SettingsRow{
				{Key: "showThinking", Label: "Show thinking", Value: "true", Editable: true, Kind: "toggle"},
				{Key: "theme", Label: "Theme", Value: "groknight", Editable: false, Kind: "text"},
				{Key: "sidebarMode", Label: "Sidebar", Value: "auto", Editable: true, Kind: "select", Options: []string{"auto", "show", "hide"}},
			}
		},
		Write: func(key, value string) error { return nil },
	})
	app.OpenSettingsOverlay()
	app.draw()
	return app
}

// The tab strip is a list the user can see, so it is a list the user can click
// (the picker, the ask card and the ledger all answer their tabs/rows). Before
// this the mouse handler only ever looked at bodyStart, so a click on a category
// did nothing while Tab cycled it.
func TestSettingsOverlayTabClickSwitchesCategory(t *testing.T) {
	app := settingsMouseApp(t)
	st := settingsStateOf(app)
	if len(st.categories) < 3 {
		t.Fatalf("fixture painted %d categories, want >= 3", len(st.categories))
	}
	// Click each label exactly where the painter put it (its own published
	// columns), so a hit-test that disagrees with the painter cannot pass.
	for ci, cell := range st.tabCells {
		col := st.tabX + cell.x0
		before := st.activeCat
		app.mu.Lock()
		app.handleSettingsOverlayMouse(tcell.NewEventMouse(col, st.tabY, tcell.Button1, tcell.ModNone), true)
		got := st.activeCat
		app.mu.Unlock()
		if got != ci {
			t.Errorf("click at x=%d (label %q) gave category %d, want %d", col, st.categories[ci], got, ci)
		}
		if got == before && ci != before {
			t.Errorf("the click on %q did nothing (category stayed %d)", st.categories[ci], got)
		}
	}
	// a click on the strip's empty padding is consumed, not leaked
	app.mu.Lock()
	app.handleSettingsOverlayMouse(tcell.NewEventMouse(st.tabX, st.tabY, tcell.Button1, tcell.ModNone), true)
	cat := st.activeCat
	app.mu.Unlock()
	if cat != len(st.categories)-1 {
		t.Errorf("a click on the strip's leading padding changed the category to %d", cat)
	}
}

// TestSettingsTabColumnsMatchThePaint pins the painter's table to the painted
// row: a label's published columns must contain the label where it was drawn.
func TestSettingsTabColumnsMatchThePaint(t *testing.T) {
	app := settingsMouseApp(t)
	st := settingsStateOf(app)
	// the strip as painted, starting at its own left edge, in RUNES (the box
	// glyphs are multi-byte, and a byte slice reads a label's columns as the
	// tail of the one before it)
	painted := []rune{}
	for x := st.tabX; x < app.width; x++ {
		ch, _, _ := app.scr.Get(x, st.tabY)
		painted = append(painted, []rune(string(ch))...)
	}
	for i, c := range st.tabCells {
		if c.category != st.categories[i] {
			t.Fatalf("tabCells[%d] is %q, want %q", i, c.category, st.categories[i])
		}
		if c.x0 > c.x1 {
			t.Fatalf("tabCells[%d] (%s) has an empty span %d..%d", i, c.category, c.x0, c.x1)
		}
		seg := string(painted[c.x0 : c.x1+1])
		if !strings.Contains(strings.ToLower(seg), c.category) {
			t.Errorf("tabCells[%d] (%s) claims cols %d..%d, which paint %q", i, c.category, c.x0, c.x1, seg)
		}
	}
}

// A read-only row looks like every other row. Clicking it must say why it is
// inert rather than swallow the gesture.
func TestSettingsOverlayReadOnlyClickExplainsItself(t *testing.T) {
	app := settingsMouseApp(t)
	st := settingsStateOf(app)
	// the Theme row is index 1 of the visible rows
	themeRow := st.bodyStart + 1
	app.mu.Lock()
	app.handleSettingsOverlayMouse(tcell.NewEventMouse(60, themeRow, tcell.Button1, tcell.ModNone), true)
	app.mu.Unlock()
	app.mu.Lock()
	notice := app.toastText()
	app.mu.Unlock()
	if strings.Contains(notice, "read-only") && strings.Contains(notice, "theme") {
		return
	}
	t.Errorf("clicking the read-only Theme row produced no read-only notice (toast=%q)", notice)
}
