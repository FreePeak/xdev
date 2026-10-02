package tui

// Split-view layout tests. The fixtures are generic shapes written from the
// unified-diff grammar, not any repository's real diff.

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// splitFixture is a diff with one hunk, context rows, a replaced pair and a
// pure deletion, so one fixture exercises every pairing rule.
const splitFixture = "--- a/calc.go\n+++ b/calc.go\n" +
	"@@ -10,6 +10,6 @@ func total(items []Item) int {\n" +
	" \tsum := 0\n" +
	"-\tfor _, it := range items {\n" +
	"+\tfor _, it := range items {\n" +
	" \t\tsum += it.Cost()\n" +
	" \t}\n" +
	"-\treturn sum\n" +
	" \treturn sum * 2\n"

// openFixtureOverlay opens the overlay on splitFixture at whatever width the
// test app was built with, so the layout decision under test is the real one.
func openFixtureOverlay(t *testing.T, app *App) {
	t.Helper()
	app.mu.Lock()
	app.blocks = append(app.blocks, &Block{
		Kind: KindToolDone, ToolName: "edit", Diff: splitFixture,
	})
	opened := app.openDiffOverlay("calc.go")
	app.mu.Unlock()
	if !opened {
		t.Fatal("no diff overlay for the fixture diff")
	}
}

func rowsText(rows []line) string {
	var b strings.Builder
	for _, ln := range rows {
		b.WriteString(runsText(ln.runs))
		b.WriteByte('\n')
	}
	return b.String()
}

// rowTextAt returns the row containing want, or "".
func rowTextAt(texts []string, want string) string {
	for _, txt := range texts {
		if strings.Contains(txt, want) {
			return txt
		}
	}
	return ""
}

func rowTexts(rows []line) []string {
	out := make([]string, 0, len(rows))
	for _, ln := range rows {
		out = append(out, runsText(ln.runs))
	}
	return out
}

// TestSplitFitsGatesOnWidthAndLongestRow pins the two gates: the terminal has
// to be wide enough, AND this diff's longest row has to fit a half-column. A
// narrow terminal and an over-long row both fall back to unified, because a
// column of ellipses is worse than the list it replaced.
func TestSplitFitsGatesOnWidthAndLongestRow(t *testing.T) {
	rows := classifyMarkedDiff(splitFixture)
	if !splitFits(140, rows) {
		t.Fatal("a 140-column viewer with a short diff must split")
	}
	if splitFits(diffSplitMinCols-1, rows) {
		t.Fatalf("a %d-column viewer must stay unified, not split", diffSplitMinCols-1)
	}
	long := make([]diffRow, 0, len(rows))
	for _, r := range rows {
		r.text = strings.Repeat("x", 200) + r.text
		long = append(long, r)
	}
	if splitFits(400, long) {
		t.Fatal("a diff whose rows cannot fit a half-column must stay unified at any width")
	}
}

// TestSplitPairsRemovedWithAddedOnOneRow pins the pairing: a removed row and
// the added row immediately after it share a terminal row, which is the whole
// reason the view exists.
func TestSplitPairsRemovedWithAddedOnOneRow(t *testing.T) {
	app, scr := newTestApp(t, 200, 40)
	defer scr.Fini()

	texts := rowTexts(app.splitRows(classifyMarkedDiff(splitFixture), 192))
	pair := rowTextAt(texts, "for _, it := range items {")
	if pair == "" {
		t.Fatalf("the replaced pair is missing:\n%s", strings.Join(texts, "\n"))
	}
	// The '+' row's own text lives on the RIGHT of the row; the '-' row's on
	// the left. Both markers present on ONE row is the pairing.
	if !strings.Contains(pair, "-\tfor") || !strings.Contains(pair, "+\tfor") {
		t.Fatalf("the removed and added row must share one terminal row, got %q", pair)
	}

	// A pure deletion: its line is on the left, and the right side is blank.
	del := rowTextAt(texts, "-\treturn sum")
	if del == "" {
		t.Fatalf("the pure deletion is missing:\n%s", strings.Join(texts, "\n"))
	}
	if strings.Contains(del, "* 2") {
		t.Fatalf("the new return must be its own row, not the deletion's: %q", del)
	}
	// And the addition with no removed partner sits on the right alone.
	add := rowTextAt(texts, "* 2")
	if add == "" {
		t.Fatalf("the addition is missing:\n%s", strings.Join(texts, "\n"))
	}
}

