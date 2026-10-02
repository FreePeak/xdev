package tui

import (
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/gdamore/tcell/v2"
	"github.com/mattn/go-runewidth"

	"github.com/FreePeak/xdev/internal/theme"
)

// The context dock (issue #291 §1 + §2): a fixed-width column right of the
// transcript holding the session's working set — the proposed plan, the task
// list, the files this session changed, the agents running — so the transcript
// keeps the reading role and the dock the scanning one. It never edits or moves
// the model's own output.
//
// Three properties are load-bearing:
//
//   - It is chrome, not a modal. It takes no exclusive focus and holds no key
//     another surface needs: a pending decision is answered where it is always
//     answered — the ask card, which owns its timeout, or the composer, which is
//     how /plan off and revision feedback already work. So the panel's entire
//     keyboard is Alt+s and Ctrl+T. Every overlay (picker, ask card, hub roster,
//     tree) still paints full width over it, because the panel is drawn first.
//   - It costs nothing per frame. Its sources are closures, and the panel
//     rebuilds only when a version moved (DockBump), the grid changed, or the
//     human folded something. #283/#284 closed the stall→stream-kill chain at the
//     draw path; a second column rebuilt at 30fps would reopen it. A rebuild is
//     therefore capped: a handful of sources, a row budget, no I/O.
//   - It decides nothing on its own. What it shows of a plan is the one
//     PlanMode.View read, and the answer stays the propose tool's own reviewer,
//     so no second surface can resolve a proposal differently.

// Dock display policy. auto follows the width rule; show/hide are the human
// overriding it from either end, and the third press returns to auto.
const (
	DockAuto = "auto"
	DockShow = "show"
	DockHide = "hide"
)

// dockCols is the panel width and dockPad the gutter it keeps on either side, so
// the content is dockCols-2*dockPad cells wide — opencode's own panel and padding
// numbers, which give its sections the same 38 columns.
const (
	dockCols    = 42
	dockPad     = 2
	dockInner   = dockCols - 2*dockPad // paintable cells between the pads
	dockMinCols = 120                  // below this, auto mode closes the panel
	dockMin     = 20                   // the transcript's own floor, shared with rightEdge
	dockMinRows = 5                    // a terminal shorter than this has no band to fill
	dockListMax = 6                    // rows one list shows before "+N more"
	dockPlanMax = 18                   // rows the plan document may take: the section is
	// the reason the panel exists, so it gets the bigger half of the budget.
	// There is no cap on the session's own name: it is the one string in the
	// panel a human reads whole, so it takes the rows it needs from the
	// sections below (dockTitleLines). ponytail: a name long enough to fill
	// the panel shows the panel and nothing else — that is the trade asked
	// for; the other one was a "…" in the middle of the name.
	dockTitleFloor = 4 // rows the sections keep whatever the name takes
)

// Section ids, in the order the panel paints them: what the session is doing,
// what it is waiting on, then the artifacts. They double as fold keys.
const (
	dockPlanID  = "plan"
	dockTaskID  = "tasks"
	dockFileID  = "files"
	dockAgentID = "agents"
	dockMCPID   = "mcp"
	// dockTrajID is the panel's own action row rather than a source of session
	// facts: its one row opens the trajectory ledger on a click.
	dockTrajID = "trajectory"
)

// dockBumpSeq is the version every source stamps. Package-level and atomic
// because the writers live on the agent goroutine — a plan publishes, a task list
// mutates — and must invalidate the panel without taking a lock the App may
// already hold. The App keeps only the last value it folded in.
var dockBumpSeq atomic.Uint64

// DockOps are the dock's sources that the TUI cannot read for itself: the plan
// state (agent package) and the task/agent rosters (tool and hub packages). cmd
// wires them; a nil field is a section that renders nothing rather than an empty
// section forever. The transcript's own facts — the files this session changed —
// are read here, not through a seam, because they are already App state.
//
// A text source returns "" when it has nothing to show, else one block whose
// FIRST line is the section heading (count or state included) and whose
// remaining non-blank lines are its rows. The caller owns what the rows say; the
// panel owns how wide they are painted, so no source has to know its own width.
type DockOps struct {
	// Plan is the proposal view: the pending document ("" when none) and whether
	// plan mode is on. It is PlanMode.View — one lock read, no second copy of the
	// agent's state kept here.
	Plan  func() (pending string, active bool)
	Tasks func() string
	// Agents is the hub roster, one row per child, heading included. It is the
	// same snapshot /hub reads; the panel never holds a roster of its own.
	Agents func() string
	// Session is the panel's identity: the live session's title — the panel's own
	// title slot — and its short id, the slot's fallback and the footer's heading.
	// A closure rather than a setter because cmd swaps the store on /new, /resume
	// and /fork: the panel follows the session, not the process.
	Session func() (title, id string)
	// MCP is the connected MCP server names. It is the registry's
	// snapshot; a nil source means MCP is off and the section is omitted.
	MCP func() string
	// Trajectory is the panel's one action row: a click opens the ledger
	// /trajectory opens. A count, not the records — the ledger builds its rows
	// when it opens, and a source that walked the session on every rebuild is
	// the per-frame cost the panel's rebuild cap exists to avoid.
	Trajectory func() string
}

// dockRow is one painted row. text is the row's own line; add and del are the
// right-aligned change counts a changed-file row carries, in the diff's own two
// inks; head marks a section heading, which paints as a bold name and the dim
// count its source appended.
type dockRow struct {
	text string
	add  string // "+41" (diff-added ink), empty on every row but a changed file
	del  string // "-12" (diff-removed ink)
	head bool
	path string // full file path of a FILES row, so a click resolves it without re-parsing the clipped text
	act  string // non-file action a click opens ("trajectory"): the panel's own buttons
}

