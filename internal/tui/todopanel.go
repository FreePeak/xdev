package tui

// The todo strip: the session's task list, pinned above the composer.
//
// omp's sticky HUD is the parity target (packages/coding-agent/src/
// modes/interactive-mode.ts #renderTodoList). What is taken is the SHAPE, which
// is the part the dock cannot give: the strip shows the CURRENT phase in full
// and every other phase as one line with its progress, so a plan of six phases
// reads as one screen instead of scrolling the sidebar. What is deliberately not
// taken, and why:
//
//   - omp's spine-and-tail progress path ("├─" plus a "└────" bar filled in
//     reading order). The task markers in todomarker.go already carry state in
//     the same inks, and a second progress encoding paints the same fact twice
//     in the one column the eye already scans.
//   - the completion strike animation. It needs per-frame strike bookkeeping for
//     a decoration; a completed row already recedes into the comment ink.
//   - the auto-clear timer. omp removed the version that mutated canonical state
//     from a timer for drift; here the strip simply stops painting when there is
//     no open task left, which is display-only by construction (rows == nil).
//
// Why a new surface rather than the dock: the dock (dock.go) is the scanning
// column BESIDE the transcript and already folds TASKS. It is width-gated
// (dockMinCols) and shares its row budget with PLAN and FILES, so on a narrow
// terminal — exactly when a long task list costs the most — it is not there at
// all. The strip takes the composer's width, which always exists.
//
// It is chrome, like queue.go and agentspanel.go: rows above the composer, the
// viewport arithmetic unmoved, painted before the queue so the running child
// keeps the row nearest the eye. Its sources are read per frame from the same
// snapshot the dock and /plan show read (todo.TodoTool.Snapshot), so no two
// surfaces can disagree about a task's state.

import (
	"fmt"
	"os"
	"strings"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
	"github.com/FreePeak/xdev/internal/tool"
)

// strip row budget. A phase header plus five task rows plus the summary is the
// same bounded preview the dock's lists use. ponytail: a flat cap, not a
// per-phase one — a plan whose active phase carries twelve tasks shows five and
// says "… 7 more", which is the trade a fixed strip makes; the upgrade path is
// a configurable height once someone asks for one.
const (
	todoStripRowMax   = 7  // the strip's total height, headers and summary included
	todoStripTaskCap  = 5  // open task rows shown for the active phase
	todoStripPhaseCap = 4  // following phases kept as one-line rows
	todoStripMinW     = 24 // narrower than this, the strip has no room to say anything
)

// todoStripRow is one painted row: its text and its kind, which decides the ink.
type todoStripRow struct {
	text string
	kind todoStripKind
	// marker is the task's own "[x]"/"[/]" when the row is a task; painted in
	// its state's ink (todomarker.go) with the rest of the row in the body ink.
	marker string
}

type todoStripKind int

const (
	todoStripHead  todoStripKind = iota // "TODO · 3/7"
	todoStripPhase                      // "II. Polish · 1/4" — a phase this update did not touch
	todoStripActiv                      // the phase being worked on: name + progress
	todoStripTask                       // one task row
	todoStripNote                       // "… 3 more", "… 2 more phases"
)

// todoStripLines is the strip's whole content for one snapshot: the policy,
// pure, so it is testable without a screen and so the painter has nothing left
// to decide. Empty means the strip paints nothing — the list is closed, or
// there is no list.
//
// The policy, all of it omp's:
//
//   - the ACTIVE phase (the one holding the in-progress task, else the first
//     open one) is expanded, and its open tasks are previewed up to the cap with
//     a trailing count for the rest;
//   - the phases after it are one-line rows, capped, with a count for the rest;
//   - closed phases BEFORE the active one are not listed at all: the strip is
//     what is left to do, and a plan that opens with six finished phases would
//     spend the strip on history.
func todoStripLines(phases []tool.TodoPhase, visible int) []todoStripRow {
	total, closed := 0, 0
	for _, p := range phases {
		for _, t := range p.Tasks {
			total++
			if todoClosed(t.Status) {
				closed++
			}
		}
	}
	if total == 0 || closed == total {
		return nil // nothing open: the strip retires itself
	}
	rows := []todoStripRow{{text: fmt.Sprintf("TODO · %d/%d", closed, total), kind: todoStripHead}}
	active := todoActivePhase(phases)
	if active < 0 {
		return rows
	}
	// The active phase, expanded. Its name wears the accent, so the one phase
	// the agent is in is the row the eye lands on.
	p := phases[active]
	rows = append(rows, todoStripRow{
		text: fmt.Sprintf("%s. %s · %d/%d", roman(active+1), p.Name, phaseClosed(p), len(p.Tasks)),
		kind: todoStripActiv,
	})
	open, hidden := todoOpenTasks(p, todoStripTaskCap)
	for _, t := range open {
		rows = append(rows, todoStripRow{
			text:   todoMarkerGlyph(t.Status) + " " + oneLine(t.Content),
			kind:   todoStripTask,
			marker: todoMarkerGlyph(t.Status),
		})
	}
	if hidden > 0 {
		rows = append(rows, todoStripRow{text: fmt.Sprintf("… %d more", hidden), kind: todoStripNote})
	}
	// The phases after the active one, one row each.
	var later []tool.TodoPhase
	var laterIdx []int
	for i := active + 1; i < len(phases) && len(later) < todoStripPhaseCap; i++ {
		if len(phases[i].Tasks) == 0 {
			continue
		}
		later = append(later, phases[i])
		laterIdx = append(laterIdx, i)
	}
	for k, p := range later {
		if todoAllClosed(p) {
			continue // a finished phase is not news
		}
		rows = append(rows, todoStripRow{
			text: fmt.Sprintf("%s. %s · %d/%d", roman(laterIdx[k]+1), p.Name, phaseClosed(p), len(p.Tasks)),
			kind: todoStripPhase,
		})
	}
	if rest := todoLaterLeft(phases, active, laterIdx); rest > 0 {
		rows = append(rows, todoStripRow{text: fmt.Sprintf("… %d more phases", rest), kind: todoStripNote})
	}
	if visible > 0 && len(rows) > visible {
		// The budget is the strip's, not the plan's: cut whole rows from the
		// bottom and say so, so a short terminal never paints half a phase.
		rows = append(rows[:visible-1], todoStripRow{text: "… more below", kind: todoStripNote})
	}
	return rows
}

