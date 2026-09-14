package tui

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/theme"
	"github.com/gdamore/tcell/v2"
)

func idxApp(w, h int) *App {
	scr := tcell.NewSimulationScreen("UTF-8")
	_ = scr.Init()
	scr.SetSize(w, h)
	app := New(scr, theme.Load("groknight"), "test/free", "sess")
	app.SetHandlers(func(string) {}, func() {}, func() {})
	app.width, app.height = w, h
	return app
}

// idxFill appends turns of the shapes the renderer must agree about: a banded
// user prompt, wrapped prose, a tool call, and a long tool result.
func idxFill(app *App, turns int, outLines int) {
	body := strings.Repeat("output line of command result 1234567890\n", outLines)
	for range turns {
		app.AddUserBlock("summarize the failing tests " + strings.Repeat("word ", 30))
		app.AddAssistantBlock("Here is what I found. " + strings.Repeat("explanation sentence. ", 10))
		app.AddSystemBlock("error: something went wrong")
		app.AddToolBlock("bash", `{"command":"go test ./..."}`)
		app.FinishTool("bash", false, body, ToolOutcome{Dur: "70ms"})
	}
}

// naiveRows rebuilds the whole transcript the way draw() used to: walk every
// block, render it, copy its rows, then take the window. The row index must
// agree with it row for row, at every scroll offset.
func naiveRows(app *App, w int, start, end int32) []rowView {
	var all []rowView
	for i := range app.blocks {
		b := app.blocks[i]
		rail, railS, ts := app.rowChrome(b)
		for j, ln := range app.blockLines(i, b, w) {
			v := rowView{ln: ln, rail: rail, railS: railS}
			if j == 0 {
				v.ts = ts
			}
			all = append(all, v)
		}
		all = append(all, rowView{}) // separator
	}
	if start > int32(len(all)) {
		start = int32(len(all))
	}
	if end > int32(len(all)) {
		end = int32(len(all))
	}
	return all[start:end]
}

func runsText(runs []cell) string {
	var b strings.Builder
	for _, r := range runs {
		b.WriteString(r.text)
	}
	return b.String()
}

func rowText(v rowView) string { return runsText(v.ln.runs) }

func joinedLines(lines []line) string {
	var b strings.Builder
	for _, ln := range lines {
		b.WriteString(runsText(ln.runs))
		b.WriteByte('\n')
	}
	return b.String()
}

// The row index is an optimization, so it is only correct if it paints exactly
// what the old full rebuild painted — including the blank separator rows, the
// rail, and the timestamp that belongs to a block's first row only.
func TestRowIndexAgreesWithFullRebuildAtEveryOffset(t *testing.T) {
	app := idxApp(120, 40)
	idxFill(app, 12, 300)
	w := app.contentWidth()
	total := app.sync(w)
	if want := int32(len(naiveRows(app, w, 0, 1<<30))); total != want {
		t.Fatalf("sync total = %d, naive rebuild = %d", total, want)
	}
	// Every offset, so the block-boundary and separator arithmetic is pinned
	// everywhere, not just at the tail.
	for start := int32(0); start+8 <= total; start += 7 {
		got := app.viewRows(start, start+8)
		want := naiveRows(app, w, start, start+8)
		if len(got) != len(want) {
			t.Fatalf("rows at %d: got %d, want %d", start, len(got), len(want))
		}
		for i := range got {
			if rowText(got[i]) != rowText(want[i]) || got[i].rail != want[i].rail || got[i].ts != want[i].ts {
				t.Fatalf("row %d (offset %d): got %q rail=%q ts=%q, want %q rail=%q ts=%q",
					i, start, rowText(got[i]), got[i].rail, got[i].ts, rowText(want[i]), want[i].rail, want[i].ts)
			}
		}
	}
}

// Streaming and Ctrl+O must invalidate exactly the blocks that changed and
// leave the rest of the layout intact — the whole point of the stamp.
func TestSettledBlocksSurviveTailMutations(t *testing.T) {
	app := idxApp(120, 40)
	idxFill(app, 6, 300)
	w := app.contentWidth()
	app.sync(w)
	before := append([]blockRend(nil), app.rowIdx.rend[:20]...)

	app.BeginAssistant()
	app.AppendAssistant("continuing the answer")
	if total := app.sync(w); total <= before[19].rows {
		t.Fatalf("sync total %d did not grow", total)
	}
	for i := range before {
		if app.rowIdx.rend[i].key != before[i].key || len(app.rowIdx.rend[i].lines) != len(before[i].lines) {
			t.Fatalf("settled block %d re-rendered by a tail append", i)
		}
	}

	// Expanding a settled result changes its own row set and nothing before it.
	app.blocks[4].Expanded = true
	app.sync(w)
	if app.rowIdx.rend[4].key == before[4].key {
		t.Fatal("expanding a result left its stamp unchanged (it would still render collapsed)")
	}
	for i := range before {
		if i == 4 {
			continue
		}
		if app.rowIdx.rend[i].key != before[i].key {
			t.Fatalf("block %d re-rendered by an expand on block 4", i)
		}
	}
}

