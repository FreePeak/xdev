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
// So this panel is the always-on view: every running child at every depth,
// as a TREE — a parent, then the children it spawned under it, joined by
// Crush's connectors ("├─" / "╰─" / "│") — with the child's own transcript
// one click away. It is chrome over the transcript's tail, the same
// discipline the mid-turn queue uses (queue.go): the rows paint above the
// composer and the viewport arithmetic does not move.
//
// The tree is what a chain needs. The transcript's rows are per-CALL, so a
// grandchild has no row of its own to hang under; the panel is the only
// surface that can show the whole chain — and a flat list of child names
// says nothing about which child spawned which.
//
// Deliberate: background hub jobs are NOT listed. /hub (Alt+A) and the dock's
// agents section already cover them, and one panel mixing a blocking child
// with a background job would offer one set of controls for two lifetimes.

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// agentPanelRowMax is the paint budget: three running children plus a
// "+N more" line is the same budget the transcript's child rows use
// (subRowsMax): a strip that grows with the fan-out eats the transcript it is
// supposed to annotate, and a batch of eight is the normal case.
//
// It is a budget on the FAN-OUT, not on the tree: a kept row drags its
// ancestors in (see agentTreeCut), so a chain can paint a row or two more.
// The chain's depth is bounded (taskMaxDepth), so the strip stays bounded.
const agentPanelRowMax = 3

// agentPanelRow is one running child, one level of the tree. It is keyed by
// the child's LABEL, which a batch and a chain both make unique (childLabel),
// and it carries no call id on purpose: the panel's job is to show the tree,
// and the transcript rows are where call attribution lives.
type agentPanelRow struct {
	Label string
	Agent string
	Model string
	Tool  string
	Args  string
	Start time.Time
	// Parent names the child that spawned THIS one ("" = the top-level
	// session spawned it). It is the tree's edge: a chain is the only way
	// a child has a parent, and without the edge the strip can only draw a
	// flat list of names.
	Parent string
	// TranscriptID is the id this child's transcript is readable under
	// (Hub.TrackForeground's "fg-N"). Empty when nothing tracked it, which is
	// exactly when the row must not offer to open one.
	TranscriptID string
	// End and Done mark a child that finished. A settled row is kept, not
	// removed: the strip is this session's record of what ran, and a row that
	// vanished the moment its child did would read as a bug. Done rows paint
	// dim and still, so only a live row moves.
	End  time.Time
	Done bool
	// seq is the arrival order, so two children that started in the same
	// millisecond still have a stable order to sort by.
	seq int
}

