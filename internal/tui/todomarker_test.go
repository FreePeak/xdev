package tui

// Tests for the todo markers' inks (todomarker.go). Every fixture is invented:
// a synthesized phased list. No session's own tasks are quoted, because a
// fixture that quotes one carries that session's content into this repo.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/theme"
	"github.com/FreePeak/xdev/internal/tool"
)

// todoFixture is a task list in the exact shape tool.TodoTool.View and the todo
// tool's own result both print: a heading, an optional phase name, one row per
// task.
var todoFixture = strings.Join([]string{
	"TASKS · 2/4 done, 1 blocked",
	"[x] wire the registry",
	"[/] write the tests",
	"[!] publish the release (blocked: waiting on review)",
	"[ ] update the docs",
}, "\n")

// todoFixtureRows is the fixture's task rows, trimmed — what the panel paints
// (clipped to its own interior, so the comparison is by prefix).
func todoFixtureRows() []string {
	var out []string
	for _, row := range strings.Split(todoFixture, "\n") {
		if _, _, ok := todoRowInk(row); ok {
			out = append(out, strings.TrimRight(row, " "))
		}
	}
	return out
}

// The marker table is the whole feature: five glyphs, every one a slot the theme
// contract already defines, and no two non-pending states sharing an ink (two
// states painted the same colour is a claim they are the same thing).
//
// `[ ]` is the deliberate exception: pending is the list's resting state and it
// wears tokPlain — the row's own ink — which is still a SPLIT (the brackets stay
// their own run) but no colour claim.
func TestTodoMarkerTableIsComplete(t *testing.T) {
	if len(todoMarkerInk) != 5 {
		t.Fatalf("the table has %d markers, want the tool's five", len(todoMarkerInk))
	}
	seen := map[tokKind]string{}
	for marker, k := range todoMarkerInk {
		if len(marker) != todoMarkerLen || marker[0] != '[' || marker[2] != ']' {
			t.Fatalf("marker %q is not the three-cell shape", marker)
		}
		if k == tokPlain {
			if marker != "[ ]" {
				t.Fatalf("only a pending marker may wear the row's own ink, got %q", marker)
			}
			continue
		}
		if slot := syntaxSlots[k]; slot == "" {
			t.Fatalf("marker %q maps to a token kind with no theme slot", marker)
		}
		if prev, dup := seen[k]; dup {
			t.Fatalf("markers %q and %q wear the same ink, so the list cannot be read", prev, marker)
		}
		seen[k] = marker
	}
}

// A marker is claimed only where it is a marker: three cells then a space. A row
// that merely opens with the same characters is prose.
func TestTodoRowInkNeedsTheWholeMarker(t *testing.T) {
	cases := []struct {
		row  string
		want bool
	}{
		{"[x] done", true},
		{"[/] working", true},
		{"[!] stuck", true},
		{"[-] dropped", true},
		{"[ ] pending", true},
		{"[x]done", false},      // no space after the marker
		{"[z] invented", false}, // not one of the five
		{"[x]", false},          // the marker alone is not a row
		{"[ ]", false},
		{"TASKS · 2/4 done", false},
		{"- [x] a markdown box", false},
		{"", false},
		{"wire the registry", false},
	}
	for _, c := range cases {
		if _, _, ok := todoRowInk(c.row); ok != c.want {
			t.Errorf("todoRowInk(%q) = %v, want %v", c.row, ok, c.want)
		}
	}
}

// The row painter splits the marker from its text, and never changes a byte. A
// marker with no ink of its own ([ ]) still SPLITS: the brackets are a marker,
// not the first three characters of the task's text.
func TestTodoRowSplitsTheMarkerAndKeepsEveryByte(t *testing.T) {
	app, _ := newTestApp(t, 100, 40)
	cs := app.mdStyle().code
	body := app.mdStyle().body
	for row, k := range todoMarkerInk {
		src := row + " a task"
		ln := app.todoRow(src, cs, body, app.mdStyle().muted)
		if got := runsString(ln.runs); got != src {
			t.Fatalf("todoRow(%q) changed bytes to %q", src, got)
		}
		if len(ln.runs) != 2 || ln.runs[0].text != row {
			t.Fatalf("todoRow(%q) = %+v, want the marker as its own run", src, ln.runs)
		}
		if want := cs.styleFor(k); ln.runs[0].style != want {
			t.Fatalf("the marker of %q wore %v, want its slot's ink %v", src, ln.runs[0].style, want)
		}
		if ln.runs[1].style != body {
			t.Fatalf("the text of %q wore %v, want the body ink", src, ln.runs[1].style)
		}
	}
}

