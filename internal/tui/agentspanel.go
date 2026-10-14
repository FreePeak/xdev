package tui

// The live subagent panel: a strip of running children above the composer.
//
// Why it exists next to the rows already under a `task` call (subLines,
// app.go) and the /hub roster (hubroster.go):
//
//   - the transcript rows are per-CALL. They hang under the call that spawned
//     the child, which is exactly right for a foreground spawn and impossible
//     for a chain: a grandchild's events arrive with no call row of their own
//     (its parent's call is inside the child's own session), so a chain of
//     three renders as ONE row and two invisible levels.
//   - the /hub roster is background JOBS. A foreground child blocks the turn
//     that spawned it, so it is not a job, and the roster is a modal overlay
//     the user opens; nothing is on screen while it works.
//
// So this panel is the flat, always-on view: every running child at every
// depth, in one list, with the child's own transcript one click away. It is
// chrome over the transcript's tail, the same discipline the mid-turn queue
// uses (queue.go): the rows paint above the composer and the viewport
// arithmetic does not move.
//
// Deliberate: background hub jobs are NOT listed. /hub (Alt+A) and the dock's
// agents section already cover them, and one panel mixing a blocking child
// with a background job would offer one set of controls for two lifetimes.

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// agentPanelRowMax caps the painted rows. Three running children plus a
// "+N more" line is the same budget the transcript's child rows use
// (subRowsMax): a strip that grows with the fan-out eats the transcript it is
// supposed to annotate, and a batch of eight is the normal case.
const agentPanelRowMax = 3

// agentPanelRow is one running child, flattened across every depth. It is
// keyed by the child's LABEL, which a batch and a chain both make unique
// (childLabel), and it carries no call id on purpose: the panel's job is to
// show the tree flat, and the transcript rows are where call attribution lives.
type agentPanelRow struct {
	Label string
	Agent string
	Model string
	Tool  string
	Args  string
	Start time.Time
	// TranscriptID is the id this child's transcript is readable under
	// (Hub.TrackForeground's "fg-N"). Empty when nothing tracked it, which is
	// exactly when the row must not offer to open one.
	TranscriptID string
	// seq is the arrival order, so two children that started in the same
	// millisecond still have a stable order to sort by.
	seq int
}

// agentPanelHit is one painted row's click target: the row body opens the
// child's transcript. Published per frame like every other hit table here
// (queueHits, tabHits), so a click can never land on a row from an older frame.
type agentPanelHit struct {
	label string
	id    string
	row   panelRect
}

// agentPanel is the panel's live state. Its own mutex rather than a.mu: the
// events arrive from a CHILD's goroutine (agent.SubagentSpec.OnEvent's
// contract is "cheap, non-blocking, never call back into the spawner"), and
// a.mu is held across the transcript's paint. Taking a.mu from that callback
// would put a child's tool dispatch behind a full frame.
type agentPanel struct {
	mu   sync.Mutex
	rows []agentPanelRow
	next int
	on   bool
}

func newAgentPanel(on bool) *agentPanel { return &agentPanel{on: on} }

// enabled reports whether the strip paints at all.
func (p *agentPanel) enabled() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.on
}

// setEnabled is the Alt+P toggle.
func (p *agentPanel) setEnabled(on bool) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.on = on
	p.mu.Unlock()
}

// start records a child that just began.
func (p *agentPanel) start(label, agent, model, transcriptID string) {
	if p == nil || label == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if r := p.findLocked(label); r != nil {
		// A label that is already running is the same child re-announcing
		// (a batch item adopted by an update before its start event, or a
		// revive). Refresh it rather than appending a duplicate row.
		r.Agent, r.Model = agent, model
		if transcriptID != "" {
			r.TranscriptID = transcriptID
		}
		return
	}
	p.next++
	p.rows = append(p.rows, agentPanelRow{
		Label: label, Agent: agent, Model: model,
		Start: time.Now(), TranscriptID: transcriptID, seq: p.next,
	})
}

