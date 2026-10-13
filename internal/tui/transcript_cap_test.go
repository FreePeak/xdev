package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// capFill appends n turns of one long system block each — the cheapest way to
// drive the row count past transcriptCap, since a system block is one rendered
// row per line and never trimmed by the tool window.
func capFill(app *App, turns, linesEach int) {
	for i := range turns {
		app.AddUserBlock("prompt " + strings.Repeat("x", 20))
		app.AddSystemBlock(strings.Join(repeatLines(linesEach, i), "\n"))
	}
}

func repeatLines(n, tag int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "L" + string(rune('a'+tag%26)) + string(rune('0'+i%10))
	}
	return out
}

// The row cap is the memory bound: past transcriptCap rows the OLDEST blocks
// are dropped, and the transcript keeps exactly the newest window. The session
// JSONL is the record; this is what stops the window from being the record too.
func TestTranscriptCapDropsTheOldestBlocks(t *testing.T) {
	app := idxApp(120, 40)
	capFill(app, 400, 60)
	w := app.contentWidth()
	total := app.sync(w)

	if total > transcriptCap {
		t.Fatalf("transcript kept %d rows, cap is %d", total, transcriptCap)
	}
	// The tail is what survives: the last block must still be there, and the
	// first must be gone.
	app.mu.Lock()
	last := app.blocks[len(app.blocks)-1]
	firstKind := app.blocks[0].Kind
	dropped := app.droppedRows
	app.mu.Unlock()
	if !strings.Contains(last.Text, "L") {
		t.Fatalf("the newest block was dropped: %q", last.Text)
	}
	if dropped == 0 {
		t.Fatal("nothing was dropped, so the fixture never crossed the cap")
	}
	if firstKind != KindSystem && firstKind != KindUser {
		t.Fatalf("the transcript's head is %v, want a surviving block", firstKind)
	}
	// The render tables moved with the blocks: every retained block has a
	// render, and the offsets are the retained rows, not the session's.
	app.mu.Lock()
	renders, starts := len(app.rowIdx.rend), len(app.rowIdx.start)
	base := app.rowIdx.start[0]
	app.mu.Unlock()
	if renders != len(app.blocks) {
		t.Fatalf("row index carries %d renders for %d blocks", renders, len(app.blocks))
	}
	if starts != len(app.blocks)+1 {
		t.Fatalf("start has %d entries for %d blocks", starts, len(app.blocks))
	}
	if base != 0 {
		t.Fatalf("the retained transcript starts at row %d, want 0 (the dropped prefix is gone)", base)
	}
}

// The cap must not fire on a session that fits: a short transcript keeps every
// block and pays nothing.
func TestTranscriptCapLeavesShortSessionsAlone(t *testing.T) {
	app := idxApp(120, 40)
	idxFill(app, 3, 20)
	app.sync(app.contentWidth())
	app.mu.Lock()
	blocks, dropped := len(app.blocks), app.droppedRows
	app.mu.Unlock()
	if dropped != 0 {
		t.Fatalf("a short session dropped %d rows", dropped)
	}
	if blocks != 15 { // 3 turns × 5 blocks (user, assistant, system, call, result)
		t.Fatalf("blocks = %d, want 15", blocks)
	}
}

// A dropped prompt is still a prompt of this session: the ordinal the
// user-message seam counts by must not restart at the top of what is left on
// screen, or a menu opened after a trim would rewind to the wrong turn.
func TestUserOrdinalStaysAbsoluteAfterATrim(t *testing.T) {
	app := idxApp(120, 40)
	capFill(app, 400, 60)
	app.sync(app.contentWidth())

	app.mu.Lock()
	defer app.mu.Unlock()
	if app.droppedUserRows == 0 {
		t.Fatal("the fixture dropped no user rows, so this proves nothing")
	}
	// The first prompt still on screen: its ordinal is the number of prompts
	// the cap retired plus the ones above it in the retained window (none).
	for bi, b := range app.blocks {
		if b.Kind != KindUser {
			continue
		}
		ord := app.droppedUserRows
		for i := 0; i < bi; i++ {
			if app.blocks[i].Kind == KindUser {
				ord++
			}
		}
		if ord != app.droppedUserRows {
			t.Fatalf("first visible prompt ordinal = %d, want %d", ord, app.droppedUserRows)
		}
		if got, want := app.countUserRows(), app.droppedUserRows+countKind(app, KindUser); got != want {
			t.Fatalf("countUserRows = %d, want %d (dropped + on screen)", got, want)
		}
		break
	}
}

