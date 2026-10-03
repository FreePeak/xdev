//go:build !windows

package tui

import (
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/FreePeak/xdev/internal/theme"
	"github.com/gdamore/tcell/v2"
)

// A frame is a DELTA, and the prompt box is the one place xdev overrides it.
//
// tcell writes only a cell whose content changed, so a frame during a turn
// carries the transcript tail, the spinner and the clock — and carries the
// prompt box's row only where one of ITS cells moved. paint() fills the whole
// row with spaces, but a space that was already a space is not a change, so
// those columns arrive in no frame at all: measured below, 56 of them right of
// "Type a message…" reach no frame during a streaming turn.
//
// A terminal keeps what it was sent, so the delta is correct there and always
// has been. A pane that re-renders from its own grid — treating a frame as
// "here is the region that changed" — keeps ITS value in the columns xdev
// never addressed, and on a pane whose clear pattern is X the box reads
// "TypeXaXmessage…XXXXXX": the reported symptom. The pane's reset is silent —
// a reattach, a cleared terminal, a resize that re-clears announces nothing —
// so xdev cannot wait for an event to repair it. draw() therefore marks the
// box's own rows dirty every frame (markComposerDirty), which sends them whole
// for ~10% more bytes; composer_pane_test.go is the end-to-end proof.
//
// So the delta survives everywhere EXCEPT the box: the columns past the
// context dock's edge still arrive only when they change, which is what this
// pins. A change that starts writing the whole screen, or stops sending the
// box's rows whole, is visible in this output instead of in a user's pane.
type frameTty struct {
	mu    sync.Mutex
	frame []byte
	w, h  int
}

func (f *frameTty) Start() error        { return nil }
func (f *frameTty) Stop() error         { return nil }
func (f *frameTty) Drain() error        { return nil }
func (f *frameTty) NotifyResize(func()) {}
func (f *frameTty) WindowSize() (tcell.WindowSize, error) {
	return tcell.WindowSize{Width: f.w, Height: f.h}, nil
}
func (f *frameTty) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frame = append(f.frame, p...)
	return len(p), nil
}
func (f *frameTty) Read([]byte) (int, error) { return 0, nil }
func (f *frameTty) Close() error             { return nil }
func (f *frameTty) take() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	b := f.frame
	f.frame = nil
	return b
}

// frameCells walks one frame the way a terminal does and reports, per 1-based
// screen row, the columns it addressed with a glyph. A cursor move is not
// content: a row the frame only visits still owns nothing.
func frameCells(frame []byte, w, h int) map[int]map[int]bool {
	rows := map[int]map[int]bool{}
	x, y := 1, 1
	for i := 0; i < len(frame); {
		if frame[i] == 0x1b {
			j := i + 1
			if j < len(frame) && frame[j] == '[' {
				k := j + 1
				for k < len(frame) && !(frame[k] >= '@' && frame[k] <= '~') {
					k++
				}
				if k >= len(frame) {
					break
				}
				if frame[k] == 'H' || frame[k] == 'f' {
					p := strings.Split(string(frame[j+1:k]), ";")
					y, x = 1, 1
					if len(p) > 0 && p[0] != "" {
						y = vtAtoi(p[0])
					}
					if len(p) > 1 && p[1] != "" {
						x = vtAtoi(p[1])
					}
				}
				i = k + 1
				continue
			}
			if j < len(frame) && strings.IndexByte("())(*+", frame[j]) >= 0 {
				i = j + 2 // ESC ( B and friends
				continue
			}
			i = j + 1
			continue
		}
		r, sz := vtRune(string(frame[i:]))
		if sz == 0 {
			break
		}
		switch r {
		case '\r':
			x = 1
		case '\n':
			y++
			x = 1
		case '\b':
			x--
		case '\t':
			x += 8 - (x-1)%8
		default:
			if r >= 0x20 && x >= 1 && x <= w && y >= 1 && y <= h {
				if rows[y] == nil {
					rows[y] = map[int]bool{}
				}
				rows[y][x] = true
			}
			x++
		}
		i += sz
	}
	return rows
}

func vtAtoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func vtRune(s string) (rune, int) {
	for _, r := range s {
		return r, len(string(r))
	}
	return 0, 0
}

// unwritten reports the column runs of one row that the frame left for the
// terminal to keep.
func unwritten(row map[int]bool, w int) [][2]int {
	var spans [][2]int
	start := 0
	for x := 1; x <= w+1; x++ {
		gap := x <= w && !row[x]
		switch {
		case start == 0 && gap:
			start = x
		case start != 0 && !gap:
			spans = append(spans, [2]int{start, x - 1})
			start = 0
		}
	}
	return spans
}

func spanLen(spans [][2]int) int {
	n := 0
	for _, s := range spans {
		n += s[1] - s[0] + 1
	}
	return n
}

