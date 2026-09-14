package tui

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
)

// Mouse text selection: drag anywhere on screen, release copies the covered text
// to the clipboard. The screen runs with mouse capture enabled (EnableMouse in
// cmd/xdev/tui.go), so button events reach the app; the transcript's own wheel
// scroll lives in handleKey.
//
// The gesture follows what a terminal's native selection does, because that is
// the behavior an omp user gets — omp has no selection code of its own: it
// captures the mouse only for click-to-focus, behind a `tui.mouse` setting that
// defaults off, and leaves selection and copying to the terminal. So: the
// highlight stays up after the release until the next click, a click without a
// drag clears it instead of writing an empty string over the clipboard, the copy
// is confirmed on the composer's info divider, and dragging past the top or
// bottom row scrolls the transcript so one selection can cover more than a
// screen.
//
// Selection covers every row the app paints: transcript, welcome, composer,
// status, overlays. A gesture that starts in the transcript anchors its corners
// to *document* rows, so the viewport sliding under a held drag follows the text
// the user grabbed instead of crawling over it, and rows that scroll away are
// kept in a per-gesture cache so the copy stays complete. A gesture that starts
// on chrome anchors to screen rows, which never move.
//
// Shift is the native-selection escape hatch: omp documents it ("native text
// selection becomes Shift+drag") and so does every terminal that implements
// mouse reporting — while the app holds the mouse, Shift+drag means "the terminal
// selects and copies, not the app". The SGR reports still reach us with ModShift
// set, so the app has to decline them: otherwise the release-time app copy
// clobbers the terminal's native selection copy, and the paste comes out wrong.
// Same contract as omp and Claude Code.
//
// Known limit: a document row is a position in the rendered-line index, so a
// transcript that grows or compacts mid-gesture moves those numbers with it. The
// copy is taken at release, so the worst case is off by the rows that arrived
// during the drag.

// selRow is one selectable row: the rendered text and the column where it starts
// (3 = rail+pad for transcript rows, 0 for rows read back from the grid, whose
// leading cells are part of the layout).
type selRow struct {
	text string
	x0   int
}

// selCorner is one corner of a drag. x and y are the screen cell the pointer was
// on; doc is the transcript row that screen row showed at the time, or -1 for a
// corner on chrome, whose screen row is its identity.
type selCorner struct {
	x, y, doc int
}

// selGrace is how long the copy confirmation stays on the divider.
const selGrace = 2 * time.Second

// handleMouse routes mouse events for selection. Wheel stays with the scroll
// model (its caller). The primary button drives the drag lifecycle: press starts
// it, a drag with the button held extends it (scrolling the transcript at the
// edges), release on ButtonNone finishes it and copies. Caller: UI thread
// (handleKey, mu held). Shift-modified events are declined outright — see the
// file comment: they belong to the terminal's native selection.
func (a *App) handleMouse(m *tcell.EventMouse) {
	if m.Modifiers()&tcell.ModShift != 0 {
		// Decline the gesture, and drop an in-flight one: a plain drag
		// that picks up Shift mid-stroke gets no release we can act on
		// (tcell reports it Shift-modified too), so the highlight would
		// stay stuck on screen while the terminal makes its own
		// selection on top of ours.
		if a.selDown || a.selShown {
			a.selDown, a.selShown = false, false
			a.selAnchor, a.selEnd = selCorner{}, selCorner{}
			a.selCache = nil
			a.poke()
		}
		return
	}
	x, y := m.Position()
	btn := m.Buttons()
	switch {
	case btn&tcell.Button1 != 0 && !a.selDown: // press
		// A new press always takes over, held selection or not: the release
		// of a drag whose terminal never reported the button up (released
		// outside the window) must not leave the app wedged.
		a.selDown, a.selShown = true, true
		a.selCache = map[int]selRow{}
		a.selDocMode = false // classify this corner by where it landed
		a.selAnchor = a.selCornerAt(x, y)
		a.selDocMode = a.selAnchor.doc >= 0
		a.selEnd = a.selAnchor
		a.poke()
	case btn&tcell.Button1 != 0 && a.selDown: // drag
		a.selAutoScroll(y) // then name the row under the pointer, post-scroll
		a.selEnd = a.selCornerAt(x, y)
		a.selShown = true
		a.poke()
	case btn&tcell.Button1 == 0 && a.selDown: // release
		a.selDown = false
		if a.selAnchor == a.selEnd {
			// No motion: a click. Clear the highlight and leave the
			// clipboard alone, exactly like every terminal does.
			a.selShown = false
		} else {
			// Resolved while the cache is still alive: the rows an edge
			// auto-scroll pushed out of the viewport exist nowhere else.
			text := a.selectionText()
			a.selCache = nil
			if strings.TrimSpace(text) == "" {
				a.selShown = false
			} else if a.copyToClipboard(text) == nil {
				a.selNotice = "Copied " + strconv.Itoa(utf8.RuneCountInString(text)) + " chars"
				a.selNoticeUntil = time.Now().Add(selGrace)
			}
		}
		a.poke()
	}
	// Remaining buttons (right/middle, bare motion) are ignored; wheel was
	// already handled by the caller.
}