// todoStripVisible is the row budget for the terminal height: the full budget
// on a tall terminal, and one row less for every row the composer stack is
// short of its usual room. ponytail: a proportional cut — a 12-row terminal gets
// two rows, which is the header and nothing else. That is the honest answer for
// a screen that has no room, and it needs no second layout path.
func todoStripVisible(height int) int {
	if height < 16 {
		return 2
	}
	n := todoStripRowMax
	if height < 24 {
		n = 4
	}
	return n
}

// todoActivePhase is the index of the phase the strip expands: the one holding
// the in-progress task, else the first with an open task, else -1.
func todoActivePhase(phases []tool.TodoPhase) int {
	first := -1
	for i, p := range phases {
		for _, t := range p.Tasks {
			if t.Status == tool.TodoInProgress {
				return i
			}
		}
		if first < 0 && len(p.Tasks) > 0 && !todoAllClosed(p) {
			first = i
		}
	}
	return first
}

// todoOpenTasks picks the tasks the active phase previews: the in-progress row
// first (so a promoted task leads), then the following open tasks in list order,
// up to cap. A closed task is context, not work, and the strip has no room for
// history — the counts in the header carry it. Returns how many open tasks did
// not fit.
func todoOpenTasks(p tool.TodoPhase, cap int) (rows []tool.TodoItem, hidden int) {
	start := 0
	for i, t := range p.Tasks {
		if t.Status == tool.TodoInProgress {
			start = i
			break
		}
	}
	for i := start; i < len(p.Tasks); i++ {
		if todoClosed(p.Tasks[i].Status) {
			continue
		}
		if len(rows) == cap {
			hidden++
			continue
		}
		rows = append(rows, p.Tasks[i])
	}
	for i := 0; i < start && len(rows) < cap; i++ {
		// A pending task that precedes the in-progress one (blocked ones, a
		// list where the model started a later task) still has to be visible:
		// it is open work the header counts.
		if todoClosed(p.Tasks[i].Status) {
			continue
		}
		rows = append(rows, p.Tasks[i])
	}
	open := 0
	for _, t := range p.Tasks {
		if !todoClosed(t.Status) {
			open++
		}
	}
	return rows, max(0, open-len(rows))
}

// todoLaterLeft counts the open phases past the ones the strip listed.
func todoLaterLeft(phases []tool.TodoPhase, active int, shown []int) int {
	seen := map[int]bool{}
	for _, i := range shown {
		seen[i] = true
	}
	n := 0
	for i := active + 1; i < len(phases); i++ {
		if seen[i] || len(phases[i].Tasks) == 0 || todoAllClosed(phases[i]) {
			continue
		}
		n++
	}
	return n
}

func todoClosed(s tool.TodoStatus) bool {
	return s == tool.TodoCompleted || s == tool.TodoDropped
}

func todoAllClosed(p tool.TodoPhase) bool {
	if len(p.Tasks) == 0 {
		return true
	}
	for _, t := range p.Tasks {
		if !todoClosed(t.Status) {
			return false
		}
	}
	return true
}

func phaseClosed(p tool.TodoPhase) int {
	n := 0
	for _, t := range p.Tasks {
		if todoClosed(t.Status) {
			n++
		}
	}
	return n
}

// todoMarkerGlyph is the task's own marker, from the tool — one home for it, so
// the strip cannot print a state the model was never shown.
func todoMarkerGlyph(s tool.TodoStatus) string { return tool.TodoMarker(s) }

// roman is the phase numeral the model's own result prints (internal/tool's
// renderTodoPhases), so the strip and the transcript name a phase the same way.
func roman(n int) string { return tool.RomanNumeral(n) }

// --- painting -------------------------------------------------------------

