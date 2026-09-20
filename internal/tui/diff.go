package tui

// Diff colouring for tool results. A file change's unified diff reaches the
// renderer as plain text (Block.Diff) and is painted here: added/removed/
// context rows take an ink, and the changed words inside a replaced pair are
// bolded so a one-token edit reads as that token rather than as two whole
// lines of noise.
//
// The ink defaults to the terminal's own ANSI palette and no row ever paints
// a background: xdev cannot learn the emulator's colours, so a fixed RGB it
// chooses is free to land on the user's red or green — and a band tinted from
// such an ink buries that ink under itself, which is how red came to sit on
// red. A theme may still name tool_diff_* explicitly (a custom palette, or
// color-blind mode), and that override is honoured; what no theme may do is
// make a row's background someone else's foreground.
//
// The pass is stateless per row on purpose: the model-visible text may be
// head/tail trimmed, a diff split across a hidden middle has no partner to
// word-diff against, and a wrong "which words changed" guess is worse than
// none. The one exception is the adjacent -/+ pair, which is the diff a user
// actually reads — so pairing runs over the rows being rendered, never where
// the diff was attached, and a trim that separates the pair just loses the
// emphasis instead of painting a lie.

import (
	"strings"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/mattn/go-runewidth"

	"github.com/FreePeak/xdev/internal/theme"
)

// diffCells renders a unified diff as styled rows for a box interior of
// width inner, wrapped to the same budget wrap() uses for plain bodies.
func (a *App) diffCells(text string, inner int) []line {
	ds := a.diffStyle()
	src := strings.Split(strings.TrimRight(text, "\n"), "\n")
	rows := classifyDiff(src)
	markWordDiff(rows)

	out := make([]line, 0, len(rows))
	for _, r := range rows {
		out = append(out, wrapCells(ds.row(r), inner)...)
	}
	return out
}

// diff kinds, in the order a unified-diff row falls into them.
const (
	diffCtx = iota
	diffAdded
	diffRemoved
	diffHunk
	diffFile
)

// classifyDiff labels each line of a unified diff. Before the first hunk
// header, +/- rows are the file pair (git's ---/+++ and its /dev/null form)
// and are painted as headers; after it, every row is content — so a removed
// line whose own text starts with "-- " stays a removal, not a header.
func classifyDiff(src []string) []diffRow {
	rows := make([]diffRow, 0, len(src))
	inHunk := false
	for _, ln := range src {
		k := diffCtx
		switch {
		case strings.HasPrefix(ln, "@@"):
			inHunk, k = true, diffHunk
		case !inHunk && strings.HasPrefix(ln, "++"):
			k = diffFile
		case !inHunk && strings.HasPrefix(ln, "--"):
			k = diffFile
		case strings.HasPrefix(ln, "+"):
			k = diffAdded
		case strings.HasPrefix(ln, "-"):
			k = diffRemoved
		}
		rows = append(rows, diffRow{kind: k, text: ln})
	}
	return rows
}

// DiffLooksUnified reports whether text reads as a unified diff: a hunk
// header plus at least one changed row. It gates bash output, where the text
// IS the displayed body, so a command that merely printed a line starting
// with "-" (a list, a table, a date) is never re-painted as a diff.
func DiffLooksUnified(text string) bool {
	inHunk := false
	for _, r := range classifyDiff(strings.Split(text, "\n")) {
		if r.kind == diffHunk {
			inHunk = true
		} else if inHunk && (r.kind == diffAdded || r.kind == diffRemoved) {
			return true
		}
	}
	return false
}

// diffRow is one source line plus what the render pass learned about it.
type diffRow struct {
	kind int
	text string
	segs []diffSeg
}

// diffSeg is a byte range of the row that differs from its partner.
type diffSeg struct{ o, c int }

// markWordDiff pairs adjacent -/+ rows and records the changed runs on each.
// A row without a partner in the very next position keeps no marks: the eye
// does not pair a deletion with an insertion five rows away.
func markWordDiff(rows []diffRow) {
	for i := 0; i+1 < len(rows); i++ {
		rem, add := &rows[i], &rows[i+1]
		if rem.kind == diffAdded && add.kind == diffRemoved {
			rem, add = add, rem
		}
		if rem.kind != diffRemoved || add.kind != diffAdded {
			continue
		}
		wordPair(rem, add)
		i++
	}
}

// wordPair marks the runs that differ between one removed row and its added
// partner. The +/- markers are excluded so a band starts at the content.
func wordPair(rem, add *diffRow) {
	a, b := rem.text[1:], add.text[1:]
	ra, rb := wordRanges(a), wordRanges(b)
	keep := lcsKeep(ra, a, rb, b)
	rem.segs = changedRuns(a, ra, keep[0])
	add.segs = changedRuns(b, rb, keep[1])
}