// dockRowText returns the selectable text of a dock row: the row's
// own text plus its change counts, as the user should copy it.
// Separator rows (all empty) copy as nothing, so a drag across
// two sections does not glue them together.
func (r dockRow) dockRowText() string {
	if r.text == "" && r.add == "" && r.del == "" {
		return ""
	}
	if r.add != "" || r.del != "" {
		return strings.TrimSpace(r.text + " " + r.add + " " + r.del)
	}
	return r.text
}

// right is the fragment a row aligns to the panel's right edge — and so the
// width a changed file's name has to leave room for.
func (r dockRow) right() string {
	if r.add == "" && r.del == "" {
		return ""
	}
	return r.add + " " + r.del
}

// dockCand is one candidate row of a layout pass, tagged with the section it
// belongs to (-1 = a separator).
type dockCand struct {
	fold int
	row  dockRow
}

// dockFold is one section as its source described it: rows already fitted to the
// panel's interior, so the layout math is only ever about rows.
type dockFold struct {
	id    string
	title string
	rows  []dockRow
	max   int // the section's own row cap, so the render cannot disagree with it
}

// dockState is the App's dock side. Everything here is UI-thread-owned except the
// version counter, so the panel is rebuilt only from paint().
type dockState struct {
	ops  DockOps
	mode string

	// fold is the human's override of the section states: 0 lets each section
	// decide itself, 1 opens them, 2 shuts them. Three states because Ctrl+T has
	// to be able to get back out of what it just did.
	//
	// ponytail: no per-section fold. The issue asks for collapsible sections and a
	// cycle covers the case that actually costs a human a read — four lists
	// competing with the document they are all secondary to. The upgrade path is a
	// heading click on the row map the picker already keeps.
	fold int8

	version uint64 // the last bump folded in
	lines   []dockRow
	// title and sid are the session's identity, read from the Session source on
	// every build: title paints the panel's title slot, sid is its fallback and
	// the footer's heading.
	title       string
	sid         string
	buildW      int
	bandH       int // the band the rows were budgeted for
	buildBlocks int // the transcript's shape when the Files fold was read
	// titleLines is the title slot as painted: the session's name wrapped to
	// the interior, one entry per row. The slot is never zero rows.
	titleLines []string
}

// --- display policy ---

// dockMode normalizes a persisted or typed policy; anything unrecognized follows
// the width rule, so a corrupt settings layer is never a surprise column.
func dockMode(s string) string {
	switch strings.TrimSpace(s) {
	case DockShow, DockHide:
		return strings.TrimSpace(s)
	}
	return DockAuto
}

// SetDockMode applies the persisted policy at startup. cmd owns the settings read
// and the write-back; the panel owns only the state.
func (a *App) SetDockMode(mode string) {
	a.mu.Lock()
	a.ensureDock().mode = dockMode(mode)
	a.mu.Unlock()
	a.poke()
}

// SetDockOps points the dock at its sources and forces the first build.
func (a *App) SetDockOps(ops DockOps) {
	a.mu.Lock()
	d := a.ensureDock()
	d.ops = ops
	d.version, d.lines = 0, nil
	a.mu.Unlock()
	a.poke()
}

// SetDockModeFunc wires the persistence of a policy the human changed with Alt+s
// (settings `sidebarMode`). nil = a session that cannot persist it, which is every
// non-TUI caller and every test.

func (a *App) SetDockModeFunc(set func(mode string)) {
	a.dockSetMode = set
}

func (a *App) ensureDock() *dockState {
	if a.dock == nil {
		a.dock = &dockState{mode: DockAuto}
	}
	return a.dock
}

// DockBump is the invalidation a source calls when something it shows moved: a
// plan published, a task list mutated, a tool result finished, a roster turned
// over. It takes no lock and carries no payload, so a per-event hook can afford
// it from any goroutine; the version stamp is what makes the NEXT frame rebuild
// rather than the next unrelated repaint.
func (a *App) DockBump() {
	dockBumpSeq.Add(1)
	a.poke()
}

// DockMode returns the current display policy.
func (a *App) DockMode() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.dock.policy()
}

func (d *dockState) policy() string {
	if d == nil {
		return DockAuto
	}
	return d.mode
}

// visible is the human's policy, then the width rule.
func (d *dockState) visible(avail int) bool {
	switch d.policy() {
	case DockShow:
		return true
	case DockHide:
		return false
	}
	return avail >= dockMinCols
}

// DockState is the panel's one-line answer for /settings: it is shown, or it is
// not, and here is why and what would change it.
func (a *App) DockState() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.dockOn() {
		return fmt.Sprintf("shown, %d columns — alt+s hides it", dockCols)
	}
	switch a.dock.policy() {
	case DockHide:
		return "hidden — alt+s cycles shown, hidden, pinned open"
	case DockShow:
		if a.width < dockCols+dockMin {
			return fmt.Sprintf("pinned open, but %d columns cannot hold the panel and a readable prompt — it needs %d", a.width, dockCols+dockMin)
		}
		return "pinned open — waiting for a session with something to track"
	}
	if a.width < dockMinCols {
		return fmt.Sprintf("auto: closed at %d columns (%d needed) — alt+s pins it open", a.width, dockMinCols)
	}
	return "auto: open, waiting for a session with something to track"
}

// dockCycle is Alt+s: shown, shut, pinned open regardless of width, and back to
// following the width rule — so a human can always get out of whatever they
// pinned into. The policy it lands on is the one cmd persists.
func (a *App) dockCycle() {
	a.mu.Lock()
	d := a.ensureDock()
	switch d.mode {
	case DockShow:
		d.mode = DockHide
	case DockHide:
		d.mode = DockAuto
	default:
		d.mode = DockShow
	}
	mode := d.mode
	a.mu.Unlock()
	if a.dockSetMode != nil {
		a.dockSetMode(mode)
	}
	a.poke()
}

