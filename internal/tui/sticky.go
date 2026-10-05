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

// stickyPad is the header's own air, in rows: the card's blank banded row above
// and below its text, so a pinned prompt is the same card it was inline and
// not a bar pressed against the top of the transcript. The header counts these
// in the rows it owns but never in the rows it keeps visible, so the prompt
// still shrinks one text row per row scrolled.
const stickyPad = 1

// stickyMinHeight is the floor a pinned prompt collapses to, in TEXT rows:
// enough rows to read its first line and admit with an ellipsis that the rest
// is hidden. stickyPad sits on top of it.
const stickyMinHeight = 3

// stickyGap is the blank row between a pinned prompt and the transcript below
// it (grok's HEADER_CONTENT_GAP): one row of air says the header is not part of
// the stream. A prompt being pushed off renders no gap after it, so the
// transcript's first row does not move while the push plays out.
const stickyGap = 1

// stickyPrompt is one user prompt's place in the transcript's row space: where
// its TEXT starts (row) and how many lines of that text render (full, the
// block's trailing separator excluded - the gap between blocks is transcript
// spacing, not a line of the prompt), plus the document row its card starts at
// (doc), which is stickyPad above the text.
//
// row and full are in TEXT rows, not render rows: a user card pads its own top
// and bottom rows (see blockLines), and a header that spent its rows on that
// padding would show a blank line where the request should be.
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

// bandTextRows counts a render's rows that carry text. A user card pads its own
// first and last row (see blockLines); every other block is content end to end,
// so this is its render verbatim.
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
// header owns of the viewport, padding and gap included, so the transcript
// resumes at start+rows and still ends at start+vp.
type stickyLayout struct {
	block     int   // pinned block (-1 = none)
	visible   int   // TEXT rows of the prompt on screen, after clipping
	clipTop   int   // text rows cut off its top by the next prompt pushing in
	rows      int   // screen rows the header owns: padding, text and gap
	row       int32 // the prompt's first TEXT row (it stays pinned past it)
	padTop    int   // blank banded rows the card paints above its text
	padBottom int   // blank banded rows the card paints below its text
}

// computeSticky picks the prompt to pin for a viewport whose first row is
// firstRow, and how many rows it keeps. Port of grok's compute_sticky_layout +
// calculate_render_height.
//
// The 1:1 shrink is the point: the header gives a row back for every row
// scrolled, so the content below it advances by one row per row of scroll and
// the viewport's bottom line never jumps. It counts TEXT rows, and the card's
// own padding rides on top of them (stickyPad), so a pinned prompt is the same
// card the transcript drew and the shrink still counts words.
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
	// Its text is what is left of it once the rows scrolled past its top are
	// gone — never below the floor, never taller than it renders inline, never
	// taller than the viewport minus the padding the card wraps itself in.
	text := max(min(p.full-int(firstRow-p.row), vp-2*stickyPad), min(stickyMinHeight, p.full))
	if text+2*stickyPad+stickyGap >= vp {
		return none // the header would leave the transcript no rows at all
	}
	// How many rows the card may own at all. With no following prompt it is the
	// whole viewport, and it has already been shown to fit. With one, it is
	// everything above the incoming card's OWN top padding: a header that
	// covered that row would show the new prompt without the air the card
	// paints for itself.
	room := vp
	if next := pin + 1; next < len(prompts) {
		room = int(prompts[next].row-firstRow) - stickyPad
	}
	card := text + 2*stickyPad + stickyGap
	h := stickyLayout{block: p.block, row: p.row, padTop: stickyPad, padBottom: stickyPad}
	if room >= card {
		h.visible, h.rows = text, card
		return h
	}
	// Pushed off. The push clips the card from the TOP — that is the direction
	// the next prompt arrives from — so what the header gives up comes off the
	// BOTTOM, cheapest row first: the blank gap, then the blank padding under
	// the text, then the text itself. The padding over the text goes last.
	h.padBottom = 0
	if room >= text+stickyPad {
		h.visible, room = text, room-text
		h.padTop = min(stickyPad, room)
	} else {
		h.padTop, h.visible = 0, min(text, room)
	}
	if h.visible <= 0 {
		return none // nothing of the card is left above the incoming one
	}
	h.clipTop = text - h.visible
	h.rows = h.padTop + h.visible + h.padBottom
	return h
}

// stickyHeaderRows materializes the pinned prompt's own inline render for the
// header: the same banded ❯ rows the transcript paints, so pinning changes
// WHERE a prompt is, never what it looks like. Caller holds a.mu, after sync.
//
// The window is in TEXT rows (the space stickyPrompts and computeSticky work
// in), so the card's blank pad rows are stepped over: they are the card's
// padding, not a line of the request, and the painter spends stickyPad rows
// for each of them from the header's own row budget instead.
func (a *App) stickyHeaderRows(h stickyLayout, wrapW int) []rowView {
	if h.block < 0 || h.block >= len(a.rowIdx.rend) {
		return nil
	}
	lines := a.rowIdx.rend[h.block].lines
	out := make([]rowView, 0, h.visible)
	text := 0
	for i := range lines {
		if len(lines[i].runs) == 0 {
			continue // the card's pad row: spacing, not a line
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
	// wrap width gives up its own tail for them rather than overrunning the card.
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

// stickyPadRow is the blank banded row the pinned prompt's card pads itself
// with, so the header spends the padding the transcript drew rather than
// inventing its own. An unpinned prompt (h.padTop and h.padBottom both zero)
// never reaches the painter with it.
func (a *App) stickyPadRow(h stickyLayout) rowView {
	if h.block < 0 || h.block >= len(a.rowIdx.rend) || len(a.rowIdx.rend[h.block].lines) == 0 {
		return rowView{}
	}
	return rowView{ln: a.rowIdx.rend[h.block].lines[0]}
}