// copyHint returns the copy confirmation while it is still fresh. Once its
// deadline passes the notice is dropped here, so no later draw can resurrect it;
// the caller falls back to the viewport hint.
func (a *App) copyHint() string {
	if a.selNotice == "" {
		return ""
	}
	if !time.Now().Before(a.selNoticeUntil) {
		a.selNotice = ""
		return ""
	}
	return a.selNotice
}

// --- anchoring --------------------------------------------------------------

// selViewport reports the transcript's first document row and its height in
// screen rows. Callers hold a.mu. With no transcript there is nothing to anchor
// to, so the height is 0 and every corner is chrome.
func (a *App) selViewport() (top, vp int) {
	if len(a.blocks) == 0 {
		return 0, 0
	}
	total, vp := a.totalLinesLocked(), a.viewportLinesLocked()
	return a.sm.Start(total, vp), vp
}

// selCornerAt names the row under a screen position. In a transcript gesture
// every corner takes a document row — past the edges the pointer clamps to the
// edge row, which is the one the auto-scroll is about to replace — so the
// selection tracks the text it grabbed while the viewport moves. A chrome
// gesture keeps screen rows.
func (a *App) selCornerAt(x, y int) selCorner {
	c := selCorner{x: x, y: a.clampScreen(y), doc: -1}
	top, vp := a.selViewport()
	if vp > 0 && (a.selDocMode || y < vp) {
		c.doc = top + min(max(y, 0), vp-1)
	}
	return c
}

// selAutoScroll scrolls the transcript one row when a held drag has reached its
// top or bottom edge — a terminal's edge auto-scroll, and the only way a
// selection can cover more than one screen. Dragging below the transcript (into
// the composer's rows) counts as the bottom edge: the user is still pulling the
// selection downward, and the rows that matter are the ones coming into view.
func (a *App) selAutoScroll(y int) {
	_, vp := a.selViewport()
	if vp <= 0 || !a.selDocMode {
		return
	}
	total := a.totalLinesLocked()
	switch {
	case y <= 0:
		a.sm.ScrollUp(1, total, vp)
	case y >= vp-1:
		a.sm.ScrollDown(1, total, vp)
	}
}

// selCacheRows records the frame's transcript rows under their document numbers
// while a drag is held, so a row that scrolls out of sight afterwards is still
// copyable. Each draw caches the row the last scroll revealed, which is what
// lets one gesture outgrow the viewport. Callers hold a.mu.
func (a *App) selCacheRows(top int) {
	if a.selCache == nil {
		return
	}
	for i, sr := range a.selRows {
		a.selCache[top+i] = sr
	}
}

// --- geometry ---------------------------------------------------------------

// selBounds says which cells of a row the drag covered. -1 on either side means
// "to that edge of the line", which is only knowable once the row's content is in
// hand: the anchor row is {lo: x, hi: -1}, the far row {lo: -1, hi: x}, a middle
// row {-1, -1}, and a single-row gesture names both ends.
type selBounds struct {
	lo, hi int
}

// selSpanRow is one row of the resolved selection: its screen row (-1 when it has
// scrolled out of the viewport and survives only in the cache), its content, and
// the inclusive cell range the pointer covered.
type selSpanRow struct {
	y      int
	row    selRow
	lo, hi int
}

// selSpan is the selection as an ordered list of rows, from the anchor toward the
// end. It is the one piece of geometry both the highlight and the copy use, so
// what is painted is what is copied.
func (a *App) selSpan() []selSpanRow {
	if !a.selShown && !a.selDown {
		return nil
	}
	if !a.selDocMode {
		return a.selScreenSpan()
	}
	top, _ := a.selViewport()
	d0, d1 := a.selAnchor.doc, a.selEnd.doc
	if d0 == d1 {
		return []selSpanRow{a.selDocRow(d0, top, selBounds{lo: a.selAnchor.x, hi: a.selEnd.x})}
	}
	step := 1
	if d1 < d0 {
		step = -1
	}
	var out []selSpanRow
	for d := d0; ; d += step {
		b := selBounds{lo: -1, hi: -1} // a row between the corners is taken in full
		switch d {
		case d0:
			b = selBounds{lo: a.selAnchor.x, hi: -1}
		case d1:
			b = selBounds{lo: -1, hi: a.selEnd.x}
		}
		out = append(out, a.selDocRow(d, top, b))
		if d == d1 {
			return out
		}
	}
}

