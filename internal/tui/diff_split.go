package tui

// Split view for the fullscreen diff viewer: the same rows the unified viewer
// paints, laid out side by side, which is the layout a GitHub PR uses and the
// only one where a replaced pair reads as a pair.
//
// Two columns need numbers, because neither side can be read off the other
// any more once they are beside each other, and the unified view's +/- markers
// are consumed by the band. So each side gets a gutter counting that side's own
// file, and the two counters advance independently — which is why a pair of a
// deleted line and an inserted line reads "12 | 13" and not "12 | 12".
//
// The choice is the width, not a preference: at 120 columns and up the pair
// reads without wrapping, below it each side is a column of ellipses and the
// unified list is the better reader. `s` overrides it either way, so a human
// who wants the pair at 90 columns is not arguing with the renderer.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// diffSplitMinCols is the interior width at which the pair stops wrapping.
// Below it each half is too narrow for a code line, and a half-elided column
// is worse than the unified list it replaced.
const diffSplitMinCols = 120

// diffNumCap bounds the gutter's digits. Four covers any file a human reads;
// past it the gutter stops growing and the pair keeps its width, because a
// five-digit counter costs the reader two more cells of every line to say
// something about a line nobody will scroll to.
const diffNumCap = 4

// diffSide is one row's source line plus the number that line has in its own
// file. A side with no line on this row (the right half of a pure deletion)
// carries no number, and paints a blank gutter cell instead — so the divider
// column never moves, which is what makes a pair of columns read as a pair.
type diffSide struct {
	row   diffRow
	num   int
	hasNo bool
}

// splitFits reports whether a viewer of inner columns shows the pair. Two
// gates, both about reading rather than preference: the width must be there,
// and the longest CONTENT row in THIS diff must fit a half-column.
//
// The gate skips the hunk header and the file pair, because neither is a
// column's worth of content: the header spans the whole interior and the
// file pair is the overlay's own title. Measuring them would gate on a row
// that is never split, and a diff whose widest line is a Go function
// signature in its header would then never get the pair at any width.
func splitFits(inner int, rows []diffRow) bool {
	if inner < diffSplitMinCols {
		return false
	}
	half := (inner-1)/2 - diffNumCap - 1
	for _, r := range rows {
		if r.kind == diffHunk || r.kind == diffFile {
			continue
		}
		if width(r.text) > half {
			return false
		}
	}
	return true
}

// diffNumCells is the gutter width for this diff: the widest line number its
// hunk headers declare, so the gutter is one column wide for the whole diff
// rather than growing and re-flowing the pair as the numbers get bigger. The
// header's SECOND number is the file's end line, so it bounds every number
// below it — a diff whose last hunk ends at line 120 needs three digits even
// though the first rows are single-digit.
func diffNumCells(rows []diffRow) int {
	widest := 0
	for _, r := range rows {
		if r.kind != diffHunk {
			continue
		}
		oldStart, newStart := hunkCounts(r.text)
		// start + count - 1 is the side's LAST line: a one-line hunk counts
		// 1 and ends on its start, and a hunk starting at 97 with a count of
		// 3 ends at 99, not 100 — the off-by-one is the difference between a
		// 2-digit gutter and a 3-digit one on a boundary.
		for _, n := range []int{
			max(oldStart+hunkCount(r.text, '-')-1, 1),
			max(newStart+hunkCount(r.text, '+')-1, 1),
		} {
			if d := digits(n); d > widest {
				widest = d
			}
		}
	}
	return min(max(widest, 1), diffNumCap)
}

// hunkCount is the span one side of a hunk header declares — the "7" of
// "@@ -12,7 +14,9 @@" for the old side. Zero when the header omits it, which
// is git's spelling for a single-line hunk.
func hunkCount(hdr string, side byte) int {
	for _, f := range strings.Fields(strings.TrimPrefix(strings.TrimSpace(hdr), "@@")) {
		if len(f) == 0 || f[0] != side {
			continue
		}
		rest := f[1:]
		_, span, ok := strings.Cut(rest, ",")
		if !ok {
			return 1
		}
		n, err := strconv.Atoi(span)
		if err != nil {
			return 1
		}
		return n
	}
	return 1
}

func digits(n int) int {
	d := 1
	for n >= 10 {
		n /= 10
		d++
	}
	return d
}

