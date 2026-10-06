package tui

// Tests for tool-output highlighting. Every fixture here is invented: a
// synthesized .go source, a synthesized shell listing, a synthesized diff. No
// real file from any repository is quoted, because a fixture that quotes one
// carries that repository's content into this repo.

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// joinBox renders a finished result and returns its rows' text plus the rows
// themselves, the same way the transcript reads them.
//
// NO_COLOR is cleared for every test in this file except the one whose subject
// IS NO_COLOR: the code under test reads it, so a session that happens to
// export it would otherwise make half of these tests prove nothing.
func joinBox(t *testing.T, app *App, b *Block, w int) ([]line, string) {
	t.Helper()
	t.Setenv("NO_COLOR", "")
	rows := app.toolBoxLines(len(app.blocks)-1, b, w)
	var sb strings.Builder
	for _, ln := range rows {
		sb.WriteString(runsString(ln.runs))
		sb.WriteByte('\n')
	}
	return rows, sb.String()
}

// bodyRows strips a box's own frame from every row — the side borders, the
// padding cells and the top/bottom rules — so a test can look at the output a
// command produced. Chrome runs are exactly the ones the renderer painted for
// the frame; a result's own rows never are.
func bodyRows(rows []line) []line {
	var out []line
	for _, ln := range rows {
		runs := ln.runs
		for len(runs) > 0 && runs[0].chrome {
			runs = runs[1:]
		}
		for len(runs) > 0 && runs[len(runs)-1].chrome {
			runs = runs[:len(runs)-1]
		}
		if len(runs) == 0 {
			continue // the top and bottom rules
		}
		out = append(out, line{runs: runs})
	}
	return out
}

// runStyles lists the distinct styles a row's runs wear, so a test can ask
// "was this row actually coloured" without naming a theme ink.
func runStyles(ln line) []tcell.Style {
	var out []tcell.Style
	for _, r := range ln.runs {
		if r.chrome || r.text == "" {
			continue
		}
		if n := len(out); n > 0 && out[n-1] == r.style {
			continue
		}
		out = append(out, r.style)
	}
	return out
}

// A read of a .go file paints with the Go lexer: the row carries more than one
// style, and the line-number prefix is chrome in the dim ink while the source
// beside it is coloured.
func TestReadResultHighlightsAsGo(t *testing.T) {
	app, _ := newTestApp(t, 100, 40)
	app.AddToolBlock("c1", "read", `{"path":"a.go"}`)
	app.FinishTool("c1", "read", false,
		"[pkg/a.go#1a2b]\n1:package main\n2:\n3:// note\n4:func main() { s := \"hi\"; _ = 42 }",
		ToolOutcome{})

	rows, text := joinBox(t, app, app.blocks[len(app.blocks)-1], 96)
	if !strings.Contains(text, "func main() {") {
		t.Fatalf("the source did not survive the render:\n%s", text)
	}
	var src line
	for _, ln := range bodyRows(rows) {
		if strings.Contains(runsString(ln.runs), "func main()") {
			src = ln
		}
	}
	if len(runStyles(src)) < 2 {
		t.Fatalf("the source row painted in one ink; highlighting did nothing: %v", src.runs)
	}
	if first := src.runs[0]; !strings.HasPrefix(first.text, "4:") || first.style != app.mdStyle().muted &&
		first.style != dimStyleOf(app) {
		t.Fatalf("the line-number prefix is not the dim chrome run: %+v", src.runs[0])
	}
}

// dimStyleOf is the dim ink toolBoxLines paints chrome with.
func dimStyleOf(a *App) tcell.Style {
	return tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim)))
}

// Every highlighted row keeps the tool's bytes: concatenating the runs of a
// rendered row reproduces the source line exactly, so selection and copy are
// untouched by the colouring.
func TestHighlightPreservesBytesExactly(t *testing.T) {
	app, _ := newTestApp(t, 100, 40)
	src := []string{
		"[pkg/b.rs#9f]", "1:fn main() {", "2:    let s = \"héllo 世界 🎉\";",
		"3:    // ünterminated", "4:    println!(\"{s}\");", "5:}",
	}
	app.AddToolBlock("c1", "read", `{"path":"b.rs"}`)
	app.FinishTool("c1", "read", false, strings.Join(src, "\n"), ToolOutcome{})

	rows, _ := joinBox(t, app, app.blocks[len(app.blocks)-1], 96)
	var got []string
	for _, ln := range bodyRows(rows) {
		got = append(got, runsString(ln.runs))
	}
	if len(got) != len(src) {
		t.Fatalf("got %d rows, want %d:\n%s", len(got), len(src), strings.Join(got, "\n"))
	}
	for i := range src {
		if got[i] != src[i] {
			t.Errorf("row %d changed bytes\n got %q\nwant %q", i, got[i], src[i])
		}
	}
}

