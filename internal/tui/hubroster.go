package tui

import (
	"fmt"
	"strings"
	"sync"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// Agent Hub roster inspector (issue #37): a live overlay of the session's
// background subagents — status/model/activity/cost — with kill (k),
// revive (r), and an on-demand transcript view (Enter, incremental fetch
// via hub.Transcript(id, fromSeq)). Data and actions live in the hub
// (wired from cmd); the TUI owns selection and rendering only — the same
// split as the session picker.

// HubAgent is one roster row, preformatted by cmd from hub.Roster().
type HubAgent struct {
	ID, Name, Status, Model, Activity, Cost string
}

// HubTranscriptLine is one row of a child's transcript.
type HubTranscriptLine struct {
	Role string
	Text string
}

// HubOps wires the roster to the session's hub (lives in cmd; nil ops
// degrade /hub to a notice instead of new TUI-side machinery).
type HubOps struct {
	Roster     func() []HubAgent
	Transcript func(id string, fromSeq int) ([]HubTranscriptLine, int, bool)
	Kill       func(id string) bool
	Park       func(id string) bool
	Revive     func(id string) bool
}

// hubViewRows caps the transcript panel height (rows).
const hubViewRows = 12

// hubRosterUI is the overlay's live state (selection + transcript view).
type hubRosterUI struct {
	rows    []HubAgent
	sel     int
	viewID  string
	view    []HubTranscriptLine
	viewTop int
	msg     string // transient feedback line (kill/revive results)
}

// The App struct lives in app.go, which another agent owns this wave, so
// the roster state hangs off a registry keyed by *App. Apps are
// process-lifetime singletons, so nothing is reclaimed mid-run; the
// integrator can fold the ops + ui fields onto App and delete this map.
var (
	hubRegMu sync.Mutex
	hubReg   = map[*App]*hubRosterState{}
)

type hubRosterState struct {
	ops *HubOps
	ui  hubRosterUI
}

func (a *App) hubState() *hubRosterState {
	hubRegMu.Lock()
	defer hubRegMu.Unlock()
	return hubReg[a]
}

func (a *App) setHubState(s *hubRosterState) {
	hubRegMu.Lock()
	defer hubRegMu.Unlock()
	hubReg[a] = s
}

// SetHubOps wires the /hub roster to the session's hub surface.
func (a *App) SetHubOps(ops *HubOps) {
	a.setHubState(&hubRosterState{ops: ops})
}

// HubRosterOpen reports whether the roster overlay is on screen.
func (a *App) HubRosterOpen() bool {
	st := a.hubState()
	return st != nil && len(st.ui.rows) > 0
}

// HubRoster implements CommandAPI /hub: open the live roster overlay.
func (a *App) HubRoster() error {
	st := a.hubState()
	if st == nil || st.ops == nil || st.ops.Roster == nil {
		a.AddSystemBlock("hub: no agent hub wired for this session")
		return nil
	}
	rows := st.ops.Roster()
	if len(rows) == 0 {
		a.AddSystemBlock("hub: no background agents")
		return nil
	}
	hubRegMu.Lock()
	st.ui = hubRosterUI{rows: rows}
	hubRegMu.Unlock()
	a.poke()
	return nil
}

// handleHubRosterKey routes keys while the roster is open. Returns
// handled=true when the key belonged to the overlay (modal: unmatched runes
// still fall through to the editor, matching the picker's discipline).
func (a *App) handleHubRosterKey(key *tcell.EventKey) (handled bool) {
	st := a.hubState()
	if st == nil || st.ops == nil || len(st.ui.rows) == 0 {
		return false
	}
	ops := st.ops
	hubRegMu.Lock()
	ui := &st.ui
	refresh := true
	switch key.Key() {
	case tcell.KeyUp:
		handled = true
		if ui.viewID != "" {
			if ui.viewTop > 0 {
				ui.viewTop--
			}
		} else if ui.sel > 0 {
			ui.sel--
		}
	case tcell.KeyDown:
		handled = true
		if ui.viewID != "" {
			if ui.viewTop < len(ui.view)-1 {
				ui.viewTop++
			}
		} else if ui.sel < len(ui.rows)-1 {
			ui.sel++
		}
	case tcell.KeyEnter:
		handled = true
		id := ui.rows[ui.sel].ID
		switch {
		case ui.viewID == id:
			ui.viewID, ui.view, ui.viewTop = "", nil, 0
		case ops.Transcript == nil:
			ui.msg = "no transcript source wired"
		default:
			lines, total, ok := ops.Transcript(id, 0)
			if !ok {
				ui.msg = "no transcript for " + id
			} else {
				ui.viewID = id
				ui.view = lines
				ui.viewTop = max(0, min(len(lines), total)-hubViewRows)
				ui.msg = ""
			}
		}
	case tcell.KeyEsc:
		handled = true
		if ui.viewID != "" {
			ui.viewID, ui.view, ui.viewTop = "", nil, 0
		} else {
			*ui = hubRosterUI{} // close the overlay
			refresh = false
		}
	default:
		switch key.Rune() {
		case 'k':
			handled = true
			id := ui.rows[ui.sel].ID
			if ops.Kill != nil && ops.Kill(id) {
				ui.msg = "canceled " + id
			} else {
				ui.msg = "kill: " + id + " not running"
			}
		case 'r':
			handled = true
			id := ui.rows[ui.sel].ID
			if ops.Revive != nil && ops.Revive(id) {
				ui.msg = "revived " + id
			} else {
				ui.msg = "revive: " + id + " is not parked"
			}
		case 'p':
			handled = true
			id := ui.rows[ui.sel].ID
			if ops.Park != nil && ops.Park(id) {
				ui.msg = "parked " + id + " (r revives it)"
			} else {
				ui.msg = "park: " + id + " is still running"
			}
		}
	}
	// Statuses are live: refresh the snapshot on every handled key that
	// did not just close the overlay.
	if handled && refresh && ui.viewID == "" && ops.Roster != nil {
		ui.rows = ops.Roster()
		if ui.sel >= len(ui.rows) {
			ui.sel = max(0, len(ui.rows)-1)
		}
	}
	hubRegMu.Unlock()
	if handled {
		a.poke()
	}
	return handled
}

// drawHubRoster renders the overlay above the composer. Callers hold a.mu
// (draw does) — this must not re-lock a.mu.
func (a *App) drawHubRoster(yComposerTop int) {
	st := a.hubState()
	if st == nil || len(st.ui.rows) == 0 {
		return
	}
	hubRegMu.Lock()
	ui := st.ui
	hubRegMu.Unlock()

	s := a.scr
	w := a.width
	selSt := tcell.StyleDefault.Background(a.cellColor(a.th.Get(theme.BgHighlight)))
	rowSt := tcell.StyleDefault.Background(a.cellColor(a.th.Get(theme.BgBase)))
	brdSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.PromptBorderActive)))
	fgSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.TextPrimary)))
	dimSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.Gray)))

	if ui.viewID != "" {
		lines := ui.view
		start := min(ui.viewTop, max(0, len(lines)-1))
		end := min(len(lines), start+hubViewRows)
		visible := lines[start:end]
		h := len(visible) + 2
		y := yComposerTop - h - 2
		panelW := min(w-2, hubPanelWidth(w, visible)+3)
		fillPanelRows(s, y+1, y+h, 2, w-2, rowSt)
		hubRosterBox(s, y, h, hubPanelWidth(w, visible), brdSt)
		drawText(s, 3, y+1, rosterSnippet("transcript · "+rosterNameByID(ui, ui.viewID)+" ("+ui.viewID+")", printW(panelW, 3)), dimSt)
		for i, ln := range visible {
			drawText(s, 3, y+2+i, rosterLine(ln.Role, ln.Text, panelW), fgSt)
		}
		foot := "Esc back · ↑↓ scroll"
		if ui.msg != "" {
			foot = ui.msg
		}
		drawText(s, 3, y+h, rosterSnippet(foot, printW(panelW, 3)), dimSt)
		return
	}

	rows, sel := windowRoster(ui.rows, ui.sel, 10)
	h := len(rows) + 2
	y := yComposerTop - h - 2
	// Size the panel to the widest rendered row (field sums mis-measure
	// the padding the row format adds).
	texts := make([]string, len(rows))
	wid := 44
	for i, r := range rows {
		mark := "  "
		if i == sel {
			mark = "❯ "
		}
		texts[i] = hubAgentRow(mark, r)
		wid = max(wid, len([]rune(texts[i]))+2)
	}
	wid = min(wid, w-8)
	// Fill every panel row before writing it: the transcript underneath
	// must not bleed through the overlay (same discipline as the picker).
	panelW := min(w-2, wid+3)
	fillPanelRows(s, y+1, y+h, 2, w-2, rowSt)
	hubRosterBox(s, y, h, wid, brdSt)
	drawText(s, 3, y+1, rosterSnippet("AGENT HUB — ↑↓ select · Enter transcript · k kill · p park · r revive · Esc close", printW(panelW, 3)), dimSt)
	for i, text := range texts {
		rowStyle := rowSt
		if i == sel {
			rowStyle = selSt
			fillPanelRows(s, y+2+i, y+2+i, 2, w-2, selSt)
		}
		drawText(s, 3, y+2+i, rosterSnippet(text, printW(panelW, 3)), rowStyle)
	}
	drawText(s, 3, y+h, rosterSnippet(ui.msg, printW(panelW, 3)), dimSt)
}