// update records what a running child just did.
func (p *agentPanel) update(label, tool, args string) {
	if p == nil || label == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.findLocked(label)
	if r == nil {
		// An update with no start is a host that only forwards tool events:
		// adopt it, so the row still says something is running rather than
		// dropping the child silently.
		p.next++
		p.rows = append(p.rows, agentPanelRow{Label: label, Start: time.Now(), seq: p.next})
		r = &p.rows[len(p.rows)-1]
	}
	r.Tool, r.Args = tool, args
}

// settle removes a child: the panel lists what is RUNNING, and a finished
// child's row is the transcript's business (its call row and its result box).
// Keeping settled rows here would make the strip a second, worse transcript.
func (p *agentPanel) settle(label string) {
	if p == nil || label == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, r := range p.rows {
		if r.Label == label {
			p.rows = append(p.rows[:i], p.rows[i+1:]...)
			return
		}
	}
}

// setTranscript binds a child to the id its transcript is readable under.
func (p *agentPanel) setTranscript(label, id string) {
	if p == nil || label == "" || id == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if r := p.findLocked(label); r != nil {
		r.TranscriptID = id
	}
}

// findLocked is the row with this label, or nil. Caller holds p.mu.
func (p *agentPanel) findLocked(label string) *agentPanelRow {
	for i := range p.rows {
		if p.rows[i].Label == label {
			return &p.rows[i]
		}
	}
	return nil
}

// snapshot copies the rows, newest first, for the painter. Newest first
// because the panel is a strip at the bottom of the screen: the child that
// just started is the one the eye is looking for, and the one that has been
// running for two minutes is already known.
func (p *agentPanel) snapshot() []agentPanelRow {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := append([]agentPanelRow(nil), p.rows...)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// hasRows reports whether anything is running, so the painter and the layout
// can skip the strip entirely on the common case (no children).
func (p *agentPanel) hasRows() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.rows) > 0
}

// --- wiring ---------------------------------------------------------------

// SetAgentPanelOps wires the panel's click and toggle paths, which live in cmd
// (it owns the hub the transcripts are fetched through). A nil op degrades the
// panel to a read-only strip instead of new TUI-side machinery — the same
// split the hub roster uses.
type AgentPanelOps struct {
	// OpenChild shows one child's transcript in the existing panel surface
	// (the one /hub's Enter and a click on a transcript row already open).
	OpenChild func(id, label string) bool
	// Current reports the toggle's state for the chord's feedback.
	Current func() bool
	// Set flips the panel on or off (persisted by cmd).
	Set func(on bool) error
}

// AgentPanelStart records a child that just began. It is the seam cmd's
// taskChildSink calls from the agent's OnEvent — the callback runs on the
// CHILD's goroutine, so this must stay cheap: one short lock on the panel's
// own mutex (never a.mu, which is held across a frame) and a poke.
func (a *App) AgentPanelStart(label, agent, model, transcriptID string) {
	if a == nil || a.agents == nil {
		return
	}
	a.agents.start(label, agent, model, transcriptID)
	a.poke()
}

// AgentPanelUpdate records what a running child just did.
func (a *App) AgentPanelUpdate(label, tool, args string) {
	if a == nil || a.agents == nil {
		return
	}
	a.agents.update(label, tool, args)
	a.poke()
}

// AgentPanelSettle removes a finished child. The panel lists what is RUNNING:
// a settled child's row is the transcript's business (its call row and its
// result box), and keeping it here would make the strip a second, worse
// transcript.
func (a *App) AgentPanelSettle(label string) {
	if a == nil || a.agents == nil {
		return
	}
	a.agents.settle(label)
	a.poke()
}

// AgentPanelSetTranscript binds a child to the id its transcript is readable
// under, so its row becomes clickable. Separate from Start because the id
// arrives on the same event but only when a hub tracked the child — a row
// with no id must not pretend one exists.
func (a *App) AgentPanelSetTranscript(label, id string) {
	if a == nil || a.agents == nil {
		return
	}
	a.agents.setTranscript(label, id)
}

