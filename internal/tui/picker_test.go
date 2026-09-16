package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// Session picker extras (#26): scope toggle, token search (+ cmd search
// hook), pins, confirmed delete, empty-folder hint.

func pickerTestItems() []SessionPickerItem {
	return []SessionPickerItem{
		{ID: "aaaa1111", Title: "fix parser bug", Mtime: "Jan 02 15:04", Size: "1 KB", InCwd: true},
		{ID: "bbbb2222", Title: "alpha parser notes", Mtime: "Jan 02 15:04", Size: "2 KB", InCwd: true},
		{ID: "cccc3333", Title: "other project work", Mtime: "Jan 02 15:04", Size: "3 KB", InCwd: false},
	}
}

func pickerRows(t *testing.T, app *App) []SessionPickerItem {
	t.Helper()
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.spick == nil {
		t.Fatal("picker is not open")
	}
	return append([]SessionPickerItem(nil), app.spick.view...)
}

func pickerConfirmID(t *testing.T, app *App) string {
	t.Helper()
	app.mu.Lock()
	defer app.mu.Unlock()
	return app.spick.confirm
}

func pressKey(app *App, k tcell.Key) {
	app.handleKey(tcell.NewEventKey(k, 0, tcell.ModNone))
}

func pressRune(app *App, r rune) {
	app.handleKey(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
}

// TestPickerScopeToggleKeepsItems: default = current folder, Tab flips to
// all projects and back without dropping rows; navigation never
// auto-switches scope.
func TestPickerScopeToggleKeepsItems(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.OpenSessionPicker(pickerTestItems())

	got := pickerRows(t, app)
	if len(got) != 2 || !got[0].InCwd || !got[1].InCwd {
		t.Fatalf("default scope rows = %v, want the 2 cwd rows", got)
	}
	pressKey(app, tcell.KeyDown)
	if got := pickerRows(t, app); len(got) != 2 {
		t.Fatalf("navigation changed scope: %d rows", len(got))
	}
	pressKey(app, tcell.KeyTab)
	got = pickerRows(t, app)
	if len(got) != 3 {
		t.Fatalf("all-projects rows = %d, want 3 (%v)", len(got), got)
	}
	if got[2].ID != "cccc3333" {
		t.Fatalf("all-projects lost a row: %v", got)
	}
	pressKey(app, tcell.KeyTab)
	if got := pickerRows(t, app); len(got) != 2 {
		t.Fatalf("back to current folder = %d rows, want 2", len(got))
	}
}

// TestPickerTokenSearch: every whitespace-separated token must match
// id+title (case-insensitive); Backspace trims the query.
func TestPickerTokenSearch(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.OpenSessionPicker(pickerTestItems())

	typeRunes(app, "par")
	if got := pickerRows(t, app); len(got) != 2 {
		t.Fatalf("query 'par' rows = %v, want both parser rows", got)
	}
	typeRunes(app, " notes")
	got := pickerRows(t, app)
	if len(got) != 1 || got[0].ID != "bbbb2222" {
		t.Fatalf("query 'par notes' rows = %v, want only bbbb2222", got)
	}
	typeRunes(app, "AAAA") // id tokens match too
	if got := pickerRows(t, app); len(got) != 0 {
		t.Fatalf("query 'par notes AAAA' should match nothing, got %v", got)
	}
	for i := 0; i < 15; i++ { // trim back to empty
		pressKey(app, tcell.KeyBackspace)
	}
	if got := pickerRows(t, app); len(got) != 2 {
		t.Fatalf("cleared query rows = %d, want 2", len(got))
	}
}

// TestPickerSearchHookRanksMatches: with a search hook wired (cmd
// prompt-text matches), the hook's rows are used, scoped by Tab, and the
// hook receives the raw query.
func TestPickerSearchHookRanksMatches(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	var queries []string
	app.SetPickerSearch(func(q string) []SessionPickerItem {
		queries = append(queries, q)
		return []SessionPickerItem{
			{ID: "dddd4444", Title: "prompt match", InCwd: false},
			{ID: "eeee5555", Title: "prompt match cwd", InCwd: true},
		}
	})
	app.OpenSessionPicker(pickerTestItems())
	typeRunes(app, "needle")

	if len(queries) != 6 || queries[len(queries)-1] != "needle" {
		t.Fatalf("hook queries = %v, want one per keystroke ending in 'needle'", queries)
	}
	got := pickerRows(t, app)
	if len(got) != 1 || got[0].ID != "eeee5555" {
		t.Fatalf("hooked rows = %v, want only the cwd hook result", got)
	}
	pressKey(app, tcell.KeyTab)
	if got := pickerRows(t, app); len(got) != 2 {
		t.Fatalf("all-projects hooked rows = %v, want both", got)
	}
}

// TestPickerStatusIsSearchable: the badge is only useful if the query can
// find it — typing "interrupted" must narrow the list to interrupted rows,
// and combining the badge with an id token must AND, not OR (#107).
func TestPickerStatusIsSearchable(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	items := []SessionPickerItem{
		{ID: "aaaa1111", Title: "finished work", InCwd: true, Status: "done"},
		{ID: "bbbb2222", Title: "killed work", InCwd: true, Status: "interrupted"},
		{ID: "cccc3333", Title: "interrupted rerun", InCwd: true, Status: "interrupted"},
	}
	app.OpenSessionPicker(items)

	typeRunes(app, "interrupted")
	got := pickerRows(t, app)
	if len(got) != 2 {
		t.Fatalf("query 'interrupted' rows = %v, want the two interrupted rows", got)
	}
	for _, it := range got {
		if it.Status != "interrupted" {
			t.Fatalf("row %s matched 'interrupted' but is %q", it.ID, it.Status)
		}
	}

	typeRunes(app, " bbbb") // badge AND id must narrow, not union
	got = pickerRows(t, app)
	if len(got) != 1 || got[0].ID != "bbbb2222" {
		t.Fatalf("query 'interrupted bbbb' rows = %v, want only bbbb2222", got)
	}

	// cmd's search hook is the live path for the picker query, so the badge
	// has to be matchable through it too (cmd ranks, TUI re-applies scope).
	app.CloseSessionPicker()
	app.SetPickerSearch(func(q string) []SessionPickerItem {
		var out []SessionPickerItem
		for _, it := range items {
			if strings.Contains(strings.ToLower(it.ID+" "+it.Title+" "+it.Status), q) {
				out = append(out, it)
			}
		}
		return out
	})
	app.OpenSessionPicker(items)
	typeRunes(app, "interrupted")
	if got := pickerRows(t, app); len(got) != 2 {
		t.Fatalf("hooked query 'interrupted' rows = %v, want the two interrupted rows", got)
	}
}

// TestPickerIdleCapAndSearchCap: the idle view keeps the 12-row cap; an
// active query may grow it (search needs the wider pool).
func TestPickerIdleCapAndSearchCap(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	many := make([]SessionPickerItem, 20)
	for i := range many {
		many[i] = SessionPickerItem{ID: "idabcdef", Title: "sess task", InCwd: true}
	}
	app.OpenSessionPicker(many)
	if got := pickerRows(t, app); len(got) != pickerIdleCap {
		t.Fatalf("idle rows = %d, want cap %d", len(got), pickerIdleCap)
	}
	typeRunes(app, "sess")
	if got := pickerRows(t, app); len(got) != 20 {
		t.Fatalf("searching rows = %d, want all 20", len(got))
	}
}

// TestPickerPinOrderAndToggle: pinned rows sort first; Ctrl+P toggles the
// highlight (empty query and mid-search alike) while a typed 'p' stays
// search text.
func TestPickerPinOrderAndToggle(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	items := pickerTestItems()
	items[1].Pinned = true // bbbb2222
	app.OpenSessionPicker(items)

	if got := pickerRows(t, app); got[0].ID != "bbbb2222" {
		t.Fatalf("pinned row must sort first: %v", got)
	}
	var toggled []string
	app.SetPickerPinToggle(func(id string) { toggled = append(toggled, id) })

	pressKey(app, tcell.KeyCtrlP) // unpin the highlighted (pinned) row
	if len(toggled) != 1 || toggled[0] != "bbbb2222" {
		t.Fatalf("pin toggles = %v, want bbbb2222", toggled)
	}
	if got := pickerRows(t, app); got[0].ID != "aaaa1111" {
		t.Fatalf("unpinned row still floats: %v", got)
	}
	// Typing 'p' is a search character, never a pin shortcut: the query
	// "parser" must be typeable end to end.
	typeRunes(app, "parser")
	if got := pickerRows(t, app); len(got) != 2 {
		t.Fatalf("'parser' query rows = %v, want both", got)
	}
	if len(toggled) != 1 {
		t.Fatalf("typed 'p' toggled a pin: %v", toggled)
	}
	pressKey(app, tcell.KeyCtrlP) // mid-search toggle on the highlight
	if len(toggled) != 2 || toggled[1] != "aaaa1111" {
		t.Fatalf("Ctrl+P mid-search toggles = %v, want aaaa1111 second", toggled)
	}
	if got := pickerRows(t, app); got[0].ID != "aaaa1111" {
		t.Fatalf("freshly pinned row must float: %v", got)
	}
}

// TestPickerDeleteRequiresConfirmation: first Backspace on an empty query
// only arms the delete; Esc cancels it (picker stays open); a second
// Backspace deletes and drops the row.
func TestPickerDeleteRequiresConfirmation(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	var deleted []string
	app.SetPickerDelete(func(id string) error {
		deleted = append(deleted, id)
		return nil
	})
	app.OpenSessionPicker(pickerTestItems())

	pressKey(app, tcell.KeyBackspace)
	if len(deleted) != 0 {
		t.Fatalf("delete ran without confirmation: %v", deleted)
	}
	if id := pickerConfirmID(t, app); id != "aaaa1111" {
		t.Fatalf("armed id = %q, want aaaa1111", id)
	}
	pressKey(app, tcell.KeyEsc)
	if !app.SessionPickerOpen() {
		t.Fatal("Esc during confirmation must not close the picker")
	}
	if id := pickerConfirmID(t, app); id != "" {
		t.Fatalf("Esc did not cancel the armed delete: %q", id)
	}
	pressKey(app, tcell.KeyBackspace)
	pressKey(app, tcell.KeyBackspace)
	if len(deleted) != 1 || deleted[0] != "aaaa1111" {
		t.Fatalf("deleted = %v, want [aaaa1111]", deleted)
	}
	if got := pickerRows(t, app); len(got) != 1 || got[0].ID != "bbbb2222" {
		t.Fatalf("rows after delete = %v, want only bbbb2222", got)
	}
}

// TestPickerEmptyFolderHint: a folder with no sessions shows the Tab hint
// (and Tab then reveals the other project's session).
func TestPickerEmptyFolderHint(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.OpenSessionPicker([]SessionPickerItem{{ID: "cccc3333", Title: "other project work", Mtime: "Jan 02 15:04", Size: "3 KB"}})
	if got := pickerRows(t, app); len(got) != 0 {
		t.Fatalf("current-folder scope should be empty, got %v", got)
	}
	app.draw()
	if !gridContains(scr, "No sessions in current folder. Press Tab to view all.") {
		t.Fatal("empty-folder hint not rendered")
	}
	pressKey(app, tcell.KeyTab)
	if got := pickerRows(t, app); len(got) != 1 {
		t.Fatalf("Tab did not reveal all-projects row: %v", got)
	}
	app.draw()
	if !gridContains(scr, "resume — all projects") {
		t.Fatal("all-projects scope label not rendered")
	}
}

// TestPickerEnterResumesSelection keeps the core contract honest while the
// extras land (existing behavior, now over the view slice).
func TestPickerEnterResumesSelection(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	var resumed []string
	app.SetPickerResume(func(id string) { resumed = append(resumed, id) })
	app.OpenSessionPicker(pickerTestItems())
	pressKey(app, tcell.KeyDown)
	pressKey(app, tcell.KeyEnter)
	if len(resumed) != 1 || resumed[0] != "bbbb2222" {
		t.Fatalf("resumed = %v, want [bbbb2222]", resumed)
	}
	if app.SessionPickerOpen() {
		t.Fatal("picker must close after Enter")
	}
}

// TestPickerDeleteNilSeamNoOp: an unwired delete seam must not arm a
// confirmation the user can never complete.
func TestPickerDeleteNilSeamNoOp(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.OpenSessionPicker(pickerTestItems())
	pressKey(app, tcell.KeyBackspace)
	if id := pickerConfirmID(t, app); id != "" {
		t.Fatalf("delete armed without a delete seam: %q", id)
	}
	pressRune(app, 'z') // still usable as search text
	if got := pickerRows(t, app); len(got) != 0 {
		t.Fatalf("query 'z' rows = %v, want none", got)
	}
}

// modelViews is the fixture the /model tests drive: a roles tab whose rows
// assign, plus a model catalog tab whose rows switch.
func modelViews(current string) []PickerView {
	return []PickerView{
		{
			Name: "Roles", Action: "set",
			Items: []PickerItem{
				{Label: "@default", Detail: "→ " + current, Value: "@default", Current: true},
				{Label: "@smol", Detail: "unset", Value: "@smol"},
			},
			OnSelect: func(string) {},
		},
		{
			Name: "All models", Action: "use",
			Items: []PickerItem{
				{Label: "onegw/free", Detail: "Free · 1M ctx", Value: "onegw/free", Section: "onegw", Current: true},
				{Label: "onegw/dev", Detail: "Dev · 1M ctx", Value: "onegw/dev", Section: "onegw"},
			},
		},
	}
}

func TestPickerFiltersAndSelects(t *testing.T) {
	p := newPicker(PickerOptions{Title: "model", Views: modelViews("onegw/free")})
	// Opening parks the selection on the live value, so the user sees where
	// they are instead of the head of the list.
	if it, ok := p.selected(); !ok || it.Value != "@default" {
		t.Fatalf("initial selection = %+v ok=%v, want @default", it, ok)
	}
	// Typing filters by label; on the models tab "dev" matches exactly one.
	p.switchView(1)
	p.typeFilter('d')
	p.typeFilter('e')
	p.typeFilter('v')
	if got := len(p.match); got != 1 {
		t.Fatalf("matches for 'dev' = %d, want 1", got)
	}
	if it, _ := p.selected(); it.Value != "onegw/dev" {
		t.Fatalf("filtered selection = %q, want onegw/dev", it.Value)
	}
	lines, _, selLine := p.window(10)
	for _, ln := range lines {
		if ln.header {
			t.Errorf("section header %q survived a filtered view", ln.text)
		}
	}
	if selLine < 0 {
		t.Fatal("selection not visible in the window")
	}
	// Backspace restores the full list.
	for range 3 {
		p.backspace()
	}
	if got := len(p.match); got != 2 {
		t.Fatalf("matches after clearing the filter = %d, want 2", got)
	}
}

func TestPickerSwitchViewResetsFilterAndParksOnCurrent(t *testing.T) {
	p := newPicker(PickerOptions{Title: "model", Views: modelViews("onegw/free")})
	p.typeFilter('d')
	p.switchView(1)
	if p.query != "" {
		t.Fatalf("view switch kept the filter %q", p.query)
	}
	if v := p.active(); v == nil || v.Name != "All models" {
		t.Fatalf("view = %+v, want All models", v)
	}
	if it, _ := p.selected(); it.Value != "onegw/free" {
		t.Fatalf("selection = %q, want the current model", it.Value)
	}
	// Views wrap.
	p.switchView(1)
	if v := p.active(); v == nil || v.Name != "Roles" {
		t.Fatalf("wrapped view = %+v, want Roles", v)
	}
}

func TestPickerMoveClamps(t *testing.T) {
	p := newPicker(PickerOptions{Title: "model", Views: modelViews("onegw/free")})
	p.move(-1)
	if p.sel != 0 {
		t.Fatalf("sel after move up at the head = %d, want 0", p.sel)
	}
	for range 10 {
		p.move(1)
	}
	if p.sel != len(p.match)-1 {
		t.Fatalf("sel = %d, want the last row %d", p.sel, len(p.match)-1)
	}
}

// The per-view Enter verb is what lets one picker assign on the roles tab
// and switch on a model tab without a second key fighting the filter.
func TestPickerChooseUsesViewOverride(t *testing.T) {
	assigned, used := "", ""
	views := modelViews("onegw/free")
	views[0].OnSelect = func(v string) { assigned = v }
	p := newPicker(PickerOptions{
		Title: "model", Views: views,
		OnSelect: func(v string) { used = v },
	})
	act, ok := p.choose()
	if !ok {
		t.Fatal("choose returned no action on the roles tab")
	}
	it, _ := p.selected()
	act(it.Value)
	if assigned != "@default" || used != "" {
		t.Fatalf("view override: assigned=%q used=%q", assigned, used)
	}
	// A view without its own callback falls back to the picker's.
	p.switchView(1)
	act, ok = p.choose()
	if !ok {
		t.Fatal("choose returned no action on the models tab")
	}
	it, _ = p.selected()
	act(it.Value)
	if used != "onegw/free" {
		t.Fatalf("fallback select = %q, want onegw/free", used)
	}
}

// The footer states two contracts: where you are (position) and what the two
// acting keys do. Enter takes the ACTIVE view's verb ("set" on Roles); the
// ⇥ hint names the tab Tab moves TO, not the one already bright — naming the
// active one made Tab look dead.
func TestPickerFooterNamesTheAction(t *testing.T) {
	p := newPicker(PickerOptions{Title: "model", Views: modelViews("onegw/free")})
	left, right := p.footer()
	if left != "1/2 items" {
		t.Errorf("footer left = %q, want the position", left)
	}
	if !contains(right, "⏎ set") || !contains(right, "⇥ All models") {
		t.Errorf("footer right = %q, want the active verb and the next tab", right)
	}
	p.switchView(1) // now on All models: the verb is "use", the hint wraps to Roles
	if _, right := p.footer(); !contains(right, "⏎ use") || !contains(right, "⇥ Roles") {
		t.Errorf("footer right after Tab = %q, want the next-tab hint", right)
	}
}

// /model with no argument opens the selector and never prints the old
// "available:" text listing; the roles tab's Enter opens the assign list,
// whose Enter hands the picked model to SetRole+Set (the cmd flow).
func TestSwitchModelOpensPicker(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	var switched []string
	views := func() []PickerView {
		vs := modelViews("onegw/free")
		vs[0].OnSelect = func(role string) { app.OpenRolePicker(strings.TrimPrefix(role, "@")) }
		return vs
	}
	app.SetModelOps(&ModelOps{
		Current: func() string { return "onegw/free" },
		Views:   views,
		Models: func() []PickerItem {
			return []PickerItem{{Label: "onegw/dev", Value: "onegw/dev"}}
		},
		Set:     func(ref string) error { switched = append(switched, ref); return nil },
		SetRole: func(role, ref string) error { return nil },
	})
	if err := app.SwitchModel(""); err != nil {
		t.Fatal(err)
	}
	if !app.PickerOpen() {
		t.Fatal("/model did not open the picker")
	}
	// Down to @smol, Enter -> the roles tab's OnSelect opens the assign list.
	app.handleKey(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	// Assigning a role also switches the session to it (the whole point of
	// picking a model for a slot is to use it now), so Set receives @smol.
	if len(switched) != 1 || switched[0] != "@smol" {
		t.Fatalf("switched = %v, want [@smol]", switched)
	}
	if app.PickerOpen() {
		t.Fatal("picker stayed open after a selection")
	}
}

// Esc pops one level (the assign list back to the roles view); Ctrl+C is
// swallowed by the picker instead of quitting the app.
func TestPickerEscPopsAndCtrlCDoesNotQuit(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	quit := false
	app.SetHandlers(func(string) {}, func() {}, func() { quit = true })
	app.OpenPicker(PickerOptions{Title: "one", Views: []PickerView{{Name: "v", Items: []PickerItem{{Label: "a", Value: "a"}}}}})
	app.OpenPicker(PickerOptions{Title: "two", Views: []PickerView{{Name: "v", Items: []PickerItem{{Label: "b", Value: "b"}}}}})
	app.handleKey(tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone))
	if n := len(app.pickers); n != 1 {
		t.Fatalf("picker stack = %d after Esc, want 1", n)
	}
	app.handleKey(tcell.NewEventKey(tcell.KeyCtrlC, 0, tcell.ModNone))
	if quit {
		t.Fatal("Ctrl+C quit the app while a picker was open")
	}
	if n := len(app.pickers); n != 0 {
		t.Fatalf("picker stack = %d after Ctrl+C, want 0", n)
	}
}

// A picker owns the keyboard: keys never leak into the composer behind it.
func TestPickerSwallowsEditorKeys(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	app.OpenPicker(PickerOptions{Title: "one", Views: []PickerView{{Name: "v", Items: []PickerItem{{Label: "abc", Value: "abc"}}}}})
	for _, r := range "hi" {
		app.handleKey(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
	}
	if got := app.ed.Text(); got != "" {
		t.Fatalf("editor captured %q while a picker was open", got)
	}
}

// Alt+M (omp's app.model.select) opens the same selector.
func TestModelSelectChord(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	app.SetModelOps(&ModelOps{
		Current: func() string { return "onegw/free" },
		Views:   func() []PickerView { return modelViews("onegw/free") },
		Set:     func(string) error { return nil },
	})
	app.handleKey(tcell.NewEventKey(tcell.KeyRune, 'm', tcell.ModAlt))
	if !app.PickerOpen() {
		t.Fatal("Alt+M did not open the model selector")
	}
}

// A failed switch surfaces in the transcript (the picker already closed).
func TestSwitchModelErrorIsReported(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	app.SetModelOps(&ModelOps{
		Current: func() string { return "onegw/free" },
		Views:   func() []PickerView { return modelViews("onegw/free") },
		Set:     func(string) error { return errors.New("nope") },
	})
	if err := app.SwitchModel("onegw/dev"); err == nil {
		t.Fatal("Set error must reach the command layer")
	}
	if err := app.SwitchModel(""); err != nil {
		t.Fatal(err)
	}
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone)) // Roles tab -> assign list needs Models
}

