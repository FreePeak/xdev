package tui

// Tool-output syntax highlighting: bash, read and edit/write results reach the
// transcript as ONE body-ink run, so a comment, a string, a number and a
// keyword inside a file preview or a build log all read the same. Fenced code
// blocks got the consumer for the 9 `syntax_*` theme slots in #501
// (highlight.go); this file is the consumer for the other surface those slots
// describe — the tool result box.
//
// Why the language is decided HERE and not in the tool: the renderer already
// holds the finished text, and a tool result carries its own path in its own
// header ("[internal/tool/read.go#ab12]") — the same format
// internal/tool/snapshot.go: RenderWindow writes. So the extension is ground
// truth, no new plumbing through tool.Result.Details, and a tool that never
// names a file simply gets the flat fallback.
//
// The constraint that shaped every decision here is the render cache.
// blockLines renders a block only when renderKey moves (rowindex.go), so:
//   - the cost of lexing is paid ONCE per settled block, not per frame;
//   - a live bash box re-renders on every streamed chunk (liveSeq is in the
//     key), which is bounded by the live tail window, not by output length;
//   - styling runs BEFORE wrapping, so one lex serves every row the source
//     line wraps into instead of a lex per painted fragment.
//
// The fallback is the part that matters most. Output that carries no language,
// an unknown extension, a theme that pins no syntax_* colour, or NO_COLOR all
// paint exactly as they painted before this file existed — one run, body ink.
// runsString(runs) is byte-identical to the source row in every case, so
// selection, copy and the transcript are unaffected.

import (
	"strings"

	"github.com/gdamore/tcell/v2"
)

// extLang maps a file extension to the lexer key used by codeLangs. It is the
// TUI's own table rather than a reuse of internal/lsp's builtinLanguage or
// internal/dap's extLanguage: those answer "which LSP language id", and this
// answers "which of the 29 lexers this build ships". The values are lexer keys,
// not extensions — .tsx maps to the typescript lexer, .h to c.
var extLang = map[string]string{
	".go": "go",
	".rs": "rust",
	".c":  "c", ".h": "c",
	".cc": "cpp", ".cpp": "cpp", ".cxx": "cpp", ".hpp": "cpp", ".hh": "cpp", ".hxx": "cpp",
	".java": "java",
	".js":   "javascript", ".jsx": "javascript", ".mjs": "javascript", ".cjs": "javascript",
	".ts": "typescript", ".tsx": "typescript", ".mts": "typescript", ".cts": "typescript",
	".cs":    "c#",
	".swift": "swift",
	".kt":    "kotlin", ".kts": "kotlin",
	".php":  "php",
	".dart": "dart",
	".zig":  "zig",
	".nim":  "nim", ".nims": "nim",
	".hs":  "haskell",
	".lua": "lua",
	".sql": "sql",
	".py":  "python", ".pyi": "python",
	".rb": "ruby", ".rake": "ruby", ".gemspec": "ruby",
	".sh": "shell", ".bash": "shell", ".zsh": "shell", ".ksh": "shell",
	".mk": "makefile", ".mak": "makefile",
	".dockerfile": "dockerfile",
	".yaml":       "yaml", ".yml": "yaml",
	".toml": "toml",
	".ini":  "ini", ".cfg": "ini",
	".graphql": "graphql", ".gql": "graphql",
	".proto":  "proto",
	".vue":    "vue",
	".svelte": "svelte",
}

// langForPath returns the lexer key for a file name, or "" when nothing claims
// it. The comparison is case-insensitive because a .GO or .Py is the same file
// to every other tool in the box, and because a model may name the path in
// whatever case it was given.
func langForPath(path string) string {
	i := strings.LastIndexByte(path, '.')
	if i < 0 {
		return ""
	}
	return extLang[strings.ToLower(path[i:])]
}

// langForToolOutput reads the language out of a finished tool result's text.
//
// A read/edit/write result opens with a snapshot header, "[path#TAG]"; an edit
// that MOVED a file prints "Moved old to new" above it. The path is inside the
// brackets and its extension is the whole of the answer. This is EXACT — the
// model named the file, so there is nothing to guess.
//
// A bash result has no header, so langForToolOutput returns "" for it and the
// caller runs LooksLikeShell instead. Returning "" here always means "paint it
// flat", never "paint it white".
func langForToolOutput(text string) string {
	head := text
	if i := strings.IndexByte(head, '\n'); i >= 0 {
		head = head[:i]
	}
	// "[path#TAG]" — the snapshot header. The tag is a content hash and a path
	// may itself contain '#', so the LAST '#' before the bracket is the split.
	if strings.HasPrefix(head, "[") && strings.HasSuffix(head, "]") {
		if j := strings.LastIndexByte(head, '#'); j > 0 {
			return langForPath(head[1:j])
		}
		return ""
	}
	// "Moved a/b.go to c/d.go": the DESTINATION is what the rows below show.
	if rest, ok := strings.CutPrefix(head, "Moved "); ok {
		if _, dst, found := strings.Cut(rest, " to "); found {
			return langForPath(strings.TrimSpace(dst))
		}
	}
	return ""
}