// dockGridY returns the top screen row of the dock panel, or -1
// when it is not on. Callers hold a.mu.
func (a *App) dockGridY() int {
	if !a.dockOn() {
		return -1
	}
	top, _ := a.dockGrid()
	return top
}

// --- build ---

// dockBuild recomputes the panel from its sources. Called from paint() — where the
// lock is held, so a source may read App state — and only when something moved.
// A plan long enough to fill a terminal is the normal case, so the row budget is
// the panel's whole height story and what does not fit is counted and reported,
// never dropped in silence.
func (a *App) dockBuild() {
	d := a.ensureDock()
	if !a.dockOn() {
		d.lines = nil // closed: no source runs, and the next open frame rebuilds
		return
	}
	_, bandH := a.dockGrid()
	if bandH <= 0 {
		d.lines = nil // dockOn refuses this frame; belt, so no build has no band
		return
	}
	next := dockBumpSeq.Load()
	// Three things make a build stale: a source said it moved (version), the
	// grid changed — width, or the band's height, which the composer's growth
	// controls — and the transcript changing shape, since the Files section is
	// read from the blocks. The last one is a length, not a revision: a block
	// list is append-only and every path that shortens it (Reset, /clear, theme
	// reload) rebuilds the whole panel anyway.
	if d.lines != nil && next == d.version && a.width == d.buildW && bandH == d.bandH &&
		len(a.blocks) == d.buildBlocks {
		return
	}
	d.version, d.buildW, d.bandH, d.buildBlocks = next, a.width, bandH, len(a.blocks)
	// The identity slot is read once per build, never per frame — the same rule
	// every other source follows — so the session's name costs nothing until the
	// name, the version, or the grid moves.
	d.title, d.sid = "", ""
	if d.ops.Session != nil {
		d.title, d.sid = d.ops.Session()
	}
	d.titleLines = a.dockTitleLines(bandH)
	folds := a.collect()
	// The footer is the session, not the work: the id, the directory, the branch
	// — the facts the status row carries when it has room and the panel keeps
	// readable even when the transcript fills the height.
	if f, ok := a.dockFooter(); ok {
		folds = append(folds, f)
	}
	d.lines, _, _ = d.layout(folds, bandH)
	if d.lines == nil {
		d.lines = []dockRow{} // the built-and-empty state, so the next frame skips it
	}
}

// collect asks each source for its fold. Plan mode with no proposal out still
// gets a section: a section nobody can see is one nobody can wait on.
func (a *App) collect() []dockFold {
	ops := a.dock.ops
	var out []dockFold
	if ops.Plan != nil {
		pending, act := ops.Plan()
		if f, ok := dockPlanFold(pending, act); ok {
			out = append(out, f)
		}
	}
	add := func(id string, fn func() string) {
		if fn == nil {
			return
		}
		if f, ok := dockFoldOf(id, fn(), dockListMax); ok {
			out = append(out, f)
		}
	}
	add(dockTaskID, ops.Tasks)
	if f, ok := a.dockChanges(); ok {
		out = append(out, f)
	}
	add(dockAgentID, ops.Agents)
	add(dockMCPID, ops.MCP)
	if f, ok := a.dockTrajFold(); ok {
		out = append(out, f)
	}
	return out
}

// dockTrajFold is the panel's one button row: the ledger /trajectory opens,
// wired or omitted. It carries no session facts of its own — the heading comes
// from the same source the ledger reads — and the row is the panel's only
// clickable non-file row, so dockRowAt hands its act back to the caller.
func (a *App) dockTrajFold() (dockFold, bool) {
	if a.dock.ops.Trajectory == nil {
		return dockFold{}, false
	}
	head := strings.TrimSpace(a.dock.ops.Trajectory())
	if head == "" {
		return dockFold{}, false
	}
	return dockFold{id: dockTrajID, title: dockClip(head), max: 1,
		rows: []dockRow{{text: dockClip("click to open the ledger"), act: dockTrajID}}}, true
}

// dockPlanFold is the §2 surface: the proposed document, with the gesture that
// answers it riding at the top so the human reads the ask before the text.
func dockPlanFold(pending string, act bool) (dockFold, bool) {
	switch {
	case pending != "":
		body := strings.Split(strings.TrimRight(sanitizeOutput(pending), "\n"), "\n")
		rows := []dockRow{{text: dockClip("/plan off approves · or just type your feedback")}}
		for _, l := range body {
			rows = append(rows, dockRow{text: dockClip(l)})
		}
		return dockFold{id: dockPlanID, max: dockPlanMax,
			title: fmt.Sprintf("PLAN · proposed (%d lines)", len(body)), rows: rows}, true
	case act:
		return dockFold{id: dockPlanID, max: dockListMax, title: "PLAN · writing",
			rows: []dockRow{{text: dockClip("the model is drafting; propose submits it")}}}, true
	}
	return dockFold{}, false
}

// dockFoldOf splits a source block into its heading and its rows.
func dockFoldOf(id, raw string, max int) (dockFold, bool) {
	raw = strings.TrimRight(sanitizeOutput(raw), "\n")
	if strings.TrimSpace(raw) == "" {
		return dockFold{}, false
	}
	parts := strings.Split(raw, "\n")
	f := dockFold{id: id, title: dockClip(strings.TrimSpace(parts[0])), max: max}
	for _, l := range parts[1:] {
		if strings.TrimSpace(l) != "" {
			f.rows = append(f.rows, dockRow{text: dockClip(l)})
		}
	}
	return f, true
}

