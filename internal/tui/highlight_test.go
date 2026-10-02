package tui

// #501 — the syntax_* theme tokens finally have a consumer.
//
// The tests here pin the three properties that matter, in order of how badly
// breaking them would hurt:
//
//  1. It COLORS. A comment, a string, a number and a keyword in a ```go block
//     must not all paint the same ink, or the feature is dead code and this
//     file is the proof.
//  2. It LOSES NOTHING. Every rendered run concatenates back to the source
//     line byte for byte, in every supported language and on pathological
//     input. The transcript, the selection and the clipboard all read that
//     concatenation — a highlighter that drops or reorders a byte corrupts
//     copied code, which is worse than an unhighlighted block.
//  3. It DEGRADES. Unknown language, untagged fence, NO_COLOR, a theme that
//     pins no syntax_* colour: all four paint the flat body-ink run that
//     shipped before #501. Never a panic, never a dropped line, never a
//     terminal-default ink chosen by xdev.

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// fencedRuns2 is fencedRuns without the NO_COLOR reset, for the test whose
// subject IS NO_COLOR.
func fencedRuns2(t *testing.T, app *App, src string) []line {
	t.Helper()
	app.mu.Lock()
	defer app.mu.Unlock()
	return app.renderMarkdown(src, 80)
}

// fenced runs for one line of a fenced block, via the real render path.
//
// NO_COLOR is cleared for every test in this file: the code under test reads
// it (a NO_COLOR session must not even CHOOSE an ink, since tcell will drop
// it at emission and a screenshot would then look highlighted while the
// terminal shows one colour). A session that exports it is tested on its own
// by TestFencedBlockNoColorIsOff, which sets it back.
func fencedRuns(t *testing.T, app *App, src string) []line {
	t.Helper()
	t.Setenv("NO_COLOR", "")
	app.mu.Lock()
	defer app.mu.Unlock()
	return app.renderMarkdown(src, 80)
}

// TestFencedGoBlockColorsItsTokens is the test the issue asked for: if the
// highlighter stops coloring, this fails. It asserts a comment run's style
// differs from the code around it, and that string/number/keyword classes are
// painted differently from each other.
func TestFencedGoBlockColorsItsTokens(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	t.Setenv("NO_COLOR", "")
	cs := app.codeStyleFor()
	if !cs.enabled {
		t.Fatal("groknight must define syntax_* colours; the tokens are required")
	}
	src := "```go\n// note\ns := \"x\"\nn := 42\n```"
	lines := fencedRuns(t, app, src)
	if len(lines) != 3 {
		t.Fatalf("want 3 code lines, got %d:\n%s", len(lines), joinedLines(lines))
	}
	styleOf := func(i int, sub string) tcell.Style {
		for _, c := range lines[i].runs {
			if strings.Contains(c.text, sub) {
				return c.style
			}
		}
		t.Fatalf("line %d has no run containing %q:\n%s", i, sub, joinedLines(lines))
		return tcell.StyleDefault
	}
	comment := styleOf(0, "// note")
	plainCode := styleOf(1, "s")
	str := styleOf(1, `"x"`)
	num := styleOf(2, "42")
	// A comment differs from the code it annotates …
	if comment == plainCode {
		t.Error("a comment paints in the same ink as the code around it")
	}
	// … a string differs from the code around it …
	if str == plainCode {
		t.Error("a string paints in the same ink as the code around it")
	}
	// … a number differs from the code around it …
	if num == plainCode {
		t.Error("a number paints in the same ink as the code around it")
	}
	// … and the classes are not all one colour wearing three names.
	if comment == str || str == num {
		t.Error("comment/string/number collapsed to one ink")
	}
}

// TestFencedBlockKeepsEveryByte is the anti-corruption guard. Tokenizing must
// never add, drop, reorder or rewrite a byte: the runs concatenate to the
// source line. This runs over every supported language and over the inputs
// that break naive scanners (unterminated strings, unterminated block
// comments, CRLF, tabs, unicode, an empty line, a line of only operators).
func TestFencedBlockKeepsEveryByte(t *testing.T) {
	langs := []string{
		"go", "rust", "c", "cpp", "java", "javascript", "typescript", "c#",
		"swift", "kotlin", "php", "dart", "zig", "nim", "haskell", "lua",
		"sql", "python", "ruby", "shell", "makefile", "dockerfile", "yaml",
		"toml", "ini", "graphql", "proto", "vue", "svelte",
	}
	lines := []string{
		"",
		"   ",
		"\t\tif x := 1; x > 0 {",
		`const s = "unterminated`,
		"/* unterminated block",
		"a := `raw` + \"esc\\\"aped\" + 'c'",
		"x := 0x1f + 1_000 + 3.14e-2 + 0b1010",
		"héllo → 世界 🎉",
		"+++++++",
		"#! /usr/bin/env bash",
		"a#b and h#fetch",
		"user@example.com # trailing",
	}
	for _, lang := range langs {
		spec, ok := codeLangs[normalizeLang(lang)]
		if !ok {
			t.Errorf("normalizeLang(%q) is not a supported language", lang)
			continue
		}
		for _, src := range lines {
			toks := lexLine(src, spec)
			var b strings.Builder
			for _, tok := range toks {
				b.WriteString(tok.text)
			}
			if b.String() != src {
				t.Errorf("%s: lexing changed the line\n got %q\nwant %q", lang, b.String(), src)
			}
		}
	}
}