func countKind(app *App, k BlockKind) int {
	n := 0
	for _, b := range app.blocks {
		if b.Kind == k {
			n++
		}
	}
	return n
}

// A session that is reset (/clear, /resume, /new) starts its transcript over:
// the dropped head belongs to the previous transcript and must not be carried
// into the new one's ordinals.
func TestResetClearsTheDroppedHead(t *testing.T) {
	app := idxApp(120, 40)
	capFill(app, 400, 60)
	app.sync(app.contentWidth())
	app.mu.Lock()
	if app.droppedRows == 0 {
		app.mu.Unlock()
		t.Fatal("the fixture dropped nothing")
	}
	app.mu.Unlock()

	app.Reset()
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.droppedRows != 0 || app.droppedUserRows != 0 {
		t.Fatalf("reset kept droppedRows=%d droppedUserRows=%d", app.droppedRows, app.droppedUserRows)
	}
}

// --- compact mode --------------------------------------------------------

func compactFill(app *App) {
	app.AddUserBlock("keep me")
	app.AddAssistantBlock("keep this answer too")
	app.AddSystemBlock("error: something broke")
	app.BeginThinking()
	app.AppendThinking("reasoning line one\nreasoning line two")
	app.EndThinking()
	app.AddToolBlock("", "bash", `{"command":"seq 1 400"}`)
	app.FinishTool("", "bash", false, strings.Repeat("output line\n", 300), ToolOutcome{Dur: "70ms"})
}

// rowsOfBlock renders block i and counts its rows.
func rowsOfBlock(app *App, i int) []string {
	app.mu.Lock()
	defer app.mu.Unlock()
	lines := app.blockLines(i, app.blocks[i], app.contentWidth())
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		out = append(out, lineText(ln))
	}
	return out
}

func kindIndex(app *App, k BlockKind) int {
	for i, b := range app.blocks {
		if b.Kind == k {
			return i
		}
	}
	return -1
}

// thinkRowsOf counts what a reasoning box's body wraps to, which is what the
// window, bottom rule), so its ROW COUNT is not what compact mode changes —
// the body inside it is.
func thinkRowsOf(app *App, i int) int {
	app.mu.Lock()
	defer app.mu.Unlock()
	return len(app.thinkRows(app.blocks[i], app.contentWidth()))
}

