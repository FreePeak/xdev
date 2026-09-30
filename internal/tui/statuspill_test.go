package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// The status row's two dsh pills and the popup a click on one opens
// (deepseek-harness StatsPills.tsx). These are the contract the row now has:
// two pills on the bottom line, everything else one click away, and a panel
// that neither steals a key nor lets the click that dismissed it anchor a
// selection underneath.

// pillCell returns the SCREEN COLUMN of a pill's first glyph. The needle is
// located in the row as a string and then converted to a column, because
// strings.Index counts bytes: the ⏲ ahead of the token pill is three bytes
// wide and a byte offset would click two cells to the right of the pill.
func pillCell(t *testing.T, scr tcell.SimulationScreen, needle string) int {
	t.Helper()
	row := lastRow(screenText(scr))
	i := strings.Index(row, needle)
	if i < 0 {
		t.Fatalf("%q is not on the status row: %q", needle, row)
	}
	return width(row[:i])
}

// seededPillApp is a drawn app whose two pills both have something to say:
// 12 turns of 34 steps, 4m12s of active work, 1.2M tokens at a 99% cache hit,
// 2 tool calls of which one failed, $0.41, and a 66.8k context in a 200k
// window.
func seededPillApp(t *testing.T, w, h int) (*App, tcell.SimulationScreen) {
	t.Helper()
	app, scr := drawnApp(t, w, h)
	app.SetSessionCounts(12, 34)
	app.SetWork(4*time.Minute + 12*time.Second)
	app.AddUsage(479, 1770, 64575, 0, 66824)
	app.AddCacheWrite(1100)
	app.AddCost(0.4123)
	app.AddLLMTime(38*time.Second, 840)
	app.AddToolBlock("1", "bash", "")
	app.FinishTool("1", "bash", false, "ok", ToolOutcome{Elapsed: 4 * time.Second})
	app.AddToolBlock("2", "edit", "")
	app.FinishTool("2", "edit", true, "boom", ToolOutcome{Elapsed: time.Second})
	app.SetContextWindow(200000)
	app.SetContextReplay(66824)
	app.mu.Lock()
	app.st.Rate = 42.5
	app.mu.Unlock()
	return app, scr
}

// clickPill clicks the pill whose on-screen rect contains needle's first
// column. The hit rects are published by the paint that put the glyphs there,
// so the click is tested against real geometry — a test that guessed a column
// would click a neighbour's cell and open the wrong panel.
func clickPill(t *testing.T, app *App, scr tcell.SimulationScreen, needle string) {
	t.Helper()
	app.draw()
	x := pillCell(t, scr, needle)
	y := app.height - 1
	app.mu.Lock()
	name := app.statusHitAt(x+1, y)
	rect, ok := app.statusHitRect(name)
	app.mu.Unlock()
	if name == "" || !ok {
		t.Fatalf("no pill published a rect under %q at column %d", needle, x+1)
	}
	app.mu.Lock()
	press(app, rect.x+1, rect.y)
	release(app, rect.x+1, rect.y)
	app.mu.Unlock()
	app.draw()
}

// TestPillsReadDshsOwnLabels: the row carries the two headlines dsh puts on
// its buttons, and nothing else. The time pill is the work timer, the
// turn/step counts and the decode speed; the token pill is the session total
// and the cache hit rate. The detail is NOT on the row — that is the whole
// point of the pair.
func TestPillsReadDshsOwnLabels(t *testing.T) {
	app, scr := seededPillApp(t, 200, 30)
	app.draw()

	row := lastRow(screenText(scr))
	// The token pill's total counts the cache WRITE as well as the read:
	// 479 fresh + 1,770 out + 64,575 cached + 1,100 written = 67,924.
	for _, want := range []string{"⏲4m12s", "12t·34g", "42.5 t/s", "▤67.9k", "99%"} {
		if !strings.Contains(row, want) {
			t.Fatalf("pill reading %q missing from the row: %q", want, row)
		}
	}
	// The breakdown belongs to the popup, not the row.
	for _, unwanted := range []string{"ctx ", "calls", "$0.", "↑", "↓"} {
		if strings.Contains(row, unwanted) {
			t.Fatalf("the row still carries a detail it should have moved to the popup: %q in %q", unwanted, row)
		}
	}
	// Exactly two pills, so two published hit rects.
	app.mu.Lock()
	hits := len(app.statusHits)
	app.mu.Unlock()
	if hits != 2 {
		t.Fatalf("the row published %d clickable rects, want the two pills", hits)
	}
}