// The bounded middle trim: past the recent-results window, tool output
// collapses first, and the conversation is never trimmed.
func TestAgedToolOutputCollapsesFirst(t *testing.T) {
	app := idxApp(120, 40)
	idxFill(app, toolRecentResults+3, 400)
	w := app.contentWidth()
	app.sync(w)

	var results []*Block
	for _, b := range app.blocks {
		if b.Kind == KindToolDone {
			results = append(results, b)
		}
	}
	if len(results) < toolRecentResults+2 {
		t.Fatalf("need more results than the recent window, got %d", len(results))
	}
	recent := len(app.blockLines(idxOf(app, results[len(results)-1]), results[len(results)-1], w))
	agedIdx := idxOf(app, results[0])
	aged := len(app.blockLines(agedIdx, results[0], w))
	if aged >= recent {
		t.Fatalf("aged result rendered %d rows, recent %d: nothing was trimmed", aged, recent)
	}
	if !strings.Contains(joinedLines(app.blockLines(agedIdx, results[0], w)), "lines hidden") {
		t.Error("collapsed result lost its hidden-line notice")
	}
	// The conversation spine is untouched: a user prompt keeps every row.
	for _, b := range app.blocks {
		if b.Kind == KindUser {
			// Nothing about a prompt is elided: it renders as many rows as its
			// own text wraps to, however aged the session is.
			want := len(wrap(strings.TrimRight(b.Text, "\n"), max(10, w-2)))
			if got := len(app.blockLines(idxOf(app, b), b, w)); got != want {
				t.Fatalf("user prompt rendered %d rows, its text wraps to %d", got, want)
			}
			break
		}
	}
}

// Ctrl+O opts a result out of the trim entirely.
func TestExpandedResultEscapesTrim(t *testing.T) {
	app := idxApp(120, 40)
	idxFill(app, toolRecentResults+3, 400)
	w := app.contentWidth()
	app.sync(w)
	first := app.blocks[4] // the oldest result, well past the window
	if got := len(app.blockLines(4, first, w)); got > 20 {
		t.Fatalf("oldest result rendered %d rows; expected the collapsed window", got)
	}
	first.Expanded = true
	if got := len(app.blockLines(4, first, w)); got < 400 {
		t.Fatalf("expanded result rendered %d rows; expected every output row", got)
	}
}

// The performance contract a long session broke: the per-frame work must track
// the viewport, not the transcript. The assertion compares two sessions in the
// same run, so it holds regardless of how fast the machine under it is.
func TestFrameCostFollowsViewportNotSession(t *testing.T) {
	small := idxApp(120, 40)
	idxFill(small, 20, 300)
	small.draw()
	big := idxApp(120, 40)
	idxFill(big, 400, 300)
	big.draw()
	if big.totalLinesLocked() < 8*small.totalLinesLocked() {
		t.Fatalf("fixture is not much longer: %d vs %d rows", big.totalLinesLocked(), small.totalLinesLocked())
	}
	// The frame is allowed to cost more for a longer session only through the
	// per-block stamp scan (linear in blocks), never through the row count:
	// quadrupling the transcript must not quadruple the frame. Measured on the
	// pre-fix full-rebuild frame the ratio tracked the rows (≈4×); with the row
	// index it stays near 1 because only the viewport is materialized.
	const rounds = 20
	smallFrame := timeFrame(small, rounds)
	bigFrame := timeFrame(big, rounds)
	t.Logf("frame cost: %d rows = %v, %d rows = %v",
		small.totalLinesLocked(), smallFrame, big.totalLinesLocked(), bigFrame)
	if bigFrame > 3*smallFrame {
		t.Fatalf("frame cost scales with the transcript: %v at %d rows vs %v at %d rows",
			bigFrame, big.totalLinesLocked(), smallFrame, small.totalLinesLocked())
	}
}

// timeFrame is the median draw duration, so a scheduling hiccup cannot decide
// the assertion.
func timeFrame(app *App, rounds int) time.Duration {
	var seen []time.Duration
	for range rounds {
		t0 := time.Now()
		app.draw()
		seen = append(seen, time.Since(t0))
	}
	sort.Slice(seen, func(i, j int) bool { return seen[i] < seen[j] })
	return seen[len(seen)/2]
}

// A long streaming message must not cost more per delta than a short one: the
// transcript frame and the inline markdown scan are linear, and a superseded
// render is replaced rather than accumulated.
func TestStreamingDeltaCostStaysFlat(t *testing.T) {
	app := idxApp(120, 40)
	idxFill(app, 200, 300)
	app.draw()
	app.BeginAssistant()
	var short, long time.Duration
	for i := range 4000 {
		app.AppendAssistant("word ")
		if i == 399 {
			t0 := time.Now()
			for range 50 {
				app.AppendAssistant("word ")
				app.draw()
			}
			short = time.Since(t0)
		}
		if i == 3999 {
			t0 := time.Now()
			for range 50 {
				app.AppendAssistant("word ")
				app.draw()
			}
			long = time.Since(t0)
		}
	}
	if renders := len(app.rowIdx.rend); renders != len(app.blocks) {
		t.Fatalf("renders accumulated with the stream: %d entries for %d blocks", renders, len(app.blocks))
	}
	if long > 4*short+100*time.Millisecond {
		t.Fatalf("delta cost grew with message length: %v late vs %v early", long, short)
	}
}

func idxOf(app *App, b *Block) int {
	for i, x := range app.blocks {
		if x == b {
			return i
		}
	}
	return -1
}
