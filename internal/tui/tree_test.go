package tui

import (
	"errors"
	"fmt"
	"strings"

	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
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
		{ID: "11111111aaaa", Type: "message", Role: "user", Summary: "Start task"},
		{ID: "22222222bbbb", Type: "message", Role: "assistant", Summary: "Plan"},
		{ID: "33333333cccc", Type: "message", Role: "toolResult", Summary: "bash: ls"},
		{ID: "44444444dddd", Type: "model_change", Summary: "onegw/free"},
		{ID: "55555555eeee", Type: "custom", Summary: "(branch)"},
		{ID: "66666666ffff", Type: "message", Role: "user", Summary: "Try approach A", Active: true},
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

	if !gridContains(scr, "→ user    66666666 Try approach A") {
		t.Fatalf("active leaf row missing from screen:\n%s", screenRows(scr))
	}
	if !gridContains(scr, "  agent   [milestone] 22222222 Plan") {
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
	type nav struct {
		id        string
		summarize bool
	}
	var navs []nav
	app.SetSessionOps(&SessionOps{NavigateTree: func(id string, summarize bool) (string, error) {
		navs = append(navs, nav{id, summarize})
		return "", nil
	}})

	// The selector opens on the active leaf: the "6666" row is a USER row,
	// so Enter still navigates (omp rewinds the last prompt into the
	// composer even when it is the leaf).
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if len(navs) != 1 || navs[0] != (nav{"66666666ffff", false}) {
		t.Fatalf("navigate = %+v", navs)
	}
	if app.TreeSelectorOpen() {
		t.Fatal("Enter must close the selector")
	}

	app.OpenTreeSelector()
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModShift))
	if len(navs) != 2 || navs[1] != (nav{"66666666ffff", true}) {
		t.Fatalf("Shift+Enter navigate = %+v", navs)
	}

	// Alt+S is the portable alias (plain terminals cannot send Shift+Enter).
	app.OpenTreeSelector()
	app.handleKey(tcell.NewEventKey(tcell.KeyRune, 's', tcell.ModAlt))
	if len(navs) != 3 || navs[2] != (nav{"66666666ffff", true}) {
		t.Fatalf("Alt+S navigate = %+v", navs)
	}
}

func TestTreeSelectorDoubleEscape(t *testing.T) {
	app, _, _ := openTreeTestApp(t, treeTestEntries(), nil)
	app.CloseTreeSelector()
	esc := func() {
		app.handleKey(tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone))
	}

	// The Esc ladder: clear the draft (and keep it) → give it back → clear
	// again → open the rewind picker. A press never both restores and opens:
	// the user who fat-fingers Esc wants the prompt back, not a modal over
	// the top of it.
	typeRunes(app, "draft")
	esc()
	if app.TreeSelectorOpen() {
		t.Fatal("Esc with a non-empty composer must not open the selector")
	}
	if app.ed.Text() != "" {
		t.Fatalf("first Esc must clear the draft, got %q", app.ed.Text())
	}
	esc()
	if app.TreeSelectorOpen() {
		t.Fatal("Esc with an Esc-undoable draft must restore it, not open the selector")
	}
	if app.ed.Text() != "draft" {
		t.Fatalf("second Esc must bring the prompt back, got %q", app.ed.Text())
	}
	esc() // the restored draft gets its one undo's worth: cleared again
	if app.ed.Text() != "" {
		t.Fatalf("third Esc must clear the restored draft, got %q", app.ed.Text())
	}
	esc()
	if !app.TreeSelectorOpen() {
		t.Fatal("with nothing left to undo, Esc must open the selector")
	}
	esc()
	if app.TreeSelectorOpen() {
		t.Fatal("Esc must close the selector")
	}
}

// The stash always holds what the LAST Esc cleared, and each draft gets its
// own one-undo: text typed over a stash replaces it on the next clear (so Esc
// is "undo my last Esc", never a resurrection of a prompt the user abandoned),
// and a send retires it — a prompt that surfaces after the user moved on
// duplicates text nobody typed.
func TestEscapeDraftStashDiesOnEdit(t *testing.T) {
	app, _, _ := openTreeTestApp(t, treeTestEntries(), nil)
	app.CloseTreeSelector()
	esc := func() {
		app.handleKey(tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone))
	}

	typeRunes(app, "stashed")
	esc() // clear + stash
	typeRunes(app, "typed")
	esc() // the new draft clears AND re-stashes: the stash is the last Esc, not the first
	if app.ed.Text() != "" {
		t.Fatalf("Esc over a typed draft must clear it, got %q", app.ed.Text())
	}
	esc()
	if app.ed.Text() != "typed" {
		t.Fatalf("Esc must restore what the last Esc cleared, got %q", app.ed.Text())
	}
	// Editing the handed-back draft buys it a fresh undo: Esc after an edit
	// must not delete what the user just fixed.
	typeRunes(app, " v2")
	esc()
	if app.ed.Text() != "" {
		t.Fatalf("Esc must clear the edited draft, got %q", app.ed.Text())
	}
	esc()
	if app.ed.Text() != "typed v2" {
		t.Fatalf("the edited draft got its own undo, got %q", app.ed.Text())
	}
	esc() // the restored draft's single undo is spent: cleared for real
	esc()
	if app.ed.Text() != "" {
		t.Fatalf("a spent undo must not blink the text back, got %q", app.ed.Text())
	}
	if !app.TreeSelectorOpen() {
		t.Fatal("with nothing left to undo, Esc must open the selector")
	}

	// A send retires the stash too.
	app.CloseTreeSelector()
	typeRunes(app, "one")
	esc() // stash "one"
	typeRunes(app, "two")
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	esc() // empty + retired: the picker, not "one" resurrected after "two"
	if app.ed.Text() != "" {
		t.Fatalf("a sent draft must retire the stash, got %q", app.ed.Text())
	}
	if !app.TreeSelectorOpen() {
		t.Fatal("Esc after a send must open the selector")
	}
}

