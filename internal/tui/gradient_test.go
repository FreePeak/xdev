package tui

import (
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// The composer's top rule is the frame line directly above the input, and it
// is the one full-width surface in the TUI that carries no text. It ramps
// from the accent ink to the border ink: the frame wakes up where the prompt
// is and settles into the chrome.
func TestComposerTopRuleRampsFromAccentToBorder(t *testing.T) {
	app, scr := drawnApp(t, 60, 14)
	setDraft(&app.ed, "hi", 2)
	app.draw()

	row, xs := ruleRow(t, scr, 14)
	if len(xs) < 2 {
		t.Fatalf("the composer drew no rule to ramp:\n%s", screenText(scr))
	}
	accent := app.cellColor(app.th.Get(theme.AccentAssistant))
	border := app.cellColor(app.th.Get(theme.PromptBorderActive))

	first := cellFg(t, scr, xs[0], row)
	last := cellFg(t, scr, xs[len(xs)-1], row)
	if first != accent {
		t.Errorf("the rule's first cell is %v, want the accent ink %v", first, accent)
	}
	if last != border {
		t.Errorf("the rule's last cell is %v, want the border ink %v", last, border)
	}
	if accent == border {
		return // a flat theme has no ramp to check
	}
	// A middle cell has to be BETWEEN the stops — a rule that jumps from one
	// ink to the other is two colours, not a gradient.
	mid := cellFg(t, scr, xs[len(xs)/2], row)
	if mid == first || mid == last {
		t.Fatalf("the rule's middle cell is %v, which is one of the ends (%v / %v)", mid, first, last)
	}
	fr, fg, fb := first.RGB()
	lr, lg, lb := last.RGB()
	mr, mg, mb := mid.RGB()
	for _, ch := range []struct {
		name    string
		a, b, m int32
	}{
		{"red", fr, lr, mr},
		{"green", fg, lg, mg},
		{"blue", fb, lb, mb},
	} {
		lo, hi := ch.a, ch.b
		if lo > hi {
			lo, hi = hi, lo
		}
		if ch.m < lo || ch.m > hi {
			t.Errorf("the rule's middle %s = %d, outside the ramp %d..%d", ch.name, ch.m, lo, hi)
		}
	}
}

// A theme that asks for no gradient gets a flat rule: the ramp is derived
// from the theme's own inks, and equal inks are equal cells.
func TestComposerTopRuleIsFlatWhenTheThemeHasNoAccent(t *testing.T) {
	app, scr := drawnApp(t, 60, 14)
	flat := &theme.Theme{Name: "flat", Dark: true, Slots: map[string]theme.Color{
		theme.AccentAssistant:    theme.Hex("#505058"),
		theme.PromptBorderActive: theme.Hex("#505058"),
	}}
	app.SetTheme(flat)
	app.draw()

	row, xs := ruleRow(t, scr, 14)
	if len(xs) < 2 {
		t.Fatalf("the composer drew no rule:\n%s", screenText(scr))
	}
	first := cellFg(t, scr, xs[0], row)
	for _, x := range xs[1:] {
		if got := cellFg(t, scr, x, row); got != first {
			t.Fatalf("cell %d is %v, want the flat %v", x, got, first)
		}
	}
}

// ruleRow finds the composer's top rule: the row of horizontal glyphs
// directly above the prompt row, and the cells it covers.
func ruleRow(t *testing.T, scr tcell.SimulationScreen, h int) (int, []int) {
	t.Helper()
	prim, w, _ := scr.GetContents()
	// The prompt row is the LAST row carrying the prompt glyph: a transcript
	// row can carry one too, and the composer's is the one at the bottom.
	draft := -1
	for y := h - 1; y >= 0; y-- {
		for x := 0; x < w; x++ {
			if r := prim[y*w+x].Runes; len(r) > 0 && r[0] == '❯' {
				draft = y
				break
			}
		}
		if draft >= 0 {
			break
		}
	}
	if draft < 0 {
		t.Fatalf("no prompt row on the screen:\n%s", screenText(scr))
	}
	row := draft - 1
	if row < 0 {
		t.Fatalf("no row above the prompt row %d", draft)
	}
	var xs []int
	for x := 0; x < w; x++ {
		if r := prim[row*w+x].Runes; len(r) > 0 && r[0] == '─' {
			xs = append(xs, x)
		}
	}
	return row, xs
}

// cellFg reads a painted cell's foreground colour.
func cellFg(t *testing.T, scr tcell.SimulationScreen, x, y int) tcell.Color {
	t.Helper()
	prim, w, _ := scr.GetContents()
	if y < 0 || y*w+x >= len(prim) {
		t.Fatalf("cell (%d,%d) is off the screen", x, y)
	}
	fg, _, _ := prim[y*w+x].Style.Decompose()
	return fg
}