// TestClickOnPillOpensItsBreakdown: the headline is on the row, the numbers
// behind it arrive on a click. Both halves must come from one snapshot, so
// the popup's figures are the same ones /usage reports.
func TestClickOnPillOpensItsBreakdown(t *testing.T) {
	app, scr := seededPillApp(t, 200, 30)

	clickPill(t, app, scr, "⏲4m12s")
	if !app.StatusPopupOpen() {
		t.Fatalf("a click on the time pill opened no panel")
	}
	text := screenText(scr)
	for _, want := range []string{"Session statistics", "turns / steps", "12 / 34", "LLM time", "38s", "tool time", "5s", "avg time to first token", "decode speed", "42.5 t/s", "tool calls", "2 (1 failed)", "active time"} {
		if !strings.Contains(text, want) {
			t.Fatalf("time popup missing %q:\n%s", want, text)
		}
	}

	// The other pill, one click away, replaces it: the two are one control
	clickPill(t, app, scr, "▤67.9k")
	text = screenText(scr)
	if !strings.Contains(text, "Token usage") {
		t.Fatalf("a click on the token pill did not switch panels:\n%s", text)
	}
	for _, want := range []string{"cache hit", "99%", "uncached input", "479", "cached input", "64,575", "cache writes", "1,100", "output", "1,770", "cost", "$0.4123", "context", "66.8k/200k"} {
		if !strings.Contains(text, want) {
			t.Fatalf("token popup missing %q:\n%s", want, text)
		}
	}
	// The pill's total and the report's total are the same number, read from
	// one snapshot: 479 fresh + 1,770 out + 64,575 cached + 1,100 written.
	if !strings.Contains(app.UsageReport(), "67,924 tok") {
		t.Fatalf("the report's total disagrees with the pill's:\n%s", app.UsageReport())
	}
	if !strings.Contains(app.UsageReport(), "67,924") {
		t.Fatalf("the report's total disagrees with the pill's:\n%s", app.UsageReport())
	}
}

// TestPillClickOutsideAndEscapeDismiss: three ways out, because a panel with
// no exit is the defect every transient popup has to avoid — click it again,
// click away, press Esc.
func TestPillClickOutsideAndEscapeDismiss(t *testing.T) {
	app, scr := seededPillApp(t, 200, 30)

	// A second click on the same pill closes it.
	clickPill(t, app, scr, "⏲4m12s")
	if !app.StatusPopupOpen() {
		t.Fatal("setup: the first click must open the panel")
	}
	clickPill(t, app, scr, "⏲4m12s")
	if app.StatusPopupOpen() {
		t.Fatal("a click on the open pill must close it")
	}

	// A click on the transcript, well clear of the panel, dismisses it.
	clickPill(t, app, scr, "⏲4m12s")
	if !app.StatusPopupOpen() {
		t.Fatal("setup: the panel must be open")
	}
	app.handleStatusPopupMouse(tcell.NewEventMouse(2, 5, tcell.Button1, tcell.ModNone), true)
	if app.StatusPopupOpen() {
		t.Fatal("a click outside the panel must dismiss it")
	}

	// Esc closes it too, and never reaches the double-Esc rewind ladder
	// below (a draft that would otherwise be pulled out of the composer).
	clickPill(t, app, scr, "▤67.9k")
	if !app.StatusPopupOpen() {
		t.Fatal("setup: the panel must be open")
	}
	setDraft(&app.ed, "my draft", 0)
	app.handleKey(tcell.NewEventKey(tcell.KeyEsc, 0, tcell.ModNone))
	if app.StatusPopupOpen() {
		t.Fatal("Esc must close the panel")
	}
	if got := string(app.ed.Text()); got != "my draft" {
		t.Fatalf("Esc must not reach the rewind ladder: draft is now %q", got)
	}
}

