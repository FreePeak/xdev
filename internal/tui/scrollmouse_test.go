package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// press, dragTo and release are the three phases of a gesture, named so a test
// reads as the mouse motion rather than as button bit-masking. The bool is the
// press edge the caller in app.go computes from the button state: only the first
// report of a held Button1 is a press.
func press(app *App, x, y int) {
	app.handleMouse(tcell.NewEventMouse(x, y, tcell.Button1, tcell.ModNone), true)
}

func dragTo(app *App, x, y int) {
	app.handleMouse(tcell.NewEventMouse(x, y, tcell.Button1, tcell.ModNone), false)
}

func release(app *App, x, y int) {
	app.handleMouse(tcell.NewEventMouse(x, y, tcell.ButtonNone, tcell.ModNone), false)
}

func longTranscript(t *testing.T, app *App, n int) []string {
	t.Helper()
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("L%03d", i)
	}
	app.AddSystemBlock(strings.Join(lines, "\n"))
	app.draw()
	return lines
}

// dragAtEnd parks the viewport away from the tail, presses on the first
// transcript row and pulls down to the last one — the gesture whose pointer then
// holds still. It returns the app, with mu held, and the two rows it needs back.
func dragAtEnd(t *testing.T, app *App, scr tcell.SimulationScreen) (hdr, vp, lastX, lastY int) {
	t.Helper()
	app.mu.Lock()
	_, vp = app.selViewport()
	app.sm.ScrollUp(12, app.totalLinesLocked(), vp) // park away from the tail
	app.mu.Unlock()
	app.draw()

	hdr = app.transcriptTop()
	app.mu.Lock()
	press(app, 3, hdr)
	app.mu.Unlock()
	lastX, lastY = 3+len("L000")-1, hdr+vp-1
	return hdr, vp, lastX, lastY
}

// TestHeldDragAtTheEdgeKeepsScrolling pins the bug: the pointer parked on the
// transcript's last row with the button down reports no more motion, so the old
// one-row-per-event scroll stopped the instant the hand did. The UI tick is the
// clock now, so time alone must move the viewport — and the rows it reveals must
// join the selection in order, or the copy would be missing exactly the rows the
// scroll brought in.
func TestHeldDragAtTheEdgeKeepsScrolling(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	lines := longTranscript(t, app, 60)
	_, _, lastX, lastY := dragAtEnd(t, app, scr)

	// Pull down to the last transcript row: one row from the event, then the
	// pointer holds still — the pause a real hand makes, which sends nothing.
	// arm ages the parking so the next tick is past the delay: the test cannot
	// wait 400ms for it, and the delay itself is pinned by the sweep test.
	arm := func() { app.selEdgeAt = time.Now().Add(-selEdgeDelay) }

	off := make([]int, 0, 3)
	for range 3 {
		app.mu.Lock()
		if len(off) == 0 {
			dragTo(app, lastX, lastY)
		}
		arm()
		if !app.selEdgeTick() {
			app.mu.Unlock()
			t.Fatal("held drag on the edge stopped scrolling: no event, no scroll")
		}
		off = append(off, app.sm.offset)
		app.mu.Unlock()
		app.draw() // the UI loop draws after a tick that did work
	}
	for i := 1; i < len(off); i++ {
		if step := off[i-1] - off[i]; step != selEdgeStep {
			t.Fatalf("offsets %v: tick %d moved %d rows, want %d", off, i, step, selEdgeStep)
		}
	}

	app.mu.Lock()
	lastDoc := app.selEnd.doc
	release(app, lastX, lastY)
	app.mu.Unlock()

	// The copy ends on the row under the pointer and its non-empty rows are a
	// contiguous, ascending slice of the transcript — every row the auto-scroll
	// revealed is in it, in order.
	got := strings.Split(string(scr.GetClipboardData()), "\n")
	if len(got) < 2 {
		t.Fatalf("copied %q: the auto-scrolled rows never made it in", got)
	}
	if got[len(got)-1] != lines[lastDoc] {
		t.Fatalf("copy ends at %q, want the pointer's row %q", got[len(got)-1], lines[lastDoc])
	}
	seen := 0
	for i, ln := range got {
		if ln == "" {
			continue
		}
		want := lines[lastDoc-len(got)+1+i]
		if ln != want {
			t.Fatalf("row %d = %q, want %q (copy %q)", i, ln, want, got)
		}
		seen++
	}
	if seen < 2 {
		t.Fatalf("only %d transcript rows copied: %q", seen, got)
	}
}