// agentTreeRow is one row of the strip's tree: the row's own data plus the
// connector the tree draws to its left.
//
// The shape is Crush's: a parent, then its children under it, joined by
// "├─" / "╰─" and the "│" bar that continues a branch down the rows below.
// The prefix GROWS one level per depth, so a grandchild sits under its
// parent and the eye can tell which is which — the one thing a flat list of
// child names cannot say.
type agentTreeRow struct {
	agentPanelRow
	// prefix is the tree's own column: the ancestor bars, then this row's
	// branch.
	prefix string
	// depth is how many levels down the row sits (0 = a root).
	depth int
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
	// focus is the label of the row the keyboard is on ("" when the strip is
	// not focused) — see focused/focusMove. It is the panel's own state, not
	// the App's: the App only asks whether the strip is focused, and the
	// strip decides which row that is.
	focus string
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
func (p *agentPanel) start(label, agent, model, parent, transcriptID string) {
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
		if parent != "" {
			r.Parent = parent
		}
		if transcriptID != "" {
			r.TranscriptID = transcriptID
		}
		return
	}
	p.next++
	p.rows = append(p.rows, agentPanelRow{
		Label: label, Agent: agent, Model: model, Parent: parent,
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

// settle marks a child finished: it stays on the strip, dim and still, so the
// strip is this session's record of what ran. A row that vanished the moment
// its child did would read as a bug, and a strip that only ever showed
// running children would be a spinner that forgets. The transcript owns the
// child's call row and result box; the strip owns the flat list.
func (p *agentPanel) settle(label string) {
	if p == nil || label == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.rows {
		if p.rows[i].Label == label {
			p.rows[i].Done = true
			p.rows[i].End = time.Now()
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

// treeLocked returns the strip's rows in PAINTED order: a parent, then its
// children, so a chain reads as a chain instead of a flat list of names.
//
// Roots are ordered by the NEWEST row under them, not by their own start:
// a chain's newest link is the one the eye wants, and ordering the roots by
// their own start would push that link below the cap. A flat batch is
// unaffected — every root's newest row is its own.
//
// A child whose parent the strip never saw (a background job, a parent whose
// start event was dropped) draws as a root: an orphan is still a child the
// user is watching, and hiding it would lose the work.
//
// Caller holds p.mu.
func (p *agentPanel) treeLocked() []agentTreeRow {
	if len(p.rows) == 0 {
		return nil
	}
	known := make(map[string]bool, len(p.rows))
	for i := range p.rows {
		known[p.rows[i].Label] = true
	}
	// kids maps a parent's label to its children, in start order.
	kids := make(map[string][]int, len(p.rows))
	var roots []int
	for i := range p.rows {
		parent := p.rows[i].Parent
		if parent == "" || parent == p.rows[i].Label || !known[parent] {
			roots = append(roots, i)
			continue
		}
		kids[parent] = append(kids[parent], i)
	}
	if len(roots) == 0 {
		// A cycle — impossible from a real spawn, but a hang is the worst
		// failure a strip can have. Every row becomes a root, so the strip
		// still shows something.
		for i := range p.rows {
			roots = append(roots, i)
		}
	}
	// newest[i] is the latest start under row i, so a root can be ordered
	// by the newest thing it is running. -1 marks a branch in progress, so
	// a cycle cannot recurse forever.
	newest := make([]int, len(p.rows))
	const inProgress = -1
	var newestUnder func(idx int) int
	newestUnder = func(idx int) int {
		if newest[idx] != 0 {
			if newest[idx] == inProgress {
				return 0 // a cycle: this branch contributes nothing
			}
			return newest[idx]
		}
		newest[idx] = inProgress
		best := p.rows[idx].seq
		for _, ci := range kids[p.rows[idx].Label] {
			if v := newestUnder(ci); v > best {
				best = v
			}
		}
		newest[idx] = best
		return best
	}
	for _, r := range roots {
		newestUnder(r)
	}
	sort.SliceStable(roots, func(i, j int) bool {
		return newest[roots[i]] > newest[roots[j]]
	})
	out := make([]agentTreeRow, 0, len(p.rows))
	// seen breaks a cycle: a row already painted is not painted again, so a
	// malformed parent link cannot hang the paint.
	seen := make(map[int]bool, len(p.rows))
	var walk func(idx int, prefix string, depth int, last bool)
	walk = func(idx int, prefix string, depth int, last bool) {
		if seen[idx] {
			return
		}
		seen[idx] = true
		r := p.rows[idx]
		branch := "├─"
		if last {
			branch = "╰─"
		}
		out = append(out, agentTreeRow{agentPanelRow: r, prefix: prefix + branch, depth: depth})
		// The bar under this row continues while this row has a sibling
		// below it, and stops where this row closes its branch.
		bar := "│ "
		if last {
			bar = "  "
		}
		ch := kids[r.Label]
		for c, ci := range ch {
			walk(ci, prefix+bar, depth+1, c == len(ch)-1)
		}
	}
	for k, r := range roots {
		walk(r, "", 0, k == len(roots)-1)
	}
	return out
}

// visibleTreeLocked is the tree as the strip PAINTS it: the rows the paint
// budget allows, in painted order. The keyboard walks this list, not the
// whole tree, so the focus is always on a row the user can see.
//
// Caller holds p.mu.
func (p *agentPanel) visibleTreeLocked() []agentTreeRow {
	kept, _ := agentTreeCut(p.treeLocked(), agentPanelRowMax)
	return kept
}

// agentTreeCut selects the rows the strip paints.
//
// The NEWEST rows win: a strip that shows old work while a new child runs
// answers the wrong question. A kept row drags its ANCESTORS in with it —
// a child without its parent on screen is a branch with nothing above it —
// so a chain can paint a row or two more than the budget. That is the
// price of showing a chain whole, and the chain's depth is bounded
// (taskMaxDepth).
func agentTreeCut(tree []agentTreeRow, max int) (kept []agentTreeRow, hidden int) {
	if len(tree) == 0 {
		return nil, 0
	}
	parent := make(map[string]string, len(tree))
	for _, r := range tree {
		if r.depth > 0 {
			parent[r.Label] = r.Parent
		}
	}
	order := make([]agentTreeRow, len(tree))
	copy(order, tree)
	sort.SliceStable(order, func(i, j int) bool { return order[i].seq > order[j].seq })
	keep := make(map[string]bool, max)
	picked := 0
	for _, r := range order {
		if picked >= max {
			break
		}
		if keep[r.Label] {
			continue
		}
		picked++
		keep[r.Label] = true
		if r.depth == 0 {
			continue // a root has no ancestor to pull in
		}
		for p := r.Parent; p != ""; {
			if keep[p] {
				break
			}
			keep[p] = true
			p = parent[p]
		}
	}
	kept = make([]agentTreeRow, 0, len(keep))
	for _, r := range tree {
		if keep[r.Label] {
			kept = append(kept, r)
		}
	}
	return kept, len(tree) - len(kept)
}

// snapshot copies the tree, for the painter and the keyboard. It is the
// painted order (see treeLocked), so both walk one list.
func (p *agentPanel) snapshot() []agentTreeRow {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.treeLocked()
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

// --- keyboard focus -------------------------------------------------------
//
// The strip is a keyboard target like the queue: a chord enters it, arrows
// move the selection, Enter opens the focused child, Esc leaves it. The
// focus is the strip's own state (not the App's): the App only asks whether
// the strip is focused, and the strip decides which row that is, so the
// painter and the key path read one source of truth.

// focused reports whether the strip is currently the keyboard target. It is
// read by the painter (to mark the row) and by the key path (to decide
// whether arrows belong to the strip or to the editor).
func (p *agentPanel) focused() string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.focus
}

// focusNewest moves the keyboard onto the newest row: the child that
// started most recently among the rows the strip PAINTS. The tree paints a
// parent BEFORE its children, so "newest" is not "the first row painted" —
// it is the row with the latest start. It is the chord's activation — the
// same row a person watching would open — and idempotent when the strip is
// empty.
//
// It picks a VISIBLE row on purpose: a focus on a row the paint budget cut
// would open a transcript the user cannot see on screen.
func (p *agentPanel) focusNewest() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	rows := p.visibleTreeLocked()
	if len(rows) == 0 {
		p.focus = ""
		return
	}
	best := rows[0]
	for _, r := range rows[1:] {
		if r.seq > best.seq {
			best = r
		}
	}
	p.focus = best.Label
}

// blur leaves the strip: the next arrow falls through to the editor, which
// does its usual work (scroll, history, multi-row walk).
func (p *agentPanel) blur() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.focus = ""
	p.mu.Unlock()
}

// focusMove steps the focus by delta rows (-1 up, +1 down), clamping at the
// ends of the strip. The delta is a SCREEN delta: down is one row lower,
// which is one row later in the painted tree. The strip is a short band at
// the bottom of the screen, and an arrow that runs off the end stays on the
// last row instead of teleporting the selection to the other end, which
// would read as a bug.
//
// It walks the VISIBLE rows, so the focus never lands on a row the paint
// budget cut.
func (p *agentPanel) focusMove(delta int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	rows := p.visibleTreeLocked()
	if len(rows) == 0 {
		p.focus = ""
		return
	}
	idx := -1
	for i, r := range rows {
		if r.Label == p.focus {
			idx = i
			break
		}
	}
	if idx < 0 {
		// No focus yet (or a row that is gone): start from the newest row,
		// the same one the chord's activation picks.
		best := 0
		for i, r := range rows {
			if r.seq > rows[best].seq {
				best = i
			}
		}
		idx = best
	}
	idx += delta
	if idx < 0 {
		idx = 0
	} else if idx > len(rows)-1 {
		idx = len(rows) - 1
	}
	p.focus = rows[idx].Label
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
//
// parent names the child that spawned this one ("" = the top-level session
// did): it is the tree's edge, and the strip draws the chain along it.
func (a *App) AgentPanelStart(label, agent, model, parent, transcriptID string) {
	if a == nil || a.agents == nil {
		return
	}
	a.agents.start(label, agent, model, parent, transcriptID)
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

// AgentPanelForegroundCount reports how many children the panel still lists,
// i.e. how many are running or just settled with a transcript to reopen. The
// status line reads it for the "subagent · Alt+B" chip: the chord is only
// worth advertising while there is something to open.
func (a *App) AgentPanelForegroundCount() int {
	if a == nil || a.agents == nil {
		return 0
	}
	return len(a.agents.snapshot())
}

// --- painting -------------------------------------------------------------

// agentPanelRows is the strip's height in rows, 0 when it paints nothing.
// Caller holds a.mu.
func (a *App) agentPanelRows() int {
	if a.agents == nil || !a.agents.enabled() || !a.agents.hasRows() {
		return 0
	}
	kept, hidden := agentTreeCut(a.agents.snapshot(), agentPanelRowMax)
	n := len(kept)
	if hidden > 0 {
		n++ // the "+N more" line
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

// drawAgentPanel paints the running-children TREE and publishes the hit
// table the mouse reads.
//
// The shape is Crush's: a parent, then the children it spawned under it,
// joined by "├─" / "╰─" and the "│" bar that carries a branch down the rows
// under it. A flat list of child names cannot say which child spawned which,
// and a chain is exactly the case that reads wrong.
//
// A row is: the tree's column, a status glyph (the shared spinner while the
// child works, a static tick once it settled), the openable tick for a child
// a host tracked, the child's label and agent type, what it is doing right
// now, its own elapsed clock, and — at the right end — the model, dim, only
// when the child runs on a different model than the session. A column of
// identical model names is noise, and a child that was sent to another model
// is exactly the thing worth seeing.
//
// Caller holds a.mu (draw does) — this must not re-lock a.mu.
func (a *App) drawAgentPanel(queueTop int) {
	a.agentHits = nil
	if a.agents == nil || !a.agents.enabled() {
		return
	}
	tree := a.agents.snapshot()
	if len(tree) == 0 {
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
	focusSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.TextPrimary))).Bold(true)
	runSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentRunning)))
	textSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.TextPrimary)))
	focus := a.agents.focused()
	now := time.Now()
	rows, hidden := agentTreeCut(tree, agentPanelRowMax)
	for i, r := range rows {
		y := y0 + i
		if y >= queueTop {
			break // the strip never paints over the queue or the composer
		}

		x := 1
		// The tree's own column: the ancestor bars, then this row's branch.
		drawText(s, x, y, r.prefix, dim)
		x += width(r.prefix) + 1
		// The status glyph: the shared STATUS spinner while the child
		// works, so this row moves in step with the composer divider and
		// the call row (statusSpinFrame's whole point) — a child still
		// working is a status, not an activity. A settled child wears a static
		// tick instead — a finished row that kept spinning would say
		// "still working" about work that is done.
		mark, markSt := "✓", dim
		if !r.Done {
			mark, markSt = a.statusSpinFrame(), runSt
		}
		drawText(s, x, y, mark, markSt)
		x += 2
		// A row the host tracked has a transcript to open, and says so
		// NEXT TO ITS LABEL — the same glyph the transcript's own child
		// rows wear, so "openable" reads the same in both places. It sat
		// at the far right before, where it read as a stray glyph next to
		// the model column.
		if r.TranscriptID != "" {
			drawText(s, x, y, "⎿", dim)
			x += 2
		}
		label := r.Label
		if r.Agent != "" && r.Agent != r.Label {
			label += " · " + r.Agent
		}
		// The right end first, so the text knows how much room is left:
		// the model (only when it is news), then the child's own clock.
		right := ""
		if r.Model != "" && r.Model != a.st.Model {
			right = r.Model
		}
		elapsed := humanDur(now.Sub(r.Start))
		if r.Done && !r.End.IsZero() {
			// A settled child's clock is its OWN time, not the time since
			// it started: a row that kept counting would say the child is
			// still running.
			elapsed = humanDur(r.End.Sub(r.Start))
		}
		rightX := w - 1
		if right != "" {
			rightX = w - 1 - width(right)
			drawText(s, rightX, y, right, dim)
			rightX--
		}
		clockX := rightX - width(elapsed)
		drawText(s, clockX, y, elapsed, dim)
		// What is left between the tree column and the clock. The label
		// comes first — it is the child's name, and a row that says
		// "read · internal/tui/app.go" without the name says nothing
		// about WHO is working.
		avail := clockX - 1 - x
		if avail < 12 {
			avail = 12
		}
		if width(label) > avail-8 {
			label = truncateCells(label, max(1, avail-8), "…")
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
		phrase = truncateCells(phrase, max(1, avail-width(label)-3), "…")
		labelSt := nameSt
		if r.Label == focus {
			labelSt = focusSt
		}
		drawText(s, x, y, label, labelSt)
		x += width(label)
		// The separator between the name and what the child is doing. It
		// was missing before, so "reviewer grep · TODO" read as one word.
		drawText(s, x+1, y, "·", dim)
		drawText(s, x+3, y, phrase, textSt)
		x += 3 + width(phrase)
		// Blank the row's tail so a longer phrase from an earlier frame cannot
		// show through this one.
		for bx := x; bx < clockX-1; bx++ {
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
			// Not "more running": the strip keeps settled rows, so the
			// hidden ones are not all working.
			drawText(s, 1, y, fmt.Sprintf("+%d more", hidden), dim)
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
// whether it is on, and how many children are still RUNNING (a settled row
// persists on the strip but is not running, so the notice "N running" stays
// honest while the strip keeps its record).
func (a *App) AgentPanelState() (on bool, running int) {
	if a.agents == nil {
		return false, 0
	}
	for _, r := range a.agents.snapshot() {
		if !r.Done {
			running++
		}
	}
	return a.agents.enabled(), running
}

// AgentPanelParent reports the parent the strip's tree gives a child: the
// label of the child that spawned it, or "" when the top-level session did.
// It is how a host reads back the tree's edge — the link the child's start
// event carried.
func (a *App) AgentPanelParent(label string) string {
	if a.agents == nil {
		return ""
	}
	for _, r := range a.agents.snapshot() {
		if r.Label == label {
			return r.Parent
		}
	}
	return ""
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

// --- App-level keyboard entry ---------------------------------------------
//
// The strip's own state (focus) lives on the panel; the App's half is a
// boolean saying whether the strip is the keyboard target at all. The split
// is the same one the queue uses: the queue owns its own rows and the App
// only knows it is open.

// handleAgentPanelKey owns the keys while the strip is the keyboard target.
// It is modal on the same terms as the message menu above it: every key is
// swallowed, because the strip is chrome over the composer and a key that
// fell through would land in the editor. Ctrl+C is handed back so the app
// can still quit (see handleMsgMenuKey).
//
// The strip is NOT always focused: it has to be entered first (the chord
// below), and while it is not the arrows do their usual editor work. This
// function therefore answers the strip's own keys only when the strip is
// focused, and reports false otherwise so the editor can have the rest.
func (a *App) handleAgentPanelKey(key *tcell.EventKey) bool {
	if a.agents == nil || !a.agents.enabled() || !a.agentPanelFocused {
		return false
	}
	switch key.Key() {
	case tcell.KeyEsc:
		// Leave the strip. The editor's arrows then do their usual work
		// (scroll, history, multi-row walk), and the strip can be re-entered
		// with the chord. One Esc is "I am done looking at the strip", not
		// "close the whole app" — that stays Ctrl+C.
		a.agentPanelFocused = false
		a.agents.blur()
		a.poke()
		return true
	case tcell.KeyEnter:
		// Open the focused child's transcript. Nothing focused is silence,
		// like ctrl+o on a transcript with no box — the chord must not claim
		// to have opened something.
		a.agentPanelOpen()
		a.poke()
		return true
	case tcell.KeyUp:
		a.agents.focusMove(-1)
		a.poke()
		return true
	case tcell.KeyDown:
		a.agents.focusMove(1)
		a.poke()
		return true
	case tcell.KeyCtrlC:
		return false
	}
	return true
}

// agentPanelHitFor returns the hit geometry the strip painted for one row, or
// nil when the strip is not on screen or the row is not painted. The keyboard
// resolves a focused row to its screen rectangle this way, so Enter opens the
// same child a click would.
func (a *App) agentPanelHitFor(label string) *agentPanelHit {
	for _, h := range a.agentHits {
		if h.label == label {
			return &h
		}
	}
	return nil
}

// agentPanelFocus is the keyboard's entry into the strip: it moves the focus
// onto the newest row (the one a person watching would open) and reports
// whether the strip had anything to focus. It is the chord's activation, the
// same way focusNewest is the panel's own.
func (a *App) agentPanelFocus() bool {
	if a.agents == nil || !a.agents.enabled() || !a.agents.hasRows() {
		return false
	}
	a.agents.focusNewest()
	a.agentPanelFocused = true
	return true
}

// agentPanelMove steps the keyboard focus by delta rows (-1 up, +1 down),
// wrapping at the ends of the strip. It reports whether the strip was
// focused; an unfocused strip ignores the chord (the key falls through to the
// editor, where Up/Down do their usual work).
func (a *App) agentPanelMove(delta int) bool {
	if a.agents == nil || !a.agents.enabled() || !a.agents.hasRows() {
		return false
	}
	if a.agents.focused() == "" {
		a.agents.focusNewest()
	}
	a.agents.focusMove(delta)
	return true
}

// agentPanelOpen opens the focused child's transcript, if any. It is the Enter
// chord: the click path arms an open and runs it unlocked, and this does the
// same thing without a click. Nothing to open is silence, like ctrl+o on a
// transcript with no box.
func (a *App) agentPanelOpen() {
	if a.agents == nil || !a.agents.enabled() {
		return
	}
	focus := a.agents.focused()
	if focus == "" {
		a.agents.focusNewest()
		focus = a.agents.focused()
	}
	if focus == "" {
		return
	}
	a.runPendingAgentOpenFor(focus)
}

// runPendingAgentOpenFor arms and runs an open for one strip row, the unlocked
// half of the click path. The click path resolves the row to a hit (which
// carries the transcript id) under a.mu and defers the open; the keyboard
// does the same resolution here, so the two cannot drift into opening
// different children.
func (a *App) runPendingAgentOpenFor(label string) {
	h := a.agentPanelHitFor(label)
	if h == nil {
		return
	}
	a.agentOpen = &agentPanelHit{label: h.label, id: h.id}
	a.runPendingAgentOpen()
}
