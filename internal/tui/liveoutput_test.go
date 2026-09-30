package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// A running call's first chunk opens a box, and chunks land in THAT call's
// box. Two `bash` calls in one batch run concurrently, so a box filled by
// "whatever call is running" is the pairing bug FinishTool is matched by id
// to avoid — with two commands it paints one command's bytes under the other.
func TestLiveOutputRoutesChunksToTheirOwnCall(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.AddToolBlock("call-a", "bash", `{"command":"a"}`)
	app.AddToolBlock("call-b", "bash", `{"command":"b"}`)

	app.AppendToolOutput("call-a", "bash", "a-1\n")
	app.AppendToolOutput("call-b", "bash", "b-1\n")
	app.AppendToolOutput("call-a", "bash", "a-2\n")

	if got := app.liveText("call-a", "bash"); got != "a-1\na-2\n" {
		t.Fatalf("call-a box = %q", got)
	}
	if got := app.liveText("call-b", "bash"); got != "b-1\n" {
		t.Fatalf("call-b box = %q", got)
	}
}

// The box belongs to its call ROW, not to the end of the transcript: a batch's
// calls start in any order, and a box appended at the tail would sit under a
// different call's name.
func TestLiveBoxSitsUnderItsOwnCallRow(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.AddToolBlock("call-a", "bash", `{"command":"a"}`)
	app.AddToolBlock("call-b", "bash", `{"command":"b"}`)
	// call-b, the LATER row, streams first.
	app.AppendToolOutput("call-b", "bash", "b\n")
	app.AppendToolOutput("call-a", "bash", "a\n")

	app.mu.Lock()
	defer app.mu.Unlock()
	for i, b := range app.blocks {
		if b.Kind == KindToolDone && b.Live {
			if b.CallID != app.blocks[i-1].CallID {
				t.Fatalf("live box for %q sits under %q", b.CallID, app.blocks[i-1].CallID)
			}
		}
	}
}

// A tool that streams nothing (read, grep, every MCP tool) must not cost the
// transcript an empty frame: the box is opened by the first chunk, not by the
// call being announced.
func TestSilentToolNeverOpensABox(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.AddToolBlock("call-a", "read", `{"path":"a.go"}`)
	app.FinishTool("call-a", "read", false, "the file", ToolOutcome{})
	app.AppendToolOutput("call-a", "read", "late\n")
	app.AddToolBlock("call-b", "bash", `{"command":"b"}`)

	app.mu.Lock()
	defer app.mu.Unlock()
	if got := len(app.blocks); got != 3 {
		t.Fatalf("blocks = %d, want 3 (a call row, its result, and one call row)", got)
	}
}

// A call that was never announced (a replayed row, a late start) gets no box:
// a result the transcript has no running call for would claim a command
// nobody saw start.
func TestAppendSkipsUnknownCalls(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.AppendToolOutput("ghost", "bash", "bytes")
	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.blocks) != 0 {
		t.Fatalf("a live box was opened for a call that never ran: %+v", app.blocks)
	}
}

// The settled result REPLACES the live box. Two boxes for one command is the
// transcript lying about what ran, and the settled text is already the same
// output windowed the same way.
func TestFinishToolReplacesTheLiveBox(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.AddToolBlock("call-a", "bash", `{"command":"a"}`)
	app.AppendToolOutput("call-a", "bash", "streaming\n")
	app.FinishTool("call-a", "bash", false, "settled output", ToolOutcome{Exit: 0, HasExit: true, Elapsed: time.Second})

	app.mu.Lock()
	defer app.mu.Unlock()
	var results []*Block
	for _, b := range app.blocks {
		if b.Kind == KindToolDone {
			results = append(results, b)
		}
	}
	if len(results) != 1 {
		t.Fatalf("tool results = %d, want 1 (the live box must be replaced)", len(results))
	}
	if results[0].Live {
		t.Fatal("the settled result is still marked live")
	}
	if results[0].Text != "settled output" || !results[0].HasExit {
		t.Fatalf("settled result = %+v", results[0])
	}
}

