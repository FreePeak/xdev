//go:build !windows

package tui

import (
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/theme"
	"github.com/gdamore/tcell/v2"
)

// The reported symptom, and the pane that produces it.
//
// A pane renderer that treats a frame as "here is the region that changed" and
// keeps its own grid otherwise — the shape every cell-grid overlay has — holds
// ITS value in the columns xdev never addressed. Its clear pattern is X, as
// tcell's simulation screen's is (simulation.go: s.fillchar = 'X'), so the
// prompt box reads "TypeXaXmessage…XXXXXX": xdev's own placeholder with the
// space columns left as the pane's fill.
//
// This is the end-to-end proof. It drives the REAL App over a REAL terminfo
// screen (so the bytes on the wire are the bytes a terminal would receive),
// applies each frame to such a pane, resets the pane's grid the way a
// reattach or a re-clearing resize does, and asserts the box survives.
type paneGrid struct {
	w, h int
	cell []rune
}

func newPaneGrid(w, h int) *paneGrid {
	g := &paneGrid{w: w, h: h, cell: make([]rune, w*h)}
	for i := range g.cell {
		g.cell[i] = 'X'
	}
	return g
}

// apply walks one frame the way such a renderer does: a cursor move is not
// content, so a column the frame never addressed keeps the grid's own value.
func (g *paneGrid) apply(frame []byte) {
	x, y := 0, 0
	for i := 0; i < len(frame); {
		if frame[i] == 0x1b {
			j := i + 1
			if j < len(frame) && frame[j] == '[' {
				k := j + 1
				for k < len(frame) && !(frame[k] >= '@' && frame[k] <= '~') {
					k++
				}
				if k >= len(frame) {
					return
				}
				if frame[k] == 'H' || frame[k] == 'f' {
					p := strings.Split(string(frame[j+1:k]), ";")
					y, x = 0, 0
					if len(p) > 0 && p[0] != "" {
						y = vtAtoi(p[0]) - 1
					}
					if len(p) > 1 && p[1] != "" {
						x = vtAtoi(p[1]) - 1
					}
				}
				i = k + 1
				continue
			}
			if j < len(frame) && strings.IndexByte("())(*+", frame[j]) >= 0 {
				i = j + 2
				continue
			}
			i = j + 1
			continue
		}
		r, sz := vtRune(string(frame[i:]))
		if sz == 0 {
			return
		}
		switch r {
		case '\r':
			x = 0
		case '\n':
			y++
			x = 0
		case '\b':
			x--
		case '\t':
			x += 8 - x%8
		default:
			if x >= 0 && x < g.w && y >= 0 && y < g.h {
				g.cell[y*g.w+x] = r
			}
			x++
		}
		i += sz
	}
}

func (g *paneGrid) row(y int) string {
	var b strings.Builder
	for x := 0; x < g.w; x++ {
		b.WriteRune(g.cell[y*g.w+x])
	}
	return b.String()
}

// wireApp is the real App over a real terminfo screen whose frames are captured.
func wireApp(t *testing.T, w, h int) (*App, *frameTty) {
	t.Helper()
	t.Setenv("TERM", "xterm-256color")
	ft := &frameTty{w: w, h: h}
	scr, err := tcell.NewTerminfoScreenFromTty(ft)
	if err != nil {
		t.Skip("no terminfo:", err)
	}
	if err := scr.Init(); err != nil {
		t.Skip("init:", err)
	}
	t.Cleanup(scr.Fini)
	app := New(scr, theme.Load("groknight"), "test/free", "sess1234")
	app.width, app.height = w, h
	// The context dock is off: it owns the columns right of the box, and this
	// is about the box's own columns.
	app.SetDockMode(DockHide)
	return app, ft
}

