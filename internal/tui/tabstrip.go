package tui

import (
	"fmt"
	"strconv"
	"strings"

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

// tabStripVisible reports whether the strip owns its row. Two gates, both
// named: tui.tabs.mode off hides it (the chords and /tabs keep working — the
// strip is a view), and one open session does not earn a permanent row: the
// status row already says everything it could, and a line spent on one word
// is a line the transcript loses.
func (a *App) tabStripVisible() bool { return a.tabsStrip && len(a.tabs) >= 2 }

// tabStripRow is the row the strip owns — row 0, the top row of the screen.
// It is drawn only when more than one session is open: with a single session
// the status row already says everything the strip could, and a permanent row
// spent on one word costs the transcript a line for nothing.
const tabStripRow = 0

// drawTabStrip paints the open-session strip on row tabStripRow: one cell per
// session, the running one wearing ✦, an unread one dim-bright with a ✦, and
// the current one underlined so the eye finds it without a colour. Caller
// holds a.mu.
func (a *App) drawTabStrip(s tcell.Screen, w int) {
	a.tabHits = nil
	if !a.tabStripVisible() {
		return
	}
	y := tabStripRow
	labelSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.TextSecondary)))
	currentSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.TextPrimary))).Bold(true)
	runningSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentRunning)))
	closeSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim)))

	x := 0
	for i, t := range a.tabs {
		label := t.Title
		if label == "" {
			label = shortID(t.ID)
		}
		// A fixed-width label is what makes the strip readable: a title that
		// claims 40 cells squeezes every other tab off the row, and the set
		// you cannot see is the set you cannot switch to.
		label = truncateCells(label, 24, "…")
		// The badge is the tab's number under tui.tabs.indicators: numbers,
		// and its status glyph otherwise (opencode's own default). The number
		// is what the C-1..9 chords name, so it is not decoration: it is the
		// legend for the keys.
		badge := " "
		switch {
		case a.tabsNumbered:
			badge = fmt.Sprintf("%d", i+1)
		case t.Running:
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
//
// The host callbacks fire UNLOCKED, and that is the whole point of the
// structure: the host is cmd, and closing a tab calls app.SetTabs, which
// takes a.mu itself. A `defer a.mu.Unlock()` around the lookup made the ×
// close path call SetTabs with the lock ALREADY HELD on the same goroutine —
// sync.Mutex is not reentrant, so the loop blocked forever and the watchdog
// killed the process 90s later (stall dump tui-stall-20261002-151321, pid
// 80950: goroutine 1 [sync.Mutex.Lock] in SetTabs <- closeTabByID <-
// closeTab <- handleTabStripMouse). So the lock is taken to read the hit and
// released before anything can call out.
func (a *App) handleTabStripMouse(m *tcell.EventMouse, press bool) bool {
	if !press {
		return false
	}
	x, y := m.Position()
	a.mu.Lock()
	hit, ok := a.tabHitAt(x, y)
	pick := a.onTabPick
	a.mu.Unlock()
	if !ok {
		return false
	}
	if hit.isClose {
		a.closeTab(hit.id)
		return true
	}
	// Focus through the same path /tabs uses, so a click and Enter on a row
	// cannot diverge: one callback, one owner of the tabset. The callback runs
	// on its own goroutine because the host's focus path rebuilds the whole
	// view (Reset + replay) — far too long to hold the UI loop for.
	if pick != nil {
		go func() {
			if err := pick(hit.id); err != nil {
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
// rather than leaving a chord that looks like it works. Caller holds no lock:
// the host's close path republishes the tabset (SetTabs), which takes a.mu.
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

// tabSelectIndex reads the N out of a "session.tab.select.N" action id,
// reporting false for anything else. The default dispatch arm calls it, so an
// action that is not a tab jump falls through untouched.
func tabSelectIndex(action string) (int, bool) {
	rest, ok := strings.CutPrefix(action, tabSelectPrefix)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 1 || n > len(tabSelectDigits) {
		return 0, false
	}
	return n, true
}

// selectTab focuses the Nth open session (opencode's session_tab_select_N:
// Ctrl+1..9, with 0 the tenth). It goes through onTabPick — the same
// callback a strip click and a /tabs row use — so all three ways of naming a
// session reach one owner of the tabset, and the host's rebuild runs off the
// UI loop exactly as the click path runs it. A chord aimed at a slot that is
// not open says so; it never no-ops.
func (a *App) selectTab(n int) {
	a.mu.Lock()
	tabs := append([]TabInfo(nil), a.tabs...)
	pick := a.onTabPick
	a.mu.Unlock()
	if pick == nil {
		a.AddSystemBlock("session tabs are not wired in this build")
		return
	}
	if n > len(tabs) {
		a.AddSystemBlock(fmt.Sprintf("only %d sessions open", len(tabs)))
		a.poke()
		return
	}
	id := tabs[n-1].ID
	go func() {
		if err := pick(id); err != nil {
			a.AddSystemBlock("error: " + err.Error())
		}
		a.poke()
	}()
}