// SetAgentPanelEnabled is the settings-driven initial state (settings
// subagentPanel, default on). It is the same switch Alt+P flips; keeping the
// two on one setter is what stops a chord and a config key from disagreeing.
func (a *App) SetAgentPanelEnabled(on bool) {
	if a == nil || a.agents == nil {
		return
	}
	a.agents.setEnabled(on)
	a.poke()
}

// --- painting -------------------------------------------------------------

// agentPanelRows is the strip's height in rows, 0 when it paints nothing.
// Caller holds a.mu.
func (a *App) agentPanelRows() int {
	if a.agents == nil || !a.agents.enabled() || !a.agents.hasRows() {
		return 0
	}
	n := len(a.agents.snapshot())
	if n > agentPanelRowMax {
		n = agentPanelRowMax + 1 // the rows plus the "+N more" line
	}
	return n
}

// agentPanelTop is the first screen row the strip occupies: directly above
// the queue (which is itself above the composer), inside the main pane so the
// context dock's columns stay the panel's. Caller holds a.mu.
func (a *App) agentPanelTop(queueTop int) int {
	y := queueTop - a.agentPanelRows()
	if y < a.transcriptTop() {
		return a.transcriptTop()
	}
	return y
}

// drawAgentPanel paints the running-children strip and publishes the hit table
// the mouse reads. Each row is a spinner, the child's label, its agent type,
// what it is doing right now, and its own elapsed clock; the model rides at
// the right end, dim, only when the child runs on a different model than the
// session — a column of identical model names is noise, and a child that was
// sent to another model is exactly the thing worth seeing.
//
// Caller holds a.mu (draw does) — this must not re-lock a.mu.
func (a *App) drawAgentPanel(queueTop int) {
	a.agentHits = nil
	if a.agents == nil || !a.agents.enabled() {
		return
	}
	rows := a.agents.snapshot()
	if len(rows) == 0 {
		return
	}
	s := a.scr
	w := a.rightEdge()
	y0 := a.agentPanelTop(queueTop)
	if y0 <= 0 || w < 24 {
		return
	}
	dim := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim)))
	nameSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.Gray))).Bold(true)
	runSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentRunning)))
	textSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.TextPrimary)))
	hidden := 0
	if len(rows) > agentPanelRowMax {
		hidden = len(rows) - agentPanelRowMax
		rows = rows[:agentPanelRowMax]
	}
	now := time.Now()
	for i, r := range rows {
		y := y0 + i
		if y >= queueTop {
			break // the strip never paints over the queue or the composer
		}
		// The left gutter: the shared spinner, so this row moves in step with
		// the composer divider and the call row (spinFrame's whole point).
		drawText(s, 1, y, a.spinFrame(), runSt)
		x := 3
		name := r.Label
		if r.Agent != "" && r.Agent != r.Label {
			name += " · " + r.Agent
		}
		drawText(s, x, y, name, nameSt)
		x += width(name) + 1
		// The model rides at the right end, but only when it is news.
		right := ""
		if r.Model != "" && r.Model != a.st.Model {
			right = r.Model
		}
		elapsed := humanDur(now.Sub(r.Start))
		rightW := 0
		if right != "" {
			rightW = width(right) + 1
		}
		budget := w - 2 - rightW - width(elapsed) - 2
		if budget < 8 {
			budget = 8
		}
		phrase := r.Tool
		if d := toolDetail(r.Args); d != "" {
			if phrase == "" {
				phrase = d
			} else {
				phrase += " · " + d
			}
		}
		if phrase == "" {
			phrase = "starting"
		}
		phrase = collapseLines(utf8Only(phrase))
		if x-1 < budget {
			phrase = truncateCells(phrase, budget-(x-1), "…")
			drawText(s, x, y, phrase, textSt)
		}
		// The clock is right-aligned against the model column, so a row's
		// numbers line up down the strip.
		ex := w - 1 - rightW - width(elapsed)
		if ex > x+width(phrase) {
			drawText(s, ex, y, elapsed, dim)
		}
		if right != "" {
			drawText(s, w-1-width(right), y, right, dim)
		}
		// Blank the row's tail so a longer phrase from an earlier frame cannot
		// show through this one.
		for bx := x + width(phrase); bx < w-1-rightW; bx++ {
			s.SetContent(bx, y, ' ', nil, textSt)
		}
		// Only a child a host tracked has a transcript to open; a row with
		// nothing to show is never clickable.
		if r.TranscriptID != "" {
			a.agentHits = append(a.agentHits, agentPanelHit{
				label: r.Label, id: r.TranscriptID,
				row: panelRect{x: 1, y: y, w: w - 1, h: 1},
			})
		}
	}
	if hidden > 0 {
		y := y0 + len(rows)
		if y < queueTop {
			drawText(s, 3, y, fmt.Sprintf("+%d more running", hidden), dim)
		}
	}
}