// dockChanges is the session's file list, read from the diffs the transcript
// already carries: Block.Diff is exactly what `git diff` prints, so one source
// feeds both surfaces and no second change-tracker is introduced here. Paths keep
// the order they were first touched in, and a file edited twice shows its last
// word — a net +5/-2 across two edits is what a human wants to see.
func (a *App) dockChanges() (dockFold, bool) {
	type tally struct{ add, del int }
	var order []string
	counts := map[string]*tally{}
	for _, b := range a.blocks {
		if b.Kind != KindToolDone || b.Diff == "" {
			continue
		}
		path := ""
		for _, r := range classifyDiff(strings.Split(strings.TrimRight(b.Diff, "\n"), "\n")) {
			switch r.kind {
			case diffFile:
				if p := strings.TrimPrefix(r.text, "+++ b/"); p != r.text && p != "/dev/null" {
					path = p
				}
			case diffAdded, diffRemoved:
				if path == "" {
					continue
				}
				t := counts[path]
				if t == nil {
					order = append(order, path)
					t = &tally{}
					counts[path] = t
				}
				if r.kind == diffAdded {
					t.add++
				} else {
					t.del++
				}
			}
		}
	}
	if len(order) == 0 {
		return dockFold{}, false
	}
	rows := make([]dockRow, 0, len(order))
	for _, p := range order {
		t := counts[p]
		r := dockRow{add: fmt.Sprintf("+%d", t.add), del: fmt.Sprintf("-%d", t.del), path: p}
		r.text = dockPath(p, dockInner-width(r.right())-1) // -1: a cell between name and counts
		rows = append(rows, r)
	}
	return dockFold{id: dockFileID, title: dockClip(fmt.Sprintf("FILES · %d", len(order))),
		max: dockListMax, rows: rows}, true
}

// dockPath fits a changed path to room cells by cutting from the LEFT — opencode's
// rule — so the file name survives and it is a directory that gets the ellipsis:
// two app.go in different trees stay distinguishable where the eye lands, which a
// basename cannot promise. The transcript block still carries the whole path for
// whoever wants it.
func dockPath(p string, room int) string {
	if room <= 0 {
		return ""
	}
	if width(p) <= room {
		return p
	}
	return runewidth.TruncatePrefix(p, room, "…")
}

// dockFooter is the panel's last section: the session's identity, from state the
// App already holds. Its heading carries the id that the title slot above shows
// only while the session has no name yet.
//
// The version rides here, under the branch it came from, and not in the title
// slot: this fold is the one section that is never folded away, and the first
// question about a session that behaves strangely is "which build is this".
// A host that never called SetVersion (a test harness, an embedder) paints
// nothing rather than a bare "xdev" — an empty promise is worse than no row.
func (a *App) dockFooter() (dockFold, bool) {
	id := a.dock.sid
	if id == "" {
		id = shortID(a.st.SessionID)
	}
	f := dockFold{id: "footer", title: dockClip("SESSION · " + id), max: 4}
	if a.cwd != "" {
		f.rows = append(f.rows, dockRow{text: dockClip(pathDisplay(a.cwd, dockInner))})
	}
	if a.branch != "" {
		f.rows = append(f.rows, dockRow{text: dockClip("on " + a.branch)})
	}
	// The model and the reasoning level are one request, so they are one row:
	// a bare "high" under a path and a branch reads as a name of its own, and
	// the divider already shows the pair beside each other. The section is the
	// one never folded away, so the request outlives a busy transcript.
	if l := a.thinkingLevel(); l != "" {
		row := l
		if a.st.Model != "" {
			row = a.st.Model + " · " + l
		}
		f.rows = append(f.rows, dockRow{text: dockClip(row)})
	}
	if a.version != "" {
		f.rows = append(f.rows, dockRow{text: dockClip("xdev " + a.version)})
	}
	if len(f.rows) == 0 {
		return dockFold{}, false
	}
	return f, true
}

// layout turns folds into painted rows for a band bandH rows tall, honoring the
// fold state, and reports the headings drawn and the rows that did not fit.
//
// The budget is spent top-down, and every row it drops pays for its own
// announcement: a section that lost rows gets a "+N more" row, and the panel ends
// with the total. That is why the fit test below walks the prefix length down
// rather than filling the band and clipping — rows reserved for the report are
// part of the cost of cutting, and a panel that trimmed its content into its last
// pixel would have to clip its own footnote, which is the silent loss this
// function exists to prevent.
func (d *dockState) layout(folds []dockFold, bandH int) (rows []dockRow, heads, hidden int) {
	if bandH < 5 {
		return nil, 0, 0
	}
	limit := bandH - d.titleRows() // the panel's title slot is not ours to paint
	// What each section would show at this fold state, before the band decides.
	want := make([]int, len(folds))
	for i, f := range folds {
		n := min(len(f.rows), f.max)
		switch d.fold {
		case foldShut:
			if f.id == dockPlanID || f.id == "footer" {
				break // the ask and the identity are never folded away
			}
			n = 0
		case foldOpen:
			n = len(f.rows)
		}
		want[i] = min(n, len(f.rows))
	}
	// The candidate rows in paint order, each tagged with its section so a prefix
	// of them can be recounted per section. A blank row separates sections.
	var cands []dockCand
	headAt := make([]int, len(folds))
	for i, f := range folds {
		if i > 0 {
			cands = append(cands, dockCand{fold: -1})
		}
		headAt[i] = len(cands)
		cands = append(cands, dockCand{fold: i, row: dockRow{text: dockClip(f.title), head: true}})
		for j := 0; j < want[i]; j++ {
			cands = append(cands, dockCand{fold: i, row: f.rows[j]})
		}
	}
	for keep := min(len(cands), limit); keep >= 0; keep-- {
		var headsNow int
		rows, headsNow, hidden = dockEmit(cands, headAt, folds, keep, limit, d.fold)
		if rows != nil {
			return rows, headsNow, hidden
		}
	}
	return nil, 0, 0
}