// Output that names no language paints exactly as it did before highlighting:
// one body-ink run per row. This is the fallback the whole feature rests on.
func TestUnknownToolOutputPaintsFlat(t *testing.T) {
	app, _ := newTestApp(t, 100, 40)
	body := "[pkg/notes.unknownext#aa]\n1:func main() { return 1 }\n2:let x = \"str\""
	app.AddToolBlock("c1", "read", `{"path":"notes.unknownext"}`)
	app.FinishTool("c1", "read", false, body, ToolOutcome{})

	rows, text := joinBox(t, app, app.blocks[len(app.blocks)-1], 96)
	if !strings.Contains(text, "func main() { return 1 }") {
		t.Fatalf("the source did not survive the render:\n%s", text)
	}
	for _, ln := range bodyRows(rows) {
		for _, r := range ln.runs {
			if r.style != app.mdStyle().body {
				t.Fatalf("an unknown extension painted a coloured run %+v in %v", r, ln.runs)
			}
		}
	}
}

// A grep result's "path:line: text" rows are coloured: the path in the
// function slot, the line number in the number slot, the colon in the
// punctuation slot, and the matched text in the body ink.
func TestGrepResultHighlightsPathLineAndText(t *testing.T) {
	app, _ := newTestApp(t, 100, 40)
	body := "internal/tui/toolhl.go:200:func (a *App) toolOutputHL(b *Block) toolHL {\n" +
		"internal/tui/app.go:4511:func (a *App) toolBoxLines(i int, b *Block, w int) []line {"
	app.AddToolBlock("c1", "grep", `{"pattern":"toolOutputHL"}`)
	app.FinishTool("c1", "grep", false, body, ToolOutcome{})

	rows, text := joinBox(t, app, app.blocks[len(app.blocks)-1], 96)
	if !strings.Contains(text, "internal/tui/toolhl.go:200:func") {
		t.Fatalf("the grep result did not survive the render:\n%s", text)
	}
	var src line
	for _, ln := range bodyRows(rows) {
		if strings.Contains(runsString(ln.runs), "internal/tui/toolhl.go:200:func") {
			src = ln
		}
	}
	if len(runStyles(src)) < 3 {
		t.Fatalf("a grep row painted in %d inks; the path, line number and text should each wear their own: %v", len(runStyles(src)), src.runs)
	}
}

// A grep result's bytes are preserved exactly: concatenating the runs of a
// rendered row reproduces the source line, so selection and copy are untouched.
func TestGrepHighlightPreservesBytesExactly(t *testing.T) {
	app, _ := newTestApp(t, 100, 40)
	src := []string{
		"internal/tui/toolhl.go:200:func (a *App) toolOutputHL(b *Block) toolHL {",
		"internal/tui/app.go:4511:func (a *App) toolBoxLines(i int, b *Block, w int) []line {",
	}
	app.AddToolBlock("c1", "grep", `{"pattern":"toolOutputHL"}`)
	app.FinishTool("c1", "grep", false, strings.Join(src, "\n"), ToolOutcome{})

	rows, _ := joinBox(t, app, app.blocks[len(app.blocks)-1], 96)
	var got []string
	for _, ln := range bodyRows(rows) {
		got = append(got, runsString(ln.runs))
	}
	for i, want := range src {
		if got[i] != want {
			t.Fatalf("row %d: got %q, want %q", i, got[i], want)
		}
	}
}

// A grep row that does not match the "path:line: text" shape (a continuation,
// a note, a blank) keeps the body ink — the same fallback every other path
// uses.
func TestGrepNonMatchingRowStaysFlat(t *testing.T) {
	app, _ := newTestApp(t, 100, 40)
	body := "internal/tui/toolhl.go:200:func (a *App) toolOutputHL(b *Block) toolHL {\n" +
		"  a continuation line\n" +
		"[showing first 10 matches]"
	app.AddToolBlock("c1", "grep", `{"pattern":"toolOutputHL"}`)
	app.FinishTool("c1", "grep", false, body, ToolOutcome{})

	rows, _ := joinBox(t, app, app.blocks[len(app.blocks)-1], 96)
	bodyRows := bodyRows(rows)
	if len(bodyRows) < 3 {
		t.Fatalf("expected at least 3 body rows, got %d", len(bodyRows))
	}
	for i, ln := range bodyRows[1:] {
		if len(runStyles(ln)) > 1 {
			t.Fatalf("row %d painted a coloured run: %v", i+1, ln.runs)
		}
	}
}
// Bash output that reads as shell — a listing of variables and strings — is
// coloured; bash output that reads as a log is not.
func TestLooksLikeShellSeparatesScriptsFromLogs(t *testing.T) {
	script := strings.Join([]string{
		`if [ -f "config.yaml" ]; then`,
		`  export SETUP_MODE="ready"`,
		`  echo "installed"`,
		`fi`,
	}, "\n")
	if !LooksLikeShell(script) {
		t.Fatal("a shell session was not recognised as shell")
	}
	log := strings.Join([]string{
		"2026/01/01 10:00:00 listening on :8080",
		"  - accepted connection",
		"  - worker started",
		"  - ready",
	}, "\n")
	if LooksLikeShell(log) {
		t.Fatal("a server log was mistaken for shell")
	}
	if LooksLikeShell("ls\ncd build\nmake") {
		t.Fatal("three rows is too few to sample; the bar must hold")
	}
	if LooksLikeShell("") {
		t.Fatal("empty output is not shell")
	}
}

