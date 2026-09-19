package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// The context dock (issue #291 §1 + §2): a fixed-width column right of the
// transcript. Four things are load-bearing and tested here — the display policy
// and what it costs the rest of the layout, the fold cycle's promise that the
// pending plan is never folded away, the row budget that reports what it cuts
// instead of swallowing it, and the rule that a frame with nothing new does not
// rebuild. The last one is the reason #283/#284 cannot come back.

// dockTestApp is a drawn app with sources that report whether they ran, so a
// test can tell "rebuilt" from "repainted from the cache".
func dockTestApp(t *testing.T, w, h int) (*App, tcell.SimulationScreen, *int) {
	t.Helper()
	app, scr := newTestApp(t, w, h)
	app.AddSystemBlock("ready")
	runs := new(int)
	app.SetDockOps(DockOps{
		Plan: func() (string, bool) { return "1. do the thing\n2. verify it", true },
		Tasks: func() string {
			*runs++
			return "TASKS · 1/2 done\n[x] wire the dock\n[ ] write tests"
		},
		Agents: func() string { *runs++; return "AGENTS · 1 running / 1\nrunning reviewer · reading dock.go" },
		// The identity the panel's title slot reads. Not counted by runs: the
		// session's name is not one of the lists whose rebuild this file bounds.
		Session: func() (string, string) { return "opencode sidebar", "sess1234" },
	})
	return app, scr, runs
}

// dockRows builds section rows out of their text, so a fold literal here reads
// like the source block it stands in for.
func dockRows(lines ...string) []dockRow {
	out := make([]dockRow, len(lines))
	for i, l := range lines {
		out[i] = dockRow{text: l}
	}
	return out
}

// dockLines flattens laid-out rows back to text, one per line — what the
// assertions below read.
func dockLines(rows []dockRow) string {
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(r.text)
		b.WriteByte('\n')
	}
	return b.String()
}

// TestDockAutoFollowsWidth pins the width rule: auto is closed on a terminal
// narrower than the floor and open at or above it, and show/hide override in
// either direction. The reserve is what every other surface lays out against,
// so it must track the rule exactly.
func TestDockAutoFollowsWidth(t *testing.T) {
	app, _, _ := dockTestApp(t, 100, 40)
	app.mu.Lock()
	if app.dockOn() {
		app.mu.Unlock()
		t.Fatal("auto must close below the floor")
	}
	if got := app.dockReserve(); got != 0 {
		app.mu.Unlock()
		t.Fatalf("closed dock reserves %d columns", got)
	}
	if got := app.rightEdge(); got != 100 {
		app.mu.Unlock()
		t.Fatalf("closed dock must leave the full width, got rightEdge %d", got)
	}
	app.mu.Unlock()

	app.SetDockMode(DockShow)
	app.mu.Lock()
	if !app.dockOn() {
		app.mu.Unlock()
		t.Fatal("show pins the dock open regardless of width")
	}
	if got := app.dockReserve(); got != dockCols {
		app.mu.Unlock()
		t.Fatalf("open dock reserves %d, want %d", got, dockCols)
	}
	if got := app.rightEdge(); got != 100-dockCols {
		app.mu.Unlock()
		t.Fatalf("rightEdge must stop at the panel: %d, want %d", got, 100-dockCols)
	}
	app.mu.Unlock()

	app.SetDockMode(DockHide)
	app.mu.Lock()
	if app.dockOn() {
		app.mu.Unlock()
		t.Fatal("hide wins over the width rule at any size")
	}
	app.mu.Unlock()

	// A corrupt layer is never a surprise column.
	app.SetDockMode("yes-please")
	if got := app.DockMode(); got != DockAuto {
		t.Fatalf("unknown policy %q must normalize to auto", got)
	}
}

