package tui

// Sticky prompt header (grok scrollback/sticky.rs, verified against
// github.com/xai-org/grok-build main 2026-10-02). A user prompt is the section
// header of a turn: scrolled past, it pins to the top of the viewport and
// shrinks one row per scrolled row, so the request on screen stays readable
// while its answer scrolls underneath. The next prompt arriving pushes the
// pinned one off. Three numbers do all of it, and — as in grok — they are pure
// 1D coordinate math over total heights: nothing here looks inside a block, so
// the painter decides how to spend the rows it is handed and this file is
// testable without a screen.

// stickyMinHeight is the floor a pinned prompt collapses to: enough rows to read
// its first line and admit with an ellipsis that the rest is hidden. ponytail:
// grok's min_height is 4 (2 content rows + 2 of padding); xdev's user band
// carries no padding, so the prompt spends those rows on its own text.
const stickyMinHeight = 3

// stickyGap is the blank row between a pinned prompt and the transcript below
// it (grok's HEADER_CONTENT_GAP): one row of air says the header is not part of
// the stream. A prompt being pushed off renders no gap after it, so the
// transcript's first row does not move while the push plays out.
const stickyGap = 1

// stickyPrompt is one user prompt's place in the transcript's row space: where
// its TEXT starts (row) and how many lines of that text render (full, the
// block's trailing separator excluded — the gap between blocks is transcript
// spacing, not a line of the prompt).
//
// Both numbers are in TEXT rows, not render rows: a user band pads its own
// first row (see blockLines), and a header that spent one of its three rows
// on that padding would show a blank line where the request should be.
type stickyPrompt struct {
	block int   // index into App.blocks
	row   int32 // first transcript row of the prompt's text (its inline top)
	full  int   // rendered text lines of the prompt
}

// stickyPrompts lists the prompts eligible to pin: every user block. A
// single-line prompt pins at its one row — most prompts ARE one line, and the
// request being answered is exactly what a reader scrolls away from. Caller
// holds a.mu, after sync (the row offsets live in the row index).
func (a *App) stickyPrompts() []stickyPrompt {
	x := &a.rowIdx
	out := make([]stickyPrompt, 0, 8)
	for i, b := range a.blocks {
		if b.Kind != KindUser || i >= len(x.start) || i >= len(x.rend) {
			continue
		}
		lines := x.rend[i].lines
		if first := bandTextIndex(lines); first >= 0 {
			out = append(out, stickyPrompt{block: i, row: x.start[i] + int32(first), full: bandTextRows(lines)})
		}
	}
	return out
}

// bandTextRows counts a render's rows that carry text. A banded block pads its
// first row with blank cells (the sent message's own margin, see blockLines);
// every other block is content end to end, so this is its render verbatim.
func bandTextRows(lines []line) int {
	n := 0
	for i := range lines {
		if len(lines[i].runs) > 0 {
			n++
		}
	}
	return n
}

// bandTextIndex is the render row of the first row that carries text, or -1
// when the render is chrome end to end and there is nothing to pin.
func bandTextIndex(lines []line) int {
	for i := range lines {
		if len(lines[i].runs) > 0 {
			return i
		}
	}
	return -1
}

// stickyLayout is one frame's header decision. block -1 means no header: the
// prompt renders inline in the transcript, exactly as before. rows is what the
// header owns of the viewport, the gap included, so the transcript resumes at
// start+rows and still ends at start+vp.
type stickyLayout struct {
	block   int   // pinned block (-1 = none)
	visible int   // rows of the prompt on screen, after clipping
	clipTop int   // rows cut off its top by the next prompt pushing in
	rows    int   // screen rows the header owns, gap included
	row     int32 // the prompt's own first document row (it stays pinned past it)
}

// computeSticky picks the prompt to pin for a viewport whose first row is
// firstRow, and how many rows it keeps. Port of grok's compute_sticky_layout +
// calculate_render_height.
//
// The 1:1 shrink is the point: the header gives a row back for every row
// scrolled, so the content below it advances by one row per row of scroll and
// the viewport's bottom line never jumps.
func computeSticky(firstRow int32, vp int, prompts []stickyPrompt) stickyLayout {
	none := stickyLayout{block: -1}
	if len(prompts) == 0 || firstRow <= 0 {
		return none // nothing has scrolled past yet
	}
	// The pinned prompt is the last one whose own top row is above the viewport.
	pin := -1
	for i := range prompts {
		if prompts[i].row < firstRow {
			pin = i
		}
	}
	if pin < 0 {
		return none
	}
	p := prompts[pin]
	// Its height is what is left of it once the rows scrolled past its top are
	// gone — never below the floor, never taller than it renders inline, never
	// taller than the viewport.
	render := max(min(p.full-int(firstRow-p.row), vp), min(stickyMinHeight, p.full))
	if render+stickyGap >= vp {
		return none // the header would leave the transcript no rows at all
	}
	// The next prompt pushes: as it reaches the top of the viewport it clips the
	// pinned one from above, and the gap row goes before any content.
	if next := pin + 1; next < len(prompts) {
		if naive := int(prompts[next].row - firstRow); naive <= render+stickyGap {
			vis := naive - stickyGap
			if vis <= 0 {
				// Only the gap row is left, or the next prompt already owns row
				// 0: this header's job is done.
				return none
			}
			if vis < render {
				// Pushed off: no gap after it, so the transcript's first row
				// stays put while the push plays out.
				return stickyLayout{block: p.block, visible: vis, clipTop: render - vis, rows: vis, row: p.row}
			}
		}
	}
	return stickyLayout{block: p.block, visible: render, rows: render + stickyGap, row: p.row}
}

// stickyHeaderRows materializes the pinned prompt's own inline render for the
// header: the same banded ❯ rows the transcript paints, so pinning changes
// WHERE a prompt is, never what it looks like. Caller holds a.mu, after sync.
//
// The window is in TEXT rows (the space stickyPrompts and computeSticky work
// in), so the band's blank pad rows are stepped over: they are the card's
// padding, not a line of the request.
func (a *App) stickyHeaderRows(h stickyLayout, wrapW int) []rowView {
	if h.block < 0 || h.block >= len(a.rowIdx.rend) {
		return nil
	}
	lines := a.rowIdx.rend[h.block].lines
	out := make([]rowView, 0, h.visible)
	text := 0
	for i := range lines {
		if len(lines[i].runs) == 0 {
			continue // the band's pad row: spacing, not a line
		}
		if text < h.clipTop {
			text++
			continue // scrolled past the top of the pinned window
		}
		if text >= h.clipTop+h.visible {
			break
		}
		// The runs are copied, not shared: the ellipsis below edits the last
		// one, and the row index's render is the transcript's own.
		out = append(out, rowView{ln: line{
			runs:  append([]cell(nil), lines[i].runs...),
			bg:    lines[i].bg,
			inset: lines[i].inset,
		}})
		text++
	}
	if len(out) == 0 || h.clipTop+len(out) >= bandTextRows(lines) {
		return out // nothing hidden below: no ellipsis to admit
	}
	// The ellipsis admits the hidden rows in two cells; a row already at the
	// wrap width gives up its own tail for them rather than overrunning the band.
	last := out[len(out)-1].ln
	used := 0
	for _, r := range last.runs {
		used += paintedWidth(r.text)
	}
	tail := &last.runs[len(last.runs)-1]
	tail.text = truncateCells(tail.text, max(0, wrapW-used-2), "") + " …"
	out[len(out)-1].ln = last
	return out
}