// handleAgentPanelMouse answers a click on a strip row: it opens that child's
// own transcript. It reports whether it consumed the gesture, so the caller
// never also anchors a selection on the transcript row underneath.
//
// The open itself is deferred: opening takes the hub registry lock and fills
// the panel's transcript view, and doing that under a.mu is the deadlock
// openSubChildTranscript documents. The arm-then-run split is the queue's
// (queueMouse/runPendingQueueAction).
//
// Caller: UI thread, a.mu held.
func (a *App) handleAgentPanelMouse(x, y int, press bool) bool {
	if len(a.agentHits) == 0 {
		return false
	}
	for _, h := range a.agentHits {
		if !h.row.contains(x, y) {
			continue
		}
		if press {
			a.agentOpen = &agentPanelHit{label: h.label, id: h.id}
		}
		return true
	}
	return false
}

// agentPanelMouse is handleAgentPanelMouse's caller: it takes the lock the row
// geometry lives under and leaves the armed open for the caller to run
// UNLOCKED.
func (a *App) agentPanelMouse(m *tcell.EventMouse, press bool) bool {
	x, y := m.Position()
	a.mu.Lock()
	consumed := a.handleAgentPanelMouse(x, y, press)
	a.mu.Unlock()
	return consumed
}

// runPendingAgentOpen runs the open a click armed, if any. The caller is the
// UI loop, right after the lock was released.
func (a *App) runPendingAgentOpen() {
	a.mu.Lock()
	open := a.agentOpen
	a.agentOpen = nil
	a.mu.Unlock()
	if open == nil {
		return
	}
	a.openSubChildTranscript(open.id, open.label)
	a.poke()
}

// AgentPanelToggle is the Alt+P action: it flips the strip and reports the new
// state, so the caller can say which way it went.
func (a *App) AgentPanelToggle() (bool, bool) {
	if a.agents == nil || a.agentPanelOps == nil || a.agentPanelOps.Set == nil {
		return false, false
	}
	on := !a.agents.enabled()
	if err := a.agentPanelOps.Set(on); err != nil {
		return false, false
	}
	a.agents.setEnabled(on)
	a.poke()
	return on, true
}

// SetAgentPanelOps wires the panel's click and toggle paths.
func (a *App) SetAgentPanelOps(ops *AgentPanelOps) {
	a.mu.Lock()
	a.agentPanelOps = ops
	a.mu.Unlock()
}

// AgentPanelState reports the strip's state for a test or a status read:
// whether it is on, and how many children it is listing.
func (a *App) AgentPanelState() (on bool, running int) {
	if a.agents == nil {
		return false, 0
	}
	return a.agents.enabled(), len(a.agents.snapshot())
}

// agentPanelSummary is the one-line state a notice prints.
func agentPanelSummary(on bool, running int) string {
	state := "off"
	if on {
		state = "on"
	}
	return fmt.Sprintf("subagent panel %s (%d running)", state, running)
}

// agentPanelRowText is the row's text without styling, for a test to assert on.
func agentPanelRowText(r agentPanelRow) string {
	parts := []string{r.Label}
	if r.Agent != "" && r.Agent != r.Label {
		parts = append(parts, r.Agent)
	}
	if r.Tool != "" {
		parts = append(parts, r.Tool)
	}
	return strings.Join(parts, " · ")
}
