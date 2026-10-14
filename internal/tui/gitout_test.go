package tui

// Tests for the git listing arm (gitout.go). Every fixture is invented: a
// synthesized status, a synthesized history, a synthesized branch header. No
// real repository's output is quoted, because a fixture that quotes one carries
// that repository's content into this repo.

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// statusShortFixture is a `git status --short --branch` listing: the header,
// one row per state, and nothing else.
var statusShortFixture = strings.Join([]string{
	"## work/example...origin/work/example",
	" M internal/tui/render.go",
	"?? docs/notes.md",
	"A  cmd/xdev/new.go",
	" D old/removed.go",
}, "\n")

// statusLongFixture is the same tree in the form git prints with no arguments:
// the branch header, a section heading, and one entry per change (the tab git
// writes after the label arrives expanded, the way the renderer sanitizes it).
var statusLongFixture = strings.Join([]string{
	"On branch main",
	"Your branch is up to date with 'origin/main'.",
	"",
	"Changes to be committed:",
	"  new file:   cmd/xdev/new.go",
	"",
	"Changes not staged for commit:",
	"  modified:   internal/tui/render.go",
	"  deleted:    old/removed.go",
}, "\n")

// logFixture is a `git log --oneline` history plus one full-form entry.
var logFixture = strings.Join([]string{
	"a1b2c3d wire the example",
	"c0ffee1 second commit",
	"commit deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
	"Author: A U Thor <author@example.invalid>",
	"Date:   Thu Jan 1 00:00:00 2026 +0000",
}, "\n")

// A listing is recognised, and the shapes that merely look like one are not.
func TestLooksLikeGitSeparatesListingsFromLogs(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"porcelain status", statusShortFixture, true},
		{"long-form status", statusLongFixture, true},
		{"oneline history", logFixture, true},
		{"a bare branch header over entries", "On branch main\n  modified:   a.go", true},
		// One shape is a shape an ordinary log line can have; two in one
		// result is a listing. The bar is the whole safety story.
		{"one hash row alone", "a1b2c3d printed the thing", false},
		{"one branch header alone", "On branch main", false},
		{"a server log", "2026/01/01 10:00:00 listening on :8080\n  - accepted connection", false},
		{"a table", "  name       value\n  alpha      1", false},
		{"a plain word list", "faced\ndecade\ncafe", false},
		{"a note that merely reads like a label", "note:   this is prose\nwarning: check it", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		if got := LooksLikeGit(c.text); got != c.want {
			t.Errorf("%s: LooksLikeGit = %v, want %v", c.name, got, c.want)
		}
	}
}

// Every row of a listing either carries a fact the painter inks or is one of
// the two evidence-only families. A row that is neither would mean the detector
// recognised a listing out of rows it cannot account for.
func TestEveryGitRowIsAccountedFor(t *testing.T) {
	for _, text := range []string{statusShortFixture, statusLongFixture, logFixture} {
		for _, row := range strings.Split(text, "\n") {
			if strings.TrimSpace(row) == "" {
				continue
			}
			if !gitEvidence(row) {
				t.Errorf("a row of a recognised listing matched nothing: %q", row)
			}
		}
	}
}

