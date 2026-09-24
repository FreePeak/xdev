package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// TestMarkdownTableRendersGrid pins the omp grid: sharp corners, a full rule
// between every pair of rows, bold flush-left header, content-width columns.
func TestMarkdownTableRendersGrid(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	src := "| Name | Cmd |\n| --- | --- |\n| xdev | go build |\n| qa | make test |"
	app.mu.Lock()
	lines := app.renderMarkdown(src, 80)
	app.mu.Unlock()
	want := strings.Join([]string{
		"┌──────┬───────────┐",
		"│ Name │ Cmd       │",
		"├──────┼───────────┤",
		"│ xdev │ go build  │",
		"├──────┼───────────┤",
		"│ qa   │ make test │",
		"└──────┴───────────┘",
	}, "\n")
	if got := strings.TrimSuffix(joinedLines(lines), "\n"); got != want {
		t.Fatalf("table grid:\n%s\nwant:\n%s", got, want)
	}
	// The header row carries the bold bit; body rows do not.
	if _, _, attr := lines[1].runs[1].style.Decompose(); attr&tcell.AttrBold == 0 {
		t.Error("header cell is not bold")
	}
	if _, _, attr := lines[3].runs[1].style.Decompose(); attr&tcell.AttrBold != 0 {
		t.Error("body cell must not be bold")
	}
}

// TestMarkdownTableShrinksToFit: even a table whose longest words overflow the
// width renders as a grid — every border row lands on the same column set and
// nothing exceeds w.
func TestMarkdownTableShrinksToFit(t *testing.T) {
	app, _ := newTestApp(t, 40, 24)
	src := "| Feature | Ref |\n| --- | --- |\n" +
		"| markdown-table-rendering | https://example.com/very/long/path/that/keeps/going |"
	app.mu.Lock()
	lines := app.renderMarkdown(src, 40)
	app.mu.Unlock()
	joined := joinedLines(lines)
	if !strings.Contains(joined, "┌") {
		t.Fatalf("expected a grid at 40 cols, got:\n%s", joined)
	}
	for n, ln := range lines {
		if wd := runsWidth(ln.runs); wd != 40 {
			t.Fatalf("row %d is %d cells wide, want 40:\n%s", n, wd, runsString(ln.runs))
		}
	}
	if !strings.Contains(joined, "markdown-") || !strings.Contains(joined, "https://") {
		t.Errorf("long cells were lost instead of wrapped:\n%s", joined)
	}
}

// TestMarkdownTableNarrowFallsBack: below 4r+1 cells the grid cannot fit and
// the raw markdown lines render instead (omp's escape hatch), un-boxed.
func TestMarkdownTableNarrowFallsBack(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	src := "| Name | Cmd |\n| --- | --- |\n| xdev | go build |"
	app.mu.Lock()
	lines := app.renderMarkdown(src, 8)
	app.mu.Unlock()
	joined := joinedLines(lines)
	if strings.Contains(joined, "┌") || !strings.Contains(joined, "| Name | Cmd |") {
		t.Fatalf("narrow table must fall back to raw lines, got:\n%s", joined)
	}
}

// TestMarkdownTableNeedsDelimiter: a paragraph with pipes is not a table.
func TestMarkdownTableNeedsDelimiter(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.mu.Lock()
	lines := app.renderMarkdown("a | b\nc | d", 80)
	app.mu.Unlock()
	joined := joinedLines(lines)
	if strings.Contains(joined, "┌") || !strings.Contains(joined, "a | b") {
		t.Fatalf("non-table pipe lines were boxed:\n%s", joined)
	}
}

// TestMarkdownTableCellSyntax: \| is a literal cell pipe, alignment colons
// parse (and render flush-left like every other cell), and inline markdown
// inside cells still styles.
func TestMarkdownTableCellSyntax(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	src := "| A | B |\n| :--- | ---: |\n| x \\| y | `z` |"
	app.mu.Lock()
	lines := app.renderMarkdown(src, 80)
	app.mu.Unlock()
	joined := joinedLines(lines)
	if !strings.Contains(joined, "x | y") {
		t.Fatalf("escaped pipe lost:\n%s", joined)
	}
	if strings.Contains(joined, "\\|") || strings.Contains(joined, "`z`") {
		t.Fatalf("markers must render hidden, not literal:\n%s", joined)
	}
}

// TestMarkdownTableCJKWidth: double-width cells pad by display cells, so the
// right border still lands on one column.
func TestMarkdownTableCJKWidth(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	src := "| 項目 | 説明 |\n| --- | --- |\n| テーブル | ok |"
	app.mu.Lock()
	lines := app.renderMarkdown(src, 80)
	app.mu.Unlock()
	if len(lines) != 5 {
		t.Fatalf("rows = %d, want 5:\n%s", len(lines), joinedLines(lines))
	}
	w0 := runsWidth(lines[0].runs)
	for n, ln := range lines[1:] {
		if wd := runsWidth(ln.runs); wd != w0 {
			t.Fatalf("row %d is %d cells wide, want %d:\n%s", n+1, wd, w0, runsString(ln.runs))
		}
	}
}

