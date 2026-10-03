package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// TestDiffOverlayRowBandReachesTheBorder pins the band's geometry in the
// popup: a changed row's band is a stripe, so it runs to the panel's right
// border. It was padded to the TRANSCRIPT's wrap budget (contentWidth), not the
// popup's, so with the sidebar open a changed row banded 70 cells and the rest
// of the popup's interior stayed terminal-default — a stripe that stopped
// halfway across the box, in the middle of the line it belongs to.
func TestDiffOverlayRowBandReachesTheBorder(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	app, scr := newTestApp(t, 120, 30)
	defer scr.Fini()

	app.mu.Lock()
	app.dock = &dockState{mode: DockShow}
	app.dock.ops = DockOps{Session: func() (string, string) { return "s", "sess" }}
	app.blocks = append(app.blocks, &Block{
		Kind: KindToolDone, ToolName: "edit",
		Diff: "--- a/internal/tui/dock.go\n+++ b/internal/tui/dock.go\n@@ -1,1 +1,1 @@\n-old\n+new\n",
	})
	app.DockBump()
	app.dockBuild()
	opened := app.openDiffOverlay("internal/tui/dock.go")
	inner := app.diffOverlayInner()
	app.mu.Unlock()
	if !opened {
		t.Fatal("no diff overlay for the changed file")
	}
	if want := app.width - 2*diffPad - 3; inner != want {
		t.Fatalf("overlay interior = %d, want the panel minus its margins and borders (%d)", inner, want)
	}
	app.draw()

	remRow, _ := app.th.Slot(theme.ToolDiffRemovedBg)
	addRow, _ := app.th.Slot(theme.ToolDiffAddedBg)
	band := map[tcell.Color]bool{
		app.cellColor(remRow): true,
		app.cellColor(addRow): true,
	}
	// A band's rows start at the panel's inset (margin + border), so the stripe
	// has to reach the border one cell short of the panel's right edge.
	right := app.width - diffPad - 2
	found := 0
	for y := 0; y < app.height; y++ {
		for x := 0; x < app.width; x++ {
			_, _, st, _ := scr.GetContent(x, y)
			_, bg, _ := st.Decompose()
			if !band[bg] {
				continue
			}
			found++
			_, _, est, _ := scr.GetContent(right, y)
			_, ebg, _ := est.Decompose()
			if !band[ebg] {
				t.Fatalf("changed row %d bands %v but its cell at the panel's right edge (%d) is %v: the stripe stops inside the box", y, bg, right, ebg)
			}
		}
	}
	if found == 0 {
		t.Fatal("no changed row painted a band at all")
	}
}

// TestDiffOverlayRunsAdvance pins the row paint itself: a diff row is several
// styled runs (marker, body, changed words, and the pad that carries the band
// to the panel's edge), and each one goes at the column the one before it
// ended. Drawing them all at the row's first cell stacked them, so the marker
// and the words vanished under the pad — the longest run, and the one that
// won.
func TestDiffOverlayRunsAdvance(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	app, scr := newTestApp(t, 120, 30)
	defer scr.Fini()

	app.mu.Lock()
	app.dock = &dockState{mode: DockShow}
	app.dock.ops = DockOps{Session: func() (string, string) { return "s", "sess" }}
	app.blocks = append(app.blocks, &Block{
		Kind: KindToolDone, ToolName: "edit",
		Diff: "--- a/internal/tui/dock.go\n+++ b/internal/tui/dock.go\n@@ -1,1 +1,1 @@\n-oldValue\n+newValue\n",
	})
	app.DockBump()
	app.dockBuild()
	if !app.openDiffOverlay("internal/tui/dock.go") {
		app.mu.Unlock()
		t.Fatal("no diff overlay for the changed file")
	}
	app.mu.Unlock()
	app.draw()

	// The removed row is the one wearing the removed band; its marker is the
	// first cell a row is painted on — the panel's margin, its border, then
	// the inset the rows start at.
	remRow, _ := app.th.Slot(theme.ToolDiffRemovedBg)
	remBg := app.cellColor(remRow)
	markerX, markerY := 2*diffPad, -1
	for y := 0; y < app.height && markerY < 0; y++ {
		r, _, st, _ := scr.GetContent(markerX, y)
		_, bg, _ := st.Decompose()
		if r == '-' && bg == remBg {
			markerY = y
		}
	}
	if markerY < 0 {
		t.Fatal("no removed row on screen wearing its band: the runs are painted over each other")
	}
	if row := diffRowText(scr, markerY, app.width); !strings.Contains(row, "-oldValue") {
		t.Fatalf("removed row reads %q, want the marker and the words in order", trimRow(row))
	}
}

