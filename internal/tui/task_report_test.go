package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// A `task` result is prose a MODEL wrote for a person, so it paints through
// the markdown renderer the assistant's own answer uses: the structure the
// subagent wrote is the structure the reader gets. This is the one tool path
// that changes rows' text (a heading loses its "#"), so it is asserted as such
// — a bullet becomes "•", the heading keeps its words, the fence keeps its
// source, and the code line wears the lexer's ink rather than the box's.
func TestTaskResultRendersAsMarkdown(t *testing.T) {
	app, _ := newTestApp(t, 100, 40)
	body := strings.Join([]string{
		"subagent yielded",
		"",
		"## Findings",
		"",
		"- one thing at `internal/tui/app.go`",
		"- another",
		"",
		"```go",
		"const userBandMargin = 1",
		"```",
	}, "\n")
	app.AddToolBlock("c1", "task", `{"prompt":"recon"}`)
	app.FinishTool("c1", "task", false, body, ToolOutcome{})

	rows, text := joinBox(t, app, app.blocks[len(app.blocks)-1], 96)
	if !strings.Contains(text, "subagent yielded") || !strings.Contains(text, "Findings") {
		t.Fatalf("the report lost its words:\n%s", text)
	}
	// The layout is the report's own: hashes are headings, dashes are bullets.
	if strings.Contains(text, "## Findings") || strings.Contains(text, "- one thing") {
		t.Fatalf("the markdown was printed as literal source:\n%s", text)
	}
	if !strings.Contains(text, "• one thing") {
		t.Fatalf("the bullet did not render as one:\n%s", text)
	}
	// Inline code keeps its text and leaves the backticks behind; the fence's
	// own source survives whole.
	if !strings.Contains(text, "• one thing at internal/tui/app.go") {
		t.Fatalf("the inline code lost its backticks or its text:\n%s", text)
	}
	if !strings.Contains(text, "const userBandMargin = 1") {
		t.Fatalf("the fenced source did not survive:\n%s", text)
	}
	// Colour: a heading wears the md_heading ink (bold), a prose row the body
	// ink, and the fenced line the lexer's — three different inks on rows that
	// a flat tool box would have painted identically.
	ink := func(needle string) tcell.Style {
		t.Helper()
		for _, ln := range bodyRows(rows) {
			if strings.Contains(runsString(ln.runs), needle) {
				return ln.runs[len(ln.runs)-1].style
			}
		}
		t.Fatalf("row %q is not on screen:\n%s", needle, text)
		return tcell.StyleDefault
	}
	ms := app.mdStyle()
	if got := ink("Findings"); got != ms.h2 {
		t.Fatalf("the heading painted %v, want the md h2 ink %v", got, ms.h2)
	}
	if got := ink("subagent yielded"); got != ms.body {
		t.Fatalf("prose painted %v, want the body ink %v", got, ms.body)
	}
	if got := ink("const userBandMargin"); got == ms.body {
		t.Fatalf("the fenced source painted in the flat body ink, so nothing lexed it: %v", got)
	}
}

// Every other tool is untouched: a grep result that happens to contain a
// heading stays flat, so "a tool whose output contains markdown" cannot
// silently become a report. `task` is named because it is the only tool that
// spawns a model; nothing else composes prose.
func TestNonTaskOutputDoesNotBecomeAReport(t *testing.T) {
	app, _ := newTestApp(t, 100, 40)
	body := "## Not a report\n- just lines a command printed\n"
	for _, tool := range []string{"grep", "bash", "read", "hub"} {
		app.AddToolBlock("c-"+tool, tool, `{"pattern":"x"}`)
		app.FinishTool("c-"+tool, tool, false, body, ToolOutcome{})
		rows, text := joinBox(t, app, app.blocks[len(app.blocks)-1], 96)
		if !strings.Contains(text, "## Not a report") || !strings.Contains(text, "- just lines") {
			t.Fatalf("%s output was rendered as markdown:\n%s", tool, text)
		}
		for _, ln := range bodyRows(rows) {
			if len(runStyles(ln)) > 1 {
				t.Fatalf("%s painted a multi-ink row: %v", tool, ln.runs)
			}
		}
	}
}

// A `task` result that is not markdown at all — the transport errors the tool
// returns as its own text ("task: no provider configured") — still paints as a
// report rather than as code: the markdown renderer passes a plain paragraph
// through in the body ink, which is the same one ink the box used before, so
// the fallback is invisible.
func TestTaskPlainResultStillReadsAsBefore(t *testing.T) {
	app, _ := newTestApp(t, 100, 40)
	app.AddToolBlock("c1", "task", `{"prompt":"x"}`)
	app.FinishTool("c1", "task", false, "task: no provider configured for subagents", ToolOutcome{})

	rows, text := joinBox(t, app, app.blocks[len(app.blocks)-1], 96)
	if !strings.Contains(text, "task: no provider configured for subagents") {
		t.Fatalf("the plain result changed its text:\n%s", text)
	}
	for _, ln := range bodyRows(rows) {
		if strings.TrimSpace(runsString(ln.runs)) == "" {
			continue
		}
		if got := ln.runs[len(ln.runs)-1].style; got != app.mdStyle().body {
			t.Fatalf("a plain task result painted %v, want the body ink the box used before", got)
		}
	}
}