func TestMarkdownLinkTargetsAndBareURLs(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)
	lines := app.renderMarkdown("## See [PR #24775](https://github.com/pulumi/pull/24775) and https://example.com/a?x=1.", 100)
	if got := runsString(lines[0].runs); got != "See PR #24775 and https://example.com/a?x=1." {
		t.Fatalf("rendered heading = %q", got)
	}
	var got []string
	for _, run := range lines[0].runs {
		if run.link != "" {
			got = append(got, run.link)
		}
	}
	want := []string{"https://github.com/pulumi/pull/24775", "https://example.com/a?x=1"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("heading link targets = %q, want %q", got, want)
	}
}

func TestMarkdownHeadingKeepsLiteralAsterisks(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)
	for _, src := range []string{"# Compare foo*bar*baz", "# Keep a * b * c"} {
		lines := app.renderMarkdown(src, 100)
		want := strings.TrimSpace(strings.TrimPrefix(src, "#"))
		if got := runsString(lines[0].runs); got != want {
			t.Errorf("%q rendered %q, want %q", src, got, want)
		}
	}
}

func TestMarkdownFormattedLinkKeepsTarget(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)
	lines := app.renderMarkdown("**[PR #24775](https://example.com/a)**", 100)
	for _, run := range lines[0].runs {
		if run.text == "PR #24775" {
			if run.link != "https://example.com/a" {
				t.Fatalf("formatted link = %+v", run)
			}
			return
		}
	}
	t.Fatalf("formatted label missing from %q", runsString(lines[0].runs))
}

func TestMarkdownLinkDestinationKeepsBalancedParentheses(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)
	lines := app.renderMarkdown("[docs](https://example.com/a_(b))", 100)
	if got := runsString(lines[0].runs); got != "docs" {
		t.Fatalf("rendered text = %q", got)
	}
	if got := lines[0].runs[0].link; got != "https://example.com/a_(b)" {
		t.Fatalf("link = %q", got)
	}
}

func TestMarkdownLinkDestinationIgnoresOptionalTitle(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)
	lines := app.renderMarkdown(`[docs](https://example.com/docs "Docs")`, 100)
	if got := lines[0].runs[0].link; got != "https://example.com/docs" {
		t.Fatalf("link = %q", got)
	}
}

func TestMarkdownLinkDestinationUnescapesTarget(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)
	lines := app.renderMarkdown(`[docs](https://example.com/a\(b\))`, 100)
	if got := lines[0].runs[0].link; got != "https://example.com/a(b)" {
		t.Fatalf("link = %q", got)
	}
}

func TestMarkdownNonWebLinkStillHidesDestination(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)
	lines := app.renderMarkdown("[security](mailto:security@example.com)", 100)
	if got := runsString(lines[0].runs); got != "security" {
		t.Fatalf("rendered text = %q", got)
	}
	for _, run := range lines[0].runs {
		if run.link != "" {
			t.Fatalf("non-web target became clickable: %+v", run)
		}
	}
}
func TestMarkdownLinkDestinationStopsBeforeTitleParen(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)
	lines := app.renderMarkdown(`[docs](https://example.com "API v1)")`, 100)
	if got := runsString(lines[0].runs); got != "docs" {
		t.Fatalf("rendered text = %q", got)
	}
	if got := lines[0].runs[0].link; got != "https://example.com" {
		t.Fatalf("link = %q", got)
	}
}

func TestMarkdownLinkDestinationAcceptsSingleQuotedTitle(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)
	lines := app.renderMarkdown(`[docs](https://example.com 'Docs')`, 100)
	if got := lines[0].runs[0].link; got != "https://example.com" {
		t.Fatalf("link = %q", got)
	}
}

func TestMarkdownAngleDestinationRequiresClosingParen(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)
	lines := app.renderMarkdown(`[label](<https://example.com> trailing)`, 100)
	for _, run := range lines[0].runs {
		if run.link != "" && run.text == "label" {
			t.Fatalf("malformed angle destination became clickable: %+v", run)
		}
	}
}

func TestMarkdownLinkTargetSurvivesWrapping(t *testing.T) {
	app, _ := newTestApp(t, 24, 24)
	lines := wrapLine(line{runs: app.inlineRuns("[a very long linked label](https://example.com/path)", app.mdStyle())}, 24)
	found := false
	for _, ln := range lines {
		for _, run := range ln.runs {
			if strings.Contains(run.text, "linked") {
				if run.link != "https://example.com/path" {
					t.Fatalf("wrapped link = %+v", run)
				}
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("wrapped link missing from %q", joinedLines(lines))
	}
}

func TestMarkdownRejectsNonWebLinkTarget(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	lines := app.renderMarkdown("[local](file:///tmp/x)", 80)
	for _, run := range lines[0].runs {
		if run.link != "" {
			t.Fatalf("non-web target became clickable: %+v", run)
		}
	}
}

func TestMarkdownLinkTargetSurvivesTableCell(t *testing.T) {
	app, _ := newTestApp(t, 40, 24)
	lines := app.renderMarkdown("| Ref |\n| --- |\n| [docs](https://example.com/very/long/path) |", 40)
	found := false
	for _, ln := range lines {
		for _, run := range ln.runs {
			if run.text == "docs" {
				found = true
				if run.link != "https://example.com/very/long/path" {
					t.Fatalf("table link = %+v", run)
				}
			}
		}
	}
	if !found {
		t.Fatalf("table link missing from %q", joinedLines(lines))
	}
}
