package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

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

func TestPickerFooterNamesTheAction(t *testing.T) {
	p := newPicker(PickerOptions{Title: "model", Views: modelViews("onegw/free")})
	left, right := p.footer()
	if left != "1/2 items" {
		t.Errorf("footer left = %q, want the position", left)
	}
	if !contains(right, "⏎ set") || !contains(right, "⇥ Roles") {
		t.Errorf("footer right = %q, want the view's verb and tab hint", right)
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