// SetRole persists through ModelOps and re-opens with the new value marked.
func TestOpenRolePickerAssigns(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	var gotRole, gotRef string
	app.SetModelOps(&ModelOps{
		Current: func() string { return "onegw/free" },
		Views:   func() []PickerView { return modelViews("onegw/free") },
		Models: func() []PickerItem {
			return []PickerItem{{Label: "onegw/dev", Value: "onegw/dev"}}
		},
		Set:     func(string) error { return nil },
		SetRole: func(role, ref string) error { gotRole, gotRef = role, ref; return nil },
	})
	app.OpenRolePicker("smol")
	if !app.PickerOpen() {
		t.Fatal("assign list did not open")
	}
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if gotRole != "smol" || gotRef != "onegw/dev" {
		t.Fatalf("SetRole(%q, %q), want smol/onegw/dev", gotRole, gotRef)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// --- open implies painted ---
//
// draw() never called drawPicker, so /model and /resume opened a modal that
// owned every key while painting nothing: the user pressed Enter, saw nothing
// happen, and the terminal looked dead. Every assertion above checked
// PickerOpen() — state, not screen — which is why the suite stayed green over
// a feature that was invisible in practice.

func TestPickerIsPaintedOnScreen(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	app.OpenPicker(PickerOptions{Title: "model", Views: modelViews("onegw/free")})
	app.draw()
	text := screenText(scr)
	for _, want := range []string{"model", "Roles", "onegw/free", "esc cancel"} {
		if !strings.Contains(text, want) {
			t.Fatalf("picker panel not painted: no %q on screen\n%s", want, text)
		}
	}
}

// A panel that cannot fit must close rather than stay open-and-invisible —
// the invariant 5b999f0 established for the tree selector, now enforced for
// the picker stack (narrow width, and a terminal too short to float above
// the composer).
func TestPickerWithNoRoomClosesInsteadOfOwningKeys(t *testing.T) {
	for _, size := range [][2]int{{20, 30}, {100, 6}} {
		app, _ := newTestApp(t, size[0], size[1])
		app.OpenPicker(PickerOptions{Title: "model", Views: modelViews("onegw/free")})
		app.draw()
		if app.PickerOpen() {
			t.Fatalf("%dx%d: picker stayed open with nothing painted", size[0], size[1])
		}
	}
}

// With no configured models the selector has nothing to show, so the reason
// is stated in the transcript instead of opening an empty panel.
func TestEmptyModelSelectorReportsInsteadOfOpening(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	app.SetModelOps(&ModelOps{
		Current: func() string { return "onegw/free" },
		Views:   func() []PickerView { return []PickerView{{Name: "All models", Action: "use"}} },
		Set:     func(string) error { return nil },
	})
	if err := app.SwitchModel(""); err != nil {
		t.Fatal(err)
	}
	if app.PickerOpen() {
		t.Fatal("picker opened over zero rows")
	}
	app.mu.Lock()
	last := app.blocks[len(app.blocks)-1]
	app.mu.Unlock()
	if last.Kind != KindSystem || !strings.Contains(last.Text, "no models to select") {
		t.Fatalf("last block = %+v, want the no-models notice", last)
	}
}

// TestPickerMouseWheelAndClick: the modal list answers the mouse the way omp's
// SelectList does — the wheel moves the selection one row per notch, and a single
// click on a row takes it and chooses it immediately (omp's clickItem calls
// onSelect itself). A click inside the panel must also not start a text
// selection on the transcript behind it.
func TestPickerMouseWheelAndClick(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.AddSystemBlock("transcript behind the panel")
	var chosen string
	app.OpenPicker(PickerOptions{
		Title: "model",
		Views: []PickerView{{Name: "roles", Items: []PickerItem{
			{Label: "one", Value: "v1"},
			{Label: "two", Value: "v2"},
			{Label: "three", Value: "v3"},
		}}},
		OnSelect: func(v string) { chosen = v },
	})
	app.draw()

	press := tcell.NewEventMouse(0, 0, tcell.WheelDown, tcell.ModNone)
	app.handleKey(press)
	app.draw()
	app.mu.Lock()
	sel := app.pickers[len(app.pickers)-1].sel
	app.mu.Unlock()
	if sel != 1 {
		t.Fatalf("wheel down left the selection at %d, want 1", sel)
	}

	// Click the last painted row of the list.
	app.mu.Lock()
	p := app.pickers[len(app.pickers)-1]
	row := -1
	for i, itemIdx := range p.hitItem {
		if itemIdx >= 0 {
			row = i
		}
	}
	if row < 0 {
		app.mu.Unlock()
		t.Fatal("the painter published no clickable rows")
	}
	want := p.item(p.hitItem[row]).Value
	y := p.hitY0 + row
	app.mu.Unlock()

	app.handleKey(tcell.NewEventMouse(6, y, tcell.Button1, tcell.ModNone))
	app.handleKey(tcell.NewEventMouse(6, y, tcell.ButtonNone, tcell.ModNone))
	if chosen != want {
		t.Fatalf("click chose %q, want %q", chosen, want)
	}
	if app.PickerOpen() {
		t.Fatal("the picker stayed open after a click chose a row")
	}
	app.mu.Lock()
	shown := app.selShown
	app.mu.Unlock()
	if shown {
		t.Fatal("the click also started a text selection behind the panel")
	}
}

// TestSessionPickerRowCarriesStatus: the lifecycle badge closes the row, so a
// session the user killed mid-turn is legible before Enter (#107).
func TestSessionPickerRowCarriesStatus(t *testing.T) {
	done := sessionPickerRowText(SessionPickerItem{
		ID: "aaaa1111", Title: "fix parser bug", Mtime: "Jan 02 15:04", Size: "1 KB", Status: "done",
	})
	if !strings.HasSuffix(done, "done") {
		t.Fatalf("row does not end on the status: %q", done)
	}
	cut := sessionPickerRowText(SessionPickerItem{
		ID: "bbbb2222", Title: "alpha parser notes", Mtime: "Jan 02 15:04", Size: "2 KB", Status: "interrupted",
	})
	if !strings.HasSuffix(cut, "interrupted") {
		t.Fatalf("row does not end on the status: %q", cut)
	}
	if !strings.Contains(cut, "interrupted") || strings.Contains(done, "interrupted") {
		t.Fatalf("statuses bled between rows: %q / %q", done, cut)
	}
	// An unclassified row (a caller that never read a tail) stays as it was.
	plain := sessionPickerRowText(SessionPickerItem{ID: "cccc3333", Title: "t", Mtime: "Jan 02 15:04", Size: "3 KB"})
	if want := "cccc3333  t  Jan 02 15:04  3 KB"; plain != want {
		t.Fatalf("unclassified row = %q, want %q", plain, want)
	}
}

// TestSessionPickerDrawsStatus: the badge reaches the screen, not just the row
// builder — the window/clip path must not eat the trailing column.
func TestSessionPickerDrawsStatus(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	app.OpenSessionPicker([]SessionPickerItem{
		{ID: "aaaa1111", Title: "fix parser bug", Mtime: "Jan 02 15:04", Size: "1 KB", InCwd: true, Status: "done"},
		{ID: "bbbb2222", Title: "alpha notes", Mtime: "Jan 02 15:04", Size: "2 KB", InCwd: true, Status: "interrupted"},
	})
	app.draw()
	if !gridContains(scr, "done") {
		t.Fatal("done badge not drawn")
	}
	if !gridContains(scr, "interrupted") {
		t.Fatal("interrupted badge not drawn")
	}
}

// TestResumeSessionPickerShowsStatus: no-arg /resume opens the generic picker
// over ResumeOption, so the lifecycle badge has to survive into Detail — the
// live surface, not just the session picker (#107).
func TestResumeSessionPickerShowsStatus(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	app.SetSessionOps(&SessionOps{
		Resume: func(string) error { return nil },
		Recent: func() []ResumeOption {
			return []ResumeOption{
				{ID: "aaaa1111", Title: "finished work", Detail: "aaaa1111 · Jan 02 15:04 · done"},
				{ID: "bbbb2222", Title: "killed work", Detail: "bbbb2222 · Jan 02 15:04 · interrupted"},
			}
		},
	})
	if err := app.ResumeSession(""); err != nil {
		t.Fatal(err)
	}
	if !app.PickerOpen() {
		t.Fatal("/resume with no argument must open the picker")
	}
	app.draw()
	for _, want := range []string{"done", "interrupted"} {
		if !gridContains(scr, want) {
			t.Fatalf("status %q not drawn on the resume picker", want)
		}
	}
}