// TestDockCycleWalksPolicy is Alt+s: shown, hidden, auto, shown — so a human can
// always get out of whatever they pinned into — and the policy it lands on is
// handed to the persistence seam exactly once per press.
func TestDockCycleWalksPolicy(t *testing.T) {
	app, _, _ := dockTestApp(t, 200, 40)
	var saved []string
	app.SetDockModeFunc(func(mode string) { saved = append(saved, mode) })

	want := []string{DockShow, DockHide, DockAuto, DockShow}
	for i, expect := range want {
		app.dockCycle()
		if got := app.DockMode(); got != expect {
			t.Fatalf("press %d: mode %q, want %q", i+1, got, expect)
		}
	}
	if strings.Join(saved, ",") != strings.Join(want, ",") {
		t.Fatalf("persisted %v, want %v", saved, want)
	}
}

// TestDockFoldKeepsThePlan is Ctrl+T's one promise: the pending document survives
// every fold state, because folding away the thing the human is being asked to
// read is worse than no panel at all.
func TestDockFoldKeepsThePlan(t *testing.T) {
	app, _, _ := dockTestApp(t, 200, 40)
	app.mu.Lock()
	defer app.mu.Unlock()
	for state := int8(0); state <= 2; state++ {
		app.dock.fold = state
		app.dock.lines = nil
		folds := []dockFold{
			{id: dockPlanID, title: "PLAN · proposed (2 lines)", rows: dockRows("1. do the thing", "2. verify it"), max: dockPlanMax},
			{id: dockTaskID, title: "TASKS · 1/2 done", rows: dockRows("[x] wire the dock", "[ ] write tests"), max: dockListMax},
		}
		rows, heads, hidden := app.dock.layout(folds, 40)
		joined := dockLines(rows)
		if !strings.Contains(joined, "PLAN") || !strings.Contains(joined, "do the thing") {
			t.Fatalf("fold %d dropped the pending plan:\n%s", state, joined)
		}
		if state == foldShut {
			if strings.Contains(joined, "wire the dock") {
				t.Fatalf("fold %d must shut the lists:\n%s", state, joined)
			}
			if heads != len(folds) || hidden != 2 {
				t.Fatalf("fold %d: heads %d hidden %d", state, heads, hidden)
			}
		}
		if state == foldOpen && !strings.Contains(joined, "write tests") {
			t.Fatalf("fold %d must open the lists:\n%s", state, joined)
		}
	}
}

// TestDockBudgetReportsTheCut is the panel's honesty rule: a plan longer than the
// band is truncated with a count, never silently, and the rows it returns always
// fit the band it was budgeted for.
func TestDockBudgetReportsTheCut(t *testing.T) {
	d := &dockState{mode: DockShow}
	long := dockFold{id: dockPlanID, title: "PLAN · proposed (40 lines)", max: dockPlanMax}
	for i := 0; i < 40; i++ {
		long.rows = append(long.rows, dockRow{text: "step"})
	}
	list := dockFold{id: dockFileID, title: "FILES · 9", max: dockListMax}
	for i := 0; i < 9; i++ {
		list.rows = append(list.rows, dockRow{text: "a.go"})
	}
	for bandH := 5; bandH <= 40; bandH++ {
		rows, _, hidden := d.layout([]dockFold{long, list}, bandH)
		if len(rows) > bandH-1 {
			t.Fatalf("bandH %d: %d rows for a %d-row band", bandH, len(rows), bandH)
		}
		if hidden == 0 {
			t.Fatalf("bandH %d: 49 rows of content fit with nothing cut?", bandH)
		}
		if !strings.Contains(dockLines(rows), "more") {
			t.Fatalf("bandH %d: a cut section must say so:\n%s", bandH, dockLines(rows))
		}
	}
	// Too short for even a heading is not an error, it is no panel.
	if rows, _, _ := d.layout([]dockFold{long}, 4); rows != nil {
		t.Fatalf("a 4-row band must yield nothing, got %d rows", len(rows))
	}
}

// TestDockClipsDoeNotWrap is the panel's width rule: every row is a row, so a
// long path or a wide heading is cut with an ellipsis instead of pushing the
// sections below it out of the band.
func TestDockClipsDoNotWrap(t *testing.T) {
	d := &dockState{mode: DockShow}
	rows, _, _ := d.layout([]dockFold{{id: dockFileID, title: strings.Repeat("x", 200),
		rows: dockRows(strings.Repeat("y", 200))},
	}, 40)
	for _, r := range rows {
		if w := width(r.text); w > dockInner {
			t.Fatalf("row %q is %d cells, the interior is %d", r.text, w, dockInner)
		}
	}
}

