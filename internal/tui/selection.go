package tui

import (
	"strconv"
	"strings"
	"time"
	"unicode"
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
// screen. That scroll is time-driven, not event-driven (selEdgeTick): a real
// pointer parked at the edge sends no further reports, since tcell strips the
// SGR motion bit, and a scroll that stops when the hand stops is a scroll that
// stops. The right-edge scrollbar answers the mouse too — a press there drags
// the thumb instead of selecting, because that column is the viewport control,
// not text.
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
	y    int // screen row this row lives on; for dock rows only
}

// selCorner is one corner of a drag. x and y are the screen cell the pointer was
// on; doc is the transcript row that screen row showed at the time, or -1 for a
// corner on chrome, whose screen row is its identity.
type selCorner struct {
	x, y, doc int
}

// selGrace is how long the copy confirmation stays on the divider.
const selGrace = 2 * time.Second

// clearClick resets the click-count state after a double/triple-click
// gesture completes (or when a new gesture begins from a different
// position). Callers hold a.mu.
func (a *App) clearClick() {
	a.selClickCount = 0
	a.selClickTime = time.Time{}
	a.selClickX, a.selClickY = 0, 0
}

// handleClick is the press-side logic for a primary button that is
// not a drag start (no selDown yet, not on the scrollbar). It advances
// the click count from the release state of the previous click: if the
// previous release was within clickWordWindow and clickWordTol, this
// is the next click in a sequence; otherwise it starts fresh.
// On count >= 2 it converts the click into a word or line selection
// instead of a drag. count=1 starts a normal drag.
func (a *App) handleClick(x, y int) bool {
	now := time.Now()
	if a.selClickCount > 0 &&
		now.Sub(a.selClickTime) <= clickWordWindow &&
		abs(x-a.selClickX) <= clickWordTol &&
		abs(y-a.selClickY) <= clickWordTol {
		a.selClickCount++
	} else {
		a.selClickCount = 1
	}
	a.selClickTime = now
	a.selClickX, a.selClickY = x, y

	if a.selClickCount >= 2 {
		// Double or triple click: consume it - no drag follows. The caller
		// must not then re-anchor the gesture at the raw click point: that
		// would collapse the word/line selection back into a no-motion click
		// and copy nothing. true says "handled, stop here".
		//
		// clearClick is deliberately NOT called here: its call zeroed
		// selClickTime as well as the count, so the third press of a
		// triple-click sequence saw count=0 and started fresh at 1 —
		// triple-click was indistinguishable from a single click. The
		// count resets on its own when the position moves or the window
		// expires.
		if a.selClickCount >= 3 {
			a.selStartLineSelect(x, y)
		} else {
			a.selStartWordSelect(x, y)
		}
		a.poke()
		return true
	}
	// Count 1: a normal drag starts on the next mouse motion.
	return false
}

// selStartWordSelect begins a word-selection gesture at (x, y):
// finds the word boundaries on the row under the pointer and pins
// the anchor and end there, so a drag expands the selection word by
// word. If no word is found at the position (whitespace click),
// it falls back to a character drag from the click point.
func (a *App) selStartWordSelect(x, y int) {
	a.selDown = true
	a.selShown = true
	a.selCache = map[int]selRow{}
	a.selDocMode = false
	a.selAnchor = selCorner{x: x, y: y, doc: -1}
	a.selEnd = a.selCornerAt(x, y)
	lo, hi := a.selWordAt(x, y)
	if lo >= 0 {
		// x is a screen column and selWordAt counts within the
		// text (which starts at row.x0), so translate the
		// text-relative word bounds into document columns.
		row := a.selRowAt(y)
		a.selAnchor.x = lo + row.x0
		a.selEnd.x = hi + row.x0
	}
	a.poke()
}

// selStartLineSelect begins a line-selection gesture at (x, y):
// selects the full width of the screen row under the pointer.
func (a *App) selStartLineSelect(x, y int) {
	a.selDown = true
	a.selShown = true
	a.selCache = map[int]selRow{}
	a.selDocMode = false
	a.selAnchor = selCorner{x: 0, y: y, doc: -1}
	a.selEnd = selCorner{x: a.width - 1, y: y, doc: -1}
	a.poke()
}

