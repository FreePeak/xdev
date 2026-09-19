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
	dockListMax = 6                    // rows one list shows before "+N more"
	dockPlanMax = 18                   // rows the plan document may take: the section is
	// the reason the panel exists, so it gets the bigger half of the budget.
)

// Section ids, in the order the panel paints them: what the session is doing,
// what it is waiting on, then the artifacts. They double as fold keys.
const (
	dockPlanID  = "plan"
	dockTaskID  = "tasks"
	dockFileID  = "files"
	dockAgentID = "agents"
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
	return out
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
func (a *App) dockFooter() (dockFold, bool) {
	id := a.dock.sid
	if id == "" {
		id = shortID(a.st.SessionID)
	}
	f := dockFold{id: "footer", title: dockClip("SESSION · " + id), max: 3}
	if a.cwd != "" {
		f.rows = append(f.rows, dockRow{text: dockClip(pathDisplay(a.cwd, dockInner))})
	}
	if a.branch != "" {
		f.rows = append(f.rows, dockRow{text: dockClip("on " + a.branch)})
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
	limit := bandH - 1 // the panel's title slot is not ours to paint
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

// rightEdge is the last screen column the transcript band may paint in — its
// fills, its timestamps, its scrollbar, and the width its rows are wrapped at.
// The composer, the info divider and the status row deliberately keep the full
// terminal: the panel lives inside the transcript's rows and nothing else, so
// opening it never squeezes the surface a human types into. A terminal too narrow
// to give up the columns keeps its full width — the panel loses that argument.
func (a *App) rightEdge() int {
	r := a.width - a.dockReserve()
	if r < 20 {
		return 20
	}
	return r
}

// dockGrid returns the band the panel paints into: the transcript's rows, so it
// starts under the top bar and stops above the composer's box.
func (a *App) dockGrid() (top, h int) {
	top = a.transcriptTop()
	h = a.height - 2 - a.composerRows() - top
	if h < 5 {
		return top, 0
	}
	return top, h
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
	if first, _ := a.topPrompts(); first != "" {
		return first
	}
	return ""
}

// selDockRowsForPaint returns the dock's rows as selectable
// rows with their screen y positions. Callers hold a.mu.
func (a *App) selDockRowsForPaint() []selRow {
	if !a.dockOn() || a.dock.lines == nil {
		return nil
	}
	x0 := a.width - dockCols + dockPad
	rows := make([]selRow, 0, len(a.dock.lines))
	dg := a.dockGridY()
	for i, r := range a.dock.lines {
		t := r.dockRowText()
		if t == "" {
			continue
		}
		rows = append(rows, selRow{text: t, x0: x0, y: dg + 1 + i})
	}
	return rows
}

// drawDock paints the panel: the surface, the session's own name in the top slot,
// and the rows the build made for the band they were budgeted for. Caller holds
// a.mu and has run dockBuild for this frame.
//
// There is no box, deliberately. The panel is a surface of its own on the theme's
// panel background with a two-cell gutter — the shape opencode's sidebar has. A
// border drawn around a column that already fills its own background is one line
// of chrome too many, and it costs the interior two columns.
func (a *App) drawDock(s tcell.Screen, x, top, h int) {
	if h <= 0 {
		return
	}
	d := a.dock
	bg := tcell.ColorDefault
	if c, ok := a.th.Slot(theme.BgBase); ok {
		bg = a.cellColor(c)
	}
	body := tcell.StyleDefault.Background(bg)
	ink := body.Foreground(a.cellColor(a.th.Get(theme.TextPrimary)))
	dim := body.Foreground(a.cellColor(a.th.Get(theme.GrayDim)))
	// The change counts wear the diff's own inks, on the panel's background: a
	// file's "+N" is the green its diff block already paints with.
	ds := a.diffStyle()
	added, removed := ds.added.Background(bg), ds.removed.Background(bg)
	for y := top; y < top+h; y++ {
		for cx := x; cx < x+dockCols; cx++ {
			s.SetContent(cx, y, ' ', nil, body)
		}
	}
	drawText(s, x+dockPad, top, dockClip(a.dockTitle()), ink.Bold(true))
	for i, r := range d.lines {
		y := top + 1 + i
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
		default:
			drawText(s, x+dockPad, y, r.text, ink)
		}
	}
}

// dockClick resolves a screen cell inside the panel to the file path
// the click hit, or "" for everything that is not a changed-file row.
// Callers hold a.mu.
func (a *App) dockClick(x, y int) string {
	if !a.dockOn() {
		return ""
	}
	d := a.dock
	if d == nil || d.lines == nil {
		return ""
	}
	top, h := a.dockGrid()
	if y < top+1 || y >= top+h {
		return ""
	}
	i := y - top - 1
	if i < 0 || i >= len(d.lines) {
		return ""
	}
	return d.lines[i].path
}

// dockJumpToBlock walks the transcript for the most recent finished
// tool result that changed path, marks it expanded so its diff paints,
// and pushes the viewport to its first row; returns whether one was
// found.
func (a *App) dockJumpToBlock(path string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
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
func (a *App) openDiffOverlay(path string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
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
		ov.scrollVp = max(1, len(ov.lines))
		ov.scrollOff = 0
		a.diffOv = ov
		return true
	}
	return false
}

// closeDiffOverlay dismisses the overlay on the next frame.
func (a *App) closeDiffOverlay() {
	a.mu.Lock()
	a.diffOv = nil
	a.mu.Unlock()
	a.poke()
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
	bg := tcell.StyleDefault.Background(a.cellColor(a.th.Get(theme.BgBase)))
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
	ds := a.diffStyle()
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
		ln := ov.lines[i]
		if len(ln.runs) > 0 && strings.HasPrefix(ln.runs[0].text, "+") {
			for _, r := range ln.runs {
				drawText(s, x+2, y, r.text, ds.added)
			}
		} else if len(ln.runs) > 0 && strings.HasPrefix(ln.runs[0].text, "-") {
			for _, r := range ln.runs {
				drawText(s, x+2, y, r.text, ds.removed)
			}
		} else {
			for _, r := range ln.runs {
				drawText(s, x+2, y, r.text, r.style)
			}
		}
	}
	drawText(s, x+2, y0+panelH-2, "Esc close · ↑↓ scroll", dimSt)
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
	if a.diffOv != nil {
		a.closeDiffOverlay()
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