// TestSweepAcrossAnEdgeRowDoesNotPageTurn pins the guard the delay buys: a
// pointer that crosses the last row on its way somewhere else scrolls for the
// event it arrived on and not one row more, however long the button is held
// there afterwards without another event.
func TestSweepAcrossAnEdgeRowDoesNotPageTurn(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	longTranscript(t, app, 60)
	app.mu.Lock()
	_, vp := app.selViewport()
	app.sm.ScrollUp(12, app.totalLinesLocked(), vp)
	app.mu.Unlock()
	app.draw()

	hdr := app.transcriptTop()
	app.mu.Lock()
	press(app, 3, hdr+2)
	dragTo(app, 3, hdr+vp-1) // crossed the edge row: one event, one step
	if off := app.sm.offset; off != 11 {
		t.Fatalf("offset = %d after one crossing event, want 11 (one row)", off)
	}
	// Parked there, the timer would take over — that is the feature. What the
	// delay buys is that the takeover waits: the tick that runs before it has
	// elapsed must do nothing, so a sweep across the row cannot page-turn.
	if app.selEdgeTick() {
		t.Fatal("a tick inside the delay scrolled: a sweep would page-turn")
	}
	app.selEdgeAt = time.Now().Add(-selEdgeDelay)
	if !app.selEdgeTick() {
		t.Fatal("a pointer parked on the edge row stopped scrolling")
	}
	app.mu.Unlock()
}

// TestScrollbarThumbDragScrollsTheTranscript pins the other half of the report:
// the right-edge bar is painted (app.go) and must also answer the mouse. A drag
// on the bar moves the viewport; it never selects the text under it.
func TestScrollbarThumbDragScrollsTheTranscript(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	lines := longTranscript(t, app, 60)
	app.mu.Lock()
	total, vp := app.totalLinesLocked(), app.viewportLinesLocked()
	app.mu.Unlock()
	if total <= vp {
		t.Fatalf("transcript of %d rows does not overflow the %d-row viewport", total, vp)
	}

	// The bar is the last column (app.go paints it at w-1) spanning the visible
	// transcript rows, so the top of the track is the first screen row.
	hdr := app.transcriptTop()
	app.mu.Lock()
	if _, ok := app.selBarAt(79, hdr); !ok {
		t.Fatalf("press on the bar at (%d,%d) not hit: on=%v x=%d vp=%d",
			79, hdr, app.selBarOn, app.selBarX, app.selBarVP)
	}
	press(app, 79, hdr)
	if !app.selThumbDrag {
		app.mu.Unlock()
		t.Fatal("a press on the bar did not grab the thumb")
	}
	dragTo(app, 79, hdr) // at the top of the track = the oldest row
	off := app.sm.offset
	selected := app.selDown && !app.selThumbDrag
	app.mu.Unlock()
	if selected {
		t.Fatal("the bar drag started a text selection")
	}
	if off != total-vp {
		t.Fatalf("offset = %d, want %d (top of the track is the oldest row)", off, total-vp)
	}

	// And back to the bottom: the thumb tracks the pointer, not the old offset.
	app.mu.Lock()
	dragTo(app, 79, hdr+app.selBarVP-1)
	off = app.sm.offset
	release(app, 79, hdr+app.selBarVP-1)
	thumbDrag, down := app.selThumbDrag, app.selDown
	app.mu.Unlock()
	if off != 0 {
		t.Fatalf("offset = %d after dragging to the bottom, want 0 (the tail)", off)
	}
	if thumbDrag || down {
		t.Fatalf("release left the gesture down: thumbDrag=%v down=%v", thumbDrag, down)
	}
	_ = lines
}

// TestNoBarNoGrab pins the negative: with the whole transcript on screen there is
// no bar drawn, so the last column must stay ordinary selectable text.
func TestNoBarNoGrab(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.AddSystemBlock("only two\nshort rows")
	app.draw()

	app.mu.Lock()
	defer app.mu.Unlock()
	if app.selBarOn {
		t.Fatal("bar published for a transcript that fits")
	}
	if _, ok := app.selBarAt(79, app.transcriptTop()); ok {
		t.Fatal("the last column grabbed without a bar on screen")
	}
}

// TestScrollbarPressKeepsItsGrip pins what grabbing the thumb means: a press
// there must not move the view (no jump to the thumb's top), and the pull that
// follows moves the thumb by exactly the rows the finger moved — one row for one
// row, not one row plus half a thumb. A press on the bare track beside the thumb
// is the opposite gesture: it brings the thumb to the finger at once.
func TestScrollbarPressKeepsItsGrip(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	longTranscript(t, app, 60)
	app.mu.Lock()
	total, vp := app.totalLinesLocked(), app.viewportLinesLocked()
	app.mu.Unlock()
	if total <= vp {
		t.Fatalf("no bar for a %d-row transcript in a %d-row viewport", total, vp)
	}
	hdr := app.transcriptTop()
	app.mu.Lock()
	app.sm.ScrollUp((total-vp)/2, total, vp) // park mid-track: room on both sides
	app.mu.Unlock()
	app.draw()

	app.mu.Lock()
	pos, thumb := app.selBarPos, app.selBarThumb
	if thumb < 3 {
		app.mu.Unlock()
		t.Skipf("thumb of %d rows cannot show a grip", thumb)
	}
	grip := thumb / 2
	y0 := hdr + pos + grip
	off0 := app.sm.offset
	press(app, 79, y0)
	if app.sm.offset != off0 {
		t.Fatalf("a press on the thumb moved the view to offset %d, want it to stay at %d", app.sm.offset, off0)
	}
	dragTo(app, 79, y0-1) // one row of travel toward the oldest rows
	app.mu.Unlock()
	app.draw()

	app.mu.Lock()
	off1, pos1 := app.sm.offset, app.selBarPos
	app.mu.Unlock()
	if pos1 != pos-1 {
		t.Fatalf("one-row pull moved the thumb to track row %d, want %d (a gripless grab would jump it to %d)", pos1, pos-1, pos-grip-1)
	}
	if off1 <= off0 {
		t.Fatalf("offset %d -> %d: pulling the thumb up did not reveal older rows", off0, off1)
	}

	// The track click: press above the thumb and the thumb lands under the
	// finger, which at the top of the bar is the oldest row of the transcript.
	app.mu.Lock()
	app.sm.ScrollUp((total-vp)/2, total, vp)
	app.mu.Unlock()
	app.draw()
	app.mu.Lock()
	press(app, 79, hdr)
	off := app.sm.offset
	app.mu.Unlock()
	if want := total - vp; off != want {
		t.Fatalf("track press at the top: offset %d, want %d (the oldest row)", off, want)
	}
}