// The status row's two columns are its own alphabet, and a table row that
// merely starts with spaces is not a status.
func TestSplitStatusRowReadsTheStateColumns(t *testing.T) {
	cases := []struct {
		row        string
		state, res string
		ok         bool
	}{
		{" M internal/tui/render.go", " M", " internal/tui/render.go", true},
		{"?? docs/notes.md", "??", " docs/notes.md", true},
		{"A  cmd/xdev/new.go", "A ", " cmd/xdev/new.go", true},
		{"D  gone.go", "D ", " gone.go", true},
		{"MM both.go", "MM", " both.go", true},
		{"  name  value", "", "", false},      // two spaces: not a status
		{"XX weird.go", "", "", false},        // not git's alphabet
		{" M ", "", "", false},                // no path
		{"## main", "", "", false},            // the header, not a status row
		{"1 M. N... file.txt", "", "", false}, // porcelain v2 paints flat
	}
	for _, c := range cases {
		state, rest, ok := splitStatusRow(c.row)
		if ok != c.ok || state != c.state || rest != c.res {
			t.Errorf("splitStatusRow(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.row, state, rest, ok, c.state, c.res, c.ok)
		}
	}
}

// The long form's entry rows: git's own label list, an indent, and a path. A
// prose row that happens to read "note:" is not an entry.
func TestSplitStatusEntryNeedsAWholeLabel(t *testing.T) {
	cases := []struct {
		row                 string
		indent, label, path string
		ok                  bool
	}{
		{"  new file:   cmd/xdev/new.go", "  ", "new file:", "   cmd/xdev/new.go", true},
		{"  modified:   a.go", "  ", "modified:", "   a.go", true},
		{"\tdeleted:    old/removed.go", "\t", "deleted:", "    old/removed.go", true},
		{"  renamed:    a.go -> b.go", "  ", "renamed:", "    a.go -> b.go", true},
		{"  note:   this is prose", "", "", "", false},
		{"  modified:", "", "", "", false},      // a header, not an entry
		{"modified:   a.go", "", "", "", false}, // unindented is not the shape
		{"", "", "", "", false},
	}
	for _, c := range cases {
		indent, label, path, ok := splitStatusEntry(c.row)
		if ok != c.ok || indent != c.indent || label != c.label || path != c.path {
			t.Errorf("splitStatusEntry(%q) = (%q, %q, %q, %v), want (%q, %q, %q, %v)",
				c.row, indent, label, path, ok, c.indent, c.label, c.path, c.ok)
		}
	}
}

// A hash is a 7..40 character hex run at a word boundary; a short hex-looking
// word is an ordinary word, which is why the length is part of the shape.
func TestSplitLogRowNeedsAWholeHash(t *testing.T) {
	cases := []struct {
		row               string
		label, hash, rest string
		ok                bool
	}{
		{"a1b2c3d wire the example", "", "a1b2c3d", " wire the example", true},
		{"commit deadbeef1234 subject", "commit ", "deadbeef1234", " subject", true},
		{"deadbeef1234", "", "deadbeef1234", "", true},
		{"faced with a problem", "", "", "", false}, // 5 hex chars: a word
		{"decade", "", "", "", false},
		{"a1b2c3dx", "", "", "", false}, // a hash must end the word
		{"deadbeef1234xyz", "", "", "", false},
		{"", "", "", "", false},
	}
	for _, c := range cases {
		label, hash, rest, ok := splitLogRow(c.row)
		if ok != c.ok || label != c.label || hash != c.hash || rest != c.rest {
			t.Errorf("splitLogRow(%q) = (%q, %q, %q, %v), want (%q, %q, %q, %v)",
				c.row, label, hash, rest, ok, c.label, c.hash, c.rest, c.ok)
		}
	}
}

// A git listing captured through bash paints with more than one ink per row —
// the porcelain state, the branch and the path are each their own run — while a
// non-git bash log keeps the one-run body ink it had before.
func TestBashGitListingPaintsItsOwnInks(t *testing.T) {
	app, _ := newTestApp(t, 100, 40)
	app.AddToolBlock("c1", "bash", `{"command":"git status --short --branch"}`)
	app.FinishTool("c1", "bash", false, statusShortFixture, ToolOutcome{})

	if hl := app.toolOutputHL(app.blocks[len(app.blocks)-1]); !hl.git {
		t.Fatalf("a status listing was not recognised as git: %+v", hl)
	}
	rows, text := joinBox(t, app, app.blocks[len(app.blocks)-1], 96)
	for _, want := range strings.Split(statusShortFixture, "\n") {
		if !strings.Contains(text, want) {
			t.Fatalf("the listing changed bytes, %q is missing:\n%s", want, text)
		}
	}
	var head, porcelain bool
	for _, ln := range bodyRows(rows) {
		row := runsString(ln.runs)
		switch {
		case strings.HasPrefix(row, "## "):
			// The header: chrome words, then the branch in its own ink.
			if len(runStyles(ln)) < 2 {
				t.Fatalf("the branch header painted in one ink: %v", ln.runs)
			}
			head = true
		case strings.HasPrefix(row, " M "):
			if len(runStyles(ln)) < 2 {
				t.Fatalf("a porcelain row painted in one ink: %v", ln.runs)
			}
			porcelain = true
		}
	}
	if !head || !porcelain {
		t.Fatalf("the header (%v) or a porcelain row (%v) is not on screen:\n%s", head, porcelain, text)
	}
}

// The long form's entries are inked too: the label and the path are two runs.
func TestBashGitLongStatusPaintsItsEntries(t *testing.T) {
	app, _ := newTestApp(t, 100, 40)
	app.AddToolBlock("c1", "bash", `{"command":"git status"}`)
	app.FinishTool("c1", "bash", false, statusLongFixture, ToolOutcome{})

	rows, text := joinBox(t, app, app.blocks[len(app.blocks)-1], 96)
	seen := 0
	for _, ln := range bodyRows(rows) {
		row := runsString(ln.runs)
		if !strings.HasPrefix(row, "  modified:") && !strings.HasPrefix(row, "  new file:") {
			continue
		}
		seen++
		if len(runStyles(ln)) < 2 {
			t.Fatalf("a status entry painted in one ink: %v", ln.runs)
		}
	}
	if seen != 2 {
		t.Fatalf("expected both entries on screen, saw %d:\n%s", seen, text)
	}
}

// The history arm: the hash is the row's identifier and wears its own ink, and
// the "commit " label git printed is chrome.
func TestBashGitHistoryPaintsTheHash(t *testing.T) {
	app, _ := newTestApp(t, 100, 40)
	app.AddToolBlock("c1", "bash", `{"command":"git log"}`)
	app.FinishTool("c1", "bash", false, logFixture, ToolOutcome{})

	rows, text := joinBox(t, app, app.blocks[len(app.blocks)-1], 96)
	if !strings.Contains(text, "commit deadbeef") {
		t.Fatalf("the history changed bytes:\n%s", text)
	}
	seen := 0
	for _, ln := range bodyRows(rows) {
		row := runsString(ln.runs)
		if !strings.Contains(row, "deadbeef") && !strings.Contains(row, "a1b2c3d") {
			continue
		}
		seen++
		if len(runStyles(ln)) < 2 {
			t.Fatalf("a history row painted in one ink: %v", ln.runs)
		}
	}
	if seen != 2 {
		t.Fatalf("expected the oneline and the full-form hash rows, saw %d:\n%s", seen, text)
	}
}

// The fallback is the part that matters: a bash log that is not a git listing
// paints exactly as it painted before this file existed — one run, body ink.
func TestBashLogIsNotPaintedAsGit(t *testing.T) {
	app, _ := newTestApp(t, 100, 40)
	log := strings.Join([]string{
		"2026/01/01 10:00:00 listening on :8080",
		"  - accepted connection",
		"  - worker started",
		"  - ready",
	}, "\n")
	app.AddToolBlock("c1", "bash", `{"command":"run the server"}`)
	app.FinishTool("c1", "bash", false, log, ToolOutcome{})

	if hl := app.toolOutputHL(app.blocks[len(app.blocks)-1]); hl.git {
		t.Fatalf("a server log was painted as a git listing: %+v", hl)
	}
	rows, _ := joinBox(t, app, app.blocks[len(app.blocks)-1], 96)
	for _, ln := range bodyRows(rows) {
		for _, r := range ln.runs {
			if r.style != app.mdStyle().body {
				t.Fatalf("a log row wore a colour: %+v", r)
			}
		}
	}
}

// The evidence-only families paint flat even inside a listing: a section
// heading and a metadata line are evidence, not a fact of their own.
func TestGitEvidenceOnlyRowsStayFlat(t *testing.T) {
	app, _ := newTestApp(t, 100, 40)
	app.AddToolBlock("c1", "bash", `{"command":"git status"}`)
	app.FinishTool("c1", "bash", false, statusLongFixture, ToolOutcome{})

	rows, _ := joinBox(t, app, app.blocks[len(app.blocks)-1], 96)
	seen := 0
	for _, ln := range bodyRows(rows) {
		switch runsString(ln.runs) {
		case "Changes to be committed:", "Changes not staged for commit:":
			seen++
			if len(runStyles(ln)) != 1 {
				t.Fatalf("a section heading was coloured: %v", ln.runs)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("expected both headings on screen, saw %d", seen)
	}
}

// A git diff is still a diff: the diff renderer claims the block before this
// arm can, so the bands survive.
func TestBashGitDiffStillPaintsAsDiff(t *testing.T) {
	app, _ := newTestApp(t, 100, 40)
	diff := strings.Join([]string{
		"diff --git a/a.go b/a.go",
		"--- a/a.go",
		"+++ b/a.go",
		"@@ -1,3 +1,3 @@",
		"-old line",
		"+new line",
	}, "\n")
	app.AddToolBlock("c1", "bash", `{"command":"git diff"}`)
	app.FinishTool("c1", "bash", false, diff, ToolOutcome{})

	rows, _ := joinBox(t, app, app.blocks[len(app.blocks)-1], 96)
	bands := 0
	for _, ln := range bodyRows(rows) {
		if _, ok := banded(ln); ok {
			bands++
		}
	}
	if bands < 2 {
		t.Fatalf("the diff lost its bands: %d banded rows", bands)
	}
}

// NO_COLOR is the one global off switch: the git arm chooses no COLOUR at all,
// exactly as every other arm of toolhl.go. The one run that survives is the
// branch name, and it survives as an ATTRIBUTE — bold — the same exemption
// diffStyle documents: attributes carry no colour and are what keeps a diff
// readable without it. A run that is neither the body ink nor bold is the
// failure this test is for.
func TestNoColorPaintsGitFlat(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	app, _ := newTestApp(t, 100, 40)
	app.AddToolBlock("c1", "bash", `{"command":"git status"}`)
	app.FinishTool("c1", "bash", false, statusLongFixture, ToolOutcome{})

	body := app.mdStyle().body
	_, _, bodyAttrs := body.Decompose()
	rows := app.toolBoxLines(len(app.blocks)-1, app.blocks[len(app.blocks)-1], 96)
	for _, ln := range bodyRows(rows) {
		for _, r := range ln.runs {
			if r.style == body {
				continue
			}
			// An attribute-only run: no colour of its own beyond the body's.
			fg, bg, attrs := r.style.Decompose()
			bodyFg, bodyBg, _ := body.Decompose()
			if fg == bodyFg && bg == bodyBg && attrs == bodyAttrs|tcell.AttrBold {
				continue
			}
			t.Fatalf("NO_COLOR painted a coloured run %+v", r)
		}
	}
}

// The sample bound: a 2000-row result costs at most gitSampleRows rows of shape
// tests, so a huge log cannot cost a shape test per line.
func TestLooksLikeGitScanIsBounded(t *testing.T) {
	var b strings.Builder
	for range 200_000 {
		b.WriteString("2026/01/01 10:00:00 an ordinary log line\n")
	}
	if LooksLikeGit(b.String()) {
		t.Fatal("an ordinary log was painted as a git listing")
	}
}

// The inks are the palette's, not a second table: every run of a git row wears
// the style codeStyle answers for one of its kinds (or the body ink), and the
// bytes survive. The check is membership, not "is it coloured" — a slot the
// palette resolves to the body ink is a legal answer, and so is a run that only
// carries an attribute.
func TestGitRowInksComeFromTheSyntaxPalette(t *testing.T) {
	app, _ := newTestApp(t, 100, 40)
	cs := app.mdStyle().code
	body := app.mdStyle().body
	legal := map[tcell.Style]bool{body: true}
	for k := range tokPunct + 1 {
		legal[cs.styleFor(tokKind(k))] = true
	}
	rows := []string{
		" M a.go", "?? b.go",
		"  modified:   a.go",
		"a1b2c3d subject", "commit deadbeef1234 subject",
		"## main", "On branch main",
		"nothing to commit, working tree clean",
	}
	for _, src := range rows {
		ln := app.gitRow(src, cs, body, app.mdStyle().muted)
		if got := runsString(ln.runs); got != src {
			t.Fatalf("gitRow(%q) changed bytes to %q", src, got)
		}
		for _, r := range ln.runs {
			base := r.style
			if _, _, attrs := base.Decompose(); attrs != 0 {
				base = base.Attributes(0) // a bold fact is the same ink, louder
			}
			if !legal[base] {
				t.Fatalf("gitRow(%q) wore a style the palette did not resolve: %+v", src, r)
			}
		}
	}
}
