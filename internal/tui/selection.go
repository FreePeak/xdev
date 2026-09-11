package tui

import (
	"strings"

	"github.com/gdamore/tcell/v2"
)

// Mouse text selection, in the spirit of grok CLI's welcome/transcript
// chrome and omp's clipboard primitive: drag across the transcript with
// the primary button, release copies the covered text to the clipboard.
// Works because the screen already runs with mouse capture enabled
// (EnableMouse in cmd/xdev/tui.go), so button events reach the app.

type selPoint struct{ x, y int }

// selRow is one transcript screen row of the last frame: the rendered
// text and the screen column where it starts (3 = rail+pad, 0 = user ❯
// band rows, which carry the marker inline).
type selRow struct {
	text string
	x0   int
}

// handleMouse routes mouse events for selection. Wheel stays with the
// scroll model; the primary button drives the drag-select lifecycle
// (press → drag with button held → release on ButtonNone). Caller: UI
// thread (handleKey).
func (a *App) handleMouse(m *tcell.EventMouse) {
	x, y := m.Position()
	btn := m.Buttons()
	switch {
	case btn&tcell.Button1 != 0 && !a.selActive: // press
		if y >= a.height-4 || len(a.blocks) == 0 {
			return // composer / welcome: nothing selectable
		}
		a.selActive = true
		a.selAnchor, a.selEnd = selPoint{x, y}, selPoint{x, y}
		a.poke()
	case btn&tcell.Button1 != 0 && a.selActive: // drag
		a.selEnd = selPoint{x, y}
		a.poke()
	case btn&tcell.Button1 == 0 && a.selActive: // release
		a.selActive = false
		if text := a.selectionText(); strings.TrimSpace(text) != "" {
			_ = a.copyToClipboard(text)
		}
		a.poke()
	}
	// Remaining buttons (right/middle, bare motion) are ignored; wheel
	// was already handled by the caller.
}

// selectionRect returns the normalized selection rectangle, clamped to
// the transcript region (rows above the composer).
func (a *App) selectionRect() (x0, y0, x1, y1 int) {
	x0, x1 = min(a.selAnchor.x, a.selEnd.x), max(a.selAnchor.x, a.selEnd.x)
	y0, y1 = min(a.selAnchor.y, a.selEnd.y), max(a.selAnchor.y, a.selEnd.y)
	if yMax := a.height - 5; y1 > yMax {
		y1 = yMax
	}
	if y0 < 0 {
		y0 = 0
	}
	return x0, y0, x1, y1
}

// selectionText reconstructs the transcript text under the selection
// rectangle from the last frame's rendered rows, with terminal-linear
// shape: the anchor row runs to the end of its line, the end row starts
// at its line start, middle rows are taken in full. Wide runes are
// included when their first cell is covered.
func (a *App) selectionText() string {
	if len(a.selRows) == 0 {
		return ""
	}
	_, y0, _, y1 := a.selectionRect()
	ax, bx := a.selAnchor.x, a.selEnd.x
	if a.selAnchor.y > a.selEnd.y {
		ax, bx = bx, ax // drag went upward: swap the row endpoints
	}
	var sb strings.Builder
	for y := y0; y <= y1; y++ {
		if y >= len(a.selRows) {
			break
		}
		sr := a.selRows[y]
		lo, hi := sr.x0, sr.x0+width(sr.text)
		switch {
		case y == y0 && y == y1:
			lo, hi = min(ax, bx), max(ax, bx)
		case y == y0:
			lo = ax
		case y == y1:
			hi = bx
		}
		if y > y0 {
			sb.WriteByte('\n')
		}
		if sr.x0 > lo {
			lo = sr.x0 // never reach left of the content
		}
		sb.WriteString(cellSlice(sr.text, lo-sr.x0, hi-sr.x0))
	}
	return strings.TrimRight(sb.String(), " \n\t")
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

// drawSelection paints the live highlight: reverse video over the
// selected cells. Caller: draw (mu held).
func (a *App) drawSelection() {
	if !a.selActive {
		return
	}
	x0, y0, x1, y1 := a.selectionRect()
	ax, bx := a.selAnchor.x, a.selEnd.x
	if a.selAnchor.y > a.selEnd.y {
		ax, bx = bx, ax
	}
	for y := y0; y <= y1 && y < a.height-4; y++ {
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