// TestTreeRewindReprimesComposer pins the omp navigateTree draft contract:
// navigating to a user row returns its prompt as a draft that re-primes the
// composer (edit and resend without duplicating the entry) — but only over
// an EMPTY composer; a typed or parked draft is never clobbered. Non-user
// rows and failed navigations touch nothing.
func TestTreeRewindReprimesComposer(t *testing.T) {
	app, _, _ := openTreeTestApp(t, treeTestEntries(), nil)
	var navs []string
	app.SetSessionOps(&SessionOps{NavigateTree: func(id string, summarize bool) (string, error) {
		navs = append(navs, id)
		if id == "11111111aaaa" {
			return "Start task", nil
		}
		return "", nil
	}})

	app.mu.Lock()
	app.tpick.sel = 0 // "Start task" — user row
	app.mu.Unlock()
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if len(navs) != 1 || navs[0] != "11111111aaaa" {
		t.Fatalf("navigate = %v", navs)
	}
	if app.ed.Text() != "Start task" {
		t.Fatalf("composer = %q, want the rewound prompt re-primed", app.ed.Text())
	}

	// An existing draft wins: omp sets it only over an empty editor.
	app.ed.Reset()
	typeRunes(app, "my draft")
	app.OpenTreeSelector()
	app.mu.Lock()
	app.tpick.sel = 0
	app.mu.Unlock()
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if app.ed.Text() != "my draft" {
		t.Fatalf("rewind clobbered the draft: %q", app.ed.Text())
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
	app.SetSessionOps(&SessionOps{NavigateTree: func(string, bool) (string, error) {
		return "Start task", errors.New("branch failed")
	}})
	app.OpenTreeSelector()
	app.mu.Lock()
	app.tpick.sel = 0
	app.mu.Unlock()
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if app.ed.Text() != "" {
		t.Fatalf("failed switch re-primed the composer: %q", app.ed.Text())
	}
}

// TestTreeSelectorAlreadyAtThisPoint pins omp's guard: re-picking the
// active leaf navigates nowhere; a non-user row gets the status notice
// instead of a pointless re-render. (A user row is still rewound — see
// TestTreeSelectorEnterSwitchesAndSummarizes.)
func TestTreeSelectorAlreadyAtThisPoint(t *testing.T) {
	entries := []TreeEntry{
		{ID: "11111111aaaa", Type: "message", Role: "user", Summary: "Start task"},
		{ID: "22222222bbbb", Type: "message", Role: "assistant", Summary: "Plan", Active: true},
	}
	app, _, _ := openTreeTestApp(t, entries, nil)
	var navs []string
	app.SetSessionOps(&SessionOps{NavigateTree: func(id string, summarize bool) (string, error) {
		navs = append(navs, id)
		return "", nil
	}})

	// The selector opens on the active assistant row: Enter is a no-op.
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if len(navs) != 0 {
		t.Fatalf("active non-user row navigated: %v", navs)
	}

	// The older user row still navigates.
	app.OpenTreeSelector()
	app.mu.Lock()
	app.tpick.sel = 0
	app.mu.Unlock()
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if len(navs) != 1 || navs[0] != "11111111aaaa" {
		t.Fatalf("navigate = %v, want the user row", navs)
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

// The row gutter is gone: depth was the parent-chain length and a long run is
// one linear chain (a 724-entry session ends at depth 722), so two cells per
// level put ~1.4k cells before a row's own text — the newest rows, the ones the
// navigator exists to reach, painted as a wall of blank gutter, and the overflow
// wrecked the row below (probe: a long drawText at y keeps painting onto y+1).
// In its place: rows start flush left and open with an eight-cell author tag,
// because "user or agent" is the question the panel is read to answer.
func TestTreeRowIsFlushLeftWithRoleTag(t *testing.T) {
	sum := strings.Repeat("x", 60) // real summaries are clipped this long
	deep := treeRowText(TreeEntry{ID: "aaaaaaaaaaaa", Type: "message", Role: "user", Summary: sum}, "")
	if !strings.HasPrefix(deep, "user    ") {
		t.Fatalf("user row = %q, want the role tag first, no gutter", deep)
	}
	// The selector's interior is min(width-4, 120) cells: anything past the
	// panel edge is cut off and past the screen width drops onto the next row.
	if width(deep) >= 120 {
		t.Fatalf("row is %d cells, too wide for the panel: %q", width(deep), deep)
	}
	// Every tag is the same width, so ids and summaries form a column whether
	// or not a label is present — the point of a fixed tag.
	for _, e := range []TreeEntry{
		{ID: "bbbbbbbb", Type: "message", Role: "assistant"},
		{ID: "cccccccc", Type: "message", Role: "toolResult", Summary: "bash: ls"},
		{ID: "dddddddd", Type: "model_change", Summary: "onegw/free"},
		{ID: "eeeeeeee", Type: "custom", Summary: "(branch)"},
		{ID: "ffffffffff", Type: "compaction", Summary: "(compaction)"},
		{ID: "11111111", Type: "branch_summary"},
		{ID: "22222222", Type: "message", Role: "weird"},
	} {
		tag := treeRoleTag(e)
		if width(tag) != 8 {
			t.Fatalf("tag %q for %+v is %d cells, want 8", tag, e, width(tag))
		}
		if got := treeRowText(e, "mile"); !strings.HasPrefix(got, tag+"[mile] ") {
			t.Fatalf("row = %q, want %q then the label", got, tag)
		}
	}
	// Colour matches the transcript, and the split is legible without it.
	if slot := treeRoleSlot(TreeEntry{Type: "message", Role: "user"}); slot != theme.AccentUser {
		t.Fatalf("user slot = %q, want the user accent", slot)
	}
	if slot := treeRoleSlot(TreeEntry{Type: "message", Role: "assistant"}); slot != theme.AccentAssistant {
		t.Fatalf("assistant slot = %q, want the assistant accent", slot)
	}
}

// OpenTreeSelector focuses the newest row. The old default was row 0 — the
// oldest entry — which only worked while the snapshot flagged the leaf: after
// /branch or Shift+Enter the leaf is a custom/branch_summary marker the
// default filter hides, and with no flag to find the selection stayed parked
// at the start of history. A long session then opened showing its first rows.
func TestTreeSelectorOpensOnTheNewestRow(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	app.SetTreeData(func() []TreeEntry {
		return []TreeEntry{
			{ID: "11111111aaaa", Type: "message", Role: "user", Summary: "first prompt"},
			{ID: "22222222bbbb", Type: "message", Role: "assistant", Summary: "answer"},
			// The leaf: a branch marker, invisible to the default filter.
			{ID: "33333333cccc", Type: "custom", Summary: "(branch)", Active: true},
		}
	})
	app.OpenTreeSelector()
	app.mu.Lock()
	sel := app.tpick.sel
	app.mu.Unlock()
	if sel != 1 {
		t.Fatalf("selection = %d, want the newest VISIBLE row (1: the leaf is filtered out)", sel)
	}

	// Same when nothing is flagged at all (a snapshot built without the leaf).
	app.SetTreeData(func() []TreeEntry {
		return []TreeEntry{
			{ID: "11111111aaaa", Type: "message", Role: "user", Summary: "first prompt"},
			{ID: "22222222bbbb", Type: "message", Role: "assistant", Summary: "answer"},
		}
	})
	app.OpenTreeSelector()
	app.mu.Lock()
	sel = app.tpick.sel
	app.mu.Unlock()
	if sel != 1 {
		t.Fatalf("unflagged selection = %d, want the last row", sel)
	}
}

// The row window is sized to the room above the composer, and the selection
// is kept inside it. maxRows = height/2 with no room term made a panel taller
// than the space left above a tall composer bail out in draw() — the modal the
// user had just opened closed itself, and the newest rows were never paintable.
func TestTreeSelectorPaintsNewestRowsUnderATallComposer(t *testing.T) {
	var entries []TreeEntry
	for i := range 40 {
		entries = append(entries, TreeEntry{
			ID: fmt.Sprintf("%08dzzzz", i), Type: "message", Role: "user",
			Summary: fmt.Sprintf("prompt %d", i),
		})
	}
	entries[len(entries)-1].Active = true
	app, scr := newTestApp(t, 100, 20)
	app.SetTreeData(func() []TreeEntry { return entries })
	app.OpenTreeSelector()
	// A multi-line draft eats the screen the panel used to demand: eight
	// hard-newline rows put composerRows at 10 of the 20.
	for _, para := range []string{"one", "two", "three", "four", "five", "six", "seven", "eight"} {
		app.ed.HandleKey(tcell.NewEventKey(tcell.KeyCtrlJ, 0, tcell.ModNone))
		for _, r := range para {
			app.ed.HandleKey(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
		}
	}
	app.draw()
	app.mu.Lock()
	open := app.tpick != nil
	app.mu.Unlock()
	if !open {
		t.Fatalf("selector closed itself instead of drawing a shorter window:\n%s", screenRows(scr))
	}
	if !gridContains(scr, "prompt 39") {
		t.Fatalf("the focused newest row is not on screen:\n%s", screenRows(scr))
	}
}
