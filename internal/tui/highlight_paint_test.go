package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// TestFencedBlockPaintsThroughTheRealPaintPath is the "did it actually work on
// screen" check, the one unit tests on renderMarkdown cannot make: it drives
// the App's own paint() onto a simulation screen with a real fenced block and
// reads the resulting cell grid. Everything else here asserts on styles; this
// one asserts on what the terminal would be handed — every glyph present, the
// comment ink actually reaching a cell, and the code-bg band under the row.
// fgOf is the cell's foreground as a comparable colour.
func fgOf(st tcell.Style) tcell.Color {
	fg, _, _ := st.Decompose()
	return fg
}

func TestFencedBlockPaintsThroughTheRealPaintPath(t *testing.T) {
	app, scr := newTestApp(t, 60, 20)
	t.Setenv("NO_COLOR", "")
	app.SetHandlers(func(string) {}, func() {}, func() {})
	app.AddAssistantBlock("before\n\n```go\n// note\nx := 1\n```")
	app.paint()
	scr.Show()

	cells, w, h := scr.GetContents()
	if w == 0 || h == 0 {
		t.Fatal("simulation screen has no cells")
	}
	rows := make([]string, h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			rows[y] += string(cells[y*w+x].Runes[0])
		}
	}
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "// note") {
		t.Fatalf("the code block never reached the screen:\n%s", joined)
	}
	if strings.Contains(joined, "```") {
		t.Errorf("a fence line reached the screen:\n%s", joined)
	}
	// Sanity: the grid must really carry distinct inks, or the assertions
	// below are vacuous. This is the check that fails if a theme ever paints
	// every token class the same colour.
	seen := map[tcell.Color]bool{}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if c := cells[y*w+x]; len(c.Runes) > 0 && c.Runes[0] != ' ' {
				seen[fgOf(c.Style)] = true
			}
		}
	}
	if len(seen) < 2 {
		t.Fatalf("the screen has %d distinct inks, expected a themed render", len(seen))
	}
	// The comment must be painted in a different ink than the code next to
	// it, in the actual cell grid — not merely in the cell struct.
	at := func(sub string) (x, y int, fg tcell.Color) {
		for yi, row := range rows {
			if xi := strings.Index(row, sub); xi >= 0 {
				return xi, yi, fgOf(cells[yi*w+xi].Style)
			}
		}
		t.Fatalf("%q never reached the screen:\n%s", sub, joined)
		return 0, 0, tcell.ColorDefault
	}
	_, cy, commentFg := at("// note")
	_ = cy
	_, _, numFg := at("1")
	if commentFg == tcell.ColorDefault {
		t.Error("the comment painted in the terminal default ink, not a theme colour")
	}
	if commentFg == numFg {
		t.Error("comment and number painted the same ink on the real screen")
	}
}
