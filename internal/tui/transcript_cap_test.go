package tui

import (
	"strings"
	"testing"
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
	dropped := app.droppedUserRows
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
	blocks, dropped := len(app.blocks), app.droppedUserRows
	app.mu.Unlock()
	if dropped != 0 {
		t.Fatalf("a short session dropped %d prompt rows", dropped)
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
	if app.droppedUserRows == 0 {
		app.mu.Unlock()
		t.Fatal("the fixture dropped nothing")
	}
	app.mu.Unlock()

	app.Reset()
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.droppedUserRows != 0 {
		t.Fatalf("reset kept droppedUserRows=%d", app.droppedUserRows)
	}
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

// A long session shows its core: thinking, the bash it ran, the step rows and
// the turns stay on screen and keep every row they have. What a long session
// loses is the OLDEST blocks, dropped whole by the row cap — never a middle
// elided out of a box the reader is looking at.
func TestLongSessionShowsThinkingBashStepsAndTurns(t *testing.T) {
	app := idxApp(120, 40)
	body := strings.Repeat("output line\n", 300)
	for range 30 {
		app.AddUserBlock("run the suite and tell me what broke")
		app.BeginThinking()
		app.AppendThinking("the failure is in the renderer\nchecking the row window")
		app.EndThinking()
		app.AddToolBlock("", "bash", `{"command":"go test ./internal/tui/"}`)
		app.FinishTool("", "bash", false, body, ToolOutcome{Dur: "70ms"})
		app.AddAssistantBlock("Two tests fail on the window notice.")
	}
	w := app.contentWidth()
	app.sync(w)

	// Nothing is elided: every block renders what it has, whatever its age.
	for i, b := range app.blocks {
		rows := len(app.blockLines(i, b, w))
		switch b.Kind {
		case KindThinking:
			// One row is the newest thought (the box is a window the wheel
			// scrolls), so the row that matters is the thought, not the frame.
			if rows < 3 {
				t.Fatalf("thinking block %d rendered %d rows, want its frame plus body", i, rows)
			}
		case KindToolDone:
			if rows < toolRecentHead {
				t.Fatalf("result block %d rendered %d rows: a box was collapsed by age", i, rows)
			}
		case KindTool:
			if rows < 1 {
				t.Fatalf("call row %d rendered nothing", i)
			}
		}
	}
	// The thinking, the command and the result are on screen as themselves, not
	// as a notice about hiding them: the newest thought of a box, the call row
	// that names what ran, and the output rows of a result.
	joined := ""
	for i, b := range app.blocks {
		switch b.Kind {
		case KindThinking, KindTool, KindToolDone:
			joined += strings.Join(rowsOfBlock(app, i), "\n")
		}
	}
	for _, want := range []string{"checking the row window", "go test ./internal/tui/", "output line"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the long session does not show %q", want)
		}
	}
	// The newest turn is whole: the last block renders, and so does the one
	// before it — the tail window a trim never touches.
	last := len(app.blocks) - 1
	if got := len(app.blockLines(last, app.blocks[last], w)); got == 0 {
		t.Fatal("the newest block rendered nothing")
	}
}