// hunkCounts reads the START line of each side out of a `@@ -a,b +c,d @@`
// header: the old file's line a, the new file's line c. The COUNT ("b", "d")
// is skipped — it is how many lines the hunk spans, not where it starts, and
// the start is what a gutter numbers.
//
// The count is also what diffNumCells reads to size the gutter: a diff whose
// last hunk ENDS at line 120 needs three digits even though every line the
// reader can see is single-digit, so the second number is folded back in by
// the caller that wants the width.
func hunkCounts(hdr string) (oldStart, newStart int) {
	seen := 0
	for _, f := range strings.Fields(strings.TrimPrefix(strings.TrimSpace(hdr), "@@")) {
		lead := strings.IndexAny(f, "-+")
		if lead < 0 {
			continue
		}
		rest := f[lead+1:]
		if comma := strings.IndexByte(rest, ','); comma >= 0 {
			rest = rest[:comma]
		}
		n, err := strconv.Atoi(rest)
		if err != nil {
			continue
		}
		if seen == 0 {
			oldStart = n
		} else {
			newStart = n
		}
		if seen++; seen == 2 {
			break
		}
	}
	return oldStart, newStart
}

// splitRows lays the diff out as paired sides, one terminal row per pair, and
// wraps each side to its own column budget. The pairing is the one a reader
// makes with their eye: a removed line and the added line immediately after it
// share a row, an added line with no removed partner shares it with a blank
// left side, and a context line is both sides' line n at once.
//
// A hunk header is chrome and spans the pair: it is written once, across the
// whole width, because it is the frame of the rows beneath it and a number in
// one gutter would misread as a line.
func (a *App) splitRows(rows []diffRow, inner int) []line {
	ds := a.diffStyle()
	numW := diffNumCells(rows)
	// The divider costs one column and the two sides split the rest evenly, so
	// the pair fills the panel rather than leaving a gutter-wide strip of
	// unused background down its right edge.
	sideW := (inner - 1) / 2
	textW := max(1, sideW-numW-1)
	if sideW <= numW+1 {
		// Too narrow for a gutter and a line of text on each side. splitFits
		// gates on this, so reaching it means a caller ignored the gate; the
		// unified list is the honest fallback rather than two empty columns.
		return diffRowsCells(rows, ds, inner)
	}

	numSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim)))
	out := make([]line, 0, len(rows))
	oldNo, newNo := 0, 0
	for i := 0; i < len(rows); {
		r := rows[i]
		switch r.kind {
		case diffHunk:
			// The header restates both counters, so a hunk after a trimmed
			// middle continues the FILE's numbering rather than whatever the
			// visible run so far happened to count.
			oldNo, newNo = hunkCounts(r.text)
			out = append(out, hunkSpan(ds.row(r), inner)...)
			i++
			continue
		case diffFile:
			// The ---/+++ pair is the overlay's own title, which already
			// names the file; repeating it across the top of the pair is
			// chrome the reader has read once.
			i++
			continue
		case diffRemoved:
			left := diffSide{row: r, num: oldNo, hasNo: true}
			oldNo++
			right := diffSide{}
			if i+1 < len(rows) && rows[i+1].kind == diffAdded {
				right = diffSide{row: rows[i+1], num: newNo, hasNo: true}
				newNo++
				i++
			}
			i++
			out = appendSplit(out, a, left, right, numW, numSt, sideW, textW, ds)
			continue
		case diffAdded:
			// An addition with no removed partner: the left side is blank,
			// and its blank gutter is what keeps the columns aligned.
			out = appendSplit(out, a, diffSide{}, diffSide{row: r, num: newNo, hasNo: true}, numW, numSt, sideW, textW, ds)
			newNo++
			i++
			continue
		}
		// Context: one line, both numbers, one painted copy.
		side := diffSide{row: r, num: oldNo, hasNo: true}
		out = appendSplit(out, a, side, side, numW, numSt, sideW, textW, ds)
		oldNo++
		newNo++
		i++
	}
	if len(out) == 0 {
		out = append(out, line{})
	}
	return out
}