// TestDockRebuildIsEventDriven is the acceptance line: an unchanged frame must not
// run a single source, while a bump, a resize, or a transcript that grew must.
// The alternative is a second column rebuilt at 30fps, which is the cost #283 and
// #284 spent two issues removing from the draw path.
func TestDockRebuildIsEventDriven(t *testing.T) {
	app, _, runs := dockTestApp(t, 200, 40)
	app.dockBuild()
	first := *runs
	if first == 0 {
		t.Fatal("the first frame must run the sources")
	}
	app.dockBuild()
	app.dockBuild()
	if *runs != first {
		t.Fatalf("two unchanged frames ran the sources %d more times", *runs-first)
	}

	// A source that moved: the next frame rebuilds, once.
	app.DockBump()
	app.dockBuild()
	if *runs != first+2 {
		t.Fatalf("after a bump: %d runs, want %d", *runs, first+2)
	}
	app.dockBuild()
	if *runs != first+2 {
		t.Fatalf("the bump must cost exactly one rebuild, got %d runs", *runs)
	}

	// The Files section reads the transcript, so a new block is a change even
	// with no bump.
	app.AddSystemBlock("another one")
	app.dockBuild()
	if *runs != first+4 {
		t.Fatalf("a longer transcript must rebuild, runs = %d", *runs)
	}

	// So is a narrower band: the row budget depends on it.
	app.mu.Lock()
	app.height = 12
	app.mu.Unlock()
	app.dockBuild()
	if *runs != first+6 {
		t.Fatalf("a shorter band must rebuild, runs = %d", *runs)
	}
}

// TestDockClosesSourcesWhenHidden: a shut panel must not read anything — the
// point of a display policy is that the cost follows the visibility.
func TestDockClosesSourcesWhenHidden(t *testing.T) {
	app, _, runs := dockTestApp(t, 200, 40)
	app.dockBuild()
	before := *runs
	app.SetDockMode(DockHide)
	app.dockBuild()
	if *runs != before {
		t.Fatalf("a hidden dock ran its sources %d more times", *runs-before)
	}
	app.mu.Lock()
	lines := app.dock.lines
	app.mu.Unlock()
	if lines != nil {
		t.Fatalf("a hidden dock must hold no rows, got %d", len(lines))
	}
}

// TestDockPaintsThePanel is the whole-surface check at a size where the panel is
// open: the session's own name is in the top slot, the sections are under it, and
// the panel's columns belong to the panel — no transcript text bleeds in, and no
// box is drawn around any of it.
func TestDockPaintsThePanel(t *testing.T) {
	app, scr, _ := dockTestApp(t, 160, 40)
	app.SetDockMode(DockShow)
	// A sentinel the panel's own text never contains, so a row that bled into the
	// dock's columns is unmistakable.
	app.AddSystemBlock(strings.Repeat("ZZtranscript ", 80))
	app.draw()

	text := screenText(scr)
	if !strings.Contains(text, "opencode sidebar") {
		t.Fatalf("the session's name is missing from the title slot:\n%s", text)
	}
	if strings.Contains(text, "CONTEXT") {
		t.Fatalf("the old panel heading is still painted:\n%s", text)
	}
	if !strings.Contains(text, "wire the dock") {
		t.Fatalf("task rows missing:\n%s", text)
	}
	if !strings.Contains(text, "reviewer") {
		t.Fatalf("roster rows missing:\n%s", text)
	}
	// The panel's columns are its own: nothing to its left reaches them, and the
	// surface is the separator, so nothing draws a box on them either.
	app.mu.Lock()
	top, h := app.dockGrid()
	app.mu.Unlock()
	for y := top; y < top+h; y++ {
		for x := 160 - dockCols; x < 160; x++ {
			ch, _, _, _ := scr.GetContent(x, y)
			if ch == 'Z' {
				t.Fatalf("transcript text painted inside the panel at x=%d y=%d", x, y)
			}
			if ch == '│' || ch == '─' {
				t.Fatalf("the panel drew a box (%q) at x=%d y=%d", ch, x, y)
			}
		}
	}
}