// The compact transcript keeps the conversation spine and collapses everything
// that is output: reasoning, a tool result and a diff go to one row each, while
// user prompts, assistant prose and system notices render exactly as they did.
func TestCompactCollapsesOutputAndKeepsTheSpine(t *testing.T) {
	app := idxApp(120, 40)
	compactFill(app)

	w := app.contentWidth()

	fullThinking := len(rowsOfBlock(app, kindIndex(app, KindThinking)))
	fullResult := len(rowsOfBlock(app, kindIndex(app, KindToolDone)))
	spine := map[BlockKind]int{}
	for _, k := range []BlockKind{KindUser, KindAssistant, KindSystem} {
		spine[k] = len(rowsOfBlock(app, kindIndex(app, k)))
	}

	app.SetCompact(true)
	// The reasoning box is EXEMPT: the mode never touches it. Collapsed it is
	// one row — the newest thought — in either mode, and compact mode used to
	// replace that row with a "… N rows hidden" notice at exactly the same
	// height (measured: frame + one body row + frame, both ways), so the mode
	// bought nothing and cost the reader the only line they came for. A long
	// session therefore showed no thought anywhere.
	if got, want := len(rowsOfBlock(app, kindIndex(app, KindThinking))), fullThinking; got != want {
		t.Fatalf("compact mode changed the reasoning box: %d rows, full was %d", got, want)
	}
	if thinkRowsOf(app, kindIndex(app, KindThinking)) == 0 {
		t.Fatal("the fixture's reasoning box has no body to hide")
	}
	// One line is still a button: the click's half of Ctrl+O does not depend
	// on the notice, it reads the block's own render (collapsedAt).
	box := rowsOfBlock(app, kindIndex(app, KindThinking))
	if !strings.Contains(strings.Join(box, "\n"), "reasoning line two") {
		t.Fatalf("the one thinking line is not the newest thought:\n%s", strings.Join(box, "\n"))
	}
	if got := len(rowsOfBlock(app, kindIndex(app, KindToolDone))); got >= fullResult {
		t.Fatalf("compact result box rendered %d rows, full was %d", got, fullResult)
	}
	// The spine is untouched: a user prompt, assistant prose and a system
	// notice render exactly as they did with the mode off.
	for k, want := range spine {
		if got := len(rowsOfBlock(app, kindIndex(app, k))); got != want {
			t.Fatalf("compact mode changed a %v block: %d rows, want %d", k, got, want)
		}
	}
	call := rowsOfBlock(app, kindIndex(app, KindTool))
	if len(call) != 1 {
		t.Fatalf("compact call row = %d rows, want 1:\n%s", len(call), strings.Join(call, "\n"))
	}
	if w <= 0 {
		t.Fatal("no content width")
	}
}

// Ctrl+O still opens one box in compact mode: the transcript-wide mode and the
// per-block state are two different things, and the second must survive the
// first.
func TestCompactKeepsCtrlOPerBlock(t *testing.T) {
	app := idxApp(120, 40)
	compactFill(app)
	app.SetCompact(true)
	i := kindIndex(app, KindToolDone)
	collapsed := len(rowsOfBlock(app, i))
	if !app.ToggleBoxExpand() {
		t.Fatal("Ctrl+O found no box")
	}
	if got := len(rowsOfBlock(app, i)); got <= collapsed {
		t.Fatalf("Ctrl+O did not open the result in compact mode: %d rows, was %d", got, collapsed)
	}
}

// The mode latches itself on past the row threshold and lets go when the
// transcript comes back under it — and never overrides a human's choice.
func TestCompactAutoLatchAndManualOverride(t *testing.T) {
	app := idxApp(120, 40)
	if app.Compact() {
		t.Fatal("a fresh session starts compact")
	}
	// Drive the row count past the threshold with one long block.
	app.AddSystemBlock(strings.Join(repeatLines(compactAfterRows+200, 1), "\n"))
	app.sync(app.contentWidth())
	if !app.Compact() {
		t.Fatalf("the transcript passed %d rows and did not latch", compactAfterRows)
	}
	if got := app.CompactState(); got != "auto (on)" {
		t.Fatalf("latched state = %q, want %q", got, "auto (on)")
	}

	// A human turning it off is final: the next frame must not re-latch it.
	app.SetCompact(false)
	app.sync(app.contentWidth())
	if app.Compact() {
		t.Fatal("the auto latch overrode an explicit off")
	}
	app.SetCompact(true)
	if got := app.CompactState(); got != "on" {
		t.Fatalf("explicit state = %q, want %q", got, "on")
	}
}

// The chord toggles the mode and the stamp follows: a frame after the toggle
// must re-render the collapsed boxes, not paint the previous layout.
func TestCompactChordRestampsTheBoxes(t *testing.T) {
	app := idxApp(120, 40)
	compactFill(app)
	w := app.contentWidth()
	app.sync(w)
	i := kindIndex(app, KindToolDone)
	before := app.rowIdx.rend[i].key

	app.handleKey(tcell.NewEventKey(tcell.KeyRune, 'g', tcell.ModCtrl))
	if !app.Compact() {
		t.Fatal("Ctrl+G did not turn compact mode on")
	}
	app.sync(w)
	if app.rowIdx.rend[i].key == before {
		t.Fatal("the result's stamp survived the toggle: the frame would paint the old layout")
	}
}