// dockEmit paints the first keep candidate rows and reports whether the result
// still fits limit. The markers a cut requires are appended here, so the fit test
// cannot lie about its own cost; a nil rows means "this prefix is too generous,
// try a shorter one".
func dockEmit(cands []dockCand, headAt []int, folds []dockFold, keep, limit int, fold int8) (rows []dockRow, heads, hidden int) {
	// Candidate rows each section kept, counting its heading.
	kept := make([]int, len(folds))
	for _, c := range cands[:keep] {
		if c.fold >= 0 {
			kept[c.fold]++
		}
	}
	for i, f := range folds {
		hidden += len(f.rows) - max(kept[i]-1, 0)
	}
	out := make([]dockRow, 0, keep+2*len(folds))
	for i, f := range folds {
		if headAt[i] >= keep {
			continue // its heading fell off too: counted, never half-painted
		}
		if i > 0 {
			out = append(out, dockRow{})
		}
		out = append(out, cands[headAt[i]].row)
		heads++
		shown := max(kept[i]-1, 0)
		for j := 0; j < shown; j++ {
			out = append(out, cands[headAt[i]+1+j].row)
		}
		// A folded section explains itself in the summary, not row by row.
		if cut := len(f.rows) - shown; cut > 0 && (shown > 0 || fold != foldShut) {
			out = append(out, dockRow{text: dockClip(fmt.Sprintf("+%d more", cut))})
		}
	}
	if hidden > 0 {
		note := fmt.Sprintf("%d rows not shown · ctrl+t folds", hidden)
		if fold == foldShut {
			note = fmt.Sprintf("%d rows folded away · ctrl+t opens them", hidden)
		}
		out = append(out, dockRow{}, dockRow{text: dockClip(note)})
	}
	if len(out) > limit {
		return nil, 0, hidden
	}
	return out, heads, hidden
}

// Fold states; the zero value lets each section decide itself.
const (
	foldAuto = int8(iota)
	foldOpen
	foldShut
)

// dockClip fits one line to the panel's interior with the package's clip():
// truncate, never wrap. A list row is a path or a one-line status, and a wrapped
// path is worse than an ellipsis; the plan document reads head-first, so a cut
// line scrolls off rather than pushing the rows below it out of the panel.
func dockClip(s string) string { return clip(strings.TrimRight(s, " "), dockInner) }

// --- geometry ---

// dockOn reports whether the panel paints this frame, and so whether anything
// else may use its columns. Callers hold a.mu.
//
// Three refusals, each one a promise to the human rather than to the panel: the
// welcome screen owns the right pane until a session exists; a terminal too narrow
// to leave the prompt its floor keeps its full width even when pinned open; and a
// terminal too short to hold a box reserves nothing, because columns given up for
// an empty band are a cost with no product. The band's height is measured from the
// composer, which wraps at its own full width and never at rightEdge, so this
// predicate does not run in a circle with the layout it gates.
func (a *App) dockOn() bool {
	if len(a.blocks) == 0 {
		return false
	}
	if a.width < dockCols+dockMin || !a.dock.visible(a.width) {
		return false
	}
	_, h := a.dockGrid()
	return h > 0
}

// dockReserve is the width every other surface lays out against: zero with the
// panel closed, the panel's columns with it open.
func (a *App) dockReserve() int {
	if !a.dockOn() {
		return 0
	}
	return dockCols
}

// rightEdge is the width the main pane lays out against: the last screen
// column the transcript band may paint in — its fills, its timestamps, its
// scrollbar, and the width its rows are wrapped at — and, since the panel
// became a window of its own, the width the top bar, the composer box and the
// status row stop at too. The panel used to live inside the transcript's rows
// and nothing else, which is why the surface a human types into kept the whole
// terminal; a two-window layout has no such exception. A terminal too narrow to
// give up the columns keeps its full width — the panel loses that argument.
func (a *App) rightEdge() int {
	r := a.width - a.dockReserve()
	if r < 20 {
		return 20
	}
	return r
}

// dockGrid returns the band the panel paints into: every row of the terminal,
// top to bottom, because the panel is a window beside the stream rather than a
// band inside it (opencode's sidebar). Nothing is reserved above or below it —
// the main pane's top bar, composer and status row are what it sits beside. A
// terminal too short to hold a title and a section reserves nothing. The height
// is a plain a.height, so a resize that changes the pane's proportions changes
// the row budget for free — the bandH the build cached is compared against it
// exactly as the old transcript-shaped band was.
func (a *App) dockGrid() (top, h int) {
	h = a.height
	if h < dockMinRows {
		return 0, 0
	}
	return 0, h
}

// --- paint ---

// dockSplit separates a section heading into the bold name and the dim count its
// source appended ("TASKS · 1/2 done" → "TASKS" + "· 1/2 done") — opencode's
// heading anatomy, so a section is found by name and counted only when the eye
// wants the number. A heading that carries no count paints whole, in bold.
func dockSplit(title string) (name, count string) {
	i := strings.Index(title, "·")
	if i < 0 {
		return strings.TrimRight(title, " "), ""
	}
	name = strings.TrimRight(title[:i], " ")
	if rest := strings.TrimSpace(title[i+len("·"):]); rest != "" {
		return name, "· " + rest
	}
	return name, ""
}

// dockTitle is the prompt the dock title takes when the session has one:
// the session title names the conversation, the first prompt names
// what it is about. Tried before the id so a dock with a live session
// shows its task, not its hash.
func (a *App) dockTitle() string {
	if d := a.dock; d != nil && d.title != "" {
		return d.title
	}
	if first := a.firstUserPrompt(); first != "" {
		return first
	}
	return ""
}

// titleRows is the rows the title slot paints, which is what every geometry
// below the slot has to shift by: 1 before the first build, the wrapped slot
// after it. Never zero — the panel's name always has its row.
func (d *dockState) titleRows() int {
	if d == nil || len(d.titleLines) == 0 {
		return 1
	}
	return len(d.titleLines)
}