// TestDockAnatomyIsOpencode: a section is a bold name with its count appended
// dim, and a changed file carries its counts right-aligned at the panel's edge —
// the two things a reader scans the panel for. The title slot is the session's
// own name, so the panel says which session it belongs to rather than how many
// sections it holds.
func TestDockAnatomyIsOpencode(t *testing.T) {
	app, scr, _ := dockTestApp(t, 160, 40)
	app.SetDockMode(DockShow)
	app.AddToolBlock("edit", `{"path":"internal/tui/dock.go"}`)
	app.FinishTool("edit", false, "edited",
		ToolOutcome{Diff: "--- a/x\n+++ b/internal/tui/dock.go\n@@ -1,2 +1,3 @@\n ctx\n+added\n-removed\n"})
	app.draw()

	app.mu.Lock()
	top, _ := app.dockGrid()
	rows := app.dock.lines
	app.mu.Unlock()

	left := 160 - dockCols
	if got := dockRowText(scr, top, left); !strings.Contains(got, "opencode sidebar") {
		t.Fatalf("title slot row = %q", got)
	}
	// Every heading splits into a name and the count its source appended; a
	// heading that did not would paint its count as part of its name.
	var heads int
	var file dockRow
	for _, r := range rows {
		if r.head {
			if name, count := dockSplit(r.text); name == "" || !strings.HasPrefix(count, "· ") {
				t.Fatalf("heading %q splits into name %q and count %q", r.text, name, count)
			}
			heads++
		}
		if r.right() != "" {
			file = r
		}
	}
	if heads == 0 {
		t.Fatal("no section headings were built")
	}
	if file.text != "internal/tui/dock.go" || file.add != "+1" || file.del != "-1" {
		t.Fatalf("file row %+v", file)
	}
	// The name starts at the panel's gutter, and the counts end at its right
	// edge — the two ends the space-between row holds open.
	y := top + 1
	for i, r := range rows {
		if r.right() != "" {
			y = top + 1 + i
			break
		}
	}
	if got := dockRowText(scr, y, left); !strings.HasPrefix(got, strings.Repeat(" ", dockPad)+file.text) {
		t.Fatalf("file row starts at %q", got)
	}
	x := left + dockPad + dockInner - width(file.right())
	if got := dockRowText(scr, y, x); !strings.HasPrefix(got, "+1 -1") {
		t.Fatalf("counts are not right-aligned at the panel's edge: %q", got)
	}
}

