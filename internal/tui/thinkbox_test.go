package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// thinkLines builds n labelled reasoning rows, so a window's head and tail are
// readable in a failure message ("t-029" at the top of the newest window).
func thinkLines(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "t-%03d\n", i)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// renderBox returns block i's rendered rows as text, with mu held for the call.
func renderBox(app *App, i int, w int) []string {
	app.mu.Lock()
	defer app.mu.Unlock()
	lines := app.blockLines(i, app.blocks[i], w)
	out := make([]string, len(lines))
	for j, ln := range lines {
		out[j] = lineText(ln)
	}
	return out
}

// TestThinkBoxRendersInAFixedWindow pins the reasoning box's shape: the same
// rounded frame a result gets, a state label in the top border, every row the
// frame's full width, at most thinkBoxRows body rows, and a hidden-row notice
// that names the Ctrl+O affordance — until Ctrl+O shows every row.
func TestThinkBoxRendersInAFixedWindow(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.BeginThinking()
	app.AppendThinking(thinkLines(40))
	app.EndThinking()

	got := renderBox(app, 0, 80)
	const body = thinkBoxRows
	if len(got) != 1+1+body+1 { // top + notice + window + bottom
		t.Fatalf("box rows = %d, want %d:\n%s", len(got), 2+body+1, strings.Join(got, "\n"))
	}
	if !strings.Contains(got[0], "Thought for") {
		t.Fatalf("top border = %q, want the settled state", got[0])
	}
	if !strings.Contains(got[1], "28 rows hidden") || !strings.Contains(got[1], "Ctrl+O") {
		t.Fatalf("hidden notice = %q", got[1])
	}
	// The window is tail-anchored: the newest reasoning is what a reader
	// following the turn sees.
	if !strings.Contains(got[2], "t-029") || !strings.Contains(got[1+body], "t-040") {
		t.Fatalf("window = %q .. %q, want t-029 .. t-040", got[2], got[1+body])
	}
	for i, ln := range got {
		if w := width(ln); w != 80 {
			t.Fatalf("row %d is %d cells wide, want 80: %q", i, w, ln)
		}
	}

	if !app.ToggleBoxExpand() {
		t.Fatal("Ctrl+O found no reasoning box to expand")
	}
	got = renderBox(app, 0, 80)
	if len(got) != 1+40+1 {
		t.Fatalf("expanded box rows = %d, want 42", len(got))
	}
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "hidden") {
		t.Fatalf("expanded frame still hides rows:\n%s", joined)
	}
	if !strings.Contains(got[1], "t-001") || !strings.Contains(got[40], "t-040") {
		t.Fatalf("expanded window = %q .. %q, want every row", got[1], got[40])
	}
}

// TestThinkWindowClampsAtBothEnds pins the window's edges directly: an offset
// past the oldest reasoning reads as the oldest window the box can show, and a
// negative one as the newest, so a wheel over-scroll cannot blank the box.
func TestThinkWindowClampsAtBothEnds(t *testing.T) {
	for _, tc := range []struct {
		n, off, wantStart, wantEnd int
	}{
		{0, 0, 0, 0},
		{5, 0, 0, 5},
		{30, 0, 18, 30},
		{30, 5, 13, 25},
		{30, 100, 0, thinkBoxRows}, // clamped to the head, not blanked
		{12, 1, 0, 12},             // a box as tall as its content cannot scroll
	} {
		start, end := thinkWindow(tc.n, tc.off)
		if start != tc.wantStart || end != tc.wantEnd {
			t.Fatalf("thinkWindow(%d, %d) = (%d, %d), want (%d, %d)",
				tc.n, tc.off, start, end, tc.wantStart, tc.wantEnd)
		}
	}
}