// Output that keeps arriving after the call settled has no running row to
// open a box under, and must be dropped rather than claiming a finished
// command (a backgrounded run's remaining bytes).
func TestAppendAfterFinishIsDropped(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.AddToolBlock("call-a", "bash", `{"command":"a"}`)
	app.AppendToolOutput("call-a", "bash", "early\n")
	app.FinishTool("call-a", "bash", false, "settled", ToolOutcome{Exit: 0, HasExit: true})
	app.AppendToolOutput("call-a", "bash", "late\n")

	app.mu.Lock()
	defer app.mu.Unlock()
	for _, b := range app.blocks {
		if strings.Contains(b.Text, "late") {
			t.Fatalf("a stale chunk reopened a settled box: %q", b.Text)
		}
	}
	if len(app.blocks) != 2 {
		t.Fatalf("blocks = %d, want 2 (a call row and its result)", len(app.blocks))
	}
}

// The settled result must land in the box's OWN slot, not at the tail: the
// box sits under its call row, so appending would hoist the result above
// every call that started after it — the same mis-pairing the box position
// was fixed to avoid.
func TestFinishToolKeepsTheResultUnderItsCallRow(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.AddToolBlock("call-a", "bash", `{"command":"a"}`)
	app.AddToolBlock("call-b", "bash", `{"command":"b"}`)
	app.AppendToolOutput("call-a", "bash", "a-live\n")
	app.FinishTool("call-a", "bash", false, "a-settled", ToolOutcome{Exit: 0, HasExit: true})

	app.mu.Lock()
	defer app.mu.Unlock()
	aLive, aSettled, bRow := -1, -1, -1
	for i, b := range app.blocks {
		switch {
		case b.Kind == KindToolDone && b.CallID == "call-a":
			if b.Live {
				aLive = i
			} else {
				aSettled = i
			}
		case b.Kind == KindTool && b.CallID == "call-b":
			bRow = i
		}
	}
	if aSettled < 0 {
		t.Fatalf("the settled result is missing: %+v", app.blocks)
	}
	if aLive >= 0 {
		t.Fatalf("the live box was left behind: %+v", app.blocks[aLive])
	}
	if bRow >= 0 && aSettled > bRow {
		t.Fatalf("the result for call-a sits at %d, above call-b's row at %d: %+v", aSettled, bRow, app.blocks)
	}
}

// A live box is bounded: a command that prints forever must not grow the
// transcript without limit. The tail is what a reader wants, so the head is
// what goes.
func TestLiveBoxIsBoundedAndKeepsTheTail(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.AddToolBlock("call-a", "bash", `{"command":"a"}`)
	// Two distinct flood markers: the cap must keep the newer one and drop
	// the older one entirely, and one marker alone cannot tell those apart
	// (the retained window is made of the same bytes either way).
	app.AppendToolOutput("call-a", "bash", strings.Repeat("HEAD\n", liveTailBytes))
	app.AppendToolOutput("call-a", "bash", strings.Repeat("MID\n", liveTailBytes))
	for i := range 64 {
		app.AppendToolOutput("call-a", "bash", fmt.Sprintf("tail-%04d\n", i))
	}
	got := app.liveText("call-a", "bash")
	if len(got) > liveTailBytes {
		t.Fatalf("live box grew to %d bytes, cap is %d", len(got), liveTailBytes)
	}
	if !strings.HasSuffix(got, "tail-0063\n") {
		t.Fatalf("the tail was not kept: ...%q", got[len(got)-40:])
	}
	if !strings.Contains(got, "MID") {
		t.Fatal("the newer flood was dropped instead of the oldest")
	}
	if strings.Contains(got, "HEAD") {
		t.Fatal("the head was not dropped")
	}
}