// TestWheelDuringAHeldDragDoesNotRestartIt pins the press edge against the one
// report that carries no button bits at all: the wheel. Reading a wheel event as
// "the button came up" makes the next motion of a held drag look like a fresh
// press, which re-anchors the gesture at the pointer and drops every row above it
// — the visible symptom of "dragging does not scroll".
func TestWheelDuringAHeldDragDoesNotRestartIt(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	longTranscript(t, app, 60) // the tail is in view, so a wheel-down changes nothing
	app.draw()

	app.mu.Lock()
	y := contentRow(t, app, "L050")
	app.mu.Unlock()

	// The real event path, so the edge the caller computes is the one under test.
	app.handleKey(tcell.NewEventMouse(3, y, tcell.Button1, tcell.ModNone))      // press
	app.handleKey(tcell.NewEventMouse(3, y+1, tcell.Button1, tcell.ModNone))    // drag
	app.handleKey(tcell.NewEventMouse(3, y+1, tcell.WheelDown, tcell.ModNone))  // wheel
	app.handleKey(tcell.NewEventMouse(3, y+2, tcell.Button1, tcell.ModNone))    // drag on
	app.handleKey(tcell.NewEventMouse(3, y+2, tcell.ButtonNone, tcell.ModNone)) // release

	got := strings.Split(string(scr.GetClipboardData()), "\n")
	if got[0] != "L050" {
		t.Fatalf("copy starts at %q, want %q: the wheel restarted the drag (full copy %q)", got[0], "L050", got)
	}
}

// TestScrollbarDragStillAnswersWithTheDockOpen pins the second half of the
// report: the bar moved when the context dock (#291) reserved its columns, so on
// any terminal wide enough to show the panel the grab tested a column the bar was
// not in. Dragging the thumb did nothing, and the far-right click it did answer
// belongs to the panel's border. The hit-test must follow the painted column.
func TestScrollbarDragStillAnswersWithTheDockOpen(t *testing.T) {
	const w = 140 // wide enough for the panel and the transcript's floor
	app, _ := newTestApp(t, w, 24)
	app.SetDockMode(DockShow) // the shipped default is auto; both open the panel here
	longTranscript(t, app, 60)
	app.mu.Lock()
	if !app.dockOn() {
		app.mu.Unlock()
		t.Fatal("the panel is not open; this test is about the dock's columns")
	}
	barX := app.rightEdge() - 1 // where app.go paints it
	if barX == w-1 {
		app.mu.Unlock()
		t.Fatalf("rightEdge did not give up a column: the bar is at the terminal edge")
	}
	press(app, barX, app.transcriptTop())
	if !app.selThumbDrag {
		app.mu.Unlock()
		t.Fatalf("a press on the bar at (%d,%d) did not grab the thumb", barX, app.transcriptTop())
	}
	if app.selDown {
		app.mu.Unlock()
		t.Fatal("the bar started a text selection")
	}
	total, vp := app.totalLinesLocked(), app.viewportLinesLocked()
	dragTo(app, barX, app.transcriptTop()) // pull the thumb to the top of the track
	off, thumbDrag := app.sm.offset, app.selThumbDrag
	app.mu.Unlock()
	if off != total-vp {
		t.Fatalf("dragging the thumb scrolled to offset %d, want %d (the oldest row)", off, total-vp)
	}
	if !thumbDrag {
		t.Fatal("the drag released the thumb: the bar was answering, not the panel")
	}

	// And the terminal's last column, which is the panel's border, stays the
	// panel's: it must not scroll or grab anything.
	app.mu.Lock()
	defer app.mu.Unlock()
	if _, ok := app.selBarAt(w-1, app.transcriptTop()); ok {
		t.Fatalf("the panel's border at column %d still answers the bar", w-1)
	}
}
