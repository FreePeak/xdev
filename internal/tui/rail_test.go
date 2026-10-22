package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// The transcript has NO left rail. omp draws a one-cell vertical rule beside
// nothing: an assistant message is plain markdown
// (packages/tui/src/chat/assistant-message.ts:196-207), a tool call is a card
// frame (render/output-block.ts:20-104), a user message is a band
// (chat/user-message.ts:53-68) and a system notice is a box
// (chrome/message-frame.ts:113-118). A magenta rule down the left edge of
// every assistant message was xdev's loudest shape difference from omp.
//
// The check is on the PAINTED screen, not on a renderer: a rule that lived in
// the row index and was drawn by the painter would still be visible here.
func TestTranscriptHasNoLeftRail(t *testing.T) {
	app, scr := drawnApp(t, 80, 20)
	// One block of every kind that used to carry a rail: the tool call, the
	// system notice and the assistant message.
	app.AddSystemBlock("a system notice")
	app.AddAssistantBlock("an assistant paragraph")
	app.AddToolBlock("", "bash", `{"command":"ls -la"}`)
	app.FinishTool("", "bash", false, "total 0\n", ToolOutcome{Dur: "1ms"})
	app.draw()

	for y, row := range strings.Split(screenText(scr), "\n") {
		for x := range transcriptContentX {
			if x >= len(row) {
				break
			}
			if r := rune(row[x]); r == '┃' || r == '│' {
				t.Fatalf("row %d column %d paints %q — the transcript has no left rail:\n%s",
					y, x, string(r), screenText(scr))
			}
		}
	}
}

// TestTranscriptTextStartsAtThePadColumn pins where the text lands once the
// rail is gone: the pane's own pad, two cells in. The offset is shared with
// the copy path (a selection's x0), the link hit-test and the diff overlay's
// wrap budget, so a move in one place without the others would show up here
// as text at the wrong column.
func TestTranscriptTextStartsAtThePadColumn(t *testing.T) {
	app, scr := drawnApp(t, 80, 20)
	app.AddAssistantBlock("flushleft")
	app.draw()

	prim, w, _ := scr.GetContents()
	for y := range 20 {
		for x := range transcriptContentX {
			c := prim[y*w+x]
			if len(c.Runes) > 0 && c.Runes[0] != ' ' {
				t.Fatalf("row %d column %d paints %q before the pad column:\n%s",
					y, x, string(c.Runes[0]), screenText(scr))
			}
		}
		// The word itself starts AT the pad column, not one cell past it.
		for x := range transcriptContentX + len("flushleft") {
			c := prim[y*w+x]
			if len(c.Runes) > 0 && c.Runes[0] == 'f' {
				if x != transcriptContentX {
					t.Fatalf("the assistant text starts at column %d, want the pad column %d",
						x, transcriptContentX)
				}
				return
			}
		}
	}
	t.Fatalf("the assistant paragraph was not painted:\n%s", screenText(scr))
}

// TestTranscriptCopyYieldsExactlyTheSelectedText is the copy path's half of
// the same change: with the rail gone the selection's anchor column moved a
// cell left, and a copy must still be the selected TEXT — no padding, no
// decoration, nothing the pointer did not cover.
func TestTranscriptCopyYieldsExactlyTheSelectedText(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.AddSystemBlock("copy me please")
	app.draw()

	y := contentRow(t, app, "copy me please")
	app.mu.Lock()
	drag(app, transcriptContentX, y, transcriptContentX+len("copy me")-1, y)
	app.mu.Unlock()

	if got := string(scr.GetClipboardData()); got != "copy me" {
		t.Fatalf("clipboard = %q, want %q", got, "copy me")
	}
}

// TestMouseClickHitsTheCellUnderThePointer pins the hit-test's half: a
// double click on a row's first character selects that word, at whatever column
// the text now starts in.
func TestMouseClickHitsTheCellUnderThePointer(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.AddSystemBlock("hello world")
	app.draw()

	y := contentRow(t, app, "hello world")
	app.mu.Lock()
	press(app, transcriptContentX, y)
	release(app, transcriptContentX, y)
	app.mu.Lock()
	press(app, transcriptContentX, y)
	app.mu.Unlock()
	app.draw()
	app.mu.Lock()
	release(app, transcriptContentX, y)
	app.mu.Unlock()
	app.draw()

	if got := string(scr.GetClipboardData()); got != "hello" {
		t.Fatalf("double-click clipboard = %q, want %q", got, "hello")
	}
	// The highlight covers the word it selected, on the row it selected it on.
	if _, _, attr := cellStyle(scr, transcriptContentX, y).Decompose(); attr&tcell.AttrReverse == 0 {
		t.Fatal("the clicked word is not highlighted")
	}
}
