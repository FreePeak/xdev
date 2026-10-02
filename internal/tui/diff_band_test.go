package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// diffFixture is one changed file: a replaced pair, so wordPair has something to
// mark, plus a context row that must stay unbanded.
const diffFixture = "--- a/f.go\n+++ b/f.go\n@@ -1,3 +1,3 @@\n a\n-b := 1\n+b := 2\n c\n"

// diffRuns paints the fixture for the app's theme and returns the interior runs
// of every row, left frame stripped (cellRow owns the borders).
//
// Each colour test clears NO_COLOR itself, for the reason highlight_test.go
// gives: the renderer reads the variable, so an exported NO_COLOR would
// silently turn every colour test in this file into a NO_COLOR test. The one
// test whose subject IS NO_COLOR sets it back.
func diffRuns(t *testing.T, app *App) [][]cell {
	t.Helper()
	rows := app.diffCells(diffFixture, 40)
	out := make([][]cell, 0, len(rows))
	for _, ln := range rows {
		out = append(out, ln.runs)
	}
	return out
}

// TestDiffBandReachesTheInteriorWidth pins the band as a stripe, not as a
// glyph run: a changed row is padded out to the box interior so the colour
// reaches the right border, and an unbanded row keeps the terminal's background
// to its edge — the overlay's blank field and the transcript both assume it.
func TestDiffBandReachesTheInteriorWidth(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	app := idxApp(120, 40)
	const inner = 40
	rows := app.diffCells(diffFixture, inner)

	striped, plain := 0, 0
	for _, ln := range rows {
		bg, ok := banded(ln)
		switch {
		case !ok:
			plain++
			continue
		case bg == tcell.ColorDefault:
			t.Fatalf("row %q reports a band of %v", runsText(ln.runs), bg)
		}
		striped++
		if w := lineWidth(ln); w != inner {
			t.Errorf("banded row %q is %d cells, want the interior width %d", runsText(ln.runs), w, inner)
		}
		// The pad is chrome: painted, never read back as text or copied.
		last := ln.runs[len(ln.runs)-1]
		if !last.chrome || strings.TrimSpace(last.text) != "" {
			t.Errorf("band pad run %+v is not blank chrome", last)
		}
	}
	if striped == 0 || plain == 0 {
		t.Fatalf("fixture painted %d striped and %d plain rows, want both", striped, plain)
	}
}

// TestDiffNoThemeBandsKeepsTheOldLook is the optional-slot contract: the four
// band slots are xdev's own, not omp's 66 tokens, so a theme that never names
// them must render exactly as it did before the bands existed — the terminal's
// own ANSI markers, no background anywhere, and bold (an attribute, which
// survives any palette) doing the word emphasis.
func TestDiffNoThemeBandsKeepsTheOldLook(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	app := idxApp(120, 40)
	th := theme.Load("groknight")
	for _, slot := range []string{theme.ToolDiffAddedBg, theme.ToolDiffRemovedBg,
		theme.ToolDiffAddedWordBg, theme.ToolDiffRemovedWordBg} {
		delete(th.Slots, slot)
		delete(th.Defaults, slot)
	}
	app.SetTheme(th)

	for _, runs := range diffRuns(t, app) {
		for _, r := range runs {
			if _, bg, _ := r.style.Decompose(); bg != tcell.ColorDefault {
				t.Fatalf("run %q paints background %v, want none", r.text, bg)
			}
		}
	}

	var bold string
	for _, runs := range diffRuns(t, app) {
		if len(runs) == 0 || !strings.HasPrefix(runs[0].text, "+") {
			continue
		}
		for _, r := range runs[1:] {
			if _, _, attrs := r.style.Decompose(); attrs&tcell.AttrBold != 0 {
				bold += r.text
			}
		}
	}
	if strings.TrimSpace(bold) != "2" {
		t.Errorf("changed word on the added row = bold %q, want %q", strings.TrimSpace(bold), "2")
	}
}

// TestDiffNO_COLORPaintsNothing: NO_COLOR means no colour output at all, so the
// renderer must not pick a band the terminal is forbidden to emit — the reason
// codeStyleFor reads the variable itself rather than trusting tcell. The
// attributes that carry no colour survive.
func TestDiffNO_COLORPaintsNothing(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	app := idxApp(120, 40)
	for _, runs := range diffRuns(t, app) {
		for _, r := range runs {
			fg, bg, _ := r.style.Decompose()
			if fg != tcell.ColorDefault || bg != tcell.ColorDefault {
				t.Fatalf("run %q paints %v on %v under NO_COLOR", r.text, fg, bg)
			}
		}
	}
}
