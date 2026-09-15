package tui

import (
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
//   - the bounded middle trim (tier), which collapses aged tool output before
//     anything else, keeping the resident render proportional to the
//     conversation rather than to how much output the session ran through.
//
// A superseded render is replaced in place, never kept alongside: the old
// per-state map cache retained every intermediate render of a streaming block
// (measured +1200 stale entries and 274 MB heap for one long assistant
// message), which is the other half of why long sessions stopped responding.

// trim tiers for one block's render window.
const (
	tierRecent int8 = iota // full render window
	tierAged               // collapsed head+tail: the middle is trimmed
)

// Row budgets for the bounded middle trim. The newest tool results keep the
// full omp frame; past that window a result collapses to a head+tail slice with
// the elided row count between them, so scrolling a long session moves through
// the conversation instead of through command output. Tool output is the first
// casualty because it is the only content that reaches tens of thousands of
// rows; user prompts and assistant prose are never trimmed, and Ctrl+O
// (Block.Expanded) opts a result out of the trim entirely.
const (
	toolRecentResults = 4
	toolRecentHead    = 200
	toolRecentTail    = 50
	toolAgedHead      = 8
	toolAgedTail      = 8

	thinkRecentBlocks = 2
	thinkRecentHead   = 100
	thinkRecentTail   = 40
	thinkAgedHead     = 6
	thinkAgedTail     = 0
)

// blockRend is one block's render: the stamp of the inputs that produced it,
// the visual lines, the decoration every row of the block shares, and how many
// transcript rows it occupies (its lines plus the blank separator row).
type blockRend struct {
	key   blockKey
	lines []line
	rail  string
	railS tcell.Style
	ts    string // right-aligned timestamp, first row only
	rows  int32
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
	tier  []int8      // parallel to App.blocks: trim tier per block
	dirty int32       // first row offset that may be stale (0 = everything)
	view  []rowView   // scratch buffer for the visible window
}

// reset drops every render (resize, theme swap, /clear, transcript rewrite).
func (x *rowIndex) reset() {
	x.rend = x.rend[:0]
	x.start = append(x.start[:0], 0)
	x.tier = x.tier[:0]
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

// --- stamps --------------------------------------------------------------

// renderKey is the stamp of everything a block's render reads. A block whose
// stamp is unchanged never re-renders, which is what makes the per-frame cost
// independent of the session length.
//
// A running tool row carries a live elapsed that no field of the block changes,
// so its stamp ages in whole seconds: one row re-renders per tick and every
// other block keeps its render.
func (a *App) renderKey(i int, b *Block, w int) blockKey {
	var age int64
	if b.Kind == KindTool && b.Status == "running" && !b.Ts.IsZero() {
		age = int64(time.Since(b.Ts).Seconds())
	}
	return blockKey{
		idx: i, kind: b.Kind, width: w, tlen: len(b.Text),
		tool: b.ToolName, status: b.Status, stream: b.stream,
		expanded: b.Expanded, age: age, trim: a.trimTier(i),
		dlen: len(b.Diff),
	}
}

// trimTier reports block i's render-window tier. An unstamped block reads as
// tierRecent, so a caller that renders one block directly (a test, a preview)
// gets the full frame rather than a collapsed one.
func (a *App) trimTier(i int) int8 {
	if i < 0 || i >= len(a.rowIdx.tier) {
		return tierRecent
	}
	return a.rowIdx.tier[i]
}

// toolWindow is a finished result's collapsed render window for a tier.
func toolWindow(tier int8) (head, tail int) {
	if tier == tierAged {
		return toolAgedHead, toolAgedTail
	}
	return toolRecentHead, toolRecentTail
}

// thinkWindow is a thinking block's collapsed render window for a tier.
func thinkWindow(tier int8) (head, tail int) {
	if tier == tierAged {
		return thinkAgedHead, thinkAgedTail
	}
	return thinkRecentHead, thinkRecentTail
}

// stampTiers walks the transcript backwards and assigns each block its trim
// tier: the newest toolRecentResults results and thinkRecentBlocks thinking
// blocks keep their full render window, everything older collapses. Counting
// from the tail (rather than the head) is what keeps the boundary stable while
// a turn streams: appending output flips exactly one aged-out block.
func (a *App) stampTiers() {
	x := &a.rowIdx
	n := len(a.blocks)
	if cap(x.tier) < n {
		x.tier = make([]int8, n)
	} else {
		x.tier = x.tier[:n]
	}
	results, thinks := 0, 0
	for i := n - 1; i >= 0; i-- {
		x.tier[i] = tierRecent
		b := a.blocks[i]
		switch {
		case b.Kind == KindToolDone && !b.Expanded:
			if results >= toolRecentResults {
				x.tier[i] = tierAged
			}
			results++
		case b.Kind == KindThinking:
			if thinks >= thinkRecentBlocks {
				x.tier[i] = tierAged
			}
			thinks++
		}
	}
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
	a.stampTiers() // the tier is part of the stamp, so it must precede the scan
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

	total := x.start[from]
	for i := from; i < int32(n); i++ {
		b := a.blocks[i]
		r := &x.rend[i]
		r.lines = a.blockLines(int(i), b, w)
		r.key = a.renderKey(int(i), b, w)
		r.rail, r.railS, r.ts = a.rowChrome(b)
		r.rows = int32(len(r.lines)) + 1
		total += r.rows
		x.start[i+1] = total
	}
	x.dirty = int32(n)
	return total
}

// rowChrome is the decoration a block's rows share: the accent rail, and the
// right-aligned timestamp that lands on the block's first row.
func (a *App) rowChrome(b *Block) (rail string, railS tcell.Style, ts string) {
	railStyle := func(slot string) tcell.Style {
		return tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(slot)))
	}
	switch b.Kind {
	case KindThinking:
		rail, railS = "┃", railStyle(theme.AccentThinking)
	case KindTool:
		rail, railS = "┃", railStyle(theme.AccentTool)
	case KindSystem:
		rail, railS = "┃", railStyle(theme.AccentError)
	case KindAssistant:
		rail, railS = "┃", railStyle(theme.AccentAssistant)
	default:
		// User rows carry their own ❯ band, and a finished result draws its
		// own rounded frame — a rail beside either would read as a doubled
		// line.
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