// TestSplitNumbersEachSideIndependently pins the gutters. A deleted line and an
// inserted line are two DIFFERENT lines of two different files, so their
// numbers differ: this is why a split view carries a gutter at all, and a
// gutter that copied one number to both sides would be a lie.
func TestSplitNumbersEachSideIndependently(t *testing.T) {
	app, scr := newTestApp(t, 200, 40)
	defer scr.Fini()

	// Hunk starts at 100 on both sides, so the context row is 100/100 and the
	// pair beneath it is 101 on the old side and 101 on the new — with the
	// DELETION counted only on the left, an addition after it moves the right
	// counter ahead while the left stays: that is where the two differ.
	diff := "@@ -100,2 +100,3 @@\n" +
		" keep\n" +
		"-gone\n" +
		"+added\n" +
		"+added too\n"
	texts := rowTexts(app.splitRows(classifyMarkedDiff(diff), 192))
	if got := rowTextAt(texts, "gone"); got == "" {
		t.Fatalf("the pair row is missing:\n%s", strings.Join(texts, "\n"))
	}
	pair := rowTextAt(texts, "gone")
	// Left gutter 101, right gutter 101: both sides numbered independently.
	if strings.Count(pair, "101") < 2 {
		t.Fatalf("both sides must be numbered 101, got %q", pair)
	}
	// The next row is a PURE ADDITION: the old side has no line here, so its
	// gutter must stay BLANK rather than printing a number that no old line
	// has. The right side is its own counter's next line.
	next := rowTextAt(texts, "added too")
	if strings.TrimLeft(next[:strings.Index(next, "102")], " ") != "" {
		t.Fatalf("a pure addition must leave the old gutter blank, got %q", next)
	}
	if !strings.Contains(next, "102") {
		t.Fatalf("the new side must number its second addition 102, got %q", next)
	}
}

// TestSplitRowsAreAllTheSameWidth pins the alignment the divider depends on:
// every PAIR row must be exactly sideW*2+1 cells, so the divider is a straight
// column down the viewer and nothing bleeds over it. A hunk header is chrome
// and spans the whole interior instead, so it is the one row allowed to be
// wider than a pair.
func TestSplitRowsAreAllTheSameWidth(t *testing.T) {
	app, scr := newTestApp(t, 200, 40)
	defer scr.Fini()

	const inner = 192
	rows := classifyMarkedDiff(splitFixture)
	laid := app.splitRows(rows, inner)
	sideW := (inner - 1) / 2
	want := 2*sideW + 1
	for i, ln := range laid {
		got := lineWidth(ln)
		if got == inner {
			continue // the hunk header spans the panel
		}
		if got != want {
			t.Fatalf("row %d is %d cells, want %d: %q", i, got, want, runsText(ln.runs))
		}
	}
	if got := lineWidth(laid[0]); got != inner {
		t.Fatalf("the hunk header must span the interior (%d cells), got %d", inner, got)
	}
}

// TestDiffOverlayPicksSplitWhenWide pins the width decision end to end through
// the real open path: a wide terminal opens split.
func TestDiffOverlayPicksSplitWhenWide(t *testing.T) {
	app, scr := newTestApp(t, 200, 40)
	defer scr.Fini()
	openFixtureOverlay(t, app)

	app.mu.Lock()
	split := app.diffOv.split
	app.mu.Unlock()
	if !split {
		t.Fatal("a 200-column viewer with a short diff must open split")
	}
}

// TestDiffOverlayPicksUnifiedWhenNarrow is the other half: a narrow terminal
// opens the list, so the reader gets one readable column rather than two
// unusable ones.
func TestDiffOverlayPicksUnifiedWhenNarrow(t *testing.T) {
	app, scr := newTestApp(t, 100, 40)
	defer scr.Fini()
	openFixtureOverlay(t, app)

	app.mu.Lock()
	split := app.diffOv.split
	app.mu.Unlock()
	if split {
		t.Fatal("a 100-column viewer must render unified")
	}
}