// selWordAt returns the grapheme-safe (lo, hi) cell range of the
// word under screen position (x, y) on its rendered row, or (-1, -1)
// if the position is not on a word. Uses width for cell width so wide
// characters are not split. Callers hold a.mu.
func (a *App) selWordAt(x, y int) (int, int) {
	row := a.selRowAt(y)
	text := row.text
	if text == "" {
		return -1, -1
	}
	col := 0
	for i, r := range text {
		w := width(string(r))
		if col+w > x {
			start := col
			end := col + w
			for j := i; j > 0; {
				prev, sz := utf8.DecodeLastRuneInString(text[:j])
				if prev == ' ' || prev == '\t' || unicode.IsSpace(prev) {
					break
				}
				start -= width(string(prev))
				j -= sz
			}
			for j := i + utf8.RuneLen(r); j < len(text); {
				next, sz := utf8.DecodeRuneInString(text[j:])
				if next == ' ' || next == '\t' || unicode.IsSpace(next) {
					break
				}
				end += width(string(next))
				j += sz
			}
			if end-start <= 0 {
				return -1, -1
			}
			return start, end - 1
		}
		col += w
	}
	return -1, -1
}

// abs returns the absolute value of n.
func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// selEdgeDelay and selEdgeStep shape the held-drag edge auto-scroll: once the
// pointer has sat on the transcript's first or last row for selEdgeDelay, the
// viewport moves selEdgeStep rows per UI tick (33ms). A terminal's edge drag
// scrolls continuously because the pointer keeps reporting motion even when it
// cannot move further; tcell strips the SGR motion bit, so a parked pointer
// sends nothing at all and the old one-row-per-event scroll stalled the instant
// the hand stopped. The delay guards against a fast sweep across an edge row
// turning a two-row selection into a page turn.
const (
	selEdgeDelay    = 400 * time.Millisecond
	selEdgeStep     = 2
	clickWordWindow = 400 * time.Millisecond
	clickWordTol    = 2 // x,y tolerance for treating clicks as the same position

)

