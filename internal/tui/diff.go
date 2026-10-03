package tui

// Diff colouring for tool results. A file change's unified diff reaches the
// renderer as plain text (Block.Diff) and is painted here in Claude Code's
// model (v2.1.287): a changed row is a BAND, the changed words inside a
// replaced pair ride a stronger band on top of it, and the +/- marker is the
// only coloured text on the row.
//
// The band is a flat tint of its own, not a tint of the polarity's ink, and
// the words wear the body's ink — so no ink is ever laid under itself. That is
// the whole difference from the model retired for "red on red": that one
// coloured a row's own text with a fixed RGB and then banded that text with
// the same ink beneath it, and the words drowned. Here the ink paints one
// cell. What the theme may NOT do is claim the whole panel: a diff lives
// inside the tool box's frame, and the frame, the rail and the margins stay
// the terminal's own. Bands ride the runs and are padded to the interior
// width (padBand), so a stripe stops at the border it is inside. A theme that
// names none of the four band slots keeps the terminal's own ANSI markers and
// paints no band — the old look, which is the correct one when the renderer
// cannot see the palette it is drawing into.
//
// The pass is stateless per row on purpose: the model-visible text may be
// head/tail trimmed, a diff split across a hidden middle has no partner to
// word-diff against, and a wrong "which words changed" guess is worse than
// none. The one exception is the adjacent -/+ pair, which is the diff a user
// actually reads — so pairing runs over the rows being rendered, never where
// the diff was attached, and a trim that separates the pair just loses the
// emphasis instead of painting a lie.

import (
	"os"
	"strings"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/mattn/go-runewidth"

	"github.com/FreePeak/xdev/internal/theme"
)

// diffCells renders a unified diff as styled rows for a box interior of
// width inner, wrapped to the same budget wrap() uses for plain bodies.
func (a *App) diffCells(text string, inner int) []line {
	return diffRowsCells(classifyMarkedDiff(text), a.diffStyle(), inner)
}

// classifyMarkedDiff is the whole per-diff analysis pass: label every source
// row, then pair the adjacent -/+ rows once. Splitting it from the paint lets
// the viewer keep the analysis and re-lay the same rows out for whichever
// shape (and width) the frame calls for, with no second word-diff.
func classifyMarkedDiff(text string) []diffRow {
	rows := classifyDiff(strings.Split(strings.TrimRight(text, "\n"), "\n"))
	markWordDiff(rows)
	return rows
}

// diffRowsCells paints already-classified rows into one column of inner width.
func diffRowsCells(rows []diffRow, ds diffStyle, inner int) []line {
	out := make([]line, 0, len(rows))
	for _, r := range rows {
		for _, ln := range wrapCells(ds.row(r), inner) {
			out = append(out, padBand(ln, inner))
		}
	}
	return out
}