// dockRowText reads one panel row back off the screen from x to the screen's
// edge, with the trailing blanks trimmed.
func dockRowText(scr tcell.SimulationScreen, y, x int) string {
	_, w, _ := scr.GetContents()
	var b strings.Builder
	for i := x; i < w; i++ {
		ch, _, _, _ := scr.GetContent(i, y)
		if ch == 0 {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(ch)
	}
	return strings.TrimRight(b.String(), " ")
}

// TestDockNarrowTerminalKeepsItsPrompt: at an 80-column terminal the panel must
// cost the session nothing, not even a squeezed composer.
func TestDockNarrowTerminalKeepsItsPrompt(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.AddSystemBlock("hello")
	app.dockBuild()
	app.mu.Lock()
	if app.dockOn() {
		app.mu.Unlock()
		t.Fatal("auto must keep an 80-column terminal full width")
	}
	if app.rightEdge() != 80 {
		app.mu.Unlock()
		t.Fatalf("rightEdge = %d, want the whole terminal", app.rightEdge())
	}
	if app.contentWidth() <= 0 {
		app.mu.Unlock()
		t.Fatal("content width collapsed")
	}
	app.mu.Unlock()
	if app.DockState() == "" {
		t.Fatal("DockState must explain itself")
	}
}

// TestDockPinnedCannotEatThePrompt: "show" is a human asking for the panel, not
// asking for a 20-column session. Below the width where the transcript could keep
// its floor beside it, the panel keeps its columns to itself and the terminal
// stays whole — and above it the pin is honored, because it means what it says.
func TestDockPinnedCannotEatThePrompt(t *testing.T) {
	for _, w := range []int{50, 55, 61} {
		app, _ := newTestApp(t, w, 24)
		app.AddSystemBlock("hello")
		app.SetDockMode(DockShow)
		app.mu.Lock()
		on := app.dockOn()
		edge := app.rightEdge()
		app.mu.Unlock()
		if on {
			t.Fatalf("w=%d: the panel opened and left %d columns for the session", w, w-dockCols)
		}
		if edge != w {
			t.Fatalf("w=%d: rightEdge %d, want the whole terminal", w, edge)
		}
	}
	app, _ := newTestApp(t, 80, 24)
	app.AddSystemBlock("hello")
	app.SetDockMode(DockShow)
	app.mu.Lock()
	defer app.mu.Unlock()
	if !app.dockOn() {
		t.Fatal("show at 80 columns must open the panel")
	}
	if got := app.rightEdge(); got != 80-dockCols {
		t.Fatalf("rightEdge = %d, want %d", got, 80-dockCols)
	}
}

// TestDockShortTerminalKeepsItsRows: the panel trades rows for its box only when
// there are rows to trade. At a height where the band cannot hold anything, the
// columns stay the transcript's — the reserve is not a fee charged for nothing.
func TestDockShortTerminalKeepsItsRows(t *testing.T) {
	app, _ := newTestApp(t, 200, 8)
	app.AddSystemBlock("hello")
	app.SetDockMode(DockShow)
	app.mu.Lock()
	defer app.mu.Unlock()
	if _, h := app.dockGrid(); h > 0 {
		t.Fatalf("an 8-row terminal has a %d-row band?", h)
	}
	if app.dockOn() {
		t.Fatal("the panel opened with no band to paint into")
	}
	if got := app.rightEdge(); got != 200 {
		t.Fatalf("rightEdge = %d, want the whole terminal", got)
	}
}

// TestDockFilesReadsTheTranscriptDiffs: the Changes section is derived from the
// diff blocks the tool results already carry, so one source feeds both surfaces.
func TestDockFilesReadsTheTranscriptDiffs(t *testing.T) {
	app, _ := newTestApp(t, 200, 40)
	app.AddSystemBlock("go")
	diff := "--- a/internal/tui/app.go\n+++ b/internal/tui/app.go\n@@ -1,2 +1,3 @@\n context\n+added\n-removed\n"
	app.AddToolBlock("edit", `{"path":"internal/tui/app.go"}`)
	app.FinishTool("edit", false, "edited", ToolOutcome{Diff: diff})
	app.AddToolBlock("edit", `{"path":"internal/tui/dock.go"}`)
	app.FinishTool("edit", false, "edited", ToolOutcome{Diff: "--- a/x\n+++ b/internal/tui/dock.go\n@@ -1 +1 @@\n-old\n+new line\n"})
	app.mu.Lock()
	defer app.mu.Unlock()
	f, ok := app.dockChanges()
	if !ok {
		t.Fatal("two diffs must produce a Changes section")
	}
	if f.title != "FILES · 2" {
		t.Fatalf("title %q", f.title)
	}
	// The name keeps its tail — a cut takes the directory, never the file — and
	// the counts are fields of their own, right-aligned when they paint.
	want := []dockRow{
		{text: "internal/tui/app.go", add: "+1", del: "-1", path: "internal/tui/app.go"},
		{text: "internal/tui/dock.go", add: "+1", del: "-1", path: "internal/tui/dock.go"},
	}
	if !slices.Equal(f.rows, want) {
		t.Fatalf("rows %+v, want %+v", f.rows, want)
	}
}

// TestDockKeyChordsAreTakenFromTheKeyMap: the panel's whole keyboard is two
// chords, and they must resolve and show up in /hotkeys like any other action.
func TestDockKeyChordsAreTakenFromTheKeyMap(t *testing.T) {
	km := DefaultKeyMap()
	for chord, want := range map[string]string{"A-s": "dock-cycle", "C-t": "dock-fold"} {
		if got := km.bindings[chord]; got != want {
			t.Fatalf("%s resolves to %q, want %q", chord, got, want)
		}
	}
	for _, action := range []string{"dock-cycle", "dock-fold"} {
		if !slices.Contains(BuiltinActions, action) {
			t.Fatalf("%s missing from BuiltinActions, so a user cannot rebind it", action)
		}
		h := km.Hotkeys()
		if !strings.Contains(h, action) {
			t.Fatalf("/hotkeys must list %s:\n%s", action, h)
		}
	}
	// The chord the issue asked for stays the pager's: Ctrl+B is scroll-page-up,
	// and the panel must not have quietly taken it.
	if got := km.bindings["C-b"]; got != "scroll-page-up" {
		t.Fatalf("C-b = %q, the pager owns it", got)
	}
	if got := km.Chord("dock-cycle"); got != "A-s" {
		t.Fatalf("dock-cycle advertises %q", got)
	}
}

func modOf(chord string) tcell.ModMask {
	switch chord[:1] {
	case "A":
		return tcell.ModAlt
	case "C":
		return tcell.ModCtrl
	case "S":
		return tcell.ModShift
	}
	return tcell.ModNone
}

// TestPlanShowCommand is §2's reading surface: /plan show prints the document and
// the phase list, says so when there is nothing, and never resolves anything.
func TestPlanShowCommand(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	app.AddSystemBlock("x")
	var shown bool
	app.SetPlanOps(&PlanOps{
		Get: func() bool { return true },
		Set: func(bool) error { return nil },
		Show: func() string {
			shown = true
			return "PLAN — proposed\n\n1. do the thing\n\nTASKS · 0/1 done\n[ ] do the thing"
		},
	})
	if err := app.PlanMode("show"); err != nil {
		t.Fatal(err)
	}
	if !shown {
		t.Fatal("/plan show must read the pending plan")
	}
	app.mu.Lock()
	last := app.blocks[len(app.blocks)-1].Text
	app.mu.Unlock()
	if !strings.Contains(last, "do the thing") {
		t.Fatalf("the document must be printed:\n%q", last)
	}
	// An empty view is a notice, not an empty block.
	app.SetPlanOps(&PlanOps{Get: func() bool { return false }, Set: func(bool) error { return nil }, Show: func() string { return "" }})
	if err := app.PlanMode("show"); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	last = app.blocks[len(app.blocks)-1].Text
	app.mu.Unlock()
	if !strings.Contains(last, "no pending plan") {
		t.Fatalf("empty show must say so: %q", last)
	}
	// The old argument set still parses; the new one is added, not swapped.
	if err := app.PlanMode("sideways"); err == nil {
		t.Fatal("an unknown /plan verb must still be refused")
	}
}

// TestDockSettingsCommand: /settings sidebarMode with no argument reports the
// current policy, a valid one applies and persists it, a bad one is refused, and
// the key is a repo-safe display setting like showThinking.
func TestDockSettingsCommand(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	app.AddSystemBlock("x")
	var saved []string
	app.SetSettingsOps(&SettingsOps{Path: "/tmp/settings.yml", SetSidebar: func(m string) error {
		saved = append(saved, m)
		return nil
	}})
	if err := app.SettingsView("sidebarMode"); err != nil {
		t.Fatal(err)
	}
	if got := app.DockMode(); got != DockAuto {
		t.Fatalf("no argument must leave the policy, got %q", got)
	}
	if err := app.SettingsView("sidebarMode show"); err != nil {
		t.Fatal(err)
	}
	if app.DockMode() != DockShow || len(saved) != 1 || saved[0] != DockShow {
		t.Fatalf("mode %q saved %v", app.DockMode(), saved)
	}
	if err := app.SettingsView("sidebarMode sideways"); err == nil {
		t.Fatal("an unknown policy must be refused")
	}
	if err := app.SettingsView("nonsense"); err == nil {
		t.Fatal("an unknown setting must be refused")
	}
}