// handleMouse routes mouse events for selection. Wheel stays with the scroll
// model (its caller). The primary button drives the drag lifecycle: press starts
// it, a drag with the button held extends it (scrolling the transcript at the
// edges), release on ButtonNone finishes it and copies. `press` is the button's
// rising edge, computed by the caller: tcell strips the SGR motion bit, so a
// held drag reports Button1 exactly like a press does, and only the edge can
// tell a gesture's first event from its continuations. Caller: UI thread
// (handleKey, mu held). Shift-modified events are declined outright — see the
// file comment: they belong to the terminal's native selection.
func (a *App) handleMouse(m *tcell.EventMouse, press bool) {
	if m.Modifiers()&tcell.ModShift != 0 {
		// Decline the gesture, and drop an in-flight one: a plain drag
		// that picks up Shift mid-stroke gets no release we can act on
		// (tcell reports it Shift-modified too), so the highlight would
		// stay stuck on screen while the terminal makes its own
		// selection on top of ours.
		if a.selDown || a.selShown || a.selThumbDrag {
			a.selDown, a.selShown, a.selThumbDrag = false, false, false
			a.selAnchor, a.selEnd = selCorner{}, selCorner{}
			a.selCache = nil
			a.poke()
		}
		return
	}
	x, y := m.Position()
	btn := m.Buttons()
	switch {
	case btn&tcell.Button1 != 0 && (press || (!a.selDown && !a.selThumbDrag)): // press
		// The rising edge names the press, because a held drag reports Button1
		// like a press does. A report with no gesture in flight counts too: the
		// release of a drag whose terminal never reported the button up
		// (released outside the window) must not leave the app wedged, and a
		// stale edge would swallow the gesture that should recover it.
		a.selEdgeStop()
		// A press on the scrollbar grabs the bar, not the text: the drag that
		// follows moves the viewport, and the gesture owns no selection at all
		// — the rows the painter recorded belong to the frame the bar was hit
		// in, which is exactly the frame a thumb drag exists to change. So this
		// branch never touches selDown: the bar cannot start a copy. It does
		// drop a highlight still held from an earlier drag, because the bar is
		// about to slide the rows out from under it.
		if grip, ok := a.selBarAt(x, y); ok {
			a.selThumbDrag, a.selShown, a.selDown, a.selCache = true, false, false, nil
			a.selGrab = max(0, grip) // where in the thumb the finger holds
			if grip < 0 {
				a.selThumbTo(y) // a track click jumps the thumb to the finger
			}
			a.poke()
			break
		}
		// Past the bar: this press is on the transcript, and it aims the wheel.
		// Pressing a reasoning box focuses it, pressing anywhere else takes the
		// aim back, so the wheel scrolls the transcript until the human asks for
		// a box by name. The notch never moves focus (app.go scrollThinkBox),
		// which is what stops a box from stealing the wheel merely by sliding
		// under a stationary pointer. thinkBoxAt returns -1 for "no box", which
		// is exactly the "aim back at the transcript" value.
		a.thinkFocus = a.thinkBoxAt(y)
		a.selThumbDrag = false
		// handleClick tracks click count from the previous
		// release and starts a word/line selection on double/triple
		// click, or a normal drag otherwise.
		// handleClick tracks click count from the previous release and
		// starts a word/line selection on double/triple click, or a normal
		// drag otherwise. If it consumed the press (double/triple) it already
		// anchored the gesture and the click ends here: re-anchoring at the
		// raw click point would collapse the word/line selection back into a
		// no-motion click and copy nothing. A single click falls through and
		// anchors here so a following drag can expand it.
		// A click on a FILES row opens the full-width diff
		// for that file: the panel is chrome, so it takes no
		// keyboard, but a click on a changed file has to
		// reach the diff surface. (Jumping to the block
		// without opening the overlay left the click doing
		// nothing the eye could see — the file diff view
		// was unreachable.)
		if path := a.dockClick(x, y); path != "" {
			a.openDiffOverlay(path)
			a.poke()
			break
		}
		if a.handleClick(x, y) {
			break
		}
		a.selDown, a.selShown = true, true
		a.selCache = map[int]selRow{}
		a.selDocMode = false
		a.selAnchor = a.selCornerAt(x, y)
		a.selDocMode = a.selAnchor.doc >= 0
		a.selEnd = a.selCornerAt(x, y)
		a.poke()
	case btn&tcell.Button1 != 0 && a.selThumbDrag: // thumb drag on the scrollbar
		a.selThumbTo(y)
		a.poke()
	case btn&tcell.Button1 != 0 && a.selDown: // drag
		a.selAutoScroll(y) // then name the row under the pointer, post-scroll
		a.selEnd = a.selCornerAt(x, y)
		a.selShown = true
		a.poke()
	case btn == tcell.ButtonNone && a.selThumbDrag: // thumb released
		// Dropping the bar leaves the viewport where it was dragged and the
		// highlight that was already up: the gesture was never a selection.
		a.selThumbDrag = false
		a.poke()
	// A release is "no button at all", not merely "not Button1": the wheel
	// constants are separate bits (mouse.go), so a horizontal wheel report —
	// which the caller has no binding for and hands here — would otherwise end a
	// drag in flight and drop the rows the user had already covered.
	case btn == tcell.ButtonNone && a.selDown: // release
		a.selDown = false
		a.selEdgeStop()
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
	hdr := a.transcriptTop()
	if vp > 0 && (a.selDocMode || (y >= hdr && y < hdr+vp)) {
		c.doc = top + min(max(y-hdr, 0), vp-1)
	}
	return c
}

// selAutoScroll extends a held drag by one auto-scroll step when it has reached
// the transcript's top or bottom edge, then arms the timer that keeps it going:
// a terminal's edge auto-scroll is the only way a selection can cover more than
// one screen, and the pointer's own reports cannot drive it on their own (see
// selEdgeTick).
func (a *App) selAutoScroll(y int) {
	a.selEdgeFrom(y)
	a.selEdgeScroll(1) // one row per event, as a terminal's edge drag steps
}

// selEdgeAtY is which way a held selection drag is pushing the viewport: -1 at
// the transcript's first row, +1 at its last, 0 anywhere else. Below the
// transcript is +1 too — the user is still pulling the selection downward and
// the rows arriving are the ones being selected. A gesture that is not anchored
// to document rows has no viewport of its own to scroll, so it never reports a
// direction.
func (a *App) selEdgeAtY(y int) int {
	if !a.selDocMode {
		return 0
	}
	_, vp := a.selViewport()
	if vp <= 0 {
		return 0
	}
	hdr := a.transcriptTop()
	switch {
	case y <= hdr:
		return -1
	case y >= hdr+vp-1:
		return 1
	}
	return 0
}

// selEdgeScroll moves the viewport n rows in the parked direction. Callers hold
// a.mu.
func (a *App) selEdgeScroll(n int) {
	if a.selEdge == 0 {
		return
	}
	total := a.totalLinesLocked()
	_, vp := a.selViewport()
	if vp <= 0 {
		return
	}
	if a.selEdge < 0 {
		a.sm.ScrollUp(n, total, vp)
	} else {
		a.sm.ScrollDown(n, total, vp)
	}
}

// selEdgeFrom arms or disarms the timer-driven half of the edge auto-scroll: the
// pointer parked on an edge row starts the clock, a pointer merely passing
// through stops it again — which is what keeps a fast sweep across an edge row
// from turning into a page turn. Callers hold a.mu.
func (a *App) selEdgeFrom(y int) {
	d := a.selEdgeAtY(y)
	if d == 0 {
		a.selEdgeStop()
		return
	}
	if a.selEdge != d {
		a.selEdge, a.selEdgeAt = d, time.Now()
	}
}

// selEdgeStop disarms the held-drag auto-scroll (release, pointer back inside
// the transcript). Callers hold a.mu.
func (a *App) selEdgeStop() {
	a.selEdge, a.selEdgeAt = 0, time.Time{}
}

// selEdgeTick drives the held-drag auto-scroll from the UI loop: while the
// pointer sits on an edge row past the delay, every tick scrolls and extends the
// selection by the rows that just arrived. Without it the pointer's own reports
// are the only clock, and a hand that stops moving reports nothing — which is
// exactly the gesture the feature is for. Callers hold a.mu; it reports whether
// the tick did any work, so the loop knows to draw.
func (a *App) selEdgeTick() bool {
	if !a.selDown || a.selEdge == 0 {
		return false
	}
	if time.Since(a.selEdgeAt) < selEdgeDelay {
		return false
	}
	// ponytail: a release the terminal never reported (button up outside the
	// window, on an emulator that withholds it) leaves the gesture held, so this
	// scrolls to the end of the transcript and stops there — bounded, and the
	// next press takes over. The upgrade path if that ever reads as a runaway is a
	// per-gesture row cap here, not another timer.
	before := a.sm.offset
	a.selEdgeScroll(selEdgeStep)
	if a.sm.offset == before {
		return false // the transcript has no more rows that way: no repaint
	}
	a.selEnd = a.selCornerAt(a.selEnd.x, a.selEnd.y)
	return true
}

// --- scrollbar --------------------------------------------------------------

// selBarAt reports whether a press at (x, y) landed on the transcript's
// scrollbar, and where inside the thumb the finger landed. The painter publishes
// the bar's geometry every frame (like the picker's hit table), so the
// hit-test is against what is on screen rather than a re-derivation that could
// disagree with it. A press on the track beside the thumb brings the thumb to
// the finger (grip 0, top under the pointer) instead of ignoring the click —
// what every modern overlay bar does, and the one gesture that reaches a row
// further away in a single press. Callers hold a.mu.
func (a *App) selBarAt(x, y int) (int, bool) {
	// The column is the painter's, not a.width-1: with the context dock open the
	// transcript's last column sits a panel's width in from the terminal's edge,
	// and a grab keyed to the edge answers a press on the panel's border while
	// the bar under the finger ignores it — the drag that does nothing.
	if !a.selBarOn || x != a.selBarX {
		return 0, false
	}
	fy := y - a.transcriptTop() // the bar's own row space: 0 = first track row
	if fy < 0 || fy >= a.selBarVP {
		return 0, false
	}
	if grip := fy - a.selBarPos; grip >= 0 && grip < a.selBarThumb {
		return grip, true // grabbed the thumb where it was held
	}
	return -1, true // the track: the caller brings the thumb to the finger now
}

// selThumbTo maps a pointer row on the bar to a viewport offset: the thumb's top
// sits under the finger minus the grip taken at press, so grabbing the middle of
// a long thumb and pulling keeps the middle under the pointer. The mapping
// linearly inverts the painter's placement (scroll.go Scrollbar): row `pos` of
// the bar's travel is offset maxOff-pos*maxOff/travel.
func (a *App) selThumbTo(y int) {
	travel := a.selBarVP - a.selBarThumb
	if travel <= 0 {
		return
	}
	pos := max(0, min(y-a.transcriptTop()-a.selGrab, travel))
	maxOff := max(0, a.selBarTotal-a.selBarVP)
	// Rounded, not truncated: the painter floors pos out of the offset, so a
	// truncated inverse puts the thumb a row away from the finger after every
	// move — the drift that makes a bar feel like it is fighting the pointer.
	a.sm.offset = maxOff - (pos*maxOff+travel/2)/travel
	a.sm.clamp(a.totalLinesLocked(), a.viewportLinesLocked())
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
	vy := d - top
	y := vy + a.transcriptTop()
	sr, have := selRow{}, false
	if vy >= 0 && vy < len(a.selRows) {
		sr, have = a.selRows[vy], true
	} else if c, cached := a.selCache[d]; cached {
		sr, have = c, true
	}
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
// painter recorded come from that capture, which leaves out the accent rail, its
// padding and every run the frame builders marked chrome, so a copied line is
// the text and not the decoration; the capture is viewport-relative, so the top
// bar's row is subtracted. Every other row — the top bar itself, welcome, the
// composer, status row, an open overlay, or blank space under a short
// transcript — is read back from the painted grid, trimmed of the trailing
// cells that only exist to fill the width and of the frame the row was drawn
// in (boxSelectable), which is the only way the composer's border can be left
// out of a copy: its box is painted cell by cell, never as runs.
func (a *App) selRowAt(y int) selRow {
	if a.selDockRows != nil {
		for _, r := range a.selDockRows {
			if r.y == y {
				return r
			}
		}
	}
	if vy := y - a.transcriptTop(); vy >= 0 && vy < len(a.selRows) {
		return a.selRows[vy]
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
	text, x0 := boxSelectable(a.th.Box(), strings.TrimRight(sb.String(), " "), 0)
	return selRow{text: text, x0: x0}
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
