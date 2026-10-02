package tui

import (
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// The two frame-path costs this branch removes are invisible in the painted
// output — a wrong width shifts a column, a stale memo repaints in the old
// ink — so they are asserted directly rather than through a screenshot.

// TestMdStyleMemoFollowsTheme: the memo is keyed on the theme pointer, so
// /theme (SetTheme swaps a.th) must not keep serving the previous palette.
// A memo that ignored the swap would paint the new theme in the old ink.
func TestMdStyleMemoFollowsTheme(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	app, _ := newTestApp(t, 80, 24)
	before := app.mdStyle().body

	// gruvbox is not a builtin; a distinct pointer with a distinct md_text.
	app.SetTheme(&theme.Theme{
		Name:  "testswap",
		Dark:  true,
		Slots: map[string]theme.Color{theme.MdHeading1: theme.Hex("#ff0000"), theme.TextSecondary: theme.Hex("#00ff00")},
	})
	after := app.mdStyle().body

	if before == after {
		t.Fatal("mdStyle memo survived a theme swap: /theme would repaint in the old ink")
	}
	fa, _, _ := after.Decompose()
	_, fb, _ := before.Decompose()
	if fa == fb {
		t.Fatalf("the swapped theme and groknight resolved the same md_text fg %v", fa)
	}
	// A second read of the same theme is the cached one, and still correct.
	if again := app.mdStyle().body; again != after {
		t.Fatal("the memo returned a different style for the same theme")
	}
}

// TestDrawTextAdvancesByRuneWidth: the painter positions each cell with
// runeWidth, so a width table disagreement shows up as a row that runs long
// or short. ASCII, wide (CJK), zero-width (combining) and a control char in
// one string: every rune lands where its own width says it should.
func TestDrawTextAdvancesByRuneWidth(t *testing.T) {
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	defer scr.Fini()
	scr.SetSize(40, 3)

	drawText(scr, 0, 0, "a宽b́c\x01d", tcell.StyleDefault)
	scr.Show() // the simulation screen's read buffer is the front buffer

	// Each rune's cell, and the cell it must NOT have landed in: the wide
	// rune skips one column, the combining mark shares one, the control
	// char paints nothing.
	want := map[int]rune{0: 'a', 1: '宽', 3: 'b', 4: 'c', 5: 'd'}
	for x, r := range want {
		got := cellRune(scr, x, 0)
		if got != r {
			t.Errorf("cell %d = %q, want %q", x, got, r)
		}
	}
	for _, x := range []int{2, 6} {
		if got := cellRune(scr, x, 0); got != ' ' {
			t.Errorf("cell %d = %q, want it left blank (skipped by an advance)", x, got)
		}
	}
}

// TestPaintedWidthMatchesPainter: hit rectangles are computed with
// paintedWidth and painted with drawText. They walked the same runes before
// this branch; if either stops using runeWidth the two drift apart and every
// click/selection rectangle lands a column off.
func TestPaintedWidthMatchesPainter(t *testing.T) {
	for _, s := range []string{"", "ascii only", "宽体漢字", "é", "▍", "\x01\x02"} {
		n := 0
		for _, r := range s {
			n += runeWidth(r)
		}
		if got := paintedWidth(s); got != n {
			t.Errorf("paintedWidth(%q) = %d, painter advances %d", s, got, n)
		}
	}
}
