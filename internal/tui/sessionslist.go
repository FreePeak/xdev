package tui

import (
	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// sessionsListMaxRows caps the strip so a long list cannot eat the transcript.
// The focused window keeps the rest of the screen.
const sessionsListMaxRows = 6

// childrenLocked is every subagent child the session is tracking, in spawn
// order: running and settled alike, because a settled child's transcript is
// still readable and the list is the way back to it. Caller holds a.mu.
func (a *App) childrenLocked() []*SubActivity {
	var out []*SubActivity
	for i := range a.blocks {
		b := a.blocks[i]
		if b.Kind != KindTool || b.ToolName != taskToolName {
			continue
		}
		out = append(out, b.Sub...)
	}
	return out
}

// sessionsListRows is the rows the strip below the status line owns: one for
// the focused main session, then one per child. Zero when the session never
// spawned a child — a list of nothing is a line the transcript needed.
func (a *App) sessionsListRows() int {
	n := len(a.childrenLocked()) + 1
	if n > sessionsListMaxRows {
		n = sessionsListMaxRows
	}
	if len(a.childrenLocked()) == 0 {
		return 0
	}
	return n
}

// drawSessionsList paints the sessions strip below the status row: the focused
// main session on top, then one row per subagent child. No header row — the
// rows ARE the list. Caller holds a.mu.
func (a *App) drawSessionsList(y, rows int) {
	if rows <= 0 {
		return
	}
	st := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim)))
	selSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentRunning)))
	maxCells := a.rightEdge()

	line := func(label, status string, sel bool) string {
		mark := "  "
		if sel {
			mark = "› "
		}
		return fitWidth("  "+mark+label+"  "+status, maxCells)
	}

	rr := y
	mainTitle := a.currentTitleLocked()
	if mainTitle != "" {
		drawText(a.scr, 0, rr, line(mainTitle, "main", a.sessionsSel == 0), st)
		rr++
	}
	for i, c := range a.childrenLocked() {
		if rr >= y+rows {
			break
		}
		label := c.Label
		if label == "" {
			label = c.Agent
		}
		if label == "" {
			label = "task"
		}
		status := c.Status
		if status == "" {
			status = "idle"
		}
		style := st
		sel := a.sessionsSel == i+1
		if sel {
			style = selSt
		}
		drawText(a.scr, 0, rr, line(label, status, sel), style)
		rr++
	}
}

// currentTitleLocked is the focused session's title, or its id when it has
// none. Caller holds a.mu.
func (a *App) currentTitleLocked() string {
	for _, t := range a.tabs {
		if !t.Current {
			continue
		}
		if t.Title != "" {
			return t.Title
		}
		return t.ID
	}
	return ""
}

// sessionsListKey drives the strip's cursor with ↑/↓. It returns false when the
// strip owns no rows, so the arrows keep scrolling the transcript in a session
// with no children — the strip never steals a key it cannot act on. Caller
// holds a.mu.
func (a *App) sessionsListKey(key tcell.Key) bool {
	n := a.sessionsListRows()
	switch key {
	case tcell.KeyUp:
		if n == 0 {
			return false
		}
		if a.sessionsSel > 0 {
			a.sessionsSel--
		}
		return true
	case tcell.KeyDown:
		if n == 0 {
			return false
		}
		if a.sessionsSel < n-1 {
			a.sessionsSel++
		}
		return true
	}
	return false
}