// fillPanelRows blanks rows [y0, y1] across [x0, x1) with st.
func fillPanelRows(s tcell.Screen, y0, y1, x0, x1 int, st tcell.Style) {
	for y := y0; y <= y1; y++ {
		for x := x0; x < x1; x++ {
			s.SetContent(x, y, ' ', nil, st)
		}
	}
}

// printW is the rune budget left of x in a panel of width panelW.
func printW(panelW, x int) int {
	return max(1, panelW-x-1)
}

// windowRoster returns the visible slice of rows (at most n) around sel,
// with sel rebased onto the window.
func windowRoster(rows []HubAgent, sel, n int) ([]HubAgent, int) {
	if len(rows) <= n {
		return rows, sel
	}
	start := min(max(0, sel-n/2), len(rows)-n)
	return rows[start : start+n], sel - start
}

// hubRosterBox draws the overlay's rounded border; the panel occupies
// y..y+h+1 with content rows y+1..y+h.
func hubRosterBox(s tcell.Screen, y, h, wid int, st tcell.Style) {
	edge := "─"
	top := "╭" + strings.Repeat(edge, max(1, wid)) + "╮"
	bot := "╰" + strings.Repeat(edge, max(1, wid)) + "╯"
	drawText(s, 2, y, top, st)
	drawText(s, 2, y+h+1, bot, st)
}

// hubPanelWidth sizes the transcript panel to its content.
func hubPanelWidth(w int, lines []HubTranscriptLine) int {
	wid := 40
	for _, ln := range lines {
		wid = max(wid, len([]rune(ln.Role))+len([]rune(ln.Text))+4)
	}
	return min(wid, w-8)
}

// hubAgentRow renders one roster row: id status model cost name — activity.
func hubAgentRow(mark string, r HubAgent) string {
	return fmt.Sprintf("%s%-8s %-8s %-14s %8s  %s — %s",
		mark, r.ID, r.Status, r.Model, r.Cost, r.Name, r.Activity)
}

// rosterLine renders one transcript row: "role  text".
func rosterLine(role, text string, w int) string {
	return rosterSnippet(role+": "+text, w-6)
}

// rosterNameByID finds the display name for an id in the roster rows.
func rosterNameByID(ui hubRosterUI, id string) string {
	for _, r := range ui.rows {
		if r.ID == id {
			return r.Name
		}
	}
	return id
}

// rosterSnippet trims s to max runes on one line.
func rosterSnippet(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) > max {
		if max <= 1 {
			return string(r[:1])
		}
		return string(r[:max-1]) + "…"
	}
	return s
}