// wordRanges splits text into token spans: a run of letters/digits/underscore
// is one unit — so renaming a variable lights the whole name — and each
// piece of whitespace or punctuation is its own.
func wordRanges(text string) [][2]int {
	var out [][2]int
	for i := 0; i < len(text); {
		j := i
		switch {
		case text[i] == ' ' || text[i] == '\t':
			for j < len(text) && (text[j] == ' ' || text[j] == '\t') {
				j++
			}
		case isWordByte(text[i]):
			for j < len(text) && isWordByte(text[j]) {
				j++
			}
		default:
			for j < len(text) && !isWordByte(text[j]) && text[j] != ' ' && text[j] != '\t' {
				j++
			}
			if j == i {
				j = i + 1 // a stray byte (mid-rune) still advances
			}
		}
		out = append(out, [2]int{i, j})
		i = j
	}
	return out
}

func isWordByte(c byte) bool {
	return c == '_' || ('0' <= c && c <= '9') || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || c >= 0x80
}

// lcsKeep returns, per side, which tokens the longest common subsequence of
// the two token lists matched. Bytes are compared, so a match is the same
// token on both sides. The token-product is capped: past it a line counts as
// fully changed, which costs a wide band and no accuracy.
func lcsKeep(ra [][2]int, a string, rb [][2]int, b string) [2][]bool {
	keep := [2][]bool{make([]bool, len(ra)), make([]bool, len(rb))}
	if len(ra)*len(rb) > wordBudget {
		return keep
	}
	n, m := len(ra), len(rb)
	dp := make([]int, (n+1)*(m+1))
	at := func(i, j int) int { return dp[i*(m+1)+j] }
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[ra[i][0]:ra[i][1]] == b[rb[j][0]:rb[j][1]] {
				dp[i*(m+1)+j] = at(i+1, j+1) + 1
			} else {
				dp[i*(m+1)+j] = max(at(i+1, j), at(i, j+1))
			}
		}
	}
	for i, j := 0, 0; i < n && j < m; {
		switch {
		case a[ra[i][0]:ra[i][1]] == b[rb[j][0]:rb[j][1]]:
			keep[0][i], keep[1][j] = true, true
			i, j = i+1, j+1
		case at(i+1, j) >= at(i, j+1):
			i++
		default:
			j++
		}
	}
	return keep
}

// wordBudget is the LCS cell cap on one row pair: past it every token counts
// as changed, which paints the row's content as one band instead of guessing.
// A code line is tens of tokens, so real edits stay inside it; the cap exists
// because a render may pair hundreds of rows and a paste of a megabyte is
// thousands of tokens, squared. ponytail: if word bands ever look too wide on
// long rewritten lines, raise it and budget the total per render instead.
const wordBudget = 16_000

// changedRuns turns "which tokens matched" into the byte ranges that did not.
func changedRuns(text string, ranges [][2]int, keep []bool) []diffSeg {
	var out []diffSeg
	start := -1
	for i, r := range ranges {
		if !keep[i] {
			if start < 0 {
				start = r[0]
			}
			continue
		}
		if start >= 0 {
			out = append(out, diffSeg{start, r[0]})
			start = -1
		}
	}
	if start >= 0 {
		out = append(out, diffSeg{start, len(text)})
	}
	return out
}

// diffStyle is one diff's row palette: five foregrounds, no backgrounds.
type diffStyle struct {
	ctx, hunk, file, added, removed tcell.Style
}

// diffInk is what a row falls back to when the theme leaves its slot to the
// terminal: two of the terminal's own sixteen system colours for the change,
// plus attributes for everything that is not a claim about the change.
//
// The fallback is the default because xdev cannot learn the emulator's
// palette: a fixed RGB it picks itself is free to land on the user's red or
// green, and a band tinted from such an ink then buries that ink under itself —
// which is how red came to sit on red. The system colours are the one palette a
// terminal is guaranteed to have tuned to its own scheme, which is what
// `git diff` paints (31m/32m) and what a user who cannot tell the two apart has
// already reached, in the emulator's own settings. Attributes — bold, dim,
// italic — claim no colour at all and clash with nothing.
var diffInk = diffStyle{
	ctx:     tcell.StyleDefault.Dim(true), // unchanged: the terminal's own text
	hunk:    tcell.StyleDefault.Italic(true),
	file:    tcell.StyleDefault.Bold(true),
	added:   tcell.StyleDefault.Foreground(tcell.ColorGreen),  // SGR 32, as `git diff`
	removed: tcell.StyleDefault.Foreground(tcell.ColorMaroon), // SGR 31
}

