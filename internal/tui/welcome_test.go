package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// TestSlashMenuQuery covers the fuzzy ranking: exact-prefix first, then
// word-start subsequence, no-match excluded.
func TestSlashMenuQuery(t *testing.T) {
	m := newSlashMenu()
	m.open("", "/nonexistent") // no markdown dir; built-ins only

	m.query("cl")
	if got := m.rows()[0].Name; got != "/clear" {
		t.Fatalf("query 'cl' first row = %q, want /clear", got)
	}
	// Unknown query: empty menu, not active.
	m.query("zzz")
	if m.active() {
		t.Fatal("query with no matches must be inactive")
	}
	// Empty query shows everything.
	m.query("")
	if len(m.rows()) == 0 {
		t.Fatal("empty query must list commands")
	}
}

// TestSlashMenuMoveWrap covers selection wrapping in both directions.
func TestSlashMenuMoveWrap(t *testing.T) {
	m := newSlashMenu()
	m.open("", "")
	n := len(m.match)
	if n < 3 {
		t.Fatalf("expected >=3 built-ins, got %d", n)
	}
	m.move(1)
	if _, ok := m.selected(); !ok || m.sel != 1 {
		t.Fatalf("move(1): sel = %d", m.sel)
	}
	m.move(-1)
	if m.sel != 0 {
		t.Fatalf("move(-1): sel = %d, want 0", m.sel)
	}
	m.move(-1)
	if m.sel != n-1 {
		t.Fatalf("move(-1) at top must wrap to %d, got %d", n-1, m.sel)
	}
}

// TestSlashMenuVisibleWindow pins the 8-row cap with a window that keeps
// the selection in view.
func TestSlashMenuVisibleWindow(t *testing.T) {
	m := newSlashMenu()
	m.visible = 8
	m.open("", "")
	for range 20 {
		m.items = append(m.items, suggestion{Name: "/zz", Description: "filler"})
		m.match = append(m.match, len(m.match))
	}
	m.sel = 12
	rows := m.rows()
	if len(rows) != 8 {
		t.Fatalf("rows = %d, want 8 (visible cap)", len(rows))
	}
}

// TestAppDropdownAppearsOnSlash drives the real key path: typing "/h"
// opens the dropdown with /help; typing plain text never opens it.
func TestAppDropdownAppearsOnSlash(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	typeRunes(app, "/h")
	app.mu.Lock()
	open := app.smenu != nil && app.smenu.active()
	app.mu.Unlock()
	if !open {
		t.Fatal("typing /h must open the dropdown")
	}

	// Space (args start) closes it; plain text never opens it.
	typeRunes(app, " ")
	app.mu.Lock()
	open = app.smenu != nil && app.smenu.active()
	app.mu.Unlock()
	if open {
		t.Fatal("space must close the dropdown")
	}
	typeRunes(app, "world")
	app.mu.Lock()
	open = app.smenu != nil && app.smenu.active()
	app.mu.Unlock()
	if open {
		t.Fatal("plain text must not open the dropdown")
	}
}

// TestAppArrowNavigatesDropdown proves Up/Down move the selection while
// the dropdown is open (they must not scroll the transcript then).
func TestAppArrowNavigatesDropdown(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	typeRunes(app, "/")
	app.handleKey(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	app.mu.Lock()
	sel := app.smenu.sel
	app.mu.Unlock()
	if sel != 1 {
		t.Fatalf("Down while dropdown open: sel = %d, want 1", sel)
	}
	app.handleKey(tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone))
	app.mu.Lock()
	sel = app.smenu.sel
	app.mu.Unlock()
	if sel != 0 {
		t.Fatalf("Up while dropdown open: sel = %d, want 0", sel)
	}
}

// TestAppTabCompletesSelection proves Tab fills the editor with the
// selected command (and keeps the menu open for args).
func TestAppTabCompletesSelection(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	typeRunes(app, "/cl")
	app.handleKey(tcell.NewEventKey(tcell.KeyTab, 0, tcell.ModNone))
	if got := app.ed.Text(); !strings.HasPrefix(got, "/clear") {
		t.Fatalf("Tab completed to %q, want /clear…", got)
	}
}

// TestAppEscClosesDropdown: Esc with the menu open must not cancel a turn
// nor close it twice.
func TestAppEscClosesDropdown(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	typeRunes(app, "/")
	app.handleKey(tcell.NewEventKey(tcell.KeyEsc, 0, tcell.ModNone))
	app.mu.Lock()
	closed := app.smenu == nil
	app.mu.Unlock()
	if !closed {
		t.Fatal("Esc must close the dropdown")
	}
}

// TestWelcomeMenuItems covers the menu contract.
func TestWelcomeMenuItems(t *testing.T) {
	items := welcomeMenuItems(false)
	if len(items) != 3 {
		t.Fatalf("fresh session menu = %d rows, want 3", len(items))
	}
	if items[0].Key != "/new" {
		t.Fatalf("first action = %q, want /new", items[0].Key)
	}
	items = welcomeMenuItems(true)
	if len(items) != 4 || items[0].Key != "ctrl+r" {
		t.Fatalf("with history, first row must be Resume (ctrl+r)")
	}
}

// TestAppWelcomeDrawnWhenEmpty pins the draw contract: empty transcript →
// welcome content on screen; after a block, it's gone.
func TestAppWelcomeDrawnWhenEmpty(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	app.draw()
	prim, _, _ := scr.GetContents()
	if len(prim) < 2 || string(prim[1].Runes) != "❯" {
		t.Fatalf("welcome top bar missing")
	}
	found := gridContains(scr, "New session")
	if !found {
		t.Fatal("welcome menu row 'New session' not drawn")
	}

	app.AddSystemBlock("hello")
	app.draw()
	if gridContains(scr, "New session") {
		t.Fatal("welcome menu must disappear once the transcript has content")
	}
}

// TestAppMouseWheelScrolls pins the #17 follow-up: wheel events drive the
// in-app transcript viewport instead of falling through to the host
// terminal's scrollback.
func TestAppMouseWheelScrolls(t *testing.T) {
	app, _ := newTestApp(t, 80, 10)
	for range 40 {
		app.AddSystemBlock(strings.Repeat("filler ", 3))
	}
	app.draw()
	if !app.sm.Following() {
		t.Fatal("must start following")
	}
	app.handleKey(tcell.NewEventMouse(10, 10, tcell.WheelUp, tcell.ModNone))
	if app.sm.Following() || app.sm.offset == 0 {
		t.Fatalf("wheel up must scroll: follow=%v off=%d", app.sm.Following(), app.sm.offset)
	}
	app.handleKey(tcell.NewEventMouse(10, 10, tcell.WheelDown, tcell.ModNone))
	app.handleKey(tcell.NewEventMouse(10, 10, tcell.WheelDown, tcell.ModNone))
	app.handleKey(tcell.NewEventMouse(10, 10, tcell.WheelDown, tcell.ModNone))
	if !app.sm.Following() {
		t.Fatal("three wheel downs must return to live (3-row step)")
	}
}

// gridContains reconstructs each screen row and reports whether it holds s.
func gridContains(scr tcell.SimulationScreen, s string) bool {
	prim, w, _ := scr.GetContents()
	for y := 0; y*w < len(prim); y++ {
		var row strings.Builder
		for x := 0; x < w && y*w+x < len(prim); x++ {
			row.WriteString(string(prim[y*w+x].Runes))
		}
		if strings.Contains(row.String(), s) {
			return true
		}
	}
	return false
}