// shellSample is how much of a bash result LooksLikeShell reads.
const (
	shellSampleRows = 200 // a 200-row sample is already decisive
	shellMinRows    = 4   // too few rows to sample honestly
	shellMinHits    = 3   // rows that carried shell's own ink
)

// shellInkWords are the shell words that do not also turn up in ordinary
// prose. shellRowHasInk leans on this list instead of the lexer's token kinds,
// and the difference is the whole heuristic: the shell lexer marks EVERY bare
// word as a variable and marks punctuation as punctuation, so a file listing,
// a test-runner report or a server log clears any "did it lex as shell" test
// that asks merely whether tokens came out. A quoted argument and a control word
// with no English sense do not appear in a log line, so they do not either.
//
// set, exit, time, break, continue, select and shift are deliberately ABSENT
// from this list: they are ordinary words in an English log line, and treating
// them as evidence is how a build log ends up wearing shell colours.
var shellInkWords = map[string]bool{
	"fi": true, "esac": true, "elif": true, "then": true, "done": true,
	"declare": true, "local": true, "export": true, "readonly": true,
	"typeset": true, "source": true, "eval": true, "trap": true, "unalias": true,
}

// LooksLikeShell reports whether a bash result's text should be lexed as shell.
//
// Bash is the only tool result with no filename, so this is the one judgement
// call in the feature, and it is a THRESHOLD rather than a classifier. The test
// is not "does this look like shell" — a stack trace is not shell, and
// keyword-colouring it would be a lie — but "did enough rows already carry
// shell's own ink to say the command was printing shell".
//
// Two conditions, both cheap and both required:
//
//   - at least shellMinRows non-blank rows were sampled, and at least
//     shellMinHits of them carried a quoted argument or a shellInkWord. A
//     `go test` report, a `git log` and an application log clear none of those
//     and stay flat, exactly as they painted before this file existed;
//   - a short result never clears the bar. Three rows is little to gain and one
//     wrong colour reads as a bug, so small output stays flat.
//
// The scan stops at shellSampleRows and short-circuits once the bar is clear,
// so a large result costs a bounded number of single-line lexes.
func LooksLikeShell(text string) bool {
	sp, ok := codeLangs["shell"]
	if !ok {
		return false
	}
	rows, hits := 0, 0
	for line := range strings.SplitSeq(text, "\n") {
		if rows >= shellSampleRows {
			break
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue // a blank row carries no evidence either way
		}
		rows++
		if shellRowHasInk(trimmed, sp) {
			hits++
			if rows >= shellMinRows && hits >= shellMinHits {
				return true
			}
		}
	}
	return false
}

// shellRowHasInk reports whether one row carries shell's own ink: a quoted
// argument, or a control/declaration word from shellInkWords. The word is
// matched as a whole lexer token, never as a substring, so `exported` is not
// `export`.
func shellRowHasInk(s string, sp langSpec) bool {
	for _, t := range lexLine(s, sp) {
		if t.kind == tokString || (t.kind == tokKeyword && shellInkWords[t.text]) {
			return true
		}
	}
	return false
}

// toolHL is the highlighting decision for one finished tool result: which
// lexer paints it, and whether its rows carry the "N:" render-window prefix
// that snapshot.go: RenderWindow writes. report is the third option: the
// result is prose a tool WROTE for a person (a subagent's findings), so it
// paints through the markdown renderer rather than a lexer or a flat wrap.
type toolHL struct {
	lang     string
	numbered bool // rows read "<line>: <source>", so the prefix is chrome
	report   bool // prose the tool composed, not output it captured
}