// A row with no marker keeps the body ink whole, so the heading and the phase
// names are not mistaken for tasks.
func TestTodoRowWithoutAMarkerStaysFlat(t *testing.T) {
	app, _ := newTestApp(t, 100, 40)
	cs := app.mdStyle().code
	body := app.mdStyle().body
	for _, src := range []string{"TASKS · 2/4 done", "Foundation", "", "nothing to do"} {
		ln := app.todoRow(src, cs, body, app.mdStyle().muted)
		if got := runsString(ln.runs); got != src {
			t.Fatalf("todoRow(%q) changed bytes to %q", src, got)
		}
		if len(ln.runs) != 1 || ln.runs[0].style != body {
			t.Fatalf("todoRow(%q) = %+v, want one body-ink run", src, ln.runs)
		}
	}
}

// The detector: a marker row is decisive, and a result without one is not a task
// list — including the tool's own error text and a markdown checklist.
func TestLooksLikeTodoNeedsAMarkerRow(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"a list", todoFixture, true},
		{"one row", "[ ] just one", true},
		{"a phased list", "TASKS · 0/2 done\nFoundation\n[x] a\n[ ] b", true},
		{"an error", `todo: Task "x" not found (todo list is empty — was it replaced or not yet created?)`, false},
		{"a server log", "2026/01/01 10:00:00 listening on :8080\n  - ready", false},
		{"a markdown checklist", "- [x] a task\n- [ ] another", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		if got := LooksLikeTodo(c.text); got != c.want {
			t.Errorf("%s: LooksLikeTodo = %v, want %v", c.name, got, c.want)
		}
	}
}

// A markdown checklist is a bullet with a checkbox in its text — the marker is
// not at the row's start — so the markdown arm keeps it. This is the one
// near-miss a reader would expect to be coloured, and it is why the shape test
// demands the marker FIRST.
func TestMarkdownChecklistIsNotTheTodoArm(t *testing.T) {
	app, _ := newTestApp(t, 100, 40)
	app.AddToolBlock("c1", "task", `{"prompt":"review"}`)
	app.FinishTool("c1", "task", false, "- [x] a task\n- [ ] another", ToolOutcome{})
	if hl := app.toolOutputHL(app.blocks[len(app.blocks)-1]); hl.todo {
		t.Fatalf("a markdown checklist was claimed by the todo arm: %+v", hl)
	}
}

// The todo tool's own result box: the marker is its own run in its own ink and
// the text beside it keeps the body ink.
func TestTodoToolResultPaintsItsMarkers(t *testing.T) {
	app, _ := newTestApp(t, 100, 40)
	app.AddToolBlock("c1", "todo", `{"op":"init","items":["wire the registry","write the tests"]}`)
	app.FinishTool("c1", "todo", false, todoFixture, ToolOutcome{})

	if hl := app.toolOutputHL(app.blocks[len(app.blocks)-1]); !hl.todo {
		t.Fatalf("a task list was not recognised: %+v", hl)
	}
	rows, text := joinBox(t, app, app.blocks[len(app.blocks)-1], 96)
	for _, want := range strings.Split(todoFixture, "\n") {
		if !strings.Contains(text, want) {
			t.Fatalf("the list changed bytes, %q is missing:\n%s", want, text)
		}
	}
	cs := app.mdStyle().code
	body := app.mdStyle().body
	inked, coloured := 0, 0
	for _, ln := range bodyRows(rows) {
		row := runsString(ln.runs)
		marker, _, _, ok := todoMarkerAt(row)
		if !ok {
			continue
		}
		inked++
		if len(ln.runs) != 2 || ln.runs[0].text != marker {
			t.Fatalf("row %q did not split its marker: %v", row, ln.runs)
		}
		if want := cs.styleFor(todoMarkerInk[marker]); ln.runs[0].style != want {
			t.Fatalf("marker %q wore %v, want its slot's ink %v", marker, ln.runs[0].style, want)
		}
		if ln.runs[1].style != body {
			t.Fatalf("row %q: the text beside the marker wore %v, want the body ink", row, ln.runs[1].style)
		}
		if ln.runs[0].style != body {
			coloured++
		}
	}
	if inked != 4 {
		t.Fatalf("expected four task rows, saw %d:\n%s", inked, text)
	}
	if coloured < 3 {
		t.Fatalf("only %d markers carried a colour of their own; the list is not being inked", coloured)
	}
}