// TestDiffOverlayKeepsIndentation pins the tab: Block.Diff reaches the popup
// raw, and a raw \t is a zero-width cell, so an indented diff lost its
// indentation here while the transcript's own diff block kept it.
func TestDiffOverlayKeepsIndentation(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	app, scr := newTestApp(t, 120, 30)
	defer scr.Fini()

	app.mu.Lock()
	app.dock = &dockState{mode: DockShow}
	app.dock.ops = DockOps{Session: func() (string, string) { return "s", "sess" }}
	app.blocks = append(app.blocks, &Block{
		Kind: KindToolDone, ToolName: "edit",
		Diff: "--- a/internal/tui/dock.go\n+++ b/internal/tui/dock.go\n@@ -1,1 +1,1 @@\n-\treturn oldValue\n+\treturn newValue\n",
	})
	app.DockBump()
	app.dockBuild()
	if !app.openDiffOverlay("internal/tui/dock.go") {
		app.mu.Unlock()
		t.Fatal("no diff overlay for the changed file")
	}
	app.mu.Unlock()
	app.draw()

	for y := 0; y < app.height; y++ {
		row := ""
		for x := 0; x < app.width; x++ {
			r, _, _, _ := scr.GetContent(x, y)
			if r == 0 {
				r = ' '
			}
			row += string(r)
		}
		if contains(row, "return oldValue") {
			if !strings.Contains(row, "-   return oldValue") {
				t.Fatalf("removed row reads %q: the tab became zero cells, not the transcript's three spaces", trimRow(row))
			}
			return
		}
	}
	t.Fatal("the removed row is not on screen")
}

// TestDiffOverlayRewrapsOnResize pins that the rows follow the popup's width.
// They were built once at open, so a window drag left them wrapped for the old
// width — overflowing the border, or banding a fraction of it.
func TestDiffOverlayRewrapsOnResize(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	app, scr := newTestApp(t, 120, 30)
	defer scr.Fini()

	line := "+func row() { longEnoughTailToMakeTheRowWrapAtTheNarrowerPopup }\n"
	app.mu.Lock()
	app.dock = &dockState{mode: DockShow}
	app.dock.ops = DockOps{Session: func() (string, string) { return "s", "sess" }}
	app.blocks = append(app.blocks, &Block{
		Kind: KindToolDone, ToolName: "edit",
		Diff: "--- a/internal/tui/dock.go\n+++ b/internal/tui/dock.go\n@@ -1,1 +1,1 @@\n-old\n" + line,
	})
	app.DockBump()
	app.dockBuild()
	if !app.openDiffOverlay("internal/tui/dock.go") {
		app.mu.Unlock()
		t.Fatal("no diff overlay for the changed file")
	}
	narrow := len(app.diffOv.lines)
	app.mu.Unlock()

	// The same rows in a narrower terminal: one more screen row of them.
	scr.SetSize(60, 30)
	app.handleKey(tcell.NewEventResize(60, 30))
	app.draw()

	app.mu.Lock()
	got := len(app.diffOv.lines)
	inner := app.diffOv.inner
	app.mu.Unlock()
	if inner != 60-2*diffPad-3 {
		t.Fatalf("overlay interior after resize = %d, want %d", inner, 60-2*diffPad-3)
	}
	if got <= narrow {
		t.Fatalf("rows after the resize = %d, want more than %d: the rows were not re-wrapped", got, narrow)
	}
	// And nothing spills past the panel's border.
	for y := 0; y < 30; y++ {
		if r, _, _, _ := scr.GetContent(60-diffPad-1, y); r == 0 {
			t.Fatalf("row %d paints no glyph at the panel's right border", y)
		}
	}
}

func diffRowText(scr tcell.SimulationScreen, y, w int) string {
	row := ""
	for x := 0; x < w; x++ {
		r, _, _, _ := scr.GetContent(x, y)
		if r == 0 {
			r = ' '
		}
		row += string(r)
	}
	return row
}

func trimRow(row string) string {
	i, j := 0, len(row)
	for i < j && row[i] == ' ' {
		i++
	}
	for j > i && row[j-1] == ' ' {
		j--
	}
	return row[i:j]
}