// appendSplit renders one pair as as many terminal rows as its longer side
// needs: both sides wrap to the same budget, so the pair grows downward
// together and the divider stays a straight line down the viewer.
func appendSplit(
	out []line, a *App, left, right diffSide,
	numW int, numSt tcell.Style, sideW, textW int, ds diffStyle,
) []line {
	ls := sideLines(a, left, numW, numSt, textW, ds)
	rs := sideLines(a, right, numW, numSt, textW, ds)
	for i := 0; i < max(len(ls), len(rs)); i++ {
		var l, r line
		if i < len(ls) {
			l = ls[i]
		}
		if i < len(rs) {
			r = rs[i]
		}
		out = append(out, joinPair(l, r, numW+1+textW))
	}
	return out
}

// sideLines wraps one side: the gutter stays fixed while the content wraps
// under it, so a wrapped line keeps the number it belongs to on its first row
// and a blank gutter on its continuation — the same shape a code editor and
// the same shape every diff viewer that numbers its lines uses.
func sideLines(a *App, s diffSide, numW int, numSt tcell.Style, textW int, ds diffStyle) []line {
	if !s.hasNo {
		return blankSide(numW + 1 + textW)
	}
	gutter := func(n int) line {
		if n == 0 {
			return textline(strings.Repeat(" ", numW), numSt)
		}
		return textline(fmt.Sprintf("%*d", numW, n), numSt)
	}
	body := ds.row(s.row)
	wrapped := wrapCells(body, textW)
	out := make([]line, 0, len(wrapped))
	for i, wl := range wrapped {
		n := 0
		if i == 0 {
			n = s.num
		}
		out = append(out, joinGutter(gutter(n), wl, numW+1+textW))
	}
	return out
}

// joinGutter puts the gutter in front of one content row and pads the row out
// to the side's full width, so the divider lands in the same column on every
// line of the viewer. The pad carries the row's own band when it has one: a
// stripe that stops short of the divider is a stripe the eye reads as broken.
func joinGutter(num, body line, w int) line {
	ln := line{runs: append([]cell{}, num.runs...)}
	ln.runs = append(ln.runs, body.runs...)
	return padTo(ln, w)
}

// blankSide is the other side's padding on a row that has no content: blank
// cells of the side's own width, so nothing of the previous row shows through
// the columns (the overlay's own fill is the terminal's background and stays).
func blankSide(w int) []line {
	return []line{padTo(textline(strings.Repeat(" ", w), tcell.StyleDefault), w)}
}

// joinPair lays the two sides on one row with a one-cell divider between
// them. The divider is the theme's gray, the one ink that is chrome in every
// theme: a themed accent would claim the change the row beside it is about.
func joinPair(l, r line, sideW int) line {
	ln := padTo(l, sideW)
	_, _, attrs := ln.runs[len(ln.runs)-1].style.Decompose()
	ln.runs = append(ln.runs, cell{text: " ", style: tcell.StyleDefault.Attributes(attrs)})
	ln.runs = append(ln.runs, padTo(r, sideW).runs...)
	return ln
}

// padTo fills a styled row out to w cells with blanks that carry the row's
// own attributes and band, so a banded row's stripe reaches the edge and an
// unbanded row's trailing blank stays the terminal's own background.
func padTo(ln line, w int) line {
	pad := w - lineWidth(ln)
	if pad <= 0 {
		return ln
	}
	if len(ln.runs) == 0 {
		return textline(strings.Repeat(" ", w), tcell.StyleDefault)
	}
	_, _, attrs := ln.runs[len(ln.runs)-1].style.Decompose()
	st := tcell.StyleDefault.Attributes(attrs)
	if bg, ok := banded(ln); ok {
		st = st.Background(bg)
	}
	ln.runs = append(ln.runs, cell{text: strings.Repeat(" ", pad), style: st, chrome: true})
	return ln
}

// hunkSpan wraps a row that spans the pair (a hunk header) to the whole
// interior and pads it out to that interior: it is content on the panel, so the
// rest of its row must be the panel's own blank rather than whatever the
// transcript painted there before the overlay filled the frame. It is wider
// than a pair row by the rounding the two sides leave over — the header is
// chrome, not half a diff, so it spans what there is.
func hunkSpan(ln line, w int) []line {
	out := wrapCells(ln, w)
	for i, l := range out {
		out[i] = padTo(l, w)
	}
	return out
}

// diffModeName is the footer's word for the layout, so the hint names the
// state `s` switches away from.
func diffModeName(split bool) string {
	if split {
		return "split"
	}
	return "unified"
}