// dockTitleLines wraps the session's name to the panel's interior instead of
// clipping it. The name is the one string in the panel a human reads whole, and
// a "\u2026" at 38 cells turned "showing full title" into a riddle — so it is
// not capped by row count: it wraps over as many rows as it takes and layout
// spends those rows out of the content budget. The band is the only bound left,
// because a title row that is not on screen is not a title. Caller holds a.mu.
func (a *App) dockTitleLines(bandH int) []string {
	return wrapCapped(strings.TrimSpace(sanitizeOutput(a.dockTitle())), dockInner,
		max(1, bandH-dockTitleFloor))
}

// selDockRowsForPaint returns the dock's rows as selectable rows with their
// screen y positions: the title slot's lines first, then the rows the build
// made below it. The title slot belongs in the table for the same reason the
// rest does — it holds the one string a human reaches for to copy (the
// session's name, or its first prompt when the session has no title of its
// own), and a drag across it used to fall through to the transcript rows
// painted behind the panel, which clipped the cell range to nothing.
// Callers hold a.mu.
func (a *App) selDockRowsForPaint() []selRow {
	if !a.dockOn() {
		return nil
	}
	x0 := a.width - dockCols + dockPad
	rows := make([]selRow, 0, len(a.dock.lines)+a.dock.titleRows())
	dg := a.dockGridY()
	for i, line := range a.dock.titleLines {
		if line != "" {
			rows = append(rows, selRow{text: line, x0: x0, y: dg + i})
		}
	}
	for i, r := range a.dock.lines {
		t := r.dockRowText()
		if t == "" {
			continue
		}
		rows = append(rows, selRow{text: t, x0: x0, y: dg + a.dock.titleRows() + i})
	}
	return rows
}

// drawDock paints the panel: the surface, the session's own name in the top slot,
// and the rows the build made for the band they were budgeted for. Caller holds
// a.mu and has run dockBuild for this frame.
//
// There is no box, deliberately. The panel is a surface of its own on the
// terminal's own background with a two-cell gutter — the shape opencode's
// sidebar has. A border drawn around a column that already fills its own
// background is one line of chrome too many, and it costs the interior two
// columns.
func (a *App) drawDock(s tcell.Screen, x, top, h int) {
	if h <= 0 {
		return
	}
	d := a.dock
	// The panel's field is the terminal's own background, never a colour this
	// program picked — the same call drawDiffOverlay makes (#466). A themed fill
	// here (bg_base, #141414) put a grey band beside a black transcript; SGR 49
	// resolves the panel to whatever the terminal is, which also keeps a light
	// theme from stranding its dark ink on a black surface.
	body := tcell.StyleDefault
	ink := body.Foreground(a.cellColor(a.th.Get(theme.TextPrimary)))
	dim := body.Foreground(a.cellColor(a.th.Get(theme.GrayDim)))
	// The change counts wear the diff's own inks, on the panel's background: a
	// file's "+N" is the green its diff block already paints with — the MARKER
	// ink, since the panel has no band behind a two-character count. Built from
	// StyleDefault, so it carries the terminal's background and only the
	// foreground is the diff's own.
	ds := a.diffStyle()
	added, removed := ds.add.mark, ds.del.mark
	for y := top; y < top+h; y++ {
		for cx := x; cx < x+dockCols; cx++ {
			s.SetContent(cx, y, ' ', nil, body)
		}
	}
	head := d.titleRows()
	for i, line := range d.titleLines {
		drawText(s, x+dockPad, top+i, line, ink.Bold(true))
	}
	for i, r := range d.lines {
		y := top + head + i
		if y >= top+h {
			break
		}
		switch {
		case r.head:
			name, count := dockSplit(r.text)
			drawText(s, x+dockPad, y, name, ink.Bold(true))
			if count != "" {
				drawText(s, x+dockPad+width(name)+1, y, count, dim)
			}
		case r.right() != "":
			// A changed file: the path flush left, its counts flush right, so the
			// numbers line up down the column whatever the names are.
			cx := x + dockPad + dockInner - width(r.right())
			drawText(s, x+dockPad, y, r.text, ink)
			drawText(s, cx, y, r.add, added)
			drawText(s, cx+width(r.add)+1, y, r.del, removed)
		case r.act != "":
			// A button row: the panel's own action, not a fact about the
			// session, so it wears the accent the links wear.
			drawText(s, x+dockPad, y, r.text, body.Foreground(a.cellColor(a.th.Get(theme.AccentUser))))
		default:
			drawText(s, x+dockPad, y, r.text, ink)
		}
	}
}

// dockClick resolves a screen cell inside the panel to the file path
// the click hit, or "" for everything that is not a changed-file row.
// Callers hold a.mu.
func (a *App) dockClick(x, y int) string {
	path, _ := a.dockRowAt(x, y)
	return path
}

// dockRowAt resolves a screen cell to the row's own action: the file path a
// FILES row opens, or the act a button row carries ("" on everything else).
// The column test is here, not at the call sites: a press in the TRANSCRIPT
// columns that happens to land on a row the panel also paints must stay the
// transcript's (dockAt's whole job), and both callers — the press branch in
// selection.go and closeDiffOverlayOnClick — want a cell IN the panel.
// Callers hold a.mu.
func (a *App) dockRowAt(x, y int) (path, act string) {
	// dockAt also answers the column: a cell outside the panel's columns is
	// the transcript's, whatever row it shares with the panel.
	if !a.dockAt(x, y) {
		return "", ""
	}
	d := a.dock
	if d == nil || d.lines == nil {
		return "", ""
	}
	top, h := a.dockGrid()
	head := d.titleRows() // the title slot's rows, not the transcript's
	if y < top+head || y >= top+h {
		return "", ""
	}
	i := y - top - head
	if i < 0 || i >= len(d.lines) {
		return "", ""
	}
	return d.lines[i].path, d.lines[i].act
}