// A resumed session replays the same list, and the arm is a shape test rather
// than a tool-name test, so it is covered: a block whose tool name is something
// else entirely still paints.
func TestTodoArmIsAShapeNotAToolName(t *testing.T) {
	app, _ := newTestApp(t, 100, 40)
	app.AddToolBlock("c1", "bash", `{"command":"plan show"}`)
	app.FinishTool("c1", "bash", false, todoFixture, ToolOutcome{})
	// The bash arms must not steal it: a task list is not shell and not a git
	// listing, so the order of the decision table is what keeps this honest.
	hl := app.toolOutputHL(app.blocks[len(app.blocks)-1])
	if !hl.todo || hl.git || hl.lang != "" {
		t.Fatalf("a replayed list resolved to %+v, want the todo arm alone", hl)
	}
}

// The dock's TASKS rows wear the same table: the marker's ink at the marker's
// cells, the panel's ordinary row ink after it.
//
// The marker's cells are read back off the GRID rather than from a run list:
// the panel paints cell by cell through drawText, so the SGR at the marker's
// column is the only evidence that the dock resolved the ink at all.
func TestDockTodoMarkersWearTheirOwnInks(t *testing.T) {
	app, scr, _ := dockTestApp(t, 200, 40)
	app.SetDockOps(DockOps{
		Session: func() (string, string) { return "todo inks", "sess1234" },
		Tasks:   func() string { return todoFixture },
	})
	app.SetDockMode(DockShow)
	app.draw()

	left := 200 - dockCols + dockPad
	app.mu.Lock()
	top, _ := app.dockGrid()
	head := app.dock.titleRows()
	app.mu.Unlock()

	// The expectation is built the way the painter builds it — the panel's own
	// row ink, and its own resolution (Slot, not Get) — so the test measures
	// that the marker's cell carries the SLOT's colour rather than the row's.
	rowInk := app.cellColor(app.th.Get(theme.TextPrimary))
	rows := todoFixtureRows()
	if len(rows) != 4 {
		t.Fatalf("the fixture must carry four task rows, got %d", len(rows))
	}
	// The panel clips each row to its own interior (dockClip, which marks the
	// cut with an ellipsis), so a row is matched by PREFIX — and the ellipsis
	// the panel appended is dropped first, because it is chrome the fixture does
	// not carry.
	found := map[string]bool{}
	for i := range 12 {
		y := top + head + i
		text := strings.TrimSuffix(dockRowText(scr, y, left), "…")
		if text == "" {
			continue
		}
		var match string
		for _, want := range rows {
			if strings.HasPrefix(want, text) || strings.HasPrefix(text, want) {
				match = want
				break
			}
		}
		if match == "" {
			continue
		}
		found[match] = true
		marker := match[:todoMarkerLen]
		kind, _, ok := todoRowInk(match)
		if !ok {
			t.Fatalf("row %q is in the want list but has no marker kind", match)
		}
		wantFg := rowInk
		if c, ok := app.th.Slot(syntaxSlots[kind]); ok {
			wantFg = app.cellColor(c)
		}

		markerX := left + strings.Index(text, "[")
		got, _, _ := cellStyle(scr, markerX, y).Decompose()
		if got != wantFg {
			t.Fatalf("row %q: marker %q ink %v, want %v (textX check follows)", text, marker, got, wantFg)
		}
		// The text beside the marker keeps the row ink, whatever the marker
		// wears: the row reads as a coloured marker on a plain row.
		textX := markerX + todoMarkerLen + 1
		if got, _, _ := cellStyle(scr, textX, y).Decompose(); got != rowInk {
			t.Fatalf("row %q: the text beside the marker wore %v, want the row ink %v", text, got, rowInk)
		}
	}
	if len(found) != 4 {
		t.Fatalf("the panel painted %d of the fixture's 4 task rows: %v", len(found), found)
	}
}