// todoPanelRows is the strip's height in rows, 0 when it paints nothing.
// Caller holds a.mu.
//
// It is the whole row budget the strip claims from the transcript — see
// viewportLinesLocked, which subtracts exactly this. A strip that painted rows
// the viewport already owns is the bug this number exists to prevent: measured
// on a 24-row terminal with a three-task plan, the transcript's last rows sat
// under the TODO header and its task rows.
func (a *App) todoPanelRows() int {
	if a.dock == nil || a.dock.ops.Todo == nil || a.width < todoStripMinW {
		return 0
	}
	return len(todoStripLines(a.dock.ops.Todo(), todoStripVisible(a.height)))
}

// todoPanelTop is the first screen row the strip occupies: directly above the
// queue and the composer, inside the main pane so the dock's columns stay the
// panel's. It never rises into the session strip's row, and never into the
// transcript: the rows it claims are rows the transcript no longer has.
//
// Caller holds a.mu.
func (a *App) todoPanelTop(agentTop int) int {
	return max(agentTop-a.todoPanelRows(), a.transcriptTop())
}

// drawTodoPanel paints the task strip and its progress.
//
// Caller holds a.mu (draw does) — this must not re-lock it, and the source is a
// closure over the tool's own Snapshot, so reading it takes the tool's lock once
// per frame and nothing else.
func (a *App) drawTodoPanel(agentTop int) {
	// a.dock is nil until something wires a source (a bare test app, a host with
	// no dock) — the one pointer deref the painter must not assume.
	if a.dock == nil || a.dock.ops.Todo == nil {
		return
	}
	ops := a.dock.ops
	s := a.scr
	w := a.rightEdge()
	if w < todoStripMinW {
		return
	}
	rows := todoStripLines(ops.Todo(), todoStripVisible(a.height))
	if len(rows) == 0 {
		return
	}
	y0 := a.todoPanelTop(agentTop)
	if y0 <= 0 {
		return // no room between the transcript's top and the rows below
	}
	dim := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim)))
	head := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentRunning))).Bold(true)
	active := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentUser))).Bold(true)
	phase := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.Gray)))
	body := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.TextPrimary)))
	// NO_COLOR is the global ink off-switch, read here rather than left to the
	// renderer for the same reason the dock reads it: a surface must not choose
	// inks it is forbidden to emit.
	noColor := os.Getenv("NO_COLOR") != ""
	for i, r := range rows {
		y := y0 + i
		if y >= agentTop {
			break
		}
		st := body
		switch r.kind {
		case todoStripHead:
			st = head
			// The header's own progress bar: the fraction painted as cells
			// after the label, so the strip shows how far along the plan is
			// without a second line for it.
			label := r.text
			drawText(s, 1, y, label, st)
			if pct, ok := todoStripPercent(rows, w); ok {
				bx := 1 + width(label) + 2
				bw := w - 1 - bx
				if bw >= 8 {
					drawText(s, bx, y, todoBar(pct, bw, noColor), dim)
				}
			}
			continue
		case todoStripActiv:
			st = active
		case todoStripPhase:
			st = phase
		case todoStripNote:
			st = dim
		case todoStripTask:
			// The marker wears its state's ink and the rest of the row the body
			// ink — the same split the dock's TASKS rows and the transcript's
			// tool box paint with (todomarker.go owns the table).
			_, rest, k, ok := todoMarkerAt(r.text)
			if !ok {
				st = body
				break
			}
			ink := a.cellColor(a.th.Get(theme.TextPrimary))
			if c, pinned := a.th.Slot(syntaxSlots[k]); pinned && !noColor {
				ink = a.cellColor(c)
			}
			ms := tcell.StyleDefault.Foreground(ink)
			drawText(s, 1, y, r.marker, ms)
			drawText(s, 1+width(r.marker), y, truncateCells(rest, w-2-width(r.marker), "…"), body)
			continue
		}
		drawText(s, 1, y, truncateCells(r.text, w-2, "…"), st)
	}
}

// todoStripPercent reads the plan's completion back out of the header row
// ("TODO · 3/7"), so the bar and the label cannot disagree — one number, parsed
// once.
func todoStripPercent(rows []todoStripRow, w int) (float64, bool) {
	if len(rows) == 0 {
		return 0, false
	}
	t := rows[0].text
	i := strings.LastIndexByte(t, '·')
	if i < 0 {
		return 0, false
	}
	var closed, total int
	if _, err := fmt.Sscanf(strings.TrimSpace(t[i+len("·"):]), "%d/%d", &closed, &total); err != nil || total <= 0 {
		return 0, false
	}
	return float64(closed) / float64(total), true
}

// todoBar paints the plan's progress as a run of filled cells. It is the strip's
// one decoration and it carries the header's own number: the strip has the width
// the dock does not, and a fraction alone under-reports how little is left.
func todoBar(pct float64, w int, noColor bool) string {
	filled := int(pct*float64(w) + 0.5)
	if filled > w {
		filled = w
	}
	if filled == 0 && pct > 0 {
		filled = 1 // any progress at all shows one cell
	}
	if noColor {
		return strings.Repeat("=", filled) + strings.Repeat("-", w-filled)
	}
	return strings.Repeat("━", filled) + strings.Repeat("─", w-filled)
}
