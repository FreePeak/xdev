package tui

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
)

// Mouse text selection: drag anywhere on screen, release copies the covered
// text to the clipboard. The screen runs with mouse capture enabled
// (EnableMouse in cmd/xdev/tui.go), so button events reach the app; the
// transcript's own wheel scroll lives in handleKey.
//
// The gesture follows what a terminal's native selection does, because that
// is the behavior an omp user has: the highlight stays up after the release
// (until the next click), a click without a drag clears it instead of
// writing an empty string over the clipboard, and the copy is confirmed on
// the composer's info divider. Selection covers every row the app paints —
// transcript, welcome, composer, status, overlays — not just the transcript.
//
// Shift is the native-selection escape hatch: omp documents it ("native text
// selection becomes Shift+drag") and so does every terminal that implements
// mouse reporting — while the app holds the mouse, Shift+drag means "the
// terminal selects and copies, not the app". The SGR reports still reach us
// with ModShift set, so the app has to decline them: otherwise the
// release-time app copy clobbers the terminal's native selection copy, and
// the paste comes out wrong. Same contract as omp and Claude Code.
//
// ponytail: a selection is screen-relative, so it cannot span more than one
// viewport — dragging to the top or bottom row does not scroll, and a
// transcript that streams while a drag is in flight re-renders under the
// highlight. Upgrade path: anchor rows in index space (a.selRow0 + screen y)
// and auto-scroll on edge drags; that needs the scroll model to report its
// start row to the selection code, which it currently keeps private.

// selPoint is a mouse point in screen cells.
type selPoint struct{ x, y int }

// selRow is one selectable screen row: the rendered text and the screen
// column where it starts (3 = rail+pad for transcript rows, 0 for rows read
// back from the grid, whose leading cells are part of the layout).
type selRow struct {
	text string
	x0   int
}

// selGrace is how long the copy confirmation stays on the divider.
const selGrace = 2 * time.Second

// handleMouse routes mouse events for selection. Wheel stays with the scroll
// model (its caller). The primary button drives the drag lifecycle: press
// starts it, a drag with the button held extends it, release on ButtonNone
// finishes it and copies. Caller: UI thread (handleKey). Shift-modified
// events are declined outright — see the file comment: they belong to the
// terminal's native selection.
func (a *App) handleMouse(m *tcell.EventMouse) {
	if m.Modifiers()&tcell.ModShift != 0 {
		// Decline the gesture, and drop an in-flight one: a plain drag
		// that picks up Shift mid-stroke gets no release we can act on
		// (tcell reports it Shift-modified too), so the highlight would
		// stay stuck on screen while the terminal makes its own
		// selection on top of ours.
		if a.selDown || a.selShown {
			a.selDown, a.selShown = false, false
			a.selAnchor, a.selEnd = selPoint{}, selPoint{}
			a.poke()
		}
		return
	}
	x, y := m.Position()
	btn := m.Buttons()
	switch {
	case btn&tcell.Button1 != 0 && !a.selDown: // press
		// A new press always takes over, held selection or not: the
		// release of a drag whose terminal never reported the button up
		// (released outside the window) must not leave the app wedged.
		a.selDown, a.selShown = true, true
		a.selAnchor, a.selEnd = selPoint{x, y}, selPoint{x, y}
		a.poke()
	case btn&tcell.Button1 != 0 && a.selDown: // drag
		a.selEnd = selPoint{x, y}
		a.selShown = true
		a.poke()
	case btn&tcell.Button1 == 0 && a.selDown: // release
		a.selDown = false
		text := a.selectionText()
		if strings.TrimSpace(text) == "" {
			// A click, not a drag: clear the highlight and leave the
			// clipboard alone, exactly like every terminal does.
			a.selShown = false
		} else if a.copyToClipboard(text) == nil {
			a.selNotice = "Copied " + strconv.Itoa(utf8.RuneCountInString(text)) + " chars"
			a.selNoticeUntil = time.Now().Add(selGrace)
		}
		a.poke()
	}
	// Remaining buttons (right/middle, bare motion) are ignored; wheel
	// was already handled by the caller.
}

// selectionRect returns the normalized selection rectangle, clamped to the
// screen.
func (a *App) selectionRect() (x0, y0, x1, y1 int) {
	x0, x1 = min(a.selAnchor.x, a.selEnd.x), max(a.selAnchor.x, a.selEnd.x)
	y0, y1 = min(a.selAnchor.y, a.selEnd.y), max(a.selAnchor.y, a.selEnd.y)
	return max(x0, 0), max(y0, 0), min(x1, a.width-1), min(y1, a.height-1)
}

// copyHint returns the copy confirmation while it is still fresh. Once its
// deadline passes the notice is dropped here, so no later draw can resurrect
// it; the caller falls back to the viewport hint.
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

// selectionText reconstructs the text under the selection rectangle with
// terminal-linear shape: the anchor row runs to the end of its line, the end
// row starts at its line start, middle rows are taken in full. Wide runes are
// included when their first cell is covered.
func (a *App) selectionText() string {
	_, y0, _, y1 := a.selectionRect()
	if y1 < y0 {
		return ""
	}
	ax, bx := a.selAnchor.x, a.selEnd.x
	if a.selAnchor.y > a.selEnd.y {
		ax, bx = bx, ax // drag went upward: swap the row endpoints
	}
	var sb strings.Builder
	for y := y0; y <= y1; y++ {
		sr := a.selRowAt(y)
		lo, hi := sr.x0, sr.x0+width(sr.text)
		switch {
		case y == y0 && y == y1:
			lo, hi = min(ax, bx), max(ax, bx)
		case y == y0:
			lo = ax
		case y == y1:
			hi = bx
		}
		if sr.x0 > lo {
			lo = sr.x0 // never reach left of the content
		}
		if y > y0 {
			sb.WriteByte('\n')
		}
		// hi is a covered cell, not one past it: the same inclusive bound
		// drawSelection paints, so the copy is what the highlight showed.
		sb.WriteString(cellSlice(sr.text, lo-sr.x0, hi-sr.x0+1))
	}
	return strings.TrimRight(sb.String(), " \n\t")
}

// selRowAt returns the selectable content of screen row y. Rows the
// transcript painter recorded come from that capture, which leaves out the
// accent rail and its padding so a copied line is the text and not the
// decoration. Every other row — welcome screen, composer, status row, an
// open overlay, or blank space under a short transcript — is read back from
// the painted grid, trimmed of the trailing cells that only exist to fill
// the width.
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

// cellSlice returns the runes of text occupying cells [a, b) (cells
// counted by display width, wide runes counted by their first cell).
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

// drawSelection paints the live highlight: reverse video over the selected
// cells. It stays up after the release, until the next click hands the mouse
// to a new gesture or Shift hands it to the terminal. Caller: draw (mu held).
func (a *App) drawSelection() {
	if !a.selShown {
		return
	}
	x0, y0, x1, y1 := a.selectionRect()
	ax, bx := a.selAnchor.x, a.selEnd.x
	if a.selAnchor.y > a.selEnd.y {
		ax, bx = bx, ax
	}
	for y := y0; y <= y1 && y < a.height; y++ {
		lo, hi := x0, x1
		switch {
		case y == y0 && y == y1:
			lo, hi = min(ax, bx), max(ax, bx)
		case y == y0:
			lo = ax
		case y == y1:
			hi = bx
		}
		for x := lo; x <= hi && x < a.width; x++ {
			mainc, combc, style, _ := a.scr.GetContent(x, y)
			a.scr.SetContent(x, y, mainc, combc, style.Reverse(true))
		}
	}
}
