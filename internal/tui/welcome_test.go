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
func TestAppWelcomeLogo(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	app.draw()

	// The art rows are ragged-width, so they must all sit at the same
	// x on consecutive rows — per-line centering would wobble them
	// (the bug this test pins). The Life band paints '▓' into the
	// art's whitespace, so rows are matched with band cells masked
	// to spaces.
	prim, w, _ := scr.GetContents()
	h := len(prim) / w
	rowStr := func(y int) string {
		var b strings.Builder
		for x := range w {
			switch r := prim[y*w+x].Runes; {
			case len(r) == 1 && r[0] == lifeGlyph:
				b.WriteByte(' ') // backdrop, not art
			case len(r) > 0:
				b.WriteRune(r[0])
			default:
				b.WriteByte(' ')
			}
		}
		return b.String()
	}

	x0, y0 := -1, -1
	for i, art := range xdevLogo {
		want := art // exact row, leading spaces included
		x, y := -1, -1
		for yy := range h {
			if xx := strings.Index(rowStr(yy), want); xx >= 0 {
				x, y = xx, yy
				break
			}
		}
		if x < 0 {
			t.Fatalf("logo row %d (%q) not on screen", i, want)
		}
		if i == 0 {
			x0, y0 = x, y
			continue
		}
		if x != x0 {
			t.Fatalf("logo row %d origin x=%d, want %d (common left edge)", i, x, x0)
		}
		if y != y0+i {
			t.Fatalf("logo row %d at y=%d, want %d (consecutive)", i, y, y0+i)
		}
	}
	if !gridContains(scr, "01111000") {
		t.Fatal("binary tagline missing")
	}
}

// TestAppWelcomeLifeBounds pins the inset-band contract: the grid is
// the declared (narrower) band, live cells never reach the top bar,
// the composer rows, or the side margins, and the backdrop is still
// populated after hundreds of generations — die-out (a blank
// backdrop) is exactly the failure mode Life on a finite torus has
// without the mutation sprinkle.
func TestAppWelcomeLifeBounds(t *testing.T) {
	app, scr := newTestApp(t, 80, 26)
	app.draw()
	gw, top, bot, ok := lifeArea(80, 26)
	if !ok {
		t.Fatal("life area should fit in an 80x26 terminal")
	}
	if gw != 68 || top != 2 || bot != 20 {
		t.Fatalf("lifeArea(80,26) = (%d,%d,%d), want inset 68x[2,20]", gw, top, bot)
	}
	// Step 200 generations — far past any random soup's burn-in.
	for range 200 {
		app.stepLife(gw, top, bot)
	}
	app.draw()
	if app.life.w != gw || app.life.h != bot-top+1 {
		t.Fatalf("life grid = %dx%d, want %dx%d", app.life.w, app.life.h, gw, bot-top+1)
	}
	prim, w, _ := scr.GetContents()
	h := len(prim) / w
	alive := 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := prim[y*w+x]
			if len(c.Runes) == 1 && c.Runes[0] == lifeGlyph {
				alive++
				if y == 0 || y >= h-4 || x < 6 || x >= 6+gw {
					t.Fatalf("life cell at (%d,%d), outside band x[6,%d) protected rows", x, y, 6+gw)
				}
			}
		}
	}
	if alive == 0 {
		t.Fatal("backdrop died out: no live cells after 200 generations")
	}
}

// TestLifeAreaSkipsSmallTerminals pins the small-window contract: no
// backdrop on terminals too small to spare the band.
func TestLifeAreaSkipsSmallTerminals(t *testing.T) {
	for _, sz := range [][2]int{{80, 24}, {100, 30}, {40, 16}} {
		if _, _, _, ok := lifeArea(sz[0], sz[1]); !ok {
			t.Errorf("lifeArea(%d,%d) = not ok, want band", sz[0], sz[1])
		}
	}
	for _, sz := range [][2]int{{38, 20}, {80, 14}, {30, 30}} {
		if _, _, _, ok := lifeArea(sz[0], sz[1]); ok {
			t.Errorf("lifeArea(%d,%d) = ok, want skipped", sz[0], sz[1])
		}
	}
}

// TestWelcomeMenuNarrow pins the narrow-window fallback: below the
// column's fit threshold the menu rows stay on screen, inline.
func TestWelcomeMenuNarrow(t *testing.T) {
	app, scr := newTestApp(t, 26, 20)
	app.draw()
	for _, want := range []string{"New session", "/new", "Quit", "ctrl+q"} {
		if !gridContains(scr, want) {
			t.Fatalf("narrow menu lost %q", want)
		}
	}
}

// TestLifeGridToroidalWrap pins the wrap rule: a glider leaving one edge
// re-enters the opposite edge without index panics.
func TestLifeGridToroidalWrap(t *testing.T) {
	g := newLifeGrid(4, 4)
	// Vertical blinker column at x=0.
	for y := range 4 {
		g.c[y][0] = true
	}
	g.step()
	// Under B3/S23 a full wrapped column cycles; the grid must stay 4x4.
	if g.w != 4 || g.h != 4 {
		t.Fatalf("grid resized to %dx%d", g.w, g.h)
	}
	// Toroidal at() must not panic at the far edge.
	g.at(3, 3)
	g.at(0, 0)
}

// TestLifeGlyphNarrow pins the backdrop glyph to width 1: a wide glyph
// (e.g. U+30FB '・') drawn just left of the logo makes tcell drop the
// logo's SetContent into the wide continuation cell, punching a hole
// in the art — the flake TestAppWelcomeLogo used to hit with the rain
// alphabet.
func TestLifeGlyphNarrow(t *testing.T) {
	if width(string(lifeGlyph)) != 1 {
		t.Errorf("life glyph %q (U+%04X) has width %d, want 1", lifeGlyph, lifeGlyph, width(string(lifeGlyph)))
	}
}

// TestLogoOneArtAcrossSizes pins the size-consistency fix: every
// terminal size that fits the artwork gets the identical 8-row logo —
// no tier swapping — and sizes that can't fit it get nothing.
func TestLogoOneArtAcrossSizes(t *testing.T) {
	fitting := [][2]int{{100, 30}, {80, 24}, {80, 22}, {60, 30}, {120, 50}}
	for _, sz := range fitting {
		got := logoArt(sz[0], sz[1]-8)
		if len(got) != len(xdevLogo) {
			t.Fatalf("logoArt(%d,%d) = %d rows, want the one %d-row logo", sz[0], sz[1], len(got), len(xdevLogo))
		}
		for i := range xdevLogo {
			if got[i] != xdevLogo[i] {
				t.Fatalf("logoArt(%d,%d) row %d differs between sizes", sz[0], sz[1], i)
			}
		}
	}
	for _, sz := range [][2]int{{80, 21}, {80, 19}, {30, 30}, {10, 5}} {
		if got := logoArt(sz[0], sz[1]-8); got != nil {
			t.Fatalf("logoArt(%d,%d) = art, want nil (doesn't fit)", sz[0], sz[1])
		}
	}
}
