package tui

import (
	"errors"

	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// screenRows dumps the simulation screen as text for failure messages.
func screenRows(scr tcell.SimulationScreen) string {
	prim, w, _ := scr.GetContents()
	var b []byte
	for y := 0; y*w < len(prim); y++ {
		for x := 0; x < w && y*w+x < len(prim); x++ {
			b = append(b, []byte(string(prim[y*w+x].Runes))...)
		}
		b = append(b, '\n')
	}
	return string(b)
}
func treeTestEntries() []TreeEntry {
	return []TreeEntry{
		{ID: "11111111aaaa", Type: "message", Role: "user", Summary: "Start task", Text: "Start task", Depth: 0},
		{ID: "22222222bbbb", Type: "message", Role: "assistant", Summary: "Plan", Depth: 1},
		{ID: "33333333cccc", Type: "message", Role: "toolResult", Summary: "bash: ls", Depth: 2},
		{ID: "44444444dddd", Type: "model_change", Summary: "onegw/free", Depth: 2},
		{ID: "55555555eeee", Type: "custom", Summary: "(branch)", Depth: 2},
		{ID: "66666666ffff", Type: "message", Role: "user", Summary: "Try approach A", Depth: 1, Active: true},
	}
}

func openTreeTestApp(t *testing.T, entries []TreeEntry, labels map[string]string) (*App, tcell.SimulationScreen, *[]string) {
	t.Helper()
	app, scr := newTestApp(t, 100, 30)
	app.SetTreeData(func() []TreeEntry { return entries })
	saved := &[]string{}
	app.SetTreeLabels(func() map[string]string {
		if labels == nil {
			return map[string]string{}
		}
		return labels
	}, func(id, label string) error {
		*saved = append(*saved, id+":"+label)
		return nil
	})
	app.OpenTreeSelector()
	return app, scr, saved
}

// visibleCount is the number of rows the current filter/search admits.
func visibleCount(app *App) int {
	app.mu.Lock()
	defer app.mu.Unlock()
	return len(app.tpick.visibleIdx(app.treeLabels))
}

// filterOf reports the selector's active filter name.
func filterOf(app *App) string {
	app.mu.Lock()
	defer app.mu.Unlock()
	return treeFilterNames[app.tpick.filter]
}

func TestTreeSelectorRendersBulletAndLabel(t *testing.T) {
	app, scr, _ := openTreeTestApp(t, treeTestEntries(), map[string]string{"22222222bbbb": "milestone"})
	app.draw()

	if !gridContains(scr, "→ 66666666 message Try approach A") {
		t.Fatalf("active leaf row missing from screen:\n%s", screenRows(scr))
	}
	if !gridContains(scr, "[milestone] 22222222 message Plan") {
		t.Fatalf("label row missing from screen:\n%s", screenRows(scr))
	}
	if !gridContains(scr, "session tree · filter:default") {
		t.Fatalf("no-tools chrome missing:\n%s", screenRows(scr))
	}
}

func TestTreeSelectorFilterCycle(t *testing.T) {
	app, _, _ := openTreeTestApp(t, treeTestEntries(), map[string]string{"22222222bbbb": "milestone"})

	want := []struct {
		filter string
		rows   int
	}{
		{"default", 4}, // user, assistant, toolResult, user (model_change/custom hidden)
		{"no-tools", 3},
		{"user-only", 2},
		{"labeled-only", 1},
		{"all", 6},
	}
	if got := filterOf(app); got != "default" {
		t.Fatalf("initial filter = %q, want default", got)
	}
	if got := visibleCount(app); got != want[0].rows {
		t.Fatalf("default rows = %d, want %d", got, want[0].rows)
	}
	for _, w := range want[1:] {
		app.handleKey(tcell.NewEventKey(tcell.KeyCtrlO, 0, tcell.ModNone))
		if got := filterOf(app); got != w.filter {
			t.Fatalf("after Ctrl+O filter = %q, want %q", got, w.filter)
		}
		if got := visibleCount(app); got != w.rows {
			t.Fatalf("filter %s rows = %d, want %d", w.filter, got, w.rows)
		}
	}
	// Wraps back to default.
	app.handleKey(tcell.NewEventKey(tcell.KeyCtrlO, 0, tcell.ModNone))
	if got := filterOf(app); got != "default" {
		t.Fatalf("cycle did not wrap: %q", got)
	}
}

func TestTreeSelectorAltFilterJumps(t *testing.T) {
	app, _, _ := openTreeTestApp(t, treeTestEntries(), map[string]string{"22222222bbbb": "milestone"})

	for _, tt := range []struct {
		key    rune
		filter string
		rows   int
	}{
		{'a', "all", 6},
		{'u', "user-only", 2},
		{'t', "no-tools", 3},
		{'l', "labeled-only", 1},
		{'d', "default", 4},
	} {
		app.handleKey(tcell.NewEventKey(tcell.KeyRune, tt.key, tcell.ModAlt))
		if got := filterOf(app); got != tt.filter {
			t.Fatalf("Alt+%c filter = %q, want %q", tt.key, got, tt.filter)
		}
		if got := visibleCount(app); got != tt.rows {
			t.Fatalf("Alt+%c rows = %d, want %d", tt.key, got, tt.rows)
		}
	}
}

func TestTreeSelectorSearchFilters(t *testing.T) {
	app, _, _ := openTreeTestApp(t, treeTestEntries(), nil)

	typeRunes(app, "bash")
	if got := visibleCount(app); got != 1 {
		t.Fatalf("search 'bash' rows = %d, want 1", got)
	}
	// The one visible row is the tool result; no-tools hides it.
	app.handleKey(tcell.NewEventKey(tcell.KeyCtrlO, 0, tcell.ModNone))
	if got := visibleCount(app); got != 0 {
		t.Fatalf("search+no-tools rows = %d, want 0", got)
	}
	// Cycle back to the default filter and clear the query with Backspace.
	for range numTreeFilters - 1 {
		app.handleKey(tcell.NewEventKey(tcell.KeyCtrlO, 0, tcell.ModNone))
	}
	for range "bash" {
		app.handleKey(tcell.NewEventKey(tcell.KeyBackspace, 0, tcell.ModNone))
	}
	if got := visibleCount(app); got != 4 {
		t.Fatalf("cleared search rows = %d, want 4", got)
	}
	typeRunes(app, "approach")
	if got := visibleCount(app); got != 1 {
		t.Fatalf("search 'approach' rows = %d, want 1", got)
	}
	// First Esc clears the query, second closes.
	app.handleKey(tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone))
	if !app.TreeSelectorOpen() {
		t.Fatal("first Esc must only clear the search")
	}
	if got := visibleCount(app); got != 4 {
		t.Fatalf("after clearing search rows = %d, want 4", got)
	}
	app.handleKey(tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone))
	if app.TreeSelectorOpen() {
		t.Fatal("second Esc must close the selector")
	}
}