// TestWheelOnlyScrollsAFocusedThinkBox pins the routing the mouse actually
// has: the wheel belongs to the transcript until a left click on a reasoning
// box focuses it, and then — and only then — the notch moves that box's own
// window. The notch itself never moves the focus, which is the whole point: a
// box that slides under a stationary pointer used to steal the wheel, so
// reading the transcript could not be done without scrolling every box it
// crossed. A click anywhere else hands the wheel back.
func TestWheelOnlyScrollsAFocusedThinkBox(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	longTranscript(t, app, 200) // scrollable prose above the box
	app.BeginThinking()
	app.AppendThinking(thinkLines(40))
	app.EndThinking()
	app.draw()

	// boxY is the box's screen row, recomputed wherever the transcript may have
	// moved: the pointer has to be over the box, not over the screen row the box
	// occupied before the last scroll.
	boxY := func() int {
		app.mu.Lock()
		defer app.mu.Unlock()
		top, _ := app.selViewport()
		return app.transcriptTop() + int(app.rowIdx.start[len(app.blocks)-1]) - top
	}
	chrome := func() (hdr, vp int) {
		app.mu.Lock()
		defer app.mu.Unlock()
		_, vp = app.selViewport()
		return app.transcriptTop(), vp
	}
	transcript := func() int {
		app.mu.Lock()
		defer app.mu.Unlock()
		return app.sm.offset
	}
	thinkOff := func() int {
		app.mu.Lock()
		defer app.mu.Unlock()
		return app.blocks[len(app.blocks)-1].ThinkOff
	}
	focus := func() int {
		app.mu.Lock()
		defer app.mu.Unlock()
		return app.thinkFocus
	}
	// wheel and click both go through the real event path, so the press edge and
	// the wheel's own bit-masking are the ones under test.
	wheel := func(y int, btn tcell.ButtonMask) {
		app.handleKey(tcell.NewEventMouse(3, y, btn, tcell.ModNone))
	}
	click := func(y int) {
		wheel(y, tcell.Button1)
		wheel(y, tcell.ButtonNone)
	}

	hdr, vp := chrome()
	if y := boxY(); y <= hdr || y+thinkBoxRows >= hdr+vp {
		t.Fatalf("thinking box is not fully on screen: boxY=%d hdr=%d vp=%d", y, hdr, vp)
	}

	// Nothing focused: the notch over the box is the transcript's, and the box
	// does not move. This is the behavior the click gate exists for.
	before := transcript()
	wheel(boxY()+1, tcell.WheelUp)
	if off := thinkOff(); off != 0 {
		t.Fatalf("ThinkOff = %d after a notch with nothing focused, want 0", off)
	}
	if after := transcript(); after != before+3 {
		t.Fatalf("transcript offset = %d after a notch over the box, want %d", after, before+3)
	}

	// The click focuses the box — the last block, and the only reasoning one —
	// and the frame says so: the focused box draws a bold rule.
	click(boxY() + 1)
	app.mu.Lock()
	box := len(app.blocks) - 1
	app.mu.Unlock()
	if got := focus(); got != box {
		t.Fatalf("thinkFocus = %d after a click on the box, want %d", got, box)
	}
	app.draw() // the UI loop repaints after handleKey
	// The rule, not the label set into it: the label is bold whether the box is
	// focused or not (boxTop), so only the rule cells carry the focus mark. The
	// box starts at x=3 (rail + pad), which is its top-left corner.
	y := boxY()
	if _, _, attr := cellStyle(scr, 3, y).Decompose(); attr&tcell.AttrBold == 0 {
		t.Fatalf("focused box's top-left corner at (3,%d) is not bold", y)
	}
	// The rule runs the box's full width, so a cell inside it proves the whole
	// border took the mark, not just the corner glyph.
	if _, _, attr := cellStyle(scr, 60, y).Decompose(); attr&tcell.AttrBold == 0 {
		t.Fatalf("focused box's top rule at (60,%d) is not bold", y)
	}

	// Focused: the notch moves the box's window and leaves the transcript alone.
	before = transcript()
	wheel(boxY()+1, tcell.WheelUp)
	if off := thinkOff(); off != 1 {
		t.Fatalf("ThinkOff = %d after one notch on the focused box, want 1", off)
	}
	if after := transcript(); after != before {
		t.Fatalf("transcript moved under a focused box: %d -> %d", before, after)
	}
	// The scrolled window is what the box renders: t-028 replaces t-029's row.
	got := renderBox(app, box, 80)
	if !strings.Contains(got[2], "t-028") {
		t.Fatalf("scrolled window starts at %q, want t-028", got[2])
	}

	// At the oldest reasoning the wheel falls through instead of stalling.
	app.mu.Lock()
	rows := len(app.thinkRows(app.blocks[box], app.contentWidth()))
	app.mu.Unlock()
	head := max(0, rows-thinkBoxRows)
	for range head {
		wheel(boxY()+1, tcell.WheelUp)
	}
	if off := thinkOff(); off != head {
		t.Fatalf("ThinkOff = %d at the head, want %d", off, head)
	}
	before = transcript()
	wheel(boxY()+1, tcell.WheelUp)
	if after := transcript(); after != before+3 {
		t.Fatalf("wheel at the box's head did not fall through: %d -> %d", before, after)
	}

	// A click off the box — here the prose above it — takes the focus back, and
	// the wheel is the transcript's again.
	click(hdr)
	if got := focus(); got != -1 {
		t.Fatalf("thinkFocus = %d after a click off the box, want -1", got)
	}
	before = transcript()
	wheel(boxY()+1, tcell.WheelUp)
	if off := thinkOff(); off != head {
		t.Fatalf("ThinkOff = %d after the focus was dropped, want %d", off, head)
	}
	if after := transcript(); after != before+3 {
		t.Fatalf("transcript offset = %d after focus was dropped, want %d", after, before+3)
	}
}

