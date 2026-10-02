package tui

import (
	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// tabHit is one clickable rectangle of the tab strip: the open session it
// names, and whether the cell under the pointer was its close × rather than
// its label. Published every frame next to the status pills' hit table, for
// the same reason: a strip that dropped a tab for width must not leave last
// frame's rectangle live.
type tabHit struct {
	id      string
	rect    panelRect
	isClose bool
}

// currentTabID is the open session a chord means by "this one". The snapshot
// is ordered and exactly one entry is current; "" means the set is empty, and
// the caller must treat that as a notice rather than closing "no session".
func currentTabID(tabs []TabInfo) string {
	for _, t := range tabs {
		if t.Current {
			return t.ID
		}
	}
	return ""
}

// tabStripVisible reports whether the strip owns its row. One open session
// does not earn a permanent row: the status row already says everything it
// could, and a line spent on one word is a line the transcript loses.
func (a *App) tabStripVisible() bool { return len(a.tabs) >= 2 }

// tabStripRow is the row the strip owns, directly under the top bar. It is
// drawn only when more than one session is open — with a single session the
// status row already says everything the strip could, and a permanent row
// spent on one word costs the transcript a line for nothing.
const tabStripRow = 1

// drawTabStrip paints the open-session strip on row tabStripRow: one cell per
// session, the running one wearing ✦, an unread one dim-bright with a ✦, and
// the current one underlined so the eye finds it without a colour. Caller
// holds a.mu.
func (a *App) drawTabStrip(s tcell.Screen, w int) {
	a.tabHits = nil
	if len(a.tabs) < 2 {
		return
	}
	y := tabStripRow
	labelSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.TextSecondary)))
	currentSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.TextPrimary))).Bold(true)
	runningSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentRunning)))
	closeSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim)))

	x := 0
	for _, t := range a.tabs {
		label := t.Title
		if label == "" {
			label = shortID(t.ID)
		}
		// A fixed-width label is what makes the strip readable: a title that
		// claims 40 cells squeezes every other tab off the row, and the set
		// you cannot see is the set you cannot switch to.
		label = truncateCells(label, 24, "…")
		badge := " "
		if t.Running {
			badge = "✦"
		}
		st := labelSt
		switch {
		case t.Running:
			st = runningSt
		case t.Current:
			st = currentSt
		}
		cellW := paintedWidth(badge) + paintedWidth(label) + 3 // badge, gap, ×, gap
		if x+cellW >= w-1 {
			break // the row is full: the rest stays reachable through /tabs
		}
		rect := panelRect{x: x, y: y, w: cellW, h: 1}
		if t.Current {
			// Underline rather than invert: an inverted band would repaint
			// the cells the transcript's own bands own on the row below.
			st = st.Underline(true)
		}
		drawText(s, x, y, badge, st)
		drawText(s, x+1, y, " "+label+" ", st)
		drawText(s, x+1+1+paintedWidth(label)+1, y, "×", closeSt)
		a.tabHits = append(a.tabHits,
			tabHit{id: t.ID, rect: rect},
			tabHit{id: t.ID, rect: panelRect{x: rect.x + rect.w - 2, y: y, w: 2, h: 1}, isClose: true},
		)
		x += cellW
	}
}

// handleTabStripMouse routes a click on the strip: the × closes that session,
// anywhere else on the cell focuses it. A modal is asked first, so a strip
// under an open picker is not a way to change session behind the picker's
// back. Caller holds no lock; it takes what it needs.
func (a *App) handleTabStripMouse(m *tcell.EventMouse, press bool) bool {
	if !press {
		return false
	}
	x, y := m.Position()
	a.mu.Lock()
	defer a.mu.Unlock()
	hit, ok := a.tabHitAt(x, y)
	if !ok {
		return false
	}
	if hit.isClose {
		a.closeTab(hit.id)
		return true
	}
	// Focus through the same path /tabs uses, so a click and Enter on a row
	// cannot diverge: one callback, one owner of the tabset.
	if a.onTabPick != nil {
		go func() {
			if err := a.onTabPick(hit.id); err != nil {
				a.AddSystemBlock("error: " + err.Error())
				a.poke()
			}
		}()
	}
	return true
}

// tabHitAt returns the strip cell under (x,y), close-rectangles winning over
// the wider label rectangle they sit inside. Caller holds a.mu.
func (a *App) tabHitAt(x, y int) (tabHit, bool) {
	for _, h := range a.tabHits {
		if h.isClose && h.rect.contains(x, y) {
			return h, true
		}
	}
	for _, h := range a.tabHits {
		if !h.isClose && h.rect.contains(x, y) {
			return h, true
		}
	}
	return tabHit{}, false
}

// closeTab asks cmd to close one open session and reports what happened. It
// is the single path both close affordances use — the session.delete chord
// and the strip's × — so they cannot drift: an unwired host says so once,
// rather than leaving a chord that looks like it works.
func (a *App) closeTab(id string) {
	if a.onTabClose == nil {
		a.AddSystemBlock("session tabs are not wired in this build")
		return
	}
	if id == "" {
		return
	}
	if err := a.onTabClose(id); err != nil {
		a.AddSystemBlock("error: " + err.Error())
	}
	a.poke()
}