func TestTreeSelectorLabelSetAndClear(t *testing.T) {
	app, _, saved := openTreeTestApp(t, treeTestEntries(), nil)

	// Starts on the active leaf (index 5).
	app.mu.Lock()
	sel := app.tpick.sel
	app.mu.Unlock()
	if sel != 5 {
		t.Fatalf("initial selection = %d, want the active leaf", sel)
	}

	app.handleKey(tcell.NewEventKey(tcell.KeyRune, 'L', tcell.ModShift))
	typeRunes(app, "milestone")
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))

	app.mu.Lock()
	got := app.treeLabels["66666666ffff"]
	app.mu.Unlock()
	if got != "milestone" {
		t.Fatalf("label = %q, want milestone", got)
	}
	if len(*saved) != 1 || (*saved)[0] != "66666666ffff:milestone" {
		t.Fatalf("saved = %v", *saved)
	}

	// Shift+L prefills the current label; Backspace-ing it out clears.
	app.handleKey(tcell.NewEventKey(tcell.KeyRune, 'L', tcell.ModShift))
	app.mu.Lock()
	buf := app.tpick.labelBuf
	app.mu.Unlock()
	if buf != "milestone" {
		t.Fatalf("label prompt prefill = %q", buf)
	}
	for range "milestone" {
		app.handleKey(tcell.NewEventKey(tcell.KeyBackspace, 0, tcell.ModNone))
	}
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))

	app.mu.Lock()
	_, still := app.treeLabels["66666666ffff"]
	app.mu.Unlock()
	if still {
		t.Fatal("empty label must clear the entry")
	}
	if len(*saved) != 2 || (*saved)[1] != "66666666ffff:" {
		t.Fatalf("saved = %v", *saved)
	}
}

