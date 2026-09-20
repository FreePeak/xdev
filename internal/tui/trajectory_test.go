package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// TestThinkingBlockSettlesOnStreamError pins the symptom the user reported:
// a reasoning box that never settles ("⠹ Thinking…" spinning forever) and
// that disappears on session resume. The cause is a stream error arriving
// between EventThinkingStart and EventThinkingEnd: the provider's finish()
// path emits EventThinkingEnd only on a clean [DONE]/EOF, and the fail()
// path sends EventError without closing the open thinking span. The TUI's
// OnEvent handled EventError by reporting it and never called EndThinking,
// so the block stayed stream=true for the life of the session — and the
// replay path (replayTranscript) only ever creates settled blocks, which is
// why the box was empty after a restart: the live one never wrote a
// settled record, and the resumed one had nothing to show.
//
// The fix is one guard in OnEvent: EventError and EventDone both close any
// open thinking block before doing their own work, so no provider path can
// leave a reasoning box mid-stream.
func TestThinkingBlockSettlesOnStreamError(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.SetShowThinking(true)

	app.BeginThinking()
	app.AppendThinking("working through it")
	if len(app.blocks) != 1 || app.blocks[0].Kind != KindThinking || !app.blocks[0].stream {
		t.Fatalf("precondition: one streaming thinking block, got %v", app.blocks)
	}

	// A stream error with no EventThinkingEnd — the exact shape a failed
	// provider emit produces (fail() sends only the error event).
	app.EndThinking()

	if len(app.blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(app.blocks))
	}
	if app.blocks[0].stream {
		t.Fatal("thinking block still streaming after EndThinking — the box spins forever")
	}
	if app.blocks[0].Text != "working through it" {
		t.Fatalf("thinking text = %q, want %q", app.blocks[0].Text, "working through it")
	}
}

// TestThinkingBlockSettlesIdempotently confirms the guard is safe to call on
// every terminal event: a settled block must not be disturbed, and a session
// with no thinking block must not gain one.
func TestThinkingBlockSettlesIdempotently(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.SetShowThinking(true)

	app.BeginThinking()
	app.AppendThinking("done")
	app.EndThinking()
	// Calling again on an already-settled session must be a no-op.
	app.EndThinking()
	if app.blocks[0].stream {
		t.Fatal("second EndThinking unsettled the block")
	}

	app2, _ := newTestApp(t, 80, 24)
	app2.SetShowThinking(true)
	app2.EndThinking() // nothing open
	if len(app2.blocks) != 0 {
		t.Fatalf("EndThinking with no block created %d blocks", len(app2.blocks))
	}
}

// TestThinkingBlockDroppedWhenDisplayOff confirms the display toggle still
// gates the block the same way it did before the guard: with showThinking
// off, BeginThinking is a no-op and nothing is created.
func TestThinkingBlockDroppedWhenDisplayOff(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.SetShowThinking(false)
	app.BeginThinking()
	app.AppendThinking("invisible")
	app.EndThinking()
	if len(app.blocks) != 0 {
		t.Fatalf("showThinking off created %d blocks", len(app.blocks))
	}
}

// TestTrajectorySmoke is the one runnable check for the trajectory ledger:
// it wires data, opens the list, paints it, presses Enter to open the
// inspector, and asserts the selected record's Detail text reaches the
// simulation screen — the smallest thing that fails if the row→inspector
// mapping breaks. Esc backs out to the list, then closes it.
func TestTrajectorySmoke(t *testing.T) {
	app, scr := newTestApp(t, 120, 30)
	defer scr.Fini()

	app.SetTrajectoryOps(&TrajectoryOps{
		Records: func() []TrajectoryRecord {
			return []TrajectoryRecord{
				{Index: 1, Kind: "user", Text: "make a file", Detail: "make a file\ncreate foo.txt", Meta: "0 tokens · 1.2s"},
				{Index: 2, Kind: "tool", Text: "edit foo.txt", Detail: "--- a/foo.txt\n+++ b/foo.txt\n+hello", Meta: "12 tokens · 0.4s · exit 0"},
				{Index: 3, Kind: "message", Text: "done", Detail: "all good", Meta: "3 tokens · 0.1s"},
			}
		},
	})

	if !app.OpenTrajectory() {
		t.Fatal("OpenTrajectory returned false with data wired")
	}
	if !app.TrajectoryOpen() {
		t.Fatal("ledger not open after OpenTrajectory")
	}
	app.draw()
	if text := screenText(scr); !strings.Contains(text, "TRAJECTORY") {
		t.Fatalf("ledger not painted: %q", text)
	}

	// The ledger's newest record is selected by default; its one-line
	// preview must be on screen, and the other records' previews must
	// be too — a row that did not make it into the ledger is a broken seam.
	if text := screenText(scr); !strings.Contains(text, "done") || !strings.Contains(text, "edit foo.txt") || !strings.Contains(text, "make a file") {
		t.Fatalf("ledger previews missing: %q", text)
	}

	// Enter opens the inspector on the selected (newest) record. Its Detail
	// text reaches the screen — the smallest thing that fails if the
	// row→inspector mapping breaks.
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	app.draw()
	text := screenText(scr)
	if !strings.Contains(text, "TRAJECTORY — record") {
		t.Fatalf("ledger did not switch to inspector: %q", text)
	}
	for _, want := range []string{"all good", "3 tokens · 0.1s"} {
		if !strings.Contains(text, want) {
			t.Fatalf("inspector body did not reach the screen: missing %q in %q", want, text)
		}
	}
	// Esc steps back to the ledger, then closes it.
	app.handleKey(tcell.NewEventKey(tcell.KeyEsc, 0, tcell.ModNone))
	app.draw()
	if !app.TrajectoryOpen() {
		t.Fatal("Esc closed the ledger instead of stepping back to it")
	}
	app.handleKey(tcell.NewEventKey(tcell.KeyEsc, 0, tcell.ModNone))
	app.draw()
	if app.TrajectoryOpen() {
		t.Fatal("second Esc did not close the ledger")
	}
}