// TestComposerBoxSurvivesAPaneReset is the reported bug. A pane's grid is
// reset underneath a LONG-RUNNING session — nothing in xdev observes it — and
// the frames that follow are deltas, so before the fix the box kept the pane's
// fill and read "TypeXaXmessage…XXXXXX". Fails against the pre-fix shape
// (markComposerDirty removed from draw).
func TestComposerBoxSurvivesAPaneReset(t *testing.T) {
	const W, H = 120, 40
	app, ft := wireApp(t, W, H)
	app.AddUserBlock("hi")
	app.SetRunning(true)
	app.draw()

	grid := newPaneGrid(W, H)
	grid.apply(ft.take())
	row := app.height - app.composerRows() // the draft row: the pads sit above it

	// The pane's grid is reset: a reattach, a cleared terminal, a resize that
	// re-clears. xdev hears nothing, so the next frames are deltas.
	grid = newPaneGrid(W, H)

	// A long-running turn: frames stream in for a while. This is what makes
	// the bug visible — the box's cells only move when the transcript tail,
	// the spinner or the clock change, so a short session gets a whole repaint
	// by luck and a long one does not.
	for i := 0; i < 40; i++ {
		for j := 0; j < 5; j++ {
			app.AddSystemBlock("chunk " + strings.Repeat("s", 70))
		}
		app.st.spinnerIdx = i % len(app.th.SpinnerFrames())
		app.draw()
		grid.apply(ft.take())
	}

	line := grid.row(row)
	t.Logf("pane row %d: %q", row, line)
	if !strings.Contains(line, "Type a message") {
		t.Fatalf("the box lost its placeholder in the pane: row = %q", line)
	}
	if strings.Contains(line, "TypeXaXmessage") || strings.Contains(line, "XXXX") {
		t.Fatalf("the reported symptom: row = %q", line)
	}
}

// TestComposerBoxIsWholeOnTheWire is the same fact at the layer below: the
// frame must carry every column of the box, not the columns that happen to
// have moved. It is what markComposerDirty buys, and it is asserted so the
// cost is visible rather than assumed.
func TestComposerBoxIsWholeOnTheWire(t *testing.T) {
	const W, H = 120, 40
	app, ft := wireApp(t, W, H)
	app.AddUserBlock("hi")
	app.SetRunning(true)
	app.draw()
	ft.take()

	row := app.height - 1 - app.composerRows() + 1 // 1-based, as the wire is
	if got := app.rightEdge(); got != W {
		t.Fatalf("the box must span the pane for this measurement: rightEdge %d of %d", got, W)
	}
	const frames = 40
	whole := 0
	for i := 0; i < frames; i++ {
		for j := 0; j < 5; j++ {
			app.AddSystemBlock("chunk " + strings.Repeat("s", 70))
		}
		app.st.spinnerIdx = i % len(app.th.SpinnerFrames())
		app.draw()
		cells := frameCells(ft.take(), W, H)
		if len(cells[row]) >= W {
			whole++
		}
	}
	t.Logf("composer row arrived whole on %d of %d frames", whole, frames)
	if whole != frames {
		t.Fatalf("the box is still a delta: whole on %d of %d frames", whole, frames)
	}
}

// TestComposerCostIsBounded pins the price of the fix, so a future change
// that widens the marked region shows up here rather than as a session that
// feels slower: the marked region is the box's own rows, not the screen.
func TestComposerCostIsBounded(t *testing.T) {
	const W, H = 120, 40
	app, ft := wireApp(t, W, H)
	app.AddUserBlock("hi")
	app.SetRunning(true)
	app.draw()
	ft.take()

	stream := func() int {
		n := 0
		for i := 0; i < 40; i++ {
			for j := 0; j < 5; j++ {
				app.AddSystemBlock("chunk " + strings.Repeat("s", 70))
			}
			app.st.spinnerIdx = i % len(app.th.SpinnerFrames())
			app.draw()
			n += len(ft.take())
		}
		return n
	}
	withFix := stream()

	// The same run with the box NOT marked: the delta contract, which is what
	// the fix deliberately gives up on the wire. paint() is driven directly, so
	// no production hook is involved in the measurement.
	withoutFix := 0
	for i := 0; i < 40; i++ {
		for j := 0; j < 5; j++ {
			app.AddSystemBlock("chunk " + strings.Repeat("s", 70))
		}
		app.st.spinnerIdx = i % len(app.th.SpinnerFrames())
		app.paint()
		app.scr.Show()
		withoutFix += len(ft.take())
	}

	t.Logf("bytes over 40 streaming frames: with the fix %d (%.0f/frame), without %d (%.0f/frame), +%.1f%%",
		withFix, float64(withFix)/40, withoutFix, float64(withoutFix)/40,
		100*(float64(withFix)/float64(withoutFix)-1))
	if withFix <= withoutFix {
		t.Fatalf("the fix should cost bytes: %d with, %d without", withFix, withoutFix)
	}
	// The price scales with the box's height — it is its own rows that get
	// marked, and the padding rows made the box 5 rows tall instead of 3. The
	// ceiling moved with it; a change that marked the SCREEN still fails here.
	if withFix > withoutFix*13/10 {
		t.Fatalf("the fix costs %d vs %d bytes — wider than the box's own rows",
			withFix, withoutFix)
	}
}
