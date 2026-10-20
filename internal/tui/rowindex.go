package tui

import (
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// --- the row index -------------------------------------------------------
//
// omp's ScrollView renders a PREBUILT buffer of visual lines and slices the
// viewport out of it, so one frame costs the viewport and never the session
// (packages/tui/src/components/scroll-view.ts: render() walks `height` rows of
// a line array; the array is replaced by setLines/setTotalRows, not rebuilt
// per frame). xdev had no such buffer: every frame walked every block, hashed
// a composite cache key per block, and copied every row of the transcript into
// a fresh slice — 85k rows measured 7.8 ms per frame, and the loop repaints at
// 30fps while a turn runs, which is the long-session lag and a permanent slice
// of one core.
//
// rowIndex is that buffer, split per block instead of globally so a resize or
// a settled tool result only re-renders the blocks whose inputs moved:
//
//   - one render per block (blockRend), stamped with the inputs that produced
//     it, so a settled block costs one struct compare per frame;
//   - cumulative row offsets (start), so the total row count is O(1) for the
//     scroll keys and the viewport's first block is a binary search;
//   - the row cap (transcriptCap), which drops the OLDEST blocks outright so
//     the resident render tracks the window instead of how long the session
//     has been running.
//
// A superseded render is replaced in place, never kept alongside: the old
// per-state map cache retained every intermediate render of a streaming block
// (measured +1200 stale entries and 274 MB heap for one long assistant
// message), which is the other half of why long sessions stopped responding.

// Row budgets for ONE result's render window. A finished result longer than
// the window keeps its head and its tail with the elided row count between
// them, and Ctrl+O (Block.Expanded) opts that box out of the window entirely.
// Nothing else is windowed: thinking, tool calls, results that fit, user
// prompts and assistant prose render every row they have.

// transcriptCap is the most rows the row index keeps. Past it the OLDEST
// blocks are dropped outright — their text, their render and their rows — so a
// session that runs for hours costs what one that ran for minutes costs. The
// session JSONL is the record; the transcript is a window on it, and the window
// moves silently: the reader scrolls, the head is gone, the session still has
// every row.
//
// It is a ROW budget, not a block count, because rows are what the reader
// scrolls and what the renderer materializes; the two were measured together
// on a 200-turn fixture (60k rows, ~40 MB of renders before the cap).
//
// 10 000 is the shipped ceiling: high enough that a working session reads as
// the whole conversation, while the cap still bounds a session that runs for
// hours.
const transcriptCap = 10000

// transcriptKeepTail is how many of the newest blocks a trim never touches: the
// live turn (the streaming assistant row, the calls it opened, their child
// rows) has to survive, or the answer being written would lose the rows that
// explain it.
const transcriptKeepTail = 8

const (
	toolRecentHead = 200
	toolRecentTail = 50
	// thinkBoxCollapsed is an unfocused reasoning box's body height, and
	// thinkBoxRows the height a click grows it to. Reasoning is the one block
	// that can outgrow any screen, and a fixed twelve rows per thought pushed
	// the turn's real content off the screen: collapsed, the box is one row —
	// the newest thought — and a click opens the window the wheel scrolls.
	thinkBoxCollapsed = 1
	thinkBoxRows      = 12
)

// blockRend is one block's render: the stamp of the inputs that produced it,
// the visual lines, the decoration every row of the block shares, and how many
// transcript rows it occupies (its lines plus the blank separator row).
//
// subs are the child rows inside it a click can open, recorded by the render
// that produced the lines — so a hit can never point at a row offset a
// DIFFERENT layout put a child on. Cleared by reset with everything else.
type blockRend struct {
	key   blockKey
	lines []line
	rail  string
	railS tcell.Style
	ts    string // right-aligned timestamp, first row only
	rows  int32
	subs  []subHit
}

// rowView is one transcript row inside the viewport, ready to paint.
type rowView struct {
	ln    line
	rail  string
	railS tcell.Style
	ts    string
}

// rowIndex is the flattened layout of the transcript. Guarded by App.mu, so
// every method here assumes the caller holds it.
type rowIndex struct {
	w     int         // content width the renders were produced at
	rend  []blockRend // parallel to App.blocks
	start []int32     // start[i] = first row of block i; len(start) == len(rend)+1
	dirty int32       // first row offset that may be stale (0 = everything)
	view  []rowView   // scratch buffer for the visible window
}

// reset drops every render (resize, theme swap, /clear, transcript rewrite).
func (x *rowIndex) reset() {
	x.rend = x.rend[:0]
	x.start = append(x.start[:0], 0)
	x.dirty = 0
}

// grow extends the per-block tables to n blocks, leaving the new slots
// unstamped so the next sync renders them.
func (x *rowIndex) grow(n int) {
	x.rend = append(x.rend, make([]blockRend, n-len(x.rend))...)
	x.start = append(x.start, make([]int32, n+1-len(x.start))...)
}

// markDirty records that block i's row count may have changed, so the next
// sync recomputes the row offsets from there.
func (x *rowIndex) markDirty(i int) {
	if int32(i) < x.dirty {
		x.dirty = int32(i)
	}
}

// total is the transcript's row count; valid after sync.
func (x *rowIndex) total() int32 {
	if len(x.start) == 0 {
		return 0
	}
	return x.start[len(x.start)-1]
}

// blockAt is the block whose transcript rows contain row r, or -1 when there is
// no such row. Binary search over the cumulative offsets, the same lookup
// viewRows does for the viewport. Valid after sync.
func (x *rowIndex) blockAt(r int32) int {
	n := len(x.rend)
	if n == 0 || r < 0 || r >= x.total() {
		return -1
	}
	lo, hi := 0, n-1
	for lo < hi {
		mid := (lo + hi) / 2
		if x.start[mid+1] > r {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}

// --- stamps --------------------------------------------------------------

// renderKey is the stamp of everything a block's render reads. A block whose
// stamp is unchanged never re-renders, which is what makes the per-frame cost
// independent of the session length.
//
// A running tool row carries a live elapsed that no field of the block changes,
// so its stamp ages in whole seconds: one row re-renders per tick and every
// other block keeps its render.
//
// A row that PAINTS the running frame carries the frame index instead: the
// index moves every 25ms tick, so without it in the stamp a cached row kept
// the glyph it was first rendered with. That is why the tool bullet crawled at
// one frame a second (only `age` moved), the reasoning box froze outright (it
// had neither `age` nor any stamp), and a subagent's child rows never moved at
// all — one state, three different animations.
func (a *App) renderKey(i int, b *Block, w int) blockKey {
	var age int64
	spin := -1
	switch {
	case b.Kind == KindTool && b.Status == "running":
		if !b.Ts.IsZero() {
			age = int64(time.Since(b.Ts).Seconds())
		}
		spin = a.st.spinnerIdx
	case b.Kind == KindThinking && b.stream:
		spin = a.st.spinnerIdx
	}
	// A call row painting child rows takes the frame too, but only while one
	// of them is still working: a settled batch is static, and re-rendering it
	// every tick would buy nothing.
	if spin < 0 && len(b.Sub) > 0 {
		for _, c := range b.Sub {
			if c.Status == "running" {
				spin = a.st.spinnerIdx
				break
			}
		}
	}
	return blockKey{
		idx: i, kind: b.Kind, width: w, tlen: len(b.Text),
		tool: b.ToolName, status: b.Status, stream: b.stream,
		expanded: b.Expanded, age: age, spin: spin,
		dlen: len(b.Diff), thinkOff: b.ThinkOff, focused: i == a.thinkFocus,
		live: b.liveSeq, sub: b.subSeq, mermaid: a.renderMermaid,
	}
}

// lineText concatenates every run of a rendered line, so a reader gets the
// whole visual row (a box splits each row into border/content/border runs)
// instead of one run. The collapsedAt hit-test reads a painted row back
// through it, which is why it lives here rather than in a test file.
func lineText(ln line) string {
	var b strings.Builder
	for _, r := range ln.runs {
		b.WriteString(r.text)
	}
	return b.String()
}

// --- reconciliation ------------------------------------------------------

// clearRenderCache drops every cached render and the row layout. Every path
// that changes a rendering input outside a block's stamp (theme, showThinking,
// resize, transcript swap) goes through here.
func (a *App) clearRenderCache() {
	a.rowIdx.reset()
}

// sync brings the row layout up to date at content width w and returns the
// transcript's total row count. Only blocks from the first one whose stamp
// moved are re-rendered, so a frame during a streaming turn costs the blocks
// at the tail instead of the whole session.
func (a *App) sync(w int) int32 {
	x := &a.rowIdx
	n := len(a.blocks)
	if x.w != w {
		x.w = w
		x.reset()
	}
	if n < len(x.rend) {
		// Blocks were dropped (/clear, transcript rewrite). start[i] for the
		// retained prefix is still exact: it counts rows of blocks before i.
		x.rend = x.rend[:n]
		x.start = x.start[:n+1]
	}
	if len(x.rend) < n {
		x.grow(n)
	}
	from := int32(n)
	for i := range n {
		if x.rend[i].key != a.renderKey(i, a.blocks[i], w) {
			from = int32(i)
			break
		}
	}
	if x.dirty < from {
		from = x.dirty // a direct blockLines call may have shifted row counts
	}

	total := a.reconcile(w, from)
	// The row cap last, on the offsets just computed: a trim can only ever
	// shrink the transcript, so it never invalidates the scan above.
	a.trimTranscript(total)
	total = a.rowIdx.total()
	return total
}

// reconcile renders every block from `from` to the end and returns the
// transcript's row count. `from` is a hint, not a promise: the per-block
// offsets before it are already exact, which is what makes a frame during a
// streaming turn cost the tail instead of the session.
//
// The row cap is a bound on the renders HELD, not only on the rows kept, so
// the pass releases the renders the cap has already decided to drop instead of
// waiting for the trim that follows it. A block is dropped when the rows from
// it to the end of the transcript pass the cap; the pass knows the rows from
// it to the block it just rendered, which is a lower bound on that sum, so a
// block whose lower bound is already over the cap is dropped for certain and
// its render can go now. The newest transcriptKeepTail blocks are never
// dropped, so they are never released.
//
// Without this the pass held the whole session's renders at once and the trim
// gave them back afterwards: measured 204 000 lines (420 MB of heap) for an
// 800-block transcript, against a 10 000-row cap that keeps 40 of those
// blocks. The bound the PRD states (§3.7, "the 10 000-row cap is the whole
// bound") is on the renders held, and this is what makes it true.
func (a *App) reconcile(w int, from int32) int32 {
	x := &a.rowIdx
	n := int32(len(a.blocks))
	total := x.start[from]
	// win is the rows of the blocks whose renders are held, from the release
	// pointer to the last one rendered.
	win := total
	release := int32(0)
	keepFrom := n - transcriptKeepTail // the trim never drops the newest tail
	for i := from; i < n; i++ {
		b := a.blocks[i]
		r := &x.rend[i]
		r.lines = a.blockLines(int(i), b, w)
		r.key = a.renderKey(int(i), b, w)
		r.rail, r.railS, r.ts = a.rowChrome(b)
		r.rows = int32(len(r.lines)) + 1
		total += r.rows
		x.start[i+1] = total
		win += r.rows
		for release < i && win > transcriptCap && release < keepFrom {
			win -= x.start[release+1] - x.start[release]
			// The render is released, not the row count: the offsets stay,
			// because the trim still has to say how many rows it dropped.
			x.rend[release].lines = nil
			x.rend[release].subs = nil
			x.rend[release].key = blockKey{}
			release++
		}
	}
	x.dirty = int32(n)
	return total
}

// --- the row cap ---------------------------------------------------------

// trimTranscript drops the oldest blocks until the layout fits transcriptCap
// rows.
//
// It runs at the END of sync, on the freshly computed offsets, and only when
// the total is over the cap: the common frame pays one int compare. Everything
// it drops is dropped from a.mu-held state in one pass, so no reader — the dock,
// the sticky header, a hit-test, the scroll model — can observe a half-trimmed
// transcript: they all run under the same lock, after this returns.
//
// The block-level tables move with the blocks (rend/start and the per-block
// stamps inside them), so a dropped block's render is released rather than
// retained at an index nothing reads. The alternative — a growing offset into
// an append-only slice — keeps every dropped render alive, which is the memory
// this cap exists to bound.
func (a *App) trimTranscript(total int32) {
	if total <= transcriptCap {
		return
	}
	x := &a.rowIdx
	n := len(a.blocks)
	// Newest-first: keep the tail window unconditionally, then walk back while
	// the layout still fits. The first block whose head fits is the cut.
	cut := max(0, n-transcriptKeepTail)
	for cut > 0 && total-x.start[cut-1] <= transcriptCap {
		cut--
	}
	// A single block taller than the cap is kept anyway: dropping it would
	// leave the frame with nothing to paint, and the row cap is a budget, not
	// a promise.
	if cut == 0 {
		return
	}
	for _, b := range a.blocks[:cut] {
		if b.Kind == KindUser {
			a.droppedUserRows++
		}
	}
	a.blocks = append([]*Block(nil), a.blocks[cut:]...)
	// allocation: the retained renders are exactly the ones worth keeping, and
	// re-rendering the whole tail every trim would cost the frame this pass is
	// meant to save.
	m := copy(x.rend, x.rend[cut:])
	x.rend = x.rend[:m]
	for i := range x.rend {
		x.rend[i].key.idx = i
		for j := range x.rend[i].subs {
			x.rend[i].subs[j].block = i
		}
	}
	// start[] is rebuilt from the shifted renders rather than sliced: start[i]
	// counts rows of blocks BEFORE i, and the dropped prefix is exactly the
	// offset every retained block loses.
	base := x.start[cut]
	m = copy(x.start, x.start[cut:])
	x.start = x.start[:m]
	for i := range x.start {
		x.start[i] -= base
	}
	if len(x.start) == 0 {
		x.start = append(x.start, 0)
	}
	x.dirty = 0
}

// rowChrome is the decoration a block's rows share: the accent rail, and the
// right-aligned timestamp that lands on the block's first row.
func (a *App) rowChrome(b *Block) (rail string, railS tcell.Style, ts string) {
	railStyle := func(slot string) tcell.Style {
		return tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(slot)))
	}
	switch b.Kind {
	case KindTool:
		rail, railS = "┃", railStyle(theme.AccentTool)
	case KindSystem:
		rail, railS = "┃", railStyle(theme.AccentError)
	case KindAssistant:
		rail, railS = "┃", railStyle(theme.AccentAssistant)
	default:
		// User rows carry their own ❯ band, and a finished result or a
		// reasoning box draws its own rounded frame — a rail beside any of
		// them would read as a doubled line.
	}
	if !b.Ts.IsZero() && (b.Kind == KindUser || b.Kind == KindAssistant) {
		ts = b.Ts.Format("3:04 PM")
	}
	return rail, railS, ts
}

// --- viewport ------------------------------------------------------------

// viewRows materializes transcript rows [start,end) into the index's scratch
// buffer and returns it. The cost is the window, not the session: the block
// holding `start` is found by binary search and only the blocks the window
// spans are touched.
func (a *App) viewRows(start, end int32) []rowView {
	x := &a.rowIdx
	blocks := len(x.rend)
	if blocks == 0 || end <= start {
		return x.view[:0]
	}
	// First block whose last row is at or below `start` — i.e. the block
	// containing row `start`.
	lo := 0
	for hi := blocks - 1; lo < hi; {
		mid := (lo + hi) / 2
		if x.start[mid+1] > start {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	n := int(end - start)
	if cap(x.view) < n {
		x.view = make([]rowView, n)
	} else {
		x.view = x.view[:n]
	}
	y := 0
	for bi := lo; bi < blocks && y < n; bi++ {
		r := &x.rend[bi]
		off := 0
		if bi == lo {
			off = int(start - x.start[bi])
		}
		for k := off; k < int(r.rows) && y < n; k++ {
			if k < len(r.lines) {
				v := rowView{ln: r.lines[k], rail: r.rail, railS: r.railS}
				if k == 0 {
					v.ts = r.ts
				}
				x.view[y] = v
			} else {
				x.view[y] = rowView{} // the blank separator row
			}
			y++
		}
	}
	for ; y < n; y++ {
		x.view[y] = rowView{}
	}
	return x.view
}