// TestBlockAtFindsTheOwningBlock pins the hit-test the click arm rides: for
// every row of a
// mixed transcript, blockAt names the one block whose span contains it — the
// mapping the routing trusts, and the mapping a binary search can get wrong at a
// block's last row (a block owns [start[i], start[i+1]) ).
func TestBlockAtFindsTheOwningBlock(t *testing.T) {
	app := idxApp(120, 40)
	idxFill(app, 3, 60)
	app.BeginThinking()
	app.AppendThinking(thinkLines(40))
	app.EndThinking()
	idxFill(app, 2, 30)

	app.mu.Lock()
	defer app.mu.Unlock()
	total := app.sync(app.contentWidth())
	if got, want := int32(len(app.blocks)), int32(len(app.rowIdx.rend)); got != want {
		t.Fatalf("row index has %d blocks, transcript has %d", want, got)
	}
	for r := range total {
		bi := app.rowIdx.blockAt(r)
		if bi < 0 {
			t.Fatalf("row %d has no block", r)
		}
		if !(app.rowIdx.start[bi] <= r && r < app.rowIdx.start[bi+1]) {
			t.Fatalf("row %d mapped to block %d spanning [%d,%d)",
				r, bi, app.rowIdx.start[bi], app.rowIdx.start[bi+1])
		}
	}
	for _, r := range []int32{-1, total} {
		if bi := app.rowIdx.blockAt(r); bi != -1 {
			t.Fatalf("row %d mapped to block %d, want none", r, bi)
		}
	}
}

// TestDrawnThinkingBoxDrawsOneEdge pins the painted frame: the reasoning box
// draws its own border through draw()'s row path and its rows carry no accent
// rail, so a painted body row has exactly the frame's two vertical edges. A
// re-added rail would put a third line down the box's left side — a doubling a
// rendered-lines assertion cannot see, because the rail is chrome, not a run.
func TestDrawnThinkingBoxDrawsOneEdge(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.BeginThinking()
	app.AppendThinking(thinkLines(40))
	app.EndThinking()
	app.draw()

	app.mu.Lock()
	top, _ := app.selViewport()
	box := app.transcriptTop() + int(app.rowIdx.start[len(app.blocks)-1]) - top
	app.mu.Unlock()
	rows := strings.Split(screenText(scr), "\n")
	if box+1 >= len(rows) {
		t.Fatalf("box row %d is off the painted screen (%d rows)", box, len(rows))
	}
	body := strings.TrimRight(rows[box+1], " ")
	if n := strings.Count(body, "│"); n != 2 {
		t.Fatalf("painted body row has %d vertical edges, want 2: %q", n, body)
	}
	if strings.Contains(body, "┃") {
		t.Fatalf("painted body row still carries the block rail: %q", body)
	}
}