// selDocRow resolves one document row: its text from the live capture while it is
// on screen, from the gesture's cache once it has scrolled away.
func (a *App) selDocRow(d, top int, b selBounds) selSpanRow {
	sr, have := selRow{}, false
	if y := d - top; y >= 0 && y < len(a.selRows) {
		sr, have = a.selRows[y], true
	} else if c, cached := a.selCache[d]; cached {
		sr, have = c, true
	}
	y := d - top
	if !have {
		// Past the end of the rendered transcript: an empty line is the honest
		// answer. On screen but unrecorded (blank padding) reads from the grid.
		if y >= 0 && y < a.height {
			sr = a.selRowAt(y)
		} else {
			return selSpanRow{y: -1, lo: 1, hi: 0}
		}
	}
	if y < 0 || y >= a.height {
		y = -1
	}
	return spanRow(y, sr, b)
}

// selScreenSpan resolves a chrome-anchored gesture, where rows cannot move, so
// the screen rectangle *is* the selection.
func (a *App) selScreenSpan() []selSpanRow {
	ay, by := a.selAnchor.y, a.selEnd.y
	if ay == by {
		return []selSpanRow{spanRow(ay, a.selRowAt(ay), selBounds{lo: a.selAnchor.x, hi: a.selEnd.x})}
	}
	step := 1
	if by < ay {
		step = -1
	}
	var out []selSpanRow
	for y := ay; ; y += step {
		b := selBounds{} // walking from the anchor outward means the head and
		switch y {       // tail rules never have to care which way they dragged
		case ay:
			b = selBounds{lo: a.selAnchor.x, hi: -1}
		case by:
			b = selBounds{lo: -1, hi: a.selEnd.x}
		}
		out = append(out, spanRow(y, a.selRowAt(y), b))
		if y == by {
			return out
		}
	}
}

func (a *App) clampScreen(y int) int {
	if y < 0 {
		return 0
	}
	return min(y, a.height-1)
}

// spanRow pins a row's covered cell range against its content. The bound is
// inclusive because drawSelection paints the cell under the pointer, so the copy
// has to include it; both go through here.
func spanRow(y int, sr selRow, b selBounds) selSpanRow {
	lineEnd := sr.x0 + width(sr.text) - 1
	lo, hi := b.lo, b.hi
	if lo < 0 {
		lo = sr.x0
	}
	if hi < 0 {
		hi = lineEnd
	}
	if lo < sr.x0 {
		lo = sr.x0 // never reach left of the content
	}
	if hi > lineEnd {
		hi = lineEnd
	}
	if hi < lo {
		hi = lo - 1 // contributes nothing, but keeps the line break honest
	}
	return selSpanRow{y: y, row: sr, lo: lo, hi: hi}
}

// selectionText reconstructs the selected text, one line per row, in the shape
// selSpan describes.
func (a *App) selectionText() string {
	var sb strings.Builder
	for _, r := range a.selSpan() {
		sb.WriteString(cellSlice(r.row.text, r.lo-r.row.x0, r.hi-r.row.x0+1))
		sb.WriteByte('\n')
	}
	return strings.TrimRight(sb.String(), " \n\t")
}

// selRowAt returns the selectable content of screen row y. Rows the transcript
// painter recorded come from that capture, which leaves out the accent rail and
// its padding so a copied line is the text and not the decoration. Every other
// row — welcome screen, composer, status row, an open overlay, or blank space
// under a short transcript — is read back from the painted grid, trimmed of the
// trailing cells that only exist to fill the width.
func (a *App) selRowAt(y int) selRow {
	if y >= 0 && y < len(a.selRows) {
		return a.selRows[y]
	}
	var sb strings.Builder
	for x := 0; x < a.width; {
		s, _, w := a.scr.Get(x, y)
		sb.WriteString(s)
		if w < 1 {
			w = 1 // continuation cells report 0; never spin on them
		}
		x += w
	}
	return selRow{text: strings.TrimRight(sb.String(), " ")}
}

// cellSlice returns the runes of text occupying cells [a, b) (cells counted by
// display width, wide runes counted by their first cell).
func cellSlice(text string, a, b int) string {
	if b <= a {
		return ""
	}
	var sb strings.Builder
	col := 0
	for _, r := range text {
		w := width(string(r))
		if col+w > a && col < b {
			sb.WriteRune(r)
		}
		col += w
	}
	return sb.String()
}

// drawSelection paints the live highlight: reverse video over the selected cells.
// It stays up after the release, until the next click hands the mouse to a new
// gesture or Shift hands it to the terminal. Rows that have scrolled out of the
// viewport are simply not on screen to paint. Caller: draw (mu held).
func (a *App) drawSelection() {
	if !a.selShown {
		return
	}
	for _, r := range a.selSpan() {
		if r.y < 0 || r.y >= a.height {
			continue
		}
		for x := max(r.lo, 0); x <= r.hi && x < a.width; x++ {
			mainc, combc, style, _ := a.scr.GetContent(x, r.y)
			a.scr.SetContent(x, r.y, mainc, combc, style.Reverse(true))
		}
	}
}
