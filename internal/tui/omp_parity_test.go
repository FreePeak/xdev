package tui

// The phase-1 parity tests: the shapes xdev's transcript and status row wear.
//
// Each one is written so it FAILS against the code it replaced — the old ◈ /
// ✗ / ● tool glyphs, the dead running-tool indicator that reserved a gap and
// painted nothing, and the dock's section heading in a syntax colour.

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// toolRowText renders one tool-call row and returns the text of its first
// line: the bullet, the tool's name and the detail beside it.
func toolRowText(t *testing.T, app *App, i int) string {
	t.Helper()
	app.mu.Lock()
	lines := app.blockLines(i, app.blocks[i], 80)
	app.mu.Unlock()
	if len(lines) == 0 {
		t.Fatalf("tool block %d rendered no rows", i)
	}
	return lineText(lines[0])
}

// TestToolStatusGlyphs pins the tool call row's status glyph to omp's set:
// a finished call reads "✔ bash · ls -la", a failed one "✘ bash · ls -la",
// and a running one wears the circle-braille status spinner.
//
// The glyphs are the information — a reader scans the left column for shape
// before hue — so the assertion is on the CHARACTER, not on the colour.
func TestToolStatusGlyphs(t *testing.T) {
	app, _ := newTestApp(t, 100, 40)
	app.AddToolBlock("c1", "bash", `{"command":"ls -la"}`)

	// Running: the status spinner, not the activity one.
	app.mu.Lock()
	app.st.spinnerIdx = 0
	app.mu.Unlock()
	row := toolRowText(t, app, 0)
	if !strings.HasPrefix(row, "⣾ ") {
		t.Fatalf("running tool row = %q, want the circle-braille status frame", row)
	}

	// Finished: omp's success glyph.
	app.FinishTool("c1", "bash", false, "total 0\n", ToolOutcome{})
	row = toolRowText(t, app, 0)
	if !strings.HasPrefix(row, "✔ bash") {
		t.Fatalf("finished tool row = %q, want a leading ✔", row)
	}

	// Failed: omp's error glyph — ✘, not the ✗ this used to paint.
	app.AddToolBlock("c2", "bash", `{"command":"ls -la"}`)
	app.FinishTool("c2", "bash", true, "no such file\n", ToolOutcome{})
	row = toolRowText(t, app, 2)
	if !strings.HasPrefix(row, "✘ bash") {
		t.Fatalf("failed tool row = %q, want a leading ✘", row)
	}
	if strings.ContainsRune(row, '✗') {
		t.Errorf("the tool row still paints the ✗ glyph: %q", row)
	}
}

// TestToolStatusGlyphInk pins the inks the glyphs wear: a finished call's ✔
// is the success ink, a failed one's ✘ is the error ink, and the running
// spinner is the accent.
func TestToolStatusGlyphInk(t *testing.T) {
	app, _ := newTestApp(t, 100, 40)
	th := theme.Load("groknight")
	app.AddToolBlock("c1", "bash", `{"command":"ls -la"}`)

	inkAt := func(i int) tcell.Color {
		app.mu.Lock()
		lines := app.blockLines(i, app.blocks[i], 80)
		app.mu.Unlock()
		fg, _, _ := lines[0].runs[0].style.Decompose()
		return fg
	}
	if got, want := inkAt(0), app.cellColor(th.Get(theme.AccentRunning)); got != want {
		t.Errorf("running bullet ink = %v, want the accent %v", got, want)
	}
	app.FinishTool("c1", "bash", false, "total 0\n", ToolOutcome{})
	if got, want := inkAt(0), app.cellColor(th.Get(theme.AccentSuccess)); got != want {
		t.Errorf("ok bullet ink = %v, want the success ink %v", got, want)
	}
	app.AddToolBlock("c2", "bash", `{"command":"ls -la"}`)
	app.FinishTool("c2", "bash", true, "boom\n", ToolOutcome{})
	if got, want := inkAt(2), app.cellColor(th.Get(theme.AccentError)); got != want {
		t.Errorf("error bullet ink = %v, want the error ink %v", got, want)
	}
}

// TestStatusRowShowsTheRunningTool pins the running-tool indicator: while a
// tool runs, the status row says which one. It used to RESERVE the space and
// paint nothing — the row shortened the path to make room for a label that
// never appeared — so a long tool call was invisible.
func TestStatusRowShowsTheRunningTool(t *testing.T) {
	app, scr := drawnApp(t, 120, 24)
	app.BeginActiveCommand("bash")
	defer app.EndActiveCommand()
	app.draw()

	text := screenText(scr)
	if !strings.Contains(text, "bash") {
		t.Fatalf("the status row does not name the running tool:\n%s", text)
	}
	// It is the HUD's "command" segment, so it wears the status line's
	// separator ink — the row's own quiet ink, not a headline.
	if text, token := app.hudSegment("command"); !strings.Contains(text, "bash") {
		t.Fatalf("the command segment = %q, want the running tool's name", text)
	} else if token != theme.StatusLineSep {
		t.Errorf("the command segment's token = %q, want %q", token, theme.StatusLineSep)
	}

	// When nothing runs, the segment is empty and the row is unchanged.
	app.EndActiveCommand()
	if text, _ := app.hudSegment("command"); text != "" {
		t.Errorf("the command segment = %q, want empty once the tool finished", text)
	}
}