func TestTreeSelectorEnterSwitchesAndSummarizes(t *testing.T) {
	app, _, _ := openTreeTestApp(t, treeTestEntries(), nil)
	var branched, summarized []string
	app.SetSessionBranch(func(id string) error {
		branched = append(branched, id)
		return nil
	})
	app.SetSessionOps(&SessionOps{SummarizeAndBranch: func(id string) error {
		summarized = append(summarized, id)
		return nil
	}})

	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if len(branched) != 1 || branched[0] != "66666666ffff" {
		t.Fatalf("branched = %v", branched)
	}
	if app.TreeSelectorOpen() {
		t.Fatal("Enter must close the selector")
	}

	app.OpenTreeSelector()
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModShift))
	if len(summarized) != 1 || summarized[0] != "66666666ffff" {
		t.Fatalf("summarized = %v", summarized)
	}
	if len(branched) != 1 {
		t.Fatalf("Shift+Enter must not take the plain branch path: %v", branched)
	}

	// Alt+S is the portable alias (plain terminals cannot send Shift+Enter).
	app.OpenTreeSelector()
	app.handleKey(tcell.NewEventKey(tcell.KeyRune, 's', tcell.ModAlt))
	if len(summarized) != 2 || summarized[1] != "66666666ffff" {
		t.Fatalf("Alt+S summarized = %v", summarized)
	}
}

func TestTreeSelectorDoubleEscape(t *testing.T) {
	app, _, _ := openTreeTestApp(t, treeTestEntries(), nil)
	app.CloseTreeSelector()

	// Claude-Code double-Esc: with a draft the first press only parks the
	// draft; the next press (empty composer) opens the rewind picker.
	typeRunes(app, "draft")
	app.handleKey(tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone))
	if app.TreeSelectorOpen() {
		t.Fatal("Esc with a non-empty composer must not open the selector")
	}
	if app.ed.Text() != "" {
		t.Fatalf("first Esc must clear the draft, got %q", app.ed.Text())
	}
	app.handleKey(tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone))
	if !app.TreeSelectorOpen() {
		t.Fatal("Esc on an empty composer must open the selector")
	}
	app.handleKey(tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone))
	if app.TreeSelectorOpen() {
		t.Fatal("second Esc must close the selector")
	}
}

// TestTreeRewindReprimesComposer pins the "resume" half of double-Esc
// rewind: switching to a user row puts that prompt back in the composer so
// it can be edited and resent; switching to a non-user row leaves it alone.
func TestTreeRewindReprimesComposer(t *testing.T) {
	app, _, _ := openTreeTestApp(t, treeTestEntries(), nil)
	var branched []string
	app.SetSessionBranch(func(id string) error {
		branched = append(branched, id)
		return nil
	})

	app.mu.Lock()
	app.tpick.sel = 0 // "Start task" — user row with Text
	app.mu.Unlock()
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if len(branched) != 1 || branched[0] != "11111111aaaa" {
		t.Fatalf("branched = %v", branched)
	}
	if app.ed.Text() != "Start task" {
		t.Fatalf("composer = %q, want the rewound prompt re-primed", app.ed.Text())
	}

	// An assistant row switches but must not touch the composer.
	app.ed.Reset()
	app.OpenTreeSelector()
	app.mu.Lock()
	app.tpick.sel = 1 // assistant "Plan"
	app.mu.Unlock()
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if app.ed.Text() != "" {
		t.Fatalf("non-user switch changed the composer: %q", app.ed.Text())
	}

	// A failed switch must not fake a rewind either.
	app.SetSessionBranch(func(id string) error {
		return errors.New("branch failed")
	})
	app.OpenTreeSelector()
	app.mu.Lock()
	app.tpick.sel = 0
	app.mu.Unlock()
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if app.ed.Text() != "" {
		t.Fatalf("failed switch re-primed the composer: %q", app.ed.Text())
	}
}