// diffStyle reads the palette off the theme: a theme that names a diff ink
// (a custom palette, or color-blind mode) is honoured, and one that leaves a
// slot to the terminal keeps the terminal's own answer. Only the three
// statement-bearing rows are colourable; the hunk header and the file pair are
// chrome, so the theme's grays tint them and nothing paints a background.
func (a *App) diffStyle() diffStyle {
	ds := diffInk
	ink := func(slot string, base tcell.Style) tcell.Style {
		if c, ok := a.th.Slot(slot); ok {
			return base.Foreground(a.cellColor(c))
		}
		return base
	}
	ds.added = ink(theme.ToolDiffAdded, ds.added)
	ds.removed = ink(theme.ToolDiffRemoved, ds.removed)
	ds.ctx = ink(theme.ToolDiffContext, ds.ctx)
	if c, ok := a.th.Slot(theme.Gray); ok {
		ds.hunk = ds.hunk.Foreground(a.cellColor(c))
	}
	if c, ok := a.th.Slot(theme.TextSecondary); ok {
		ds.file = ds.file.Foreground(a.cellColor(c))
	}
	return ds
}

// row renders one diff row: the whole row in its kind's colour, with the
// changed runs bolded. The +/- marker keeps the row colour — it says what the
// row is, not that it changed — so the offsets, which wordPair counted from
// the content, shift right by it here.
func (ds diffStyle) row(r diffRow) line {
	base := ds.ctx
	switch r.kind {
	case diffHunk:
		return textline(r.text, ds.hunk)
	case diffFile:
		return textline(r.text, ds.file)
	case diffAdded:
		base = ds.added
	case diffRemoved:
		base = ds.removed
	}
	if len(r.segs) == 0 {
		return textline(r.text, base)
	}
	// The changed word is lifted with bold, not a background: the emphasis
	// has to survive a palette the renderer cannot see.
	ln := line{runs: []cell{{text: r.text[:1], style: base}}}
	off := 1
	for _, sg := range r.segs {
		o, c := sg.o+1, sg.c+1
		if o > off {
			ln.runs = append(ln.runs, cell{text: r.text[off:o], style: base})
		}
		ln.runs = append(ln.runs, cell{text: r.text[o:c], style: base.Bold(true)})
		off = c
	}
	if off < len(r.text) {
		ln.runs = append(ln.runs, cell{text: r.text[off:], style: base})
	}
	return ln
}

// wrapCells is wrap() for a styled row: the same column budget, but every
// piece keeps its style so a broken tail stays in its row's colour.
// Continuation rows start at column 0 — a diff row's leading marker says what
// kind of row it is, and indenting it would break the alignment the eye
// tracks down the box. A soft break keeps the space with the word it ended.
func wrapCells(ln line, maxW int) []line {
	if maxW <= 0 || lineWidth(ln) <= maxW {
		return []line{ln}
	}
	var out []line
	cur := line{}
	flush := func() {
		if len(cur.runs) > 0 {
			out = append(out, cur)
		}
		cur = line{}
	}
	for _, r := range ln.runs {
		for width(r.text) > maxW-lineWidth(cur) {
			// Advance rune-by-rune to find the byte index
			// where display width reaches the budget — always
			// at a rune boundary. Byte slicing splits multi-
			// byte UTF-8 (CJK, Thai, emoji) into garbled
			// fragments (shared root cause with wrap()).
			budget := maxW - lineWidth(cur)
			cut := 0
			w := 0
			for _, rr := range r.text {
				rw := runewidth.RuneWidth(rr)
				if w+rw > budget {
					break
				}
				w += rw
				cut += utf8.RuneLen(rr)
			}
			if cut == 0 {
				rr := []rune(r.text)[0]
				cut = utf8.RuneLen(rr)
			}
			if sp := strings.LastIndexAny(r.text[:cut], " \t"); sp > 0 {
				cut = sp + 1
			}
			cur.runs = append(cur.runs, cell{text: r.text[:cut], style: r.style})
			r.text = strings.TrimLeft(r.text[cut:], " ")
			flush()
			if r.text == "" {
				break
			}
		}
		if r.text != "" {
			cur.runs = append(cur.runs, r)
		}
	}
	flush()
	if len(out) == 0 {
		out = append(out, line{})
	}
	return out
}

// lineWidth is the visible cell count of a styled row.
func lineWidth(ln line) int {
	n := 0
	for _, r := range ln.runs {
		n += width(r.text)
	}
	return n
}