// dockAt reports whether a screen cell is inside the panel's own columns — the
// window the human clicked into. The panel is chrome, but it is a window: a
// click on it belongs to it, so the transcript's hit-tests (the user-message
// menu, the think-box aim, a link) must not reach across and claim a cell the
// panel painted. A drag still crosses freely — selection reads whatever rows
// the release covers, and the panel's rows are in that table (selDockRows).
// Callers hold a.mu.
func (a *App) dockAt(x, y int) bool {
	if !a.dockOn() {
		return false
	}
	top, h := a.dockGrid()
	return x >= a.width-dockCols && y >= top && y < top+h
}

// dockJumpToBlock walks the transcript for the most recent finished
// tool result that changed path, marks it expanded so its diff paints,
// and pushes the viewport to its first row; returns whether one was
// found.
func (a *App) dockJumpToBlock(path string) bool {
	for i := len(a.blocks) - 1; i >= 0; i-- {
		b := a.blocks[i]
		if b.Kind != KindToolDone || b.Diff == "" {
			continue
		}
		if !strings.Contains(b.Diff, path) {
			continue
		}
		b.Expanded = true
		a.sync(a.contentWidth())
		if i < len(a.rowIdx.start) && a.rowIdx.start[i+1] > a.rowIdx.start[i] {
			target := int(a.rowIdx.start[i]) + a.transcriptTop()
			vp := a.viewportLinesLocked()
			cur := a.sm.Start(a.totalLinesLocked(), vp)
			n := target - cur
			if n < 0 {
				n = -n
			}
			a.sm.ScrollUp(max(1, n-1), a.totalLinesLocked(), vp)
			return true
		}
		return true
	}
	return false
}

// diffOverlay is the state of a full-width overlay shown when the
// user clicks a changed file: the dock is ~40 columns, so the unified
// diff gets its own surface. Esc closes it.
type diffOverlay struct {
	path      string
	diff      string
	lines     []line
	width     int
	scrollVp  int
	scrollOff int
}

// openDiffOverlay renders the diff for the newest finished tool block
// that touches path and stores it as the active overlay.
//
// A click reaches here with a.mu already held: handleMouse's non-wheel
// branch in app.go takes it around the whole gesture, and has since the
// dock's first clickable row (#371, #373). So this takes no lock of its
// own — every caller holds it — and a second Lock here would wedge the
// UI thread on the first dock FILES click.
func (a *App) openDiffOverlay(path string) bool {
	for i := len(a.blocks) - 1; i >= 0; i-- {
		b := a.blocks[i]
		if b.Kind != KindToolDone || b.Diff == "" {
			continue
		}
		if !strings.Contains(b.Diff, path) {
			continue
		}
		b.Expanded = true
		w := a.contentWidth()
		ov := &diffOverlay{path: path, diff: b.Diff, width: w}
		ov.lines = a.diffCells(ov.diff, ov.width-4)
		// scrollVp is the painted body height, not the full line count —
		// setting it to len(lines) made maxOff always 0 so nothing scrolled.
		// drawDiffOverlay refreshes this from the real panel each frame.
		ov.scrollVp = max(1, a.height-a.composerRows()-6)
		ov.scrollOff = 0
		a.diffOv = ov
		return true
	}
	return false
}

// closeDiffOverlay dismisses the overlay on the next frame. It takes a.mu
// itself, so it is for callers that do not hold it; a caller inside the
// lock calls closeDiffOverlayLocked instead. Locking twice in one
// goroutine is not a deadlock this program recovers from — it is the UI
// thread, and the freeze it produces is the whole session.
func (a *App) closeDiffOverlay() {
	a.mu.Lock()
	a.closeDiffOverlayLocked()
	a.mu.Unlock()
	a.poke()
}

// closeDiffOverlayLocked clears the overlay. Callers hold a.mu, so they also
// poke for the frame themselves.
func (a *App) closeDiffOverlayLocked() {
	a.diffOv = nil
}

// drawDiffOverlay renders the full-width diff surface above the composer.
func (a *App) drawDiffOverlay(yComposerTop int) {
	ov := a.diffOv
	if ov == nil {
		return
	}
	s := a.scr
	w := a.width
	x := 2
	y0 := 1
	panelH := yComposerTop - y0 - 1
	if panelH < 4 {
		return
	}
	brdSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentTool)))
	fgSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.TextPrimary)))
	dimSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim)))
	// The panel's field is the terminal's own background, never a colour this
	// program picked. A themed fill here (bg_base) painted the blank interior
	// one colour while every text cell kept the default, so the viewer came up
	// as black bars on a grey band; filling with the default style (SGR 49)
	// resolves the whole panel alike — the popup is the same black the
	// transcript and composer already are. It still fills: the transcript is
	// painted earlier in this frame and must not show through the viewer.
	bg := tcell.StyleDefault
	box := a.th.Box()
	fillPanelRows(s, y0, y0+panelH-1, x, w-x, bg)
	top := boxTop(box, brdSt, "", w-2*x)
	bot := boxBottom(box, brdSt, w-2*x)
	cx := x
	for _, r := range top.runs {
		drawText(s, cx, y0, r.text, r.style)
		cx += width(r.text)
	}
	cx = x
	for _, r := range bot.runs {
		drawText(s, cx, y0+panelH-1, r.text, r.style)
		cx += width(r.text)
	}
	vr := boxRune(box.Vertical)
	for y := y0 + 1; y < y0+panelH-1; y++ {
		s.SetContent(x, y, vr, nil, brdSt)
		s.SetContent(w-x-1, y, vr, nil, brdSt)
	}
	drawText(s, x+2, y0+1, "diff "+ov.path, fgSt.Bold(true))
	// Body rows: title at y0+1, footer at y0+panelH-2, bottom border at
	// y0+panelH-1 → panelH-3 interior lines the viewport can show.
	ov.scrollVp = max(1, panelH-3)
	maxOff := max(0, len(ov.lines)-ov.scrollVp)
	if ov.scrollOff > maxOff {
		ov.scrollOff = maxOff
	}
	start := ov.scrollOff
	end := start + ov.scrollVp
	if end > len(ov.lines) {
		end = len(ov.lines)
	}
	for i := start; i < end; i++ {
		y := y0 + 2 + (i - start)
		if y >= y0+panelH-1 {
			break
		}
		// The rows carry their own styles — diffCells already painted the
		// marker, the band and the word runs — so the viewer repaints them as
		// they are. Repainting every +/- row in one ink (which this used to
		// do) flattened the band and the word emphasis back into plain text.
		for _, r := range ov.lines[i].runs {
			drawText(s, x+2, y, r.text, r.style)
		}
	}
	drawText(s, x+2, y0+panelH-2, "Esc close · ↑↓ scroll", dimSt)
}