// A click on a collapsed row expands it: the pointer's half of Ctrl+O. A click
// on a row that is NOT a collapsed box (a user prompt, an open result) is left
// to the affordances that already own it.
func TestClickCollapsedRowExpandsIt(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	defer scr.Fini()
	compactFill(app)
	app.SetCompact(true)
	app.draw()

	app.mu.Lock()
	bi := kindIndex(app, KindToolDone)
	top, _ := app.selViewport()
	y := app.transcriptTop() + int(app.rowIdx.start[bi]) - top
	if y < app.transcriptTop() || y >= app.transcriptTop()+app.viewportLinesLocked() {
		app.mu.Unlock()
		t.Skipf("the result box is off screen (row %d)", y)
	}
	app.mu.Unlock()

	clickAt(app, 8, y)
	app.mu.Lock()
	expanded := app.blocks[kindIndex(app, KindToolDone)].Expanded
	app.mu.Unlock()
	if !expanded {
		t.Fatal("a click on the collapsed result did not expand it")
	}
}

// The click freeze, driven through the real event path: handleKey takes a.mu
// around handleMouse (app.go), and expandBox — the box's own half of that
// click — took it AGAIN, so the UI loop deadlocked against itself. Seven
// recorded stalls (dumps/tui-stall-*.txt, from 2026-10-12T16:44 on) sit inside
// expandBox -> sync.Mutex.Lock, and the user saw it as "the whole TUI and its
// terminal freeze when I click a box". A bounded goroutine is what makes the
// deadlock a failure rather than a hung test binary.
func TestClickOnACollapsedBoxDoesNotDeadlock(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	defer scr.Fini()
	compactFill(app)
	app.SetCompact(true)
	app.draw()

	app.mu.Lock()
	bi := kindIndex(app, KindToolDone)
	top, _ := app.selViewport()
	y := app.transcriptTop() + int(app.rowIdx.start[bi]) - top
	app.mu.Unlock()
	if y < app.transcriptTop() || y >= app.transcriptTop()+app.viewportLinesLocked() {
		t.Skipf("the result box is off screen (row %d)", y)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		// The real path: handleKey, which is where the wrapping lock lives.
		app.handleKey(tcell.NewEventMouse(8, y, tcell.Button1, tcell.ModNone))
		app.handleKey(tcell.NewEventMouse(8, y, tcell.ButtonNone, tcell.ModNone))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the click deadlocked: expandBox took a.mu its caller already holds")
	}
	app.mu.Lock()
	expanded := app.blocks[kindIndex(app, KindToolDone)].Expanded
	app.mu.Unlock()
	if !expanded {
		t.Fatal("the click returned but never expanded the box")
	}
}

// The auto latch must not oscillate. Its input is the row count with the mode
// OFF (App.compactFullRows): the mode shrinks the very number the threshold
// compares, so a latch that read the total it had just laid out flipped on
// (2 220 rows), off (120 rows), on — every frame, each flip a full re-render
// and a toast. This is the shape that produced it: long in-flight tool CALL
// rows, which wrap into a block of their own when the transcript is not
// compact and clip to one row when it is, so the two layouts straddle
// compactAfterRows.
func TestCompactAutoLatchDoesNotFlap(t *testing.T) {
	measure := func(compact bool) int32 {
		app := idxApp(120, 40)
		wrapHeavyCalls(app, 60)
		if compact {
			app.SetCompact(true)
		}
		return app.sync(app.contentWidth())
	}
	fullTotal, compactTotal := measure(false), measure(true)
	if fullTotal <= compactAfterRows || compactTotal > compactAfterRows {
		t.Skipf("fixture does not straddle %d rows (full %d, compact %d)", compactAfterRows, fullTotal, compactTotal)
	}

	app := idxApp(120, 40)
	wrapHeavyCalls(app, 60)
	w := app.contentWidth()
	states, totals := make([]bool, 0, 12), make([]int32, 0, 12)
	for range 12 {
		totals = append(totals, app.sync(w))
		states = append(states, app.Compact())
	}
	for i := 1; i < len(states); i++ {
		if states[i] != states[i-1] {
			t.Fatalf("compact mode flipped on frame %d: states=%v totals=%v", i, states, totals)
		}
	}
	if !states[0] {
		t.Fatalf("the fixture never latched the mode on: states=%v totals=%v", states, totals)
	}
}

