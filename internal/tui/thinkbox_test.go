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

// TestWheelScrollsTheThinkBoxUnderIt pins the routing: the notch over a
// reasoning box moves that box's own window and leaves the transcript where it
// was; the notch anywhere else scrolls the transcript, as it always has.
func TestWheelScrollsTheThinkBoxUnderIt(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
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
	wheel := func(y int, btn tcell.ButtonMask) {
		app.handleKey(tcell.NewEventMouse(3, y, btn, tcell.ModNone))
	}

	hdr, vp := chrome()
	if y := boxY(); y <= hdr || y+thinkBoxRows >= hdr+vp {
		t.Fatalf("thinking box is not fully on screen: boxY=%d hdr=%d vp=%d", y, hdr, vp)
	}

	before := transcript()
	wheel(boxY()+1, tcell.WheelUp)
	if off := thinkOff(); off != 1 {
		t.Fatalf("ThinkOff = %d after one notch over the box, want 1", off)
	}
	if after := transcript(); after != before {
		t.Fatalf("transcript moved under the wheel: %d -> %d", before, after)
	}
	// The scrolled window is what the box renders: t-028 replaces t-029's row.
	got := renderBox(app, len(app.blocks)-1, 80)
	if !strings.Contains(got[2], "t-028") {
		t.Fatalf("scrolled window starts at %q, want t-028", got[2])
	}

	wheel(hdr, tcell.WheelUp) // above the box: the transcript's own scroll
	if off := thinkOff(); off != 1 {
		t.Fatalf("ThinkOff = %d after a notch off the box, want 1", off)
	}
	if after := transcript(); after != before+3 {
		t.Fatalf("transcript offset = %d, want %d", after, before+3)
	}

	// At the oldest reasoning the wheel falls through instead of stalling.
	app.mu.Lock()
	rows := len(app.thinkRows(app.blocks[len(app.blocks)-1], app.contentWidth()))
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
}

// TestBlockAtFindsTheOwningBlock pins the wheel's hit-test: for every row of a
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
