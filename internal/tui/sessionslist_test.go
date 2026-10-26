package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// The sessions strip is chrome below the status row. The bug it fixes was an
// overlap: the strip painted rows the transcript had already claimed, so the
// last lines of scrollback sat under the list. These three assertions pin the
// contract — no rows without a child, and when there IS a child the transcript
// keeps exactly the rows above the composer.

func TestSessionsStripNoRowsWithoutChildren(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.mu.Lock()
	n := app.sessionsListRows()
	app.mu.Unlock()
	if n != 0 {
		t.Fatalf("sessionsListRows = %d with no subagent child, want 0", n)
	}
}

func TestSessionsStripDoesNotOverlapTranscript(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.AddUserBlock("hello")
	app.AddToolBlock("c1", taskToolName, `{"description":"scan"}`)
	app.AddTaskChild("c1", "scan", "scout", "")

	app.mu.Lock()
	strip := app.sessionsListRows()
	vp := app.viewportLinesLocked()
	top := app.transcriptTop()
	cRows := app.composerRows()
	h := app.height
	app.mu.Unlock()

	if strip == 0 {
		t.Fatal("sessionsListRows = 0 with one child, want > 0")
	}
	// The transcript's last painted row must be strictly above the composer's
	// first row — the strip and the composer both live below it.
	lastTranscriptRow := top + vp - 1
	composerTop := h - 1 - cRows - strip
	if lastTranscriptRow >= composerTop {
		t.Fatalf("transcript row %d overlaps composer at %d (h=%d vp=%d top=%d cRows=%d strip=%d)",
			lastTranscriptRow, composerTop, h, vp, top, cRows, strip)
	}
}

func TestSessionsStripArrowsWalkRows(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.AddToolBlock("c1", taskToolName, `{"description":"scan"}`)
	app.AddTaskChild("c1", "scan", "scout", "")

	app.mu.Lock()
	before := app.sessionsSel
	walked := app.sessionsListKey(tcell.KeyDown)
	after := app.sessionsSel
	app.mu.Unlock()

	if !walked || after != before+1 {
		t.Fatalf("down = (walked=%v, sel=%d), want (true, %d)", walked, after, before+1)
	}
}

func TestSessionsStripPaintsChildLabel(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.AddUserBlock("hello")
	app.AddToolBlock("c1", taskToolName, `{"description":"scan"}`)
	app.AddTaskChild("c1", "scan", "scout", "")
	app.draw()

	cells, w, h := scr.GetContents()
	var b strings.Builder
	for i, c := range cells {
		if i%w == 0 && i > 0 {
			b.WriteByte('\n')
		}
		if c.Runes == nil {
			b.WriteByte(' ')
			continue
		}
		b.WriteString(string(c.Runes))
	}
	_ = h
	if !strings.Contains(b.String(), "scan") {
		t.Fatalf("child label missing from frame:\n%s", b.String())
	}
}
