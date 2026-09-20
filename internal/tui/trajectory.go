package tui

import (
	"fmt"
	"strings"
	"sync"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// Trajectory ledger (deepseek-harness ui-trajectory parity): a turn-aware list
// of the session's materialized records — prompts, reasoning, tool calls
// and their results, compactions — where a row is a one-line preview and Enter
// opens the record's inspector, the way the reference keeps prose, payloads,
// timing and token usage in a local details pane instead of widening every row.
//
// Same split as the hub roster: the data lives in cmd (which owns the session
// store) and this file owns selection, the inspector and rendering only.
//
// Deliberate scope cut from the reference: no per-record request/response
// envelopes, no virtualized history paging, no search index — xdev's session
// entries are already in memory and a terminal viewport is one screen, so those
// are a web ledger's costs, not a TUI's. The row vocabulary (kind tag + preview
// + duration, inspector for the body) is the part that carries over.

// TrajectoryRecord is one ledger row, precomputed by cmd from the session
// store. Text is the one-line preview; Detail is the full body the inspector
// shows, and Meta the machine facts (tokens, timing) that ride beside it.
type TrajectoryRecord struct {
	Index  int    // 1-based position, painted as #N
	Kind   string // system | user | context | compacted | message | tool | subtool
	Text   string // one-line preview
	Detail string // full body for the inspector ("" → Text)
	Meta   string // machine facts: tokens, duration, exit status
	Turn   bool   // opens a new turn (painted with a heavier rule above)
}

// TrajectoryOps wires the ledger to the session store (lives in cmd; nil ops
// degrade /trajectory to a notice instead of new TUI-side store machinery).
type TrajectoryOps struct {
	Records func() []TrajectoryRecord
}

// trajMaxRows caps the ledger's visible rows (the inspector pane then gets
// the rest of the panel).
const trajMaxRows = 14

// trajectoryUI is the overlay's live state.
type trajectoryUI struct {
	open bool
	rows []TrajectoryRecord
	// sel is the selection within rows; inspect opens the details pane for the
	// selected record (Esc steps back to the ledger before closing).
	sel     int
	inspect bool
	// top is the ledger's first visible row when rows outnumber the window.
	top int
	// mx/my are the panel's screen origin as last painted, so the mouse
	// hit-test resolves against the frame the user saw rather than a
	// re-derivation that could disagree with it.
	mx, my int
	// hitIdx maps screen row offset → index into rows (published per frame).
	hitIdx []int
}

var (
	trajRegMu sync.Mutex
	trajReg   = map[*App]*trajectoryState{}
)

type trajectoryState struct {
	ops *TrajectoryOps
	ui  trajectoryUI
}

func (a *App) trajState() *trajectoryState {
	trajRegMu.Lock()
	defer trajRegMu.Unlock()
	return trajReg[a]
}

// SetTrajectoryOps wires the ledger to its record source.
func (a *App) SetTrajectoryOps(ops *TrajectoryOps) {
	trajRegMu.Lock()
	trajReg[a] = &trajectoryState{ops: ops}
	trajRegMu.Unlock()
}

// TrajectoryOpen reports whether the ledger overlay is on screen.
func (a *App) TrajectoryOpen() bool {
	st := a.trajState()
	return st != nil && st.ui.open
}

// OpenTrajectory shows the ledger, focused on the newest record, and reports
// whether it opened: the sidebar button shares this with /trajectory, and a
// ledger with nothing to list must not paint an empty frame over the screen.
func (a *App) OpenTrajectory() bool {
	st := a.trajState()
	if st == nil || st.ops == nil || st.ops.Records == nil {
		return false
	}
	rows := st.ops.Records()
	if len(rows) == 0 {
		return false
	}
	trajRegMu.Lock()
	// Open on the tail: a long session opening at row 0 showed history the
	// user had already scrolled past in the transcript.
	st.ui = trajectoryUI{open: true, rows: rows, sel: len(rows) - 1}
	st.ui.top = max(0, len(rows)-trajMaxRows)
	trajRegMu.Unlock()
	a.poke()
	return true
}

// CloseTrajectory dismisses the overlay.
func (a *App) CloseTrajectory() {
	trajRegMu.Lock()
	if st := trajReg[a]; st != nil {
		st.ui = trajectoryUI{}
	}
	trajRegMu.Unlock()
	a.poke()
}

// Trajectory implements CommandAPI /trajectory: open the event ledger.
func (a *App) Trajectory() error {
	if !a.OpenTrajectory() {
		a.AddSystemBlock("trajectory: no session records wired for this mode")
	}
	return nil
}

// handleTrajectoryKey routes keys while the ledger is open. Modal: every key is
// swallowed, except Ctrl+C which is never a modal's to keep (the same escape
// hatch the tree selector documents — a panel the user cannot see must not be
// able to trap the quit chord).
func (a *App) handleTrajectoryKey(key *tcell.EventKey) (handled bool) {
	if key.Key() == tcell.KeyCtrlC {
		return false
	}
	st := a.trajState()
	if st == nil || !st.ui.open {
		return false
	}
	trajRegMu.Lock()
	defer trajRegMu.Unlock()
	ui := &st.ui
	n := len(ui.rows)
	switch key.Key() {
	case tcell.KeyUp:
		if ui.sel > 0 {
			ui.sel--
			ui.clampTop()
		}
	case tcell.KeyDown:
		if ui.sel < n-1 {
			ui.sel++
			ui.clampTop()
		}
	case tcell.KeyPgUp:
		ui.sel = max(0, ui.sel-trajMaxRows)
		ui.clampTop()
	case tcell.KeyPgDn:
		ui.sel = min(n-1, ui.sel+trajMaxRows)
		ui.clampTop()
	case tcell.KeyHome:
		ui.sel, ui.top = 0, 0
	case tcell.KeyEnd:
		ui.sel = max(0, n-1)
		ui.clampTop()
	case tcell.KeyEnter:
		// Enter opens the inspector on the selected record; on an open
		// inspector it steps back to the ledger. A record with nothing in it
		// stays on the ledger rather than opening an empty pane.
		if ui.inspect {
			ui.inspect = false
		} else if ui.selected().body() != "" {
			ui.inspect = true
		}
	case tcell.KeyEsc:
		if ui.inspect {
			ui.inspect = false
		} else {
			*ui = trajectoryUI{}
		}
	default:
		switch key.Rune() {
		case 'q':
			*ui = trajectoryUI{}
		default:
			return false
		}
	}
	a.poke()
	return true
}

// clampTop keeps the selection inside the visible window.
func (ui *trajectoryUI) clampTop() {
	if ui.sel < ui.top {
		ui.top = ui.sel
	}
	if ui.sel >= ui.top+trajMaxRows {
		ui.top = ui.sel - trajMaxRows + 1
	}
}

// selected returns the record under the cursor.
func (ui *trajectoryUI) selected() TrajectoryRecord {
	if ui.sel < 0 || ui.sel >= len(ui.rows) {
		return TrajectoryRecord{}
	}
	return ui.rows[ui.sel]
}

// body is what the inspector shows: the full detail, falling back to the
// preview so a record that only ever had a one-liner still opens.
func (r TrajectoryRecord) body() string {
	if strings.TrimSpace(r.Detail) != "" {
		return r.Detail
	}
	return strings.TrimSpace(r.Text)
}

// handleTrajectoryMouse routes the wheel and clicks into the open ledger:
// wheel moves the selection (or scrolls the inspector once it is open), a
// click selects the record under it and opens its inspector.
// Caller: UI thread, mu held (like handleMouse).
func (a *App) handleTrajectoryMouse(m *tcell.EventMouse, press bool) bool {
	st := a.trajState()
	if st == nil || !st.ui.open {
		return false
	}
	trajRegMu.Lock()
	defer trajRegMu.Unlock()
	ui := &st.ui
	wheel := 0
	switch m.Buttons() {
	case tcell.WheelUp:
		wheel = -1
	case tcell.WheelDown:
		wheel = 1
	}
	if wheel != 0 {
		if ui.inspect {
			ui.top = max(0, ui.top+wheel)
		} else if d := ui.sel + wheel; d >= 0 && d < len(ui.rows) {
			ui.sel = d
			ui.clampTop()
		}
		a.poke()
		return true
	}
	if !press {
		return true // an open modal owns the release too, so nothing behind it selects
	}
	_, y := m.Position()
	if i := y - ui.my; i >= 0 && i < len(ui.hitIdx) {
		if row := ui.hitIdx[i]; row >= 0 {
			ui.sel = row
			ui.clampTop()
			ui.inspect = ui.selected().body() != ""
		}
	}
	a.poke()
	return true
}

// drawTrajectory renders the ledger (or its inspector) above the composer.
// Callers hold a.mu (draw does) — this must not re-lock a.mu.
func (a *App) drawTrajectory(yComposerTop int) {
	st := a.trajState()
	if st == nil || !st.ui.open {
		return
	}
	trajRegMu.Lock()
	ui := st.ui
	// Every frame starts with no clickable rows; the ledger republishes them,
	// so a click can never land on a stale row after the pane switched to its
	// inspector.
	st.ui.hitIdx = nil
	trajRegMu.Unlock()

	s := a.scr
	w := a.width
	brdSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.PromptBorderActive)))
	selSt := tcell.StyleDefault.Background(a.cellColor(a.th.Get(theme.BgHighlight)))
	rowSt := tcell.StyleDefault.Background(a.cellColor(a.th.Get(theme.BgBase)))
	dimSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.Gray)))
	box := a.th.Box()

	body := ui.inspectorLines()
	out := make([]string, 0, len(body)+2)
	for _, b := range body {
		out = append(out, rosterSnippet(b, max(20, w-8)))
	}
	h := min(len(out)+2, max(3, yComposerTop-4))
	y := yComposerTop - h - 2
	if y < 1 {
		y = 1
	}
	panelW := min(w-4, 100)
	fillPanelRows(s, y+1, y+h, 2, w-2, rowSt)
	hubRosterBox(s, y, h, panelW-2, box, brdSt)
	title := "TRAJECTORY — ledger"
	if ui.inspect {
		title = fmt.Sprintf("TRAJECTORY — record #%d · %s", ui.selected().Index, ui.selected().Kind)
	}
	drawText(s, 3, y+1, rosterSnippet(title+"  ("+ui.footer()+")", printW(panelW, 3)), dimSt)

	// The ledger publishes a row map, so a click selects the record the user
	// actually saw. The inspector is read-only: its rows stay unclickable and
	// the hit map is left empty.
	var idx []int
	for i := 0; i < len(out) && i+2 <= h-1; i++ {
		row := y + 2 + i
		rst := rowSt
		if !ui.inspect && ui.top+i == ui.sel {
			rst = selSt
			fillPanelRows(s, row, row, 2, w-2, selSt)
		}
		drawText(s, 3, row, rosterSnippet(out[i], printW(panelW, 3)), rst)
		if !ui.inspect {
			if ui.top+i < len(ui.rows) {
				idx = append(idx, ui.top+i)
			} else {
				idx = append(idx, -1)
			}
		}
	}
	trajRegMu.Lock()
	if st2 := trajReg[a]; st2 != nil && st2.ui.open {
		st2.ui.mx, st2.ui.my, st2.ui.hitIdx = 3, y+2, idx
	}
	trajRegMu.Unlock()
}