// wrapHeavyCalls appends n in-flight tool calls whose naming argument wraps
// into many rows in the full layout and clips to one row in compact mode.
func wrapHeavyCalls(app *App, n int) {
	long := strings.Repeat("segment", 300)
	for i := range n {
		app.AddToolBlock(fmt.Sprintf("c%d", i), "bash", fmt.Sprintf(`{"command":"echo %s"}`, long))
	}
}

// The one thinking line survives compact mode, and Ctrl+O still opens the box
// past it: the transcript-wide mode never reaches a reasoning block, and the
// per-block state is the only thing that changes its height.
func TestCompactKeepsTheThinkingLine(t *testing.T) {
	app := idxApp(120, 40)
	compactFill(app)
	app.SetCompact(true)
	i := kindIndex(app, KindThinking)
	collapsed := rowsOfBlock(app, i)
	if !strings.Contains(strings.Join(collapsed, "\n"), "reasoning line two") {
		t.Fatalf("compact mode hid the one thinking line:\n%s", strings.Join(collapsed, "\n"))
	}
	app.mu.Lock()
	app.blocks[i].Expanded = true
	app.mu.Unlock()
	got := rowsOfBlock(app, i)
	if len(got) <= len(collapsed) {
		t.Fatalf("Expanded reasoning stayed at %d rows (was %d):\n%s", len(got), len(collapsed), strings.Join(got, "\n"))
	}
}

// The latch also holds once the ROW CAP is cutting, and this is the shape that
// made the carry-down wrong. The cap trims the layout that is ON SCREEN — the
// compact one while the mode is on — so the rows it removes are compact rows,
// and subtracting them from the uncompacted height the latch reads drives that
// height to zero: measured on this fixture, compactFullRows reached 0 after 400
// turns and the auto latch released the mode it had set. The fixture is driven
// past the threshold first (so the latch is the threshold's own, not a human's),
// then past the cap, and the mode has to still be on at the end.
func TestCompactLatchHoldsWhileTheCapTrims(t *testing.T) {
	app := idxApp(120, 40)
	w := app.contentWidth()
	body := strings.Repeat("output line\n", 400)
	turn := func() {
		for i := range 5 {
			id := fmt.Sprintf("c%d-%d", len(app.blocks), i)
			app.AddToolBlock(id, "bash", `{"command":"seq 1 400"}`)
			app.FinishTool(id, "bash", false, body, ToolOutcome{Dur: "70ms"})
		}
		app.sync(w)
	}
	// Past the threshold first: the fixture has to be in the auto-latched state
	// before the cap starts cutting, or there is nothing for the carry-down to
	// release.
	for range 120 {
		turn()
	}
	if app.CompactState() != "auto (on)" {
		t.Skipf("the fixture did not reach the threshold's own latch (state %q, fullRows %d)",
			app.CompactState(), app.compactFullRows)
	}
	for i := range 280 {
		turn()
		if !app.Compact() {
			t.Fatalf("turn %d: the cap released the auto-latched compact mode (fullRows=%d, dropped=%d)",
				120+i, app.compactFullRows, app.droppedRows)
		}
	}
	if app.droppedRows == 0 {
		t.Fatalf("the fixture never crossed the cap: %d blocks, %d rows", len(app.blocks), app.rowIdx.total())
	}
}