// A unified diff from bash is painted by the diff renderer, so the added and
// removed bands still mean what they meant; the shell lexer never sees it.
func TestBashUnifiedDiffStillPaintsAsDiff(t *testing.T) {
	app, _ := newTestApp(t, 100, 40)
	diff := strings.Join([]string{
		"--- a/pkg/x.go",
		"+++ b/pkg/x.go",
		"@@ -1,2 +1,2 @@",
		" package main",
		"-const a = 1",
		"+const a = 2",
		" func main() {}",
	}, "\n")
	app.AddToolBlock("c1", "bash", `{"command":"git diff"}`)
	app.FinishTool("c1", "bash", false, diff, ToolOutcome{})

	b := app.blocks[len(app.blocks)-1]
	if !DiffLooksUnified(b.Text) {
		t.Fatal("the fixture is not a unified diff")
	}
	rows, _ := joinBox(t, app, b, 96)
	added, removed := 0, 0
	for _, ln := range bodyRows(rows) {
		if _, bg, _ := ln.runs[len(ln.runs)-1].style.Decompose(); bg != tcell.ColorDefault {
			if strings.HasPrefix(strings.TrimSpace(runsString(ln.runs)), "-const") {
				removed++
			}
			if strings.HasPrefix(strings.TrimSpace(runsString(ln.runs)), "+const") {
				added++
			}
		}
	}
	if added != 1 || removed != 1 {
		t.Fatalf("the diff bands did not survive highlighting: added=%d removed=%d", added, removed)
	}
}

// A long source line still colours consistently across its wrap: every painted
// row is a fragment of the same lexer, and the concatenation keeps the bytes.
func TestLongSourceLineWrapsWithoutLosingColour(t *testing.T) {
	app, _ := newTestApp(t, 60, 40)
	src := "1:" + strings.Repeat(`let x = "value"; `, 12)
	app.AddToolBlock("c1", "read", `{"path":"a.rs"}`)
	app.FinishTool("c1", "read", false, "[pkg/a.rs#bb]\n"+src, ToolOutcome{})

	rows, text := joinBox(t, app, app.blocks[len(app.blocks)-1], 40)
	if !strings.Contains(text, `let x = "value";`) {
		t.Fatalf("the wrapped source did not survive:\n%s", text)
	}
	coloured := 0
	for _, ln := range bodyRows(rows) {
		if len(runStyles(ln)) > 1 {
			coloured++
		}
	}
	if coloured < 2 {
		t.Fatalf("a long line wrapped into %d painted rows; the tail lost its colour", coloured)
	}
}

// NO_COLOR turns the whole feature off: no syntax ink is chosen, so every row
// paints in the body ink and the transcript reads exactly as it did before.
func TestNoColorPaintsFlat(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	// joinBox clears NO_COLOR for its own tests, so this one renders the box
	// directly: the app is built with NO_COLOR already exported.
	t.Setenv("NO_COLOR", "1")
	app, _ := newTestApp(t, 100, 40)
	app.AddToolBlock("c1", "read", `{"path":"a.go"}`)
	app.FinishTool("c1", "read", false,
		"[pkg/a.go#1a2b]\n1:package main\n2:func f() string { return \"x\" }",
		ToolOutcome{})

	rows := app.toolBoxLines(len(app.blocks)-1, app.blocks[len(app.blocks)-1], 96)
	for _, ln := range bodyRows(rows) {
		for _, r := range ln.runs {
			if r.style != app.mdStyle().body {
				t.Fatalf("NO_COLOR painted a coloured run %+v", r)
			}
		}
	}
}

// A write's summary line names the file but is not source: it stays flat.
func TestWriteSummaryStaysFlat(t *testing.T) {
	app, _ := newTestApp(t, 100, 40)
	app.AddToolBlock("c1", "write", `{"path":"a.go","content":"x"}`)
	app.FinishTool("c1", "write", false, "Wrote a.go (1 bytes, 1 lines)", ToolOutcome{})

	if hl := app.toolOutputHL(app.blocks[len(app.blocks)-1]); hl.lang != "" {
		t.Fatalf("a write summary picked a lexer: %q", hl.lang)
	}
}