// footer is the one-line gesture hint for the current pane.
func (ui *trajectoryUI) footer() string {
	if ui.inspect {
		return "Esc back · ↑↓ scroll"
	}
	return "↑↓ select · Enter details · q/Esc close"
}

// inspectorLines renders the current pane: the ledger rows, or the selected
// record's body with its machine facts beneath.
func (ui *trajectoryUI) inspectorLines() []string {
	if ui.inspect {
		r := ui.selected()
		out := []string{}
		for _, l := range strings.Split(strings.TrimRight(sanitizeOutput(r.body()), "\n"), "\n") {
			out = append(out, l)
		}
		if r.Meta != "" {
			out = append(out, "")
			out = append(out, r.Meta)
		}
		return out
	}
	if len(ui.rows) == 0 {
		return []string{"no records"}
	}
	out := make([]string, 0, trajMaxRows)
	end := min(len(ui.rows), ui.top+trajMaxRows)
	for i := ui.top; i < end; i++ {
		r := ui.rows[i]
		mark := "  "
		if i == ui.sel {
			mark = "❯ "
		}
		rule := " "
		if r.Turn {
			rule = "─"
		}
		out = append(out, fmt.Sprintf("%s%s #%-4d %-9s %s", rule, mark, r.Index, r.Kind, r.Text))
	}
	if hidden := len(ui.rows) - len(out); hidden > 0 {
		out = append(out, fmt.Sprintf("  … %d more", hidden))
	}
	return out
}