// The measurement: over a streaming turn, which columns of the prompt box's
// row the frame carries, and which the terminal is left to keep. With the
// context dock open the box stops at rightEdge, so the panel's own columns
// past it are the delta this still relies on.
func TestComposerRowIsCarriedAsADelta(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	const W, H = 120, 40
	ft := &frameTty{w: W, h: H}
	scr, err := tcell.NewTerminfoScreenFromTty(ft)
	if err != nil {
		t.Skip("no terminfo:", err)
	}
	if err := scr.Init(); err != nil {
		t.Skip("init:", err)
	}
	defer scr.Fini()
	app := New(scr, theme.Load("groknight"), "test/free", "sess1234")
	app.width, app.height = W, H
	app.SetDockMode(DockShow) // the box stops short of the panel, so the delta has somewhere to live
	app.AddUserBlock("hi")
	app.SetRunning(true)
	app.draw()
	ft.take()

	// 1-based, as the wire addresses it.
	row := app.height - 1 - app.composerRows() + 1
	carried, idle := 0, 0
	const turns = 40
	for i := 0; i < turns; i++ {
		for j := 0; j < 5; j++ {
			app.AddSystemBlock("chunk " + strings.Repeat("s", 70))
		}
		app.st.spinnerIdx = i % len(app.th.SpinnerFrames())
		app.draw()
		cells := frameCells(ft.take(), W, H)
		if cells[row] != nil {
			carried++
		} else {
			idle++
		}
	}
	t.Logf("streaming frames carrying the composer row: %d, omitting it: %d", carried, idle)

	// A frame measured column by column.
	app.AddSystemBlock("gap probe " + strings.Repeat("g", 50))
	app.draw()
	cells := frameCells(ft.take(), W, H)
	// The box's own columns are sent whole every frame (markComposerDirty);
	// what the delta still leaves alone is everything RIGHT of the box, which
	// belongs to the context dock or off-screen when the dock is hidden.
	spans := unwritten(cells[row], W)
	cols := map[int]bool{}
	for x := range cells[row] {
		cols[x] = true
	}
	gaps := spanLen(spans)
	t.Logf("composer row %d: %d of %d columns carry a glyph; %d columns arrive in no frame (spans %v)",
		row, len(cols), W, gaps, spans)

	// Both halves are the contract, asserted rather than described: the box's
	// own columns travel every frame, and the columns past its right edge
	// still arrive only when they change. A pane that re-renders from its own
	// grid renders the box correctly and leaves the rest alone.
	if got, want := len(cols), app.rightEdge(); got != want {
		t.Fatalf("the box carried %d of its own %d columns — markComposerDirty is not covering the box", got, want)
	}
	if len(cols) >= W {
		t.Fatalf("the whole screen was sent (%d columns) — the delta contract changed", len(cols))
	}
	for _, s := range spans {
		if s[0] <= app.rightEdge() {
			t.Fatalf("columns %d..%d are inside the box but arrived in no frame: spans %v", s[0], s[1], spans)
		}
	}
	if gaps == 0 {
		t.Fatalf("the frame carried every column — the delta contract changed")
	}
}

// TestComposerSurvivesASyncIs what a pane's own recovery looks like from here:
// after the terminal lost the row, one Sync (a full repaint) restores every
// column. It is the repair the pane needs, and it is also what C-l (redraw)
// reaches — which is why the symptom clears when the user redraws.
func TestComposerSurvivesASync(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	const W, H = 60, 14
	ft := &frameTty{w: W, h: H}
	scr, err := tcell.NewTerminfoScreenFromTty(ft)
	if err != nil {
		t.Skip("no terminfo:", err)
	}
	if err := scr.Init(); err != nil {
		t.Skip("init:", err)
	}
	defer scr.Fini()
	app := New(scr, theme.Load("groknight"), "test/free", "sess1234")
	app.width, app.height = W, H
	app.AddUserBlock("hi")
	app.draw()
	row := app.height - 1 - app.composerRows() + 1
	ft.take()

	scr.Sync()
	got := frameCells(ft.take(), W, H)
	if got[row] == nil {
		t.Fatal("Sync wrote no composer row at all")
	}
	spans := unwritten(got[row], W)
	// A Sync is a full repaint: the row must arrive whole, or the repair is
	// not a repair.
	if spanLen(spans) > 0 {
		t.Logf("Sync left %d columns unwritten (spans %v)", spanLen(spans), spans)
		sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
		for _, s := range spans {
			t.Logf("  columns %d..%d", s[0], s[1])
		}
	}
	if got, want := len(got[row]), W; got < want {
		t.Errorf("Sync carried %d of %d columns on the composer row", got, want)
	}
}

// TestComposerRowGlyphs pins WHAT the row carries, so the measurement above
// reads as the prompt the user sees rather than as numbers.
func TestComposerRowGlyphs(t *testing.T) {
	app, scr := drawnApp(t, 60, 14)
	app.draw()
	prim, w, _ := scr.GetContents()
	row := app.height - 1 - app.composerRows()
	for y := row - 1; y <= row; y++ {
		line := ""
		for x := 0; x < w; x++ {
			c := ' '
			if len(prim[y*w+x].Runes) > 0 {
				c = prim[y*w+x].Runes[0]
			}
			line += string(c)
		}
		t.Logf("y=%d %q", y, line)
	}
}