// The repaint stamp must move at most once per livePaint: a tool that floods
// chunks costs the frames a reader can see, not one frame per chunk. The row
// key must move with it every flush, or a progress bar (same bytes, same
// length) would freeze the box on its first frame.
func TestLiveBoxStampCoalescesButAlwaysMoves(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.AddToolBlock("call-a", "bash", `{"command":"a"}`)

	app.AppendToolOutput("call-a", "bash", "1")
	first := app.liveStamp("call-a", "bash")
	if first == 0 {
		t.Fatal("the first chunk did not move the render stamp")
	}
	// Inside the window: no new stamp, the next frame will carry it.
	app.AppendToolOutput("call-a", "bash", "2")
	if got := app.liveStamp("call-a", "bash"); got != first {
		t.Fatalf("stamp moved inside the coalescing window: %d -> %d", first, got)
	}
	// Past the window: the stamp moves again, and the ROW key with it.
	app.mu.Lock()
	b := app.liveBoxLocked("call-a", "bash")
	b.liveAt = time.Now().Add(-2 * livePaint)
	keyBefore := app.renderKey(len(app.blocks)-1, b, 80)
	app.mu.Unlock()
	app.AppendToolOutput("call-a", "bash", "3")
	app.mu.Lock()
	keyAfter := app.renderKey(len(app.blocks)-1, b, 80)
	app.mu.Unlock()
	if keyAfter.live <= keyBefore.live {
		t.Fatalf("the render key did not move after the window: %d -> %d", keyBefore.live, keyAfter.live)
	}
}

// Once the box is AT the tail cap, every further append leaves its length
// unchanged while the content changes — the box's own bound manufactures the
// same-length rewrite a progress bar does. Without the live sequence on
// blockKey the repaint stops exactly there: the box freezes on whatever the
// last chunk inside the window was, and a flooding command's reader watches
// stale output while the command runs on.
func TestLiveBoxRepaintsOnceTheTailCapFreezesItsLength(t *testing.T) {
	app, scr := drawnApp(t, 60, 30)
	app.AddToolBlock("call-a", "bash", `{"command":"flood"}`)
	// Fill past the cap so the box is windowed, and settle its stamp.
	app.AppendToolOutput("call-a", "bash", strings.Repeat("filler\n", liveTailBytes/7+16))
	app.mu.Lock()
	app.liveBoxLocked("call-a", "bash").liveAt = time.Now().Add(-2 * livePaint)
	app.mu.Unlock()
	app.draw()
	first := app.liveText("call-a", "bash")
	if len(first) != liveTailBytes {
		t.Fatalf("box length = %d, want the cap %d", len(first), liveTailBytes)
	}

	// At the cap this append cannot change the length — only the content.
	app.AppendToolOutput("call-a", "bash", "the-newest-line\n")
	app.mu.Lock()
	app.liveBoxLocked("call-a", "bash").liveAt = time.Now().Add(-2 * livePaint)
	app.mu.Unlock()
	app.draw()
	if got := app.liveText("call-a", "bash"); len(got) != len(first) {
		t.Fatalf("the cap did not hold: %d -> %d bytes", len(first), len(got))
	}
	if !strings.Contains(screenText(scr), "the-newest-line") {
		t.Fatalf("the box stopped repainting once its length froze:\n%s", screenText(scr))
	}
}

// A live box paints its newest rows and no footer. A box that grew past the
// window still shows the last liveRows of them, and never offers Ctrl+O:
// expanding output that is still arriving has no settled length to expand to.
func TestLiveBoxRendersTailWithoutFooter(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	app.AddToolBlock("call-a", "bash", `{"command":"a"}`)
	app.AppendToolOutput("call-a", "bash", strings.Repeat("line\n", liveRows+40))
	app.mu.Lock()
	b := app.liveBoxLocked("call-a", "bash")
	rows := app.toolBoxLines(len(app.blocks)-1, b, 96)
	app.mu.Unlock()

	var text []string
	for _, ln := range rows {
		for _, r := range ln.runs {
			text = append(text, r.text)
		}
	}
	joined := strings.Join(text, "\n")
	if strings.Contains(joined, "Wall:") || strings.Contains(joined, "Exit:") {
		t.Fatalf("a live box printed a footer it has not earned:\n%s", joined)
	}
	if strings.Contains(joined, "Ctrl+O") {
		t.Fatalf("a live box offered to expand output that is still arriving:\n%s", joined)
	}
	if got := strings.Count(joined, "line"); got != liveRows {
		t.Fatalf("live box painted %d body rows, want the newest %d", got, liveRows)
	}
}

// liveText reads a call's live box, failing the test when it is not there.
func (a *App) liveText(callID, name string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	b := a.liveBoxLocked(callID, name)
	if b == nil {
		return ""
	}
	return b.Text
}

// liveStamp reads a call's live box render stamp.
func (a *App) liveStamp(callID, name string) uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if b := a.liveBoxLocked(callID, name); b != nil {
		return b.liveSeq
	}
	return 0
}