func TestTreeSelectorKeymapAndSlashCommand(t *testing.T) {
	altT := tcell.NewEventKey(tcell.KeyRune, 't', tcell.ModAlt)
	if got := DefaultKeyMap().Resolve(altT); got != "app.session.tree" {
		t.Fatalf("DefaultKeyMap A-t = %q, want app.session.tree", got)
	}
	known := false
	for _, a := range BuiltinActions {
		if a == "app.session.tree" {
			known = true
		}
	}
	if !known {
		t.Fatal("app.session.tree missing from BuiltinActions")
	}

	app, _, _ := openTreeTestApp(t, treeTestEntries(), nil)
	app.CloseTreeSelector()
	app.handleKey(altT)
	if !app.TreeSelectorOpen() {
		t.Fatal("app.session.tree chord must open the selector")
	}

	// /tree opens the same selector and no longer dumps text.
	app.CloseTreeSelector()
	if !dispatch(app, "/tree") {
		t.Fatal("/tree did not dispatch")
	}
	if !app.TreeSelectorOpen() {
		t.Fatal("/tree must open the selector")
	}
	app.mu.Lock()
	n := len(app.blocks)
	app.mu.Unlock()
	if n != 0 {
		t.Fatalf("/tree added %d transcript blocks; the text dump must be gone", n)
	}
}

func TestTreeSelectorNavigation(t *testing.T) {
	app, _, _ := openTreeTestApp(t, treeTestEntries(), nil)

	app.handleKey(tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone))
	app.mu.Lock()
	sel := app.tpick.sel
	app.mu.Unlock()
	if sel != 2 {
		t.Fatalf("after Up sel = %d, want 2 (previous visible row)", sel)
	}
	for range 5 {
		app.handleKey(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	}
	app.mu.Lock()
	sel = app.tpick.sel
	app.mu.Unlock()
	if sel != 5 {
		t.Fatalf("after 5xDown sel = %d, want 5", sel)
	}
}

// TestTreeSelectorLabelSaveErrorNoDeadlock pins the lock discipline: the
// label-commit path must surface the sidecar write error without holding
// a.mu (AddSystemBlock re-locks — nesting would self-deadlock, the exact
// picker bug 17b2165 fixed).
func TestTreeSelectorLabelSaveErrorNoDeadlock(t *testing.T) {
	app, _, _ := openTreeTestApp(t, treeTestEntries(), nil)
	app.SetTreeLabels(func() map[string]string { return map[string]string{} },
		func(id, label string) error { return errors.New("sidecar unwritable") })
	app.OpenTreeSelector()

	app.handleKey(tcell.NewEventKey(tcell.KeyRune, 'L', tcell.ModNone|tcell.ModShift))
	typeRunes(app, "x")
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("label commit deadlocked holding a.mu")
	}
	app.mu.Lock()
	n := len(app.blocks)
	app.mu.Unlock()
	if n != 1 {
		t.Fatalf("blocks = %d, want the one save-error notice", n)
	}
	app.mu.Lock()
	got := app.treeLabels["66666666ffff"]
	app.mu.Unlock()
	if got != "x" {
		t.Fatalf("label = %q, want x kept in memory despite the save error", got)
	}
}

// TestTreeSelectorNoDataNoOpen keeps the seams honest: nothing to show →
// /tree and Esc do nothing rather than flashing an empty box.
func TestTreeSelectorNoDataNoOpen(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	app.handleKey(tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone))
	if app.TreeSelectorOpen() {
		t.Fatal("Esc with no tree data must not open the selector")
	}
	if !dispatch(app, "/tree") {
		t.Fatal("/tree must be consumed even with no tree data")
	}
	if app.TreeSelectorOpen() {
		t.Fatal("/tree with no data must not open the selector")
	}
}