// TestOnlyPaintedPillsAreClickable: the hit rects are published per frame and
// cleared each one, so a reading that was dropped for width cannot be clicked.
// Otherwise a narrow terminal would open a popup for a pill nobody can see.
func TestOnlyPaintedPillsAreClickable(t *testing.T) {
	app, scr := drawnApp(t, 60, 20)
	app.AddUsage(1200, 340, 0, 0, 1540)
	app.SetStatusSegments([]string{"tokens"}) // the time pill is not on this row
	app.draw()

	app.mu.Lock()
	for _, hit := range app.statusHits {
		if hit.name != pillToken {
			t.Fatalf("a pill that is not on the row published a hit rect: %+v", hit)
		}
	}
	// Nothing lives on the row, so nothing is clickable there.
	if name := app.statusHitAt(2, 19); name != "" {
		t.Fatalf("a cell with no pill on it resolved to %q", name)
	}
	_ = scr
	app.mu.Unlock()

	// A pill that hides (no tokens billed) drops its rect with its text.
	app.Reset()
	app.SetStatusSegments([]string{"tokens"})
	app.draw()
	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.statusHits) != 0 {
		t.Fatalf("a hidden pill still published a hit rect: %+v", app.statusHits)
	}
}

// TestPillPopupHidesWhatItCannotKnow: a zero line is a claim about a
// measurement nobody made. An unwired cost, an unmeasured speed and a session
// that never called a tool say nothing about any of them in the panel either.
func TestPillPopupHidesWhatItCannotKnow(t *testing.T) {
	app, scr := drawnApp(t, 120, 24)
	app.AddUsage(100, 0, 0, 0, 100)
	app.draw()

	clickPill(t, app, scr, "▤100")
	text := screenText(scr)
	// The buckets themselves are always listed — they are the accounting the
	// total is a sum of, and a zero there is a real zero (nothing was cached),
	// not a missing measurement. What must not appear is a figure nobody can
	// supply: a hit rate with no cached side, an unwired cost, a call count
	// for a session that called nothing, a context meter with no window.
	for _, unwanted := range []string{"cache hit", "cost", "tool calls", "cache writes", "context"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("an unmeasured figure drew in the popup: %q\n%s", unwanted, text)
		}
	}
	// The buckets the pill's own total is made of are there, exact.
	for _, want := range []string{"uncached input 100", "cached input   0", "output         0"} {
		if !strings.Contains(text, want) {
			t.Fatalf("token bucket %q missing from the panel:\n%s", want, text)
		}
	}
	// The TIME panel is the one that always has a reading: a fresh session
	// has genuinely worked for zero seconds, which is a fact and not a gap.
	// The token panel has no such row — the timer lives on the time pill.
	clickPill(t, app, scr, "⏲0s")
	text = screenText(scr)
	if !strings.Contains(text, "active time") || !strings.Contains(text, "0s") {
		t.Fatalf("active time must always report in the time panel:\n%s", text)
	}
}

// TestPillCountsAreClearedByReset: the counts belong to one session, exactly
// like the token counters. A /resume or /fork that left them behind would
// show the previous session's turns beside the new transcript — and the pill
// is the one place that always renders them.
func TestPillCountsAreClearedByReset(t *testing.T) {
	app, scr := drawnApp(t, 160, 24)
	app.SetSessionCounts(7, 19)
	app.AddCacheWrite(900)
	app.draw()
	if row := lastRow(screenText(scr)); !strings.Contains(row, "7t·19g") {
		t.Fatalf("the turn/step counts are not on the pill: %q", row)
	}

	app.Reset()
	app.draw()
	row := lastRow(screenText(scr))
	if strings.Contains(row, "7t·19g") {
		t.Fatalf("the previous session's counts survived Reset: %q", row)
	}

	// /usage agrees: the report must not print the old session's cache
	// writes either.
	if strings.Contains(app.UsageReport(), "900") {
		t.Fatalf("cache writes survived Reset:\n%s", app.UsageReport())
	}
}