// TestSidebarDiffClickPaintsEveryRow is the human path end to end: a click on
// the sidebar's changed-file row, through handleKey (the way a terminal sends
// it), then the frame that click asks for. Every run of every row has to be
// readable — the marker, the words, and the band's stripe to the panel's edge.
func TestSidebarDiffClickPaintsEveryRow(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	app, scr := newTestApp(t, 160, 40)
	defer scr.Fini()

	app.SetDockMode(DockShow)
	app.mu.Lock()
	app.dock = &dockState{mode: DockShow}
	app.dock.ops = DockOps{Session: func() (string, string) { return "s", "sess" }}
	app.blocks = append(app.blocks, &Block{
		Kind: KindToolDone, ToolName: "edit",
		Diff: "--- a/internal/tui/dock.go\n+++ b/internal/tui/dock.go\n@@ -1,2 +1,2 @@\n-\treturn oldValue + aTailLongEnoughToWrapAtThePopupWidth\n+\treturn newValue + aTailLongEnoughToWrapAtThePopupWidth\n",
	})
	app.DockBump()
	app.dockBuild()
	app.mu.Unlock()
	app.draw()

	x, y, ok := dockCell(t, app, "internal/tui/dock.go")
	if !ok {
		t.Fatal("the panel painted no changed-file row to click")
	}
	app.handleKey(tcell.NewEventMouse(x, y, tcell.Button1, tcell.ModNone))
	app.handleKey(tcell.NewEventMouse(x, y, tcell.ButtonNone, tcell.ModNone))
	app.mu.Lock()
	opened := app.diffOv != nil
	app.mu.Unlock()
	if !opened {
		t.Fatalf("a click on the panel's changed-file row did not open the viewer:\n%s", screenRows(scr))
	}
	app.draw()

	painted := screenRows(scr)
	for _, want := range []string{"-   return oldValue", "+   return newValue"} {
		if !strings.Contains(painted, want) {
			t.Fatalf("the viewer did not paint %q:\n%s", want, painted)
		}
	}
	// The two changed rows were long enough to wrap at this width, so their
	// tails sit on rows of their own — a run stack would have eaten them.
	if !strings.Contains(painted, "aTailLongEnoughToWrapAtThePopupWidth") {
		t.Fatalf("the wrapped tail is not on screen:\n%s", painted)
	}
	// And no row overflows the panel's border.
	for i, row := range strings.Split(painted, "\n") {
		if !strings.Contains(row, "│") {
			continue
		}
		if strings.Count(row, "│") < 2 {
			t.Fatalf("overlay row %d lost its right border: %q", i, row)
		}
	}
}

// TestDiffOverlayClickOnAnotherFileRepointsIt: while the popup is up, the
// sidebar is under it — that is the only place a human can reach the other
// changed files. A click on a second file's row must re-point the viewer at
// that file, not close it and not repaint the old diff.
func TestDiffOverlayClickOnAnotherFileRepointsIt(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	app, scr := newTestApp(t, 160, 40)
	defer scr.Fini()

	app.SetDockMode(DockShow)
	app.mu.Lock()
	app.dock = &dockState{mode: DockShow}
	app.dock.ops = DockOps{Session: func() (string, string) { return "s", "sess" }}
	app.blocks = append(app.blocks, &Block{
		Kind: KindToolDone, ToolName: "edit",
		Diff: "--- a/internal/tui/dock.go\n+++ b/internal/tui/dock.go\n@@ -1,1 +1,1 @@\n-oldValue\n+newValue\n",
	}, &Block{
		Kind: KindToolDone, ToolName: "edit",
		Diff: "--- a/internal/tui/diff.go\n+++ b/internal/tui/diff.go\n@@ -1,1 +1,1 @@\n-otherOld\n+otherNew\n",
	})
	app.DockBump()
	app.dockBuild()
	app.mu.Unlock()
	app.draw()

	firstX, firstY, ok := dockCell(t, app, "internal/tui/dock.go")
	if !ok {
		t.Skip("the panel painted no first changed-file row")
	}
	app.handleKey(tcell.NewEventMouse(firstX, firstY, tcell.Button1, tcell.ModNone))
	app.handleKey(tcell.NewEventMouse(firstX, firstY, tcell.ButtonNone, tcell.ModNone))
	app.mu.Lock()
	opened := app.diffOv != nil
	app.mu.Unlock()
	if !opened {
		t.Fatalf("a click on the panel's first changed file did not open the viewer:\n%s", screenRows(scr))
	}
	app.draw()

	secondX, secondY, ok := dockCell(t, app, "internal/tui/diff.go")
	if !ok {
		t.Skip("the panel painted no second changed-file row")
	}
	app.handleKey(tcell.NewEventMouse(secondX, secondY, tcell.Button1, tcell.ModNone))
	app.handleKey(tcell.NewEventMouse(secondX, secondY, tcell.ButtonNone, tcell.ModNone))
	app.mu.Lock()
	ov := app.diffOv
	path := ""
	if ov != nil {
		path = ov.path
	}
	app.mu.Unlock()
	if ov == nil {
		t.Fatal("a click on another changed file closed the viewer instead of re-pointing it")
	}
	if path != "internal/tui/diff.go" {
		t.Fatalf("viewer still shows %q, want the file that was clicked", path)
	}
	app.draw()
	painted := screenRows(scr)
	if !strings.Contains(painted, "otherNew") {
		t.Fatalf("the viewer did not repaint the file it was re-pointed at:\n%s", painted)
	}
}