// The glyphs are the tool's own, pinned through the tool rather than by copying
// its source: a rename inside internal/tool that this table does not follow
// fails here instead of painting a task list flat in silence.
func TestTodoMarkersMatchTheToolsOwnGlyphs(t *testing.T) {
	tt := tool.NewTodoTool()
	run := func(args map[string]any) string {
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		res, err := tt.Execute(context.Background(), raw)
		if err != nil {
			t.Fatalf("todo %v: %v", args["op"], err)
		}
		if res.IsError {
			t.Fatalf("todo %v: %s", args["op"], res.Text)
		}
		return res.Text
	}
	// Five tasks, then three terminal states. Auto-promotion always hands the
	// in-progress marker to the next PENDING task, so the list keeps one "[ ]"
	// row only while a task is still waiting behind the live one — the fifth is
	// what pins "[ ]", and the assertion below fails rather than passing on a
	// partial pin when it does not turn up.
	run(map[string]any{"op": "init", "items": []string{"a task", "another task", "a third", "a fourth", "a fifth"}})
	run(map[string]any{"op": "done", "task": "a task"})
	run(map[string]any{"op": "block", "task": "another task", "reason": "waiting on review"})
	run(map[string]any{"op": "drop", "task": "a third"})

	// The dock's view is the tool's own rendering of the same state, so every
	// row the VIEW prints must be one this table can parse, and all five markers
	// must turn up or the pin is partial.
	seen := map[string]bool{}
	for i, row := range strings.Split(tt.View(), "\n") {
		if i == 0 {
			continue // the heading, by the dock's contract
		}
		marker, _, _, ok := todoMarkerAt(row)
		if !ok {
			t.Fatalf("the view printed a row this table cannot parse as a task: %q", row)
		}
		seen[marker] = true
	}
	for _, marker := range []string{"[x]", "[/]", "[!]", "[-]", "[ ]"} {
		if !seen[marker] {
			t.Fatalf("the tool printed no %q row, so the table is not pinned against it: %v", marker, seen)
		}
	}
}

// NO_COLOR is the one global off switch, and this arm keeps it the same way
// every other arm does: the palette answers the body ink, so the whole list
// paints as one flat run.
func TestNoColorPaintsTheTodoListFlat(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	app, _ := newTestApp(t, 100, 40)
	app.AddToolBlock("c1", "todo", `{"op":"view"}`)
	app.FinishTool("c1", "todo", false, todoFixture, ToolOutcome{})

	rows := app.toolBoxLines(len(app.blocks)-1, app.blocks[len(app.blocks)-1], 96)
	for _, ln := range bodyRows(rows) {
		for _, r := range ln.runs {
			if r.style != app.mdStyle().body {
				t.Fatalf("NO_COLOR painted a coloured run %+v", r)
			}
		}
	}
}

// The sample bound: a huge non-list result costs at most todoSampleRows rows of
// shape tests.
func TestLooksLikeTodoScanIsBounded(t *testing.T) {
	var b strings.Builder
	for range 200_000 {
		b.WriteString("2026/01/01 10:00:00 an ordinary log line\n")
	}
	if LooksLikeTodo(b.String()) {
		t.Fatal("an ordinary log was claimed by the todo arm")
	}
}

// The flat fallback is structural: every ink comes from the palette's own
// styleFor, so a theme that pins no syntax_* slot degrades the whole list to the
// body ink without a second check. This asserts the mechanism rather than the
// result — a disabled palette answers tokPlain for every marker kind — and that
// the fallback is about INK, never about layout.
func TestTodoInksDegradeWithThePalette(t *testing.T) {
	var cs codeStyle
	for marker, k := range todoMarkerInk {
		if got, want := cs.styleFor(k), cs.styleFor(tokPlain); got != want {
			t.Fatalf("marker %q chose an ink from a disabled palette", marker)
		}
	}
	app, _ := newTestApp(t, 100, 40)
	ln := app.todoRow("[x] a task", cs, app.mdStyle().body, app.mdStyle().muted)
	if got := runsString(ln.runs); got != "[x] a task" {
		t.Fatalf("the fallback changed bytes to %q", got)
	}
	if len(ln.runs) != 2 {
		t.Fatalf("the fallback must still split the marker: %+v", ln.runs)
	}
}
