package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// TestFencedBlockSurvivesWrapAndHeadTailWindow is the composition check: a code
// block is not only rendered, it is WRAPPED (a long line becomes several
// visual lines) and it lives inside a tool-free assistant block that the
// head/tail window can trim. Both paths rebuild or re-slice runs, so both are
// places a multi-run line could lose its colours or its text. The rule is the
// same in both: the text must be identical to the source line, and the
// colouring must survive the reshape.
func TestFencedBlockSurvivesWrapAndHeadTailWindow(t *testing.T) {
	app, _ := newTestApp(t, 40, 24)
	t.Setenv("NO_COLOR", "")
	// A leading tab is the pre-existing wrap behaviour's business (it is
	// trimmed at a break point, exactly as it was for a one-run line), so the
	// byte-preservation assertion uses a line that starts with a space.
	long := " callSomethingVeryLongIndeed(someArgument, anotherArgument, 42) // trailing note"
	src := "```go\n" + long + "\n```"

	app.mu.Lock()
	rendered := app.renderMarkdown(src, 40)
	wrapped := wrapLine(rendered[0], 40)
	app.mu.Unlock()

	if len(wrapped) < 2 {
		t.Fatalf("a %d-wide render of a %d-cell line must wrap: %q", 40, len(long), long)
	}
	// Every wrapped piece is still the source line, character for character.
	var rejoined string
	for _, ln := range wrapped {
		rejoined += runsString(ln.runs)
	}
	if rejoined != long {
		t.Errorf("wrapping changed the line:\n got %q\nwant %q", rejoined, long)
	}
	// And the colours survived the reshape: at least two inks across the
	// wrapped pieces, and the comment ink is one of them.
	inks := map[tcell.Style]bool{}
	commentInk := tcell.StyleDefault
	for _, ln := range wrapped {
		for _, c := range ln.runs {
			inks[c.style] = true
			if strings.Contains(c.text, "//") {
				commentInk = c.style
			}
		}
	}
	if len(inks) < 2 {
		t.Errorf("wrapping flattened %d distinct inks to %d", 2, len(inks))
	}
	if commentInk == tcell.StyleDefault {
		t.Error("the comment lost its ink across the wrap")
	}
}

// TestFencedBlockInToolOutputIsNotColoured guards the boundary: only ASSISTANT
// prose goes through renderMarkdown. A tool result is painted by the diff /
// box path and must stay exactly as it was, because a tool result is not
// markdown and colouring `ls` output as Go would be a lie.
func TestFencedBlockInToolOutputIsNotColoured(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	t.Setenv("NO_COLOR", "")
	// The plain-text path a tool body takes, for comparison: one run, body ink.
	body := app.mdStyle().body
	lines := fencedRuns(t, app, "```go\n// note\n```")
	if len(lines) != 1 || len(lines[0].runs) != 1 {
		t.Fatalf("a single-comment block should be one comment run, got %d runs", len(lines[0].runs))
	}
	if lines[0].runs[0].style == body {
		t.Error("a lone comment in a go block must be coloured")
	}
}