// padBand fills a banded row out to the interior width so its stripe runs the
// full width of the box instead of stopping at the last glyph. The pad is
// chrome: painted, never copied (the box's own pad cell behaves this way), and
// it is banded rows only — an unbanded row keeps the terminal's background to
// its right edge, which is what the overlay's fill and the transcript both
// assume.
func padBand(ln line, inner int) line {
	bg, ok := banded(ln)
	if !ok {
		return ln
	}
	pad := inner - lineWidth(ln)
	if pad <= 0 {
		return ln
	}
	_, _, attrs := ln.runs[len(ln.runs)-1].style.Decompose()
	st := tcell.StyleDefault.Background(bg).Attributes(attrs)
	ln.runs = append(ln.runs, cell{text: strings.Repeat(" ", pad), style: st, chrome: true})
	return ln
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

// diffStyle is one diff's row palette: three chrome inks, plus a band per
// polarity. A change is read from a BACKGROUND, and the only coloured text on
// the row is the +/- marker, whose one cell says what the row is and has to
// stay legible whatever band it sits on.
//
// This is Claude Code's model (v2.1.287): addLine/addWord/deleteLine/deleteWord
// per polarity, the row's own words in the body ink. It is not the model that
// was retired for "red on red" — that one coloured the row TEXT from a fixed
// RGB and then laid a band tinted from the same ink under it, so the words
// drowned in their own colour. Here the ink paints one marker cell and the
// band is a flat tint of its own, so there is no ink for a band to bury.
type diffStyle struct {
	ctx, hunk, file tcell.Style
	add, del        diffBand
}

// diffBand is one polarity's three inks: the marker's foreground, the row's
// body foreground (the body ink — the band carries the claim, not the words),
// and the two backgrounds. wordBg 0 means the theme paints no band at all, and
// wordStyle falls back to the attribute that survives any palette.
type diffBand struct {
	mark, text tcell.Style
	bg, wordBg tcell.Color
}

// wordStyle is the changed run's style inside a -/+ pair: the word band when
// the theme named one, bold when it did not. It rides on the row's own base so
// a word band replaces the row band rather than stacking on it.
func (b diffBand) wordStyle(base tcell.Style) tcell.Style {
	if b.wordBg != tcell.ColorDefault {
		return base.Background(b.wordBg)
	}
	return base.Bold(true)
}

// diffInk is what a row falls back to when the theme leaves its slot to the
// terminal: the terminal's own two system colours for the markers — which is
// what `git diff` paints (32m/31m) and the one palette every emulator has
// tuned to its own scheme — and attributes for everything that is not a claim
// about the change. No band: a theme that names no diff band keeps the
// foreground-only look, which is exactly what this fallback is.
var diffInk = diffStyle{
	ctx:  tcell.StyleDefault.Dim(true), // unchanged: the terminal's own text
	hunk: tcell.StyleDefault.Italic(true),
	file: tcell.StyleDefault.Bold(true),
	add:  diffBand{mark: tcell.StyleDefault.Foreground(tcell.ColorGreen)},  // SGR 32
	del:  diffBand{mark: tcell.StyleDefault.Foreground(tcell.ColorMaroon)}, // SGR 31
}

// diffStyle reads the palette off the theme: a slot the theme names is
// honoured, one it leaves to the terminal keeps the terminal's own answer. The
// four band slots are optional (they are xdev's own, not part of omp's token
// contract), so an imported theme that has never heard of them renders exactly
// as it did before — marker inks, no band, bold doing the word emphasis.
//
// NO_COLOR is read HERE rather than left to tcell, for the reason
// codeStyleFor states: tcell drops colour at emission, so a run that picked an
// ink would still LOOK painted in a cell dump and in any tool that reads the
// grid, while the terminal shows one ink. Attributes carry no colour and
// survive, so the diff stays readable without it.
func (a *App) diffStyle() diffStyle {
	ds := diffInk
	if os.Getenv("NO_COLOR") != "" {
		// Attribute-only: the change still says what it is, in bold/italic/dim
		// and in the terminal's own text ink. Nothing the renderer would have
		// chosen for itself.
		ds.add.mark, ds.add.text = tcell.StyleDefault, tcell.StyleDefault
		ds.del.mark, ds.del.text = tcell.StyleDefault, tcell.StyleDefault
		ds.ctx, ds.hunk, ds.file = tcell.StyleDefault, tcell.StyleDefault, tcell.StyleDefault
		return ds
	}
	paint := func(slot string, base tcell.Style) tcell.Style {
		if c, ok := a.th.Slot(slot); ok {
			return base.Foreground(a.cellColor(c))
		}
		return base
	}
	body := paint(theme.Text, tcell.StyleDefault)
	band := func(b *diffBand, inkSlot, bg, wordBg string) {
		b.mark = paint(inkSlot, b.mark)
		b.text = body
		if c, ok := a.th.Slot(bg); ok {
			b.bg = a.cellColor(c)
		}
		if c, ok := a.th.Slot(wordBg); ok {
			b.wordBg = a.cellColor(c)
		}
	}
	band(&ds.add, theme.ToolDiffAdded, theme.ToolDiffAddedBg, theme.ToolDiffAddedWordBg)
	band(&ds.del, theme.ToolDiffRemoved, theme.ToolDiffRemovedBg, theme.ToolDiffRemovedWordBg)
	ds.ctx = paint(theme.ToolDiffContext, ds.ctx)
	if c, ok := a.th.Slot(theme.Gray); ok {
		ds.hunk = ds.hunk.Foreground(a.cellColor(c))
	}
	if c, ok := a.th.Slot(theme.TextSecondary); ok {
		ds.file = ds.file.Foreground(a.cellColor(c))
	}
	return ds
}

// row renders one diff row. A changed row is a band: the marker cell wears the
// polarity's ink, the rest of the row is body text on the row band, and the
// runs wordPair found ride a stronger band (bold where the theme named none).
// A row that is not a change keeps the foreground-only chrome inks and no band.
func (ds diffStyle) row(r diffRow) line {
	switch r.kind {
	case diffHunk:
		return textline(r.text, ds.hunk)
	case diffFile:
		return textline(r.text, ds.file)
	case diffAdded:
		return ds.bandRow(r, ds.add)
	case diffRemoved:
		return ds.bandRow(r, ds.del)
	}
	return textline(r.text, ds.ctx)
}

func (ds diffStyle) bandRow(r diffRow, b diffBand) line {
	// The band rides on the RUNS, not on line.bg: line.bg paints the whole
	// terminal row — rail, margins and the tool box's own frame — which would
	// run the band out through borders it is not inside. A diff lives in that
	// frame's interior, so diffCells pads a banded row out to the interior
	// width and the stripe stops at the frame.
	// The marker is its own run: it says what the row is, and it is the one
	// cell that has to stay legible on the band, so it takes the polarity's
	// ink rather than the body ink the words are painted in.
	// The row band goes on EVERY run of the row, the marker included: a band
	// that stops under the glyphs is not a band. The marker keeps its own ink
	// and the words keep the body ink — only the background is shared.
	row, mark := b.text, b.mark
	if b.bg != tcell.ColorDefault {
		row, mark = row.Background(b.bg), mark.Background(b.bg)
	}
	ln := line{runs: []cell{{text: r.text[:1], style: mark}}}
	push := func(text string, st tcell.Style) {
		if text != "" {
			ln.runs = append(ln.runs, cell{text: text, style: st})
		}
	}
	if len(r.segs) == 0 {
		push(r.text[1:], row)
		return ln
	}
	word := b.wordStyle(row)
	off := 1
	for _, sg := range r.segs {
		o, c := sg.o+1, sg.c+1
		push(r.text[off:o], row)
		push(r.text[o:c], word)
		off = c
	}
	push(r.text[off:], row)
	return ln
}

// banded reports the background a row paints, 0 when it paints none. The pad
// needs it: only a banded row is filled out to the interior width.
func banded(ln line) (tcell.Color, bool) {
	for _, r := range ln.runs {
		if _, bg, _ := r.style.Decompose(); bg != tcell.ColorDefault {
			return bg, true
		}
	}
	return tcell.ColorDefault, false
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