// closeDiffOverlayOnClick dismisses the diff overlay when the human
// clicks outside it, or re-opens it when clicking a different changed
// file in the dock. The overlay is a modal surface covering the whole
// width and hiding the transcript; a dock click re-points it to a
// different file, anything else closes it, so the overlay is never
// pinned — fixing "always showing" and "no way to close".
// Caller holds a.mu (handleMouse press branch).
func (a *App) closeDiffOverlayOnClick(x, y int) {
	overlayTop, overlayBot := 1, a.height-1-a.composerRows()
	inside := x >= 2 && x < a.width-2 && y >= overlayTop && y < overlayBot
	if inside {
		if path := a.dockClick(x, y); path != "" {
			a.openDiffOverlay(path)
			a.poke()
			return
		}
	}
	a.closeDiffOverlayLocked()
}

// diffBodyScroll advances the overlay viewport by n lines (down=true)
// or toward older rows (down=false).
func (a *App) diffBodyScroll(n int, down bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ov := a.diffOv
	if ov == nil || len(ov.lines) == 0 {
		return
	}
	maxOff := max(0, len(ov.lines)-ov.scrollVp)
	if down {
		ov.scrollOff += n
	} else {
		ov.scrollOff -= n
	}
	ov.scrollOff = max(0, min(ov.scrollOff, maxOff))
	a.poke()
}

// --- keys ---

// dockKey routes the panel's two chords, called from the keymap's action switch —
// which runs after every modal handler, so the dock can never take a key from a
// card or a picker. The panel holds no focus of its own: a fold is a display
// state, so the transcript still scrolls and the composer still types beside it.
func (a *App) dockKey(action string) bool {
	switch action {
	case "dock-cycle":
		a.dockCycle()
	case "dock-fold":
		a.dockFoldCycle()
	case "dock-close-diff":
		return a.dockCloseDiff()
	case "dock-toggle-diff":
		return a.dockToggleDiff()
	default:
		return false
	}
	return true
}

// dockAct runs the panel's own button rows — the surfaces a click opens that
// are not the diff overlay. The click path already holds a.mu, so every action
// here must be safe under it: the trajectory ledger opens through its own
// registry lock, never a.mu.
// ponytail: one action. The upgrade path is dockKey's action vocabulary if a
// second button row ever earns its place.
func (a *App) dockAct(act string) bool {
	switch act {
	case dockTrajID:
		return a.OpenTrajectory()
	default:
		return false
	}
}

// dockCloseDiff dismisses the overlay on Esc.
func (a *App) dockCloseDiff() bool {
	if a.diffOv != nil {
		a.closeDiffOverlay()
		return true
	}
	return false
}

// dockToggleDiff closes the overlay if open, or re-opens it
// for the dock's last changed-file row.
func (a *App) dockToggleDiff() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.diffOv != nil {
		a.closeDiffOverlayLocked()
		return true
	}
	for i := len(a.dock.lines) - 1; i >= 0; i-- {
		if a.dock.lines[i].path != "" {
			return a.openDiffOverlay(a.dock.lines[i].path)
		}
	}
	return false
}

// pending document shut, and back. The proposal is never folded away — the human
// is being asked something, and a fold that hides the question has answered it.
func (a *App) dockFoldCycle() {
	a.mu.Lock()
	d := a.ensureDock()
	d.fold = (d.fold + 1) % 3
	d.lines = nil // the fold state is what the row budget depends on
	a.mu.Unlock()
	a.poke()
}

// dockOverlayScroll advances the diff overlay's viewport
// by n lines (down=true) or toward older rows (down=false).
func (a *App) dockOverlayScroll(n int, down bool) {
	a.diffBodyScroll(n, down)
}

// handleDiffOverlayKey owns ↑↓ / PgUp/PgDn / Home/End while the diff
// overlay is open, and Esc closes it. The footer advertises ↑↓ scroll;
// without the Esc case it fell through to the double-Esc rewind block
// in handleKey and the overlay could never be dismissed by keyboard —
// "Esc close" in the footer was a lie. Returns true when the key was consumed.
func (a *App) handleDiffOverlayKey(key *tcell.EventKey) bool {
	a.mu.Lock()
	ov := a.diffOv
	a.mu.Unlock()
	if ov == nil {
		return false
	}
	switch key.Key() {
	case tcell.KeyEsc:
		a.closeDiffOverlay()
		return true
	case tcell.KeyUp:
		a.diffBodyScroll(1, false)
	case tcell.KeyDown:
		a.diffBodyScroll(1, true)
	case tcell.KeyPgUp:
		a.diffBodyScroll(max(1, ov.scrollVp-1), false)
	case tcell.KeyPgDn:
		a.diffBodyScroll(max(1, ov.scrollVp-1), true)
	case tcell.KeyHome:
		a.diffBodyScroll(len(ov.lines), false)
	case tcell.KeyEnd:
		a.diffBodyScroll(len(ov.lines), true)
	default:
		return false
	}
	return true
}

// diffOverlayOpen reports whether the full-width diff surface is up.
// Callers that only need the presence check (wheel, scroll actions) use
// this so they do not race a frame that just closed it.
func (a *App) diffOverlayOpen() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.diffOv != nil
}
