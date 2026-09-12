package tui

import (
	"testing"

	"github.com/gdamore/tcell/v2"
)

// Session picker extras (#26): scope toggle, token search (+ cmd search
// hook), pins, confirmed delete, empty-folder hint.

func pickerTestItems() []PickerItem {
	return []PickerItem{
		{ID: "aaaa1111", Title: "fix parser bug", Mtime: "Jan 02 15:04", Size: "1 KB", InCwd: true},
		{ID: "bbbb2222", Title: "alpha parser notes", Mtime: "Jan 02 15:04", Size: "2 KB", InCwd: true},
		{ID: "cccc3333", Title: "other project work", Mtime: "Jan 02 15:04", Size: "3 KB", InCwd: false},
	}
}

func pickerRows(t *testing.T, app *App) []PickerItem {
	t.Helper()
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.spick == nil {
		t.Fatal("picker is not open")
	}
	return append([]PickerItem(nil), app.spick.view...)
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
	app.SetPickerSearch(func(q string) []PickerItem {
		queries = append(queries, q)
		return []PickerItem{
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

// TestPickerIdleCapAndSearchCap: the idle view keeps the 12-row cap; an
// active query may grow it (search needs the wider pool).
func TestPickerIdleCapAndSearchCap(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	many := make([]PickerItem, 20)
	for i := range many {
		many[i] = PickerItem{ID: "idabcdef", Title: "sess task", InCwd: true}
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
	app.OpenSessionPicker([]PickerItem{{ID: "cccc3333", Title: "other project work", Mtime: "Jan 02 15:04", Size: "3 KB"}})
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