// toolOutputHL decides how a finished tool result paints.
//
// Three answers, in this order:
//
//   - the path is the exact answer and always wins: a `read` of a .go file is
//     Go whatever the rows happen to contain;
//   - a REPORT: the result is prose a tool WROTE for a person rather than
//     output it captured, so it paints as markdown. Only the tools that spawn
//     a model say this (task — and every agent it runs, scout included, since
//     a subagent's answer comes back as its task call's result). Bash is the
//     one tool that falls to a heuristic, and ONLY bash — every other tool
//     result carrying no path paints flat, because guessing a language for an
//     unknown tool's output is how a log line ends up wearing a keyword colour.
//
// The zero value paints flat, and so does everything codeStyle cannot colour
// (NO_COLOR, a theme with no syntax_* slot filled). Nothing here can turn a
// row white by accident: every undecided answer is "exactly as it painted
// before this file existed".
func (a *App) toolOutputHL(b *Block) toolHL {
	if b == nil {
		return toolHL{}
	}
	// A failed block is highlighted too: the red frame and the footer already
	// say the command failed, and a compiler's own complaint is the one output
	// where telling its keywords from its paths is the whole point. A row that
	// lexes to nothing keeps the box's own ink, error ink included.
	if lang := langForToolOutput(b.Text); lang != "" {
		return toolHL{lang: lang, numbered: true}
	}
	if b.ToolName == taskToolName {
		return toolHL{report: true}
	}
	if b.ToolName == "bash" && LooksLikeShell(b.Text) {
		return toolHL{lang: shellLang}
	}
	return toolHL{}
}

// shellLang is the normalized lexer key for a bash result. normalizeLang runs
// once at package init rather than per row, so the lookup is one map read for
// the whole block instead of one per line.
var shellLang = normalizeLang("bash")

// toolBodyRows lays a result body out into styled rows of inner width.
//
// Three paths, and the split is the whole safety story:
//
//   - nothing to colour (no language, an unknown extension, NO_COLOR, a theme
//     that pins no syntax_* ink): wrap() and textline() exactly as this painted
//     before toolhl.go existed, so every existing row is byte-identical;
//   - a language: the same wrapCells the diff renderer uses, per styled source
//     line. Styling BEFORE wrapping is what keeps a long source line
//     consistently coloured — wrap first and a wrapped line's tail arrives as
//     its own row with no "N:" prefix and nothing to hang the lexer on;
//   - a report: renderMarkdown, the same renderer the assistant's own answer
//     goes through. It is the ONE path that changes the rows' text, because
//     markdown is a layout, not a colour: a "# " heading arrives without its
//     hashes, a bullet as "•". That is the point — a subagent that wrote a
//     structured report finally reads as one, with the md_* inks it was always
//     painted with in an assistant block.
func (a *App) toolBodyRows(body string, inner int, bodySt, dimSt tcell.Style, hl toolHL) []line {
	if hl.report {
		var out []line
		for _, ln := range a.renderMarkdown(body, inner) {
			out = append(out, wrapLine(ln, inner)...)
		}
		return out
	}
	if hl.lang == "" {
		var out []line
		for _, wl := range wrap(body, inner) {
			out = append(out, textline(wl, bodySt))
		}
		return out
	}
	cs := a.mdStyle().code
	var out []line
	for _, src := range strings.Split(body, "\n") {
		for _, ln := range wrapCells(a.toolBodyRow(src, hl, cs, bodySt, dimSt), inner) {
			out = append(out, ln)
		}
	}
	return out
}

// toolBodyRow colours one source line, or returns it in the box's own ink.
// runsString of the result is always src, on both paths — the row's bytes are
// what the tool produced, and highlighting may only change which ink a byte
// wears.
func (a *App) toolBodyRow(src string, hl toolHL, cs codeStyle, bodySt, dimSt tcell.Style) line {
	prefix := ""
	body := src
	if hl.numbered {
		var isSource bool
		prefix, body, isSource = splitRowNumber(src)
		if !isSource {
			return textline(src, bodySt)
		}
	}
	runs := a.highlightCode(body, hl.lang, cs)
	if runs == nil {
		return textline(src, bodySt)
	}
	if prefix != "" {
		return line{runs: append([]cell{{text: prefix, style: dimSt}}, runs...)}
	}
	return line{runs: runs}
}

// splitRowNumber splits a snapshot row's "<n>: " prefix from its source.
//
// isSource is false only for a row that is a REPORT about the file rather than
// part of it — the "[path#TAG]" header, a "…" marker, the "Moved x to y" line.
// Those carry no colon at all, which is the whole test; they stay flat, because
// lexing them would put shell keywords on the word "Showing".
//
// Everything else is source. That includes a line opening with digits and a
// colon: "12:30 finished the job" is a log line, not row 12, and treating it as
// source paints it right where treating it as a prefix would eat its first
// digit.
func splitRowNumber(src string) (prefix, body string, isSource bool) {
	i := strings.IndexByte(src, ':')
	if i < 0 {
		return "", src, false
	}
	for j := 0; j < i; j++ {
		if c := src[j]; c < '0' || c > '9' {
			return "", src, true // a colon that is not a row number: plain source
		}
	}
	// snapshot.go writes "N:" and then the line with no space, so digits then a
	// digit is far likelier to be a time or a version than a row number.
	if i+1 < len(src) && src[i+1] >= '0' && src[i+1] <= '9' {
		return "", src, true
	}
	return src[:i+1], src[i+1:], true
}
