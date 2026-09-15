package tui

// Diff colouring for tool results. A file change's unified diff reaches the
// renderer as plain text (Block.Diff) and is painted here: added/removed/
// context rows take the theme's diff slots, and the changed words inside a
// replaced pair are lifted above their row so a one-token edit reads as that
// token rather than as two whole lines of noise.
//
// The pass is stateless per row on purpose: the model-visible text may be
// head/tail trimmed, a diff split across a hidden middle has no partner to
// word-diff against, and a wrong "which words changed" guess is worse than
// none. The one exception is the adjacent -/+ pair, which is the diff a user
// actually reads — so pairing runs over the rows being rendered, never where
// the diff was attached, and a trim that separates the pair just loses the
// band instead of painting a lie.

import (
	"strings"

	"github.com/gdamore/tcell/v2"

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

// diffStyle is the row palette for the live theme.
type diffStyle struct {
	ctx, hunk, file, added, removed tcell.Style
	bandAdd, bandRem                tcell.Style
}

func (a *App) diffStyle() diffStyle {
	ink := func(slot string) tcell.Color { return a.cellColor(a.th.Get(slot)) }
	ds := diffStyle{
		ctx:     tcell.StyleDefault.Foreground(ink(theme.ToolDiffContext)),
		hunk:    tcell.StyleDefault.Foreground(ink(theme.Gray)).Italic(true),
		file:    tcell.StyleDefault.Foreground(ink(theme.TextSecondary)).Bold(true),
		added:   tcell.StyleDefault.Foreground(ink(theme.ToolDiffAdded)),
		removed: tcell.StyleDefault.Foreground(ink(theme.ToolDiffRemoved)),
	}
	// The band tints the row's background a shade off its ink. The box behind
	// it is the terminal default, so the mix moves toward the theme's text
	// colour — darker on a dark terminal, lighter on a light one, and visible
	// either way, which mixing toward black or white would not be.
	ds.bandAdd = ds.added.Background(a.bandInk(ds.added))
	ds.bandRem = ds.removed.Background(a.bandInk(ds.removed))
	return ds
}

// bandInk mixes a diff ink 40% toward the theme's text colour.
func (a *App) bandInk(st tcell.Style) tcell.Color {
	fg, _, _ := st.Decompose()
	tx, _, _ := tcell.Style{}.Foreground(a.cellColor(a.th.Get(theme.TextPrimary))).Decompose()
	r, g, b := fg.RGB()
	tr, tg, tb := tx.RGB()
	mix := func(c, toward int32) int32 { return c + (toward-c)*40/100 }
	return tcell.NewRGBColor(mix(r, tr), mix(g, tg), mix(b, tb))
}

// row renders one diff row: the whole row in its kind's colour, with the
// changed runs lifted onto their band. The +/- marker keeps the row colour —
// it says what the row is, not that it changed — so the band offsets, which
// wordPair counted from the content, shift right by it here.
func (ds diffStyle) row(r diffRow) line {
	base, band := ds.ctx, ds.ctx
	switch r.kind {
	case diffHunk:
		return textline(r.text, ds.hunk)
	case diffFile:
		return textline(r.text, ds.file)
	case diffAdded:
		base, band = ds.added, ds.bandAdd
	case diffRemoved:
		base, band = ds.removed, ds.bandRem
	}
	if len(r.segs) == 0 {
		return textline(r.text, base)
	}
	ln := line{runs: []cell{{text: r.text[:1], style: base}}}
	off := 1
	for _, sg := range r.segs {
		o, c := sg.o+1, sg.c+1
		if o > off {
			ln.runs = append(ln.runs, cell{text: r.text[off:o], style: base})
		}
		ln.runs = append(ln.runs, cell{text: r.text[o:c], style: band})
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
			cut := maxW - lineWidth(cur)
			for cut > 1 && width(r.text[:cut]) > cut {
				cut--
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