// TestDiffOverlaySwapsLayoutOnS is the toggle: `s` re-renders in the other
// layout even when the width already decided one, and it is reversible, so a
// reader is not stuck in the layout they just rejected.
func TestDiffOverlaySwapsLayoutOnS(t *testing.T) {
	app, scr := newTestApp(t, 200, 40)
	defer scr.Fini()
	openFixtureOverlay(t, app)
	app.draw()

	app.mu.Lock()
	first, firstText := app.diffOv.split, rowsText(app.diffOv.lines)
	app.mu.Unlock()
	if !first {
		t.Fatal("a 200-column viewer with a short diff must open split")
	}

	app.handleKey(tcell.NewEventKey(tcell.KeyRune, 's', tcell.ModNone))
	app.draw()
	app.mu.Lock()
	second, secondText := app.diffOv.split, rowsText(app.diffOv.lines)
	app.mu.Unlock()
	if second || secondText == firstText {
		t.Fatal("s must re-render the diff in the unified layout")
	}

	app.handleKey(tcell.NewEventKey(tcell.KeyRune, 's', tcell.ModNone))
	app.draw()
	app.mu.Lock()
	back, backText := app.diffOv.split, rowsText(app.diffOv.lines)
	app.mu.Unlock()
	if !back || backText != firstText {
		t.Fatal("s must be reversible: the split layout must come back identically")
	}
}

// TestDiffOverlayToggleNeedsNoModifier keeps the chord from stealing the real
// actions: Ctrl+S still reaches the keymap instead of swapping the layout.
func TestDiffOverlayToggleNeedsNoModifier(t *testing.T) {
	app, scr := newTestApp(t, 200, 40)
	defer scr.Fini()
	openFixtureOverlay(t, app)
	app.mu.Lock()
	before := app.diffOv.split
	app.mu.Unlock()

	app.handleKey(tcell.NewEventKey(tcell.KeyRune, 's', tcell.ModCtrl))
	app.mu.Lock()
	after := app.diffOv.split
	app.mu.Unlock()
	if after != before {
		t.Fatal("Ctrl+S must not be the layout toggle")
	}
}

// TestDiffOverlayReflowsOnResize pins that a resize re-decides the layout: the
// width IS the decision, so dragging a narrow terminal past the threshold must
// bring the pair in rather than leave it rendering at the old budget.
func TestDiffOverlayReflowsOnResize(t *testing.T) {
	app, scr := newTestApp(t, 100, 40)
	defer scr.Fini()
	openFixtureOverlay(t, app)

	app.mu.Lock()
	if app.diffOv.split {
		app.mu.Unlock()
		t.Fatal("a 100-column viewer must start unified")
	}
	app.width = 200
	app.diffOv.reflow(app)
	split := app.diffOv.split
	app.mu.Unlock()
	if !split {
		t.Fatal("growing past the threshold must bring the split layout in")
	}

	app.mu.Lock()
	app.width = 100
	app.diffOv.reflow(app)
	split = app.diffOv.split
	app.mu.Unlock()
	if split {
		t.Fatal("shrinking below the threshold must fall back to unified")
	}
}

// TestHunkCountsReadsBothSides pins the one parse the gutter depends on: the
// START line of each side, with the hunk's line count skipped.
func TestHunkCountsReadsBothSides(t *testing.T) {
	for _, tc := range []struct {
		hdr      string
		old, new int
	}{
		{"@@ -1,3 +1,4 @@", 1, 1},
		{"@@ -12,7 +14,9 @@ func main() {", 12, 14},
		{"@@ -900 +1,2 @@", 900, 1},
		{"@@ -0,0 +1 @@", 0, 1},
	} {
		old, nw := hunkCounts(tc.hdr)
		if old != tc.old || nw != tc.new {
			t.Fatalf("hunkCounts(%q) = (%d,%d), want (%d,%d)", tc.hdr, old, nw, tc.old, tc.new)
		}
	}
}

// TestDiffNumCellsSizesTheGutterToTheDiff pins the gutter width: one column
// wide for the whole diff, sized by the widest line number it declares
// (including the END of the last hunk, which is what a later hunk needs).
func TestDiffNumCellsSizesTheGutterToTheDiff(t *testing.T) {
	if got := diffNumCells(classifyMarkedDiff("@@ -1,3 +1,4 @@\n a\n")); got != 1 {
		t.Fatalf("a diff whose last hunk ends at line 4 needs a 1-digit gutter, got %d", got)
	}
	if got := diffNumCells(classifyMarkedDiff("@@ -98,5 +98,7 @@\n a\n")); got != 3 {
		t.Fatalf("a diff whose last hunk ends at line 104 needs a 3-digit gutter, got %d", got)
	}
	if got := diffNumCells(classifyMarkedDiff("@@ -1000,3 +1000,3 @@\n a\n")); got != diffNumCap {
		t.Fatalf("a four-digit diff must use the cap, got %d", got)
	}
	if got := diffNumCells(classifyMarkedDiff("@@ -99999,3 +99999,3 @@\n a\n")); got != diffNumCap {
		t.Fatalf("a five-digit diff must still cap at %d, got %d", diffNumCap, got)
	}
}