// The decision table itself: what each shape of result resolves to.
func TestToolOutputLangTable(t *testing.T) {
	cases := []struct {
		head string
		want string
	}{
		{"[pkg/a.go#1a2b]", "go"},
		{"[pkg/App.TSX#1a2b]", "typescript"},
		{"[pkg/x.h#aa]", "c"},
		{"[Makefile#aa]", ""},
		{"[notes#aa]", ""},
		{"[pkg/a.go#aa]\n1:package main", "go"},
		{"[pkg/a.go#aa", ""}, // a header with no closing bracket is not a header
		{"Moved old/b.py to new/c.py", "python"},
		{"Wrote a.go (3 bytes, 1 lines)", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := langForToolOutput(c.head); got != c.want {
			t.Errorf("langForToolOutput(%q) = %q, want %q", c.head, got, c.want)
		}
	}
}

// Every extension in extLang must name a lexer this build actually ships, and
// must survive normalizeLang unchanged. Without this the table rots silently:
// a dropped lexer turns its extensions into dead entries that paint flat with
// no test noticing.
func TestExtLangTableMatchesShippedLexers(t *testing.T) {
	for ext, lang := range extLang {
		if got := normalizeLang(lang); got != lang {
			t.Errorf("extLang[%q] = %q, which normalizeLang rewrites to %q", ext, lang, got)
		}
		if _, ok := codeLangs[lang]; !ok {
			t.Errorf("extLang[%q] = %q, which is not a lexer in codeLangs", ext, lang)
		}
	}
}

// Every extension the languages table names is covered, so a new lexer in
// highlight.go gets a path form here rather than staying unreachable from the
// only surface that has file paths.
func TestEveryLexerIsReachableFromAnExtension(t *testing.T) {
	have := map[string]bool{}
	for _, lang := range extLang {
		have[lang] = true
	}
	for lang := range codeLangs {
		if !have[lang] {
			t.Errorf("codeLangs[%q] has no extension mapping; tool output in that language paints flat", lang)
		}
	}
}

// splitRowNumber decides the three cases that decide the whole feature's safety:
// a numbered row keeps its prefix, a header or marker stays flat, and a line
// that merely OPENS with a colon is still source. A change that gets any of
// these wrong either eats a byte of code or colours a report about the code.
func TestSplitRowNumberThreeCases(t *testing.T) {
	cases := []struct {
		row          string
		prefix, body string
		isSource     bool
	}{
		{"42:var x = 1", "42:", "var x = 1", true},
		{"1:package main", "1:", "package main", true},
		{"7:", "7:", "", true},
		{"0:", "0:", "", true},
		{"12:30 finished the job", "", "12:30 finished the job", true},
		{"key: value", "", "key: value", true},
		{"", "", "", false},
		{"[pkg/a.go#ab12]", "", "[pkg/a.go#ab12]", false},
		{"\u2026", "", "\u2026", false},
		{"\u2026 showing lines 40-49 of 900", "", "\u2026 showing lines 40-49 of 900", false},
		{"Moved a/old.py to b/new.py", "", "Moved a/old.py to b/new.py", false},
	}
	for _, c := range cases {
		prefix, body, isSource := splitRowNumber(c.row)
		if prefix != c.prefix || body != c.body || isSource != c.isSource {
			t.Errorf("splitRowNumber(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.row, prefix, body, isSource, c.prefix, c.body, c.isSource)
		}
	}
}

// "12:30 finished the job" inside a numbered window is lexed whole: the digits
// stay part of the source, so the row paints in the Go lexer's ink rather than
// opening with a chrome prefix.
func TestTimeLikeLineIsColouredWhole(t *testing.T) {
	app, _ := newTestApp(t, 100, 40)
	src := "12:30 finished the job for user \"ada\""
	app.AddToolBlock("c1", "read", `{"path":"a.py"}`)
	app.FinishTool("c1", "read", false, "[pkg/a.py#cc]\n"+src, ToolOutcome{})

	rows, text := joinBox(t, app, app.blocks[len(app.blocks)-1], 96)
	if !strings.Contains(text, src) {
		t.Fatalf("the source changed bytes:\n%s", text)
	}
	for _, ln := range bodyRows(rows) {
		if !strings.Contains(runsString(ln.runs), "12:30") {
			continue
		}
		if len(runStyles(ln)) < 2 {
			t.Fatalf("the row painted in one ink, so nothing was lexed: %v", ln.runs)
		}
		return
	}
	t.Fatalf("the row is not on screen:\n%s", text)
}