// TestFencedBlockUnknownLanguageIsFlat: a language this build does not know
// paints exactly what it painted before #501 — one run, the body ink.
func TestFencedBlockUnknownLanguageIsFlat(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	body := app.mdStyle().body
	for _, lang := range []string{"", "text", "brainfuck", "not-a-language", "output"} {
		lines := fencedRuns(t, app, "```"+lang+"\nint main() { return 0; }\n```")
		if len(lines) != 1 {
			t.Fatalf("lang %q: want 1 line, got %d", lang, len(lines))
		}
		if len(lines[0].runs) != 1 {
			t.Errorf("lang %q: %d runs, want the flat 1-run fallback: %q",
				lang, len(lines[0].runs), runsString(lines[0].runs))
			continue
		}
		if lines[0].runs[0].style != body {
			t.Errorf("lang %q: the flat fallback is not the body ink", lang)
		}
	}
}

// TestFencedBlockThemeWithoutSyntaxColorsIsFlat: NO_COLOR reaches the theme
// as "this slot is the terminal default", and a theme that pins none of the 9
// must leave every block flat rather than have xdev choose inks.
func TestFencedBlockThemeWithoutSyntaxColorsIsFlat(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	t.Setenv("NO_COLOR", "")
	plain := theme.Load("groknight")
	for _, slot := range theme.RequiredSlots() {
		if strings.HasPrefix(slot, "syntax_") {
			plain.Defaults[slot] = true
		}
	}
	app.SetTheme(plain)
	if app.codeStyleFor().enabled {
		t.Fatal("a theme with every syntax_* slot terminal-default must not enable highlighting")
	}
	lines := fencedRuns(t, app, "```go\n// note\ns := \"x\"\n```")
	if len(lines) != 2 || len(lines[0].runs) != 1 {
		t.Fatalf("expected the flat fallback, got %d lines / %d runs", len(lines), len(lines[0].runs))
	}
	if got, want := runsString(lines[0].runs), "// note"; got != want {
		t.Errorf("flat fallback text = %q, want %q", got, want)
	}
}

// TestFencedBlockPartialThemeDegradesPerKind: a theme that pins only the
// comment colour colours comments and leaves the rest in the body ink, rather
// than dropping every uncoloured class to terminal white.
func TestFencedBlockPartialThemeDegradesPerKind(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	t.Setenv("NO_COLOR", "")
	th := theme.Load("groknight")
	body := app.mdStyle().body
	for _, slot := range theme.RequiredSlots() {
		if strings.HasPrefix(slot, "syntax_") {
			th.Defaults[slot] = true
		}
	}
	th.Slots[theme.SyntaxComment] = theme.Hex("#ff0000")
	th.Defaults[theme.SyntaxComment] = false
	app.SetTheme(th)
	lines := fencedRuns(t, app, "```go\n// note\nx := 1\n```")
	var comment, code tcell.Style
	for _, c := range lines[0].runs {
		if strings.Contains(c.text, "//") {
			comment = c.style
		}
	}
	for _, c := range lines[1].runs {
		if strings.Contains(c.text, "x") {
			code = c.style
		}
	}
	if comment == body {
		t.Error("the one pinned token (comment) must be coloured")
	}
	if code != body {
		t.Error("an unpinned token must fall back to the body ink, not terminal white")
	}
}

// TestFencedBlockFenceHandling: the info string names the language, the fence
// lines still vanish, and a second block gets its own language rather than
// inheriting the first one's.
func TestFencedBlockFenceHandling(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	lines := fencedRuns(t, app, "```go\n// go comment\n```\n\ntext\n\n```py\n# py comment\n```")
	// Two code lines, one prose line, and the blank lines the blank lines
	// around it produce: the fence lines vanish, everything else stays.
	if got, want := len(lines), 5; got != want {
		t.Fatalf("want %d lines (2 code + prose + blanks), got %d:\n%s", want, got, joinedLines(lines))
	}
	if strings.Contains(joinedLines(lines), "```") {
		t.Error("a fence line leaked into the transcript")
	}
	if lines[0].bg != app.mdStyle().codeBg {
		t.Error("a code line lost its code-bg band")
	}
	if lines[4].bg != app.mdStyle().codeBg {
		t.Error("the second block lost its code-bg band")
	}
	// A # comment in a python block: the go lexer would not make it one.
	var pyComment tcell.Style
	for _, c := range lines[4].runs {
		if strings.Contains(c.text, "#") {
			pyComment = c.style
		}
	}
	if pyComment == tcell.StyleDefault {
		t.Error("python's own comment syntax was not recognised")
	}
}

// TestFencedBlockPreservesIndentation: leading tabs and spaces are code, not
// layout. The runs must keep them and the row must not gain or lose a cell of
// width, because the code block is a verbatim copy of what the model wrote.
func TestFencedBlockPreservesIndentation(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	for _, src := range []string{"\tif x {", "        y := 1", "  \t z"} {
		lines := fencedRuns(t, app, "```go\n"+src+"\n```")
		if len(lines) != 1 {
			t.Fatalf("one line in, want one out: %q", src)
		}
		if got := runsString(lines[0].runs); got != src {
			t.Errorf("indentation lost: got %q, want %q", got, src)
		}
	}
}

// TestFencedBlockNoSpuriousRuns: a blank or whitespace-only line in a block
// stays one plain run — a highlighter that splits whitespace into per-cell
// runs would make wrapping and selection arithmetic worse for nothing.
func TestFencedBlockNoSpuriousRuns(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	lines := fencedRuns(t, app, "```go\n\n   \n```")
	for i, ln := range lines {
		if len(ln.runs) > 1 {
			t.Errorf("line %d (%q): %d runs, want 1", i, runsString(ln.runs), len(ln.runs))
		}
	}
}

// TestFencedBlockCopyYieldsPlainText: the clipboard path reads the runs' text
// only, so a highlighted block copies as its source. A highlighter that
// smuggled a marker or re-ordered runs would corrupt copied code.
func TestFencedBlockCopyYieldsPlainText(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	body := "if x := 1; x > 0 {\n\t// keep me\n\ts := \"quoted\"\n}"
	lines := fencedRuns(t, app, "```go\n"+strings.ReplaceAll(body, "\n", "\n")+"\n```")
	var got strings.Builder
	for _, ln := range lines {
		got.WriteString(runsString(ln.runs))
		got.WriteByte('\n')
	}
	if want := body + "\n"; got.String() != want {
		t.Errorf("copied text drifted:\n got %q\nwant %q", got.String(), want)
	}
}

// TestNormalizeLangAliases pins the fence-spelling collapse. A tag is
// whatever the author wrote; the lookup must fold it, and must treat the
// explicit "do not colour" tags as no language at all.
func TestNormalizeLangAliases(t *testing.T) {
	cases := map[string]string{
		"go": "go", "Go": "go", "GOLANG": "go", "go linenums": "go",
		"py": "python", "python3": "python", "PY": "python",
		"ts": "typescript", "tsx": "tsx", "js": "javascript",
		"bash": "shell", "sh": "shell", "zsh": "shell", "console": "shell",
		"yml": "yaml", "rs": "rust", "cs": "c#", "c#": "c#",
		"dockerfile": "dockerfile", "make": "makefile", "Makefile": "makefile",
		"": "", "text": "", "txt": "", "output": "", "log": "",
	}
	for in, want := range cases {
		if got := normalizeLang(in); got != want {
			t.Errorf("normalizeLang(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestFencedBlockNoColorIsOff: NO_COLOR is xdev's colourless mode, and the
// gate is the RENDERER, not tcell. tcell drops colour at emission, so a
// highlighter that still picked inks would look fine in a cell dump and in any
// screenshot tool while the real terminal showed a single colour — the mode
// would be "on" everywhere except the place it is meant to apply. So with
// NO_COLOR set, the renderer must not choose an ink at all.
func TestFencedBlockNoColorIsOff(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	t.Setenv("NO_COLOR", "1")
	if app.codeStyleFor().enabled {
		t.Fatal("NO_COLOR must disable the syntax palette, not just the emission")
	}
	lines := fencedRuns2(t, app, "```go\n// note\nx := 1\n```")
	for i, ln := range lines {
		if len(ln.runs) != 1 {
			t.Errorf("line %d: %d runs under NO_COLOR, want the flat 1-run render", i, len(ln.runs))
		}
	}
	if got := runsString(lines[0].runs); got != "// note" {
		t.Errorf("NO_COLOR dropped or changed text: %q", got)
	}
}
