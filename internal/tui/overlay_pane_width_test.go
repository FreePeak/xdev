package tui

// Every overlay that floats above the composer is a surface of the MAIN PANE,
// not of the terminal. The context panel is a window of its own: it owns the
// right-hand columns of every row, and a panel-width popup painted over its
// FILES list and put its own border under the panel's (the user's report, for
// the "/" dropdown and the /trajectory ledger — and true of every sibling).
//
// These tests open one overlay at a time with the panel pinned open and assert
// the pane's columns are still the PANEL's: its own rows are readable, and no
// overlay cell leaked across the boundary.

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// paneCleanAt asserts nothing the overlay painted survives in the panel's own
// columns: every cell there is still the panel's surface (its bg is the
// terminal default and it carries no box chrome). A dropdown whose fill or
// border ran into the panel fails it on the very first row it touched.
func paneCleanAt(t *testing.T, scr tcell.SimulationScreen, app *App, edge int, y int) {
	t.Helper()
	for x := edge; x < app.width; x++ {
		ch, _, st, _ := scr.GetContent(x, y)
		if _, bg, _ := st.Decompose(); bg != tcell.ColorDefault {
			t.Fatalf("overlay painted a background at x=%d y=%d, inside the panel", x, y)
		}
		if ch == '│' || ch == '─' || ch == '╭' || ch == '╮' || ch == '╰' || ch == '╯' {
			t.Fatalf("overlay painted box chrome %q at x=%d y=%d, inside the panel", ch, x, y)
		}
	}
}

// screenRow is one whole row of the screen, as text.
func screenRow(scr tcell.SimulationScreen, y int) string {
	var b strings.Builder
	w, _ := scr.Size()
	for x := 0; x < w; x++ {
		ch, _, _, _ := scr.GetContent(x, y)
		if ch == 0 {
			ch = ' '
		}
		b.WriteRune(ch)
	}
	return b.String()
}

// panelRowText is the panel's own row, read out of the columns it owns — the
// proof the panel is still painted and clickable rather than covered.
func panelRowText(t *testing.T, scr tcell.SimulationScreen, app *App, edge, y int) string {
	t.Helper()
	var b strings.Builder
	for x := edge; x < app.width; x++ {
		ch, _, _, _ := scr.GetContent(x, y)
		if ch == 0 {
			ch = ' '
		}
		b.WriteRune(ch)
	}
	return b.String()
}

// paneApp is a drawn session with the panel pinned open and a draft in the
// composer: the state every overlay is opened on top of.
func paneApp(t *testing.T) (*App, tcell.SimulationScreen, int) {
	t.Helper()
	app, scr := newTestApp(t, 160, 40)
	t.Cleanup(scr.Fini)
	app.SetDockMode(DockShow)
	app.SetDockOps(DockOps{
		Session: func() (string, string) { return "a long enough session name to fill the panel", "sess1234" },
		Tasks:   func() string { return "TASKS · 1/2 done\n[x] keep the overlay inside the pane" },
	})
	app.AddUserBlock("a prompt so the panel is on screen")
	app.AddUsage(1200, 340, 0, 0, 1540)
	app.draw()
	edge := app.width - dockCols
	if app.rightEdge() != edge {
		t.Fatalf("precondition: rightEdge %d, want the panel's edge %d", app.rightEdge(), edge)
	}
	if strings.TrimSpace(panelRowText(t, scr, app, edge, 0)) == "" {
		t.Fatal("precondition: the panel painted nothing in its own columns")
	}
	return app, scr, edge
}

// TestSlashDropdownStopsAtTheSidebar is the reported one: typing "/<command"
// opened a dropdown the width of the terminal, so it lay its rows across the
// sidebar and its border ran under the panel's. The popup is a surface of the
// pane — the pane it floats above and the composer it belongs to.
func TestSlashDropdownStopsAtTheSidebar(t *testing.T) {
	app, scr, edge := paneApp(t)

	// Typing "/" is what opens it, through the same sync the keystroke runs.
	setDraft(&app.ed, "/", 0)
	app.mu.Lock()
	app.syncSlashMenu()
	rows := app.smenu.rows()
	composerTop := app.height - 1 - app.composerRows()
	app.mu.Unlock()
	if len(rows) == 0 {
		t.Fatalf("typing %q opened no dropdown to measure", "/")
	}
	top := composerTop - len(rows) - 2
	app.draw() // the frame that menu opened: this is the one under test
	for y := top; y <= composerTop; y++ {
		paneCleanAt(t, scr, app, edge, y)
	}
	// And the popup is really on those rows — otherwise the loop above is
	// measuring a frame with nothing open.
	if got := screenRow(scr, top); strings.TrimSpace(got) == "" {
		t.Fatalf("the dropdown painted no rows: %q", got)
	}
	// And the panel is still the panel on those rows: its own title row and
	// FILES heading, not a blank field the popup wiped.
	if got := panelRowText(t, scr, app, edge, 0); !strings.Contains(got, "a long enough session name") {
		t.Fatalf("the panel's title row is %q with the dropdown open", got)
	}
}

// TestPickerStopsAtTheSidebar: /model and every other modal picker. Same
// rule, same window.
func TestPickerStopsAtTheSidebar(t *testing.T) {
	app, scr, edge := paneApp(t)
	app.OpenPicker(PickerOptions{
		Title: "model",
		Views: []PickerView{{Name: "all", Items: []PickerItem{
			{Label: "a-model-with-a-long-name", Value: "a-model-with-a-long-name", Detail: "the provider that serves it"},
			{Label: "another-model", Value: "another-model", Detail: "the other one"},
		}}},
	})
	app.draw()

	if !strings.Contains(screenText(scr), "a-model-with-a-long-name") {
		t.Fatalf("the picker painted nothing:\n%s", screenText(scr))
	}
	for y := range app.height {
		paneCleanAt(t, scr, app, edge, y)
	}
}

// TestSessionPickerStopsAtTheSidebar: the /resume list, whose box width is
// content-sized but whose ROWS are cleared to the terminal's edge.
func TestSessionPickerStopsAtTheSidebar(t *testing.T) {
	app, scr, edge := paneApp(t)
	app.OpenSessionPicker([]SessionPickerItem{
		{ID: "aaaa1111", Title: "a session with a title long enough to fill the box", InCwd: true},
		{ID: "bbbb2222", Title: "another session", InCwd: true},
	})
	app.draw()

	painted := screenText(scr)
	if !strings.Contains(painted, "a session with a title") {
		t.Fatalf("the resume picker painted nothing:\n%s", painted)
	}
	for y := range app.height {
		paneCleanAt(t, scr, app, edge, y)
	}
}

// TestTrajectoryStopsAtTheSidebar: the /trajectory ledger, whose rows are
// filled across the whole terminal width and whose border the panel's own
// columns cut in half.
func TestTrajectoryStopsAtTheSidebar(t *testing.T) {
	app, scr, edge := paneApp(t)
	app.SetTrajectoryOps(&TrajectoryOps{
		Records: func() []TrajectoryRecord {
			return []TrajectoryRecord{
				{Index: 1, Kind: "user", Text: "make a file", Detail: "make a file\ncreate foo.txt", Meta: "0 tokens · 1.2s"},
				{Index: 2, Kind: "tool", Text: "edit foo.txt", Detail: "--- a/foo.txt\n+++ b/foo.txt\n+hello", Meta: "12 tokens · 0.4s · exit 0"},
			}
		},
	})
	if !app.OpenTrajectory() {
		t.Fatal("OpenTrajectory returned false with data wired")
	}
	app.draw()

	painted := screenText(scr)
	if !strings.Contains(painted, "TRAJECTORY") || !strings.Contains(painted, "make a file") {
		t.Fatalf("the ledger painted nothing:\n%s", painted)
	}
	for y := range app.height {
		paneCleanAt(t, scr, app, edge, y)
	}
	// The inspector is the wider of the two panes: Enter opens it, and the
	// record's detail must stay inside the pane too.
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	app.draw()
	if !strings.Contains(screenText(scr), "TRAJECTORY — record") {
		t.Fatalf("the ledger did not switch to its inspector:\n%s", screenText(scr))
	}
	for y := range app.height {
		paneCleanAt(t, scr, app, edge, y)
	}
}

// TestHubRosterStopsAtTheSidebar: /hub, the third overlay with a hand-rolled
// full-width fill.
func TestHubRosterStopsAtTheSidebar(t *testing.T) {
	app, scr, edge := paneApp(t)
	app.SetHubOps(&HubOps{Roster: func() []HubAgent {
		return []HubAgent{
			{ID: "agent-1", Status: "running", Model: "a-model", Name: "the first background agent", Activity: "reading a file"},
			{ID: "agent-2", Status: "done", Model: "a-model", Name: "the second background agent", Activity: "writing one"},
		}
	}})
	if err := app.HubRoster(); err != nil {
		t.Fatalf("HubRoster: %v", err)
	}
	app.draw()

	painted := screenText(scr)
	if !strings.Contains(painted, "AGENT HUB") || !strings.Contains(painted, "agent-1") {
		t.Fatalf("the roster painted nothing:\n%s", painted)
	}
	for y := range app.height {
		paneCleanAt(t, scr, app, edge, y)
	}
}

// TestTreeSelectorStopsAtTheSidebar: the session tree, whose row fills are
// cleared to the terminal's edge.
func TestTreeSelectorStopsAtTheSidebar(t *testing.T) {
	app, scr, edge := paneApp(t)
	app.SetTreeData(func() []TreeEntry {
		return []TreeEntry{
			{ID: "aaaa1111bbbb2222", Type: "message", Role: "user", Active: true},
			{ID: "cccc3333dddd4444", Type: "message", Role: "assistant"},
		}
	})
	app.OpenTreeSelector()
	app.draw()

	if !strings.Contains(screenText(scr), "session tree") {
		t.Fatalf("the tree painted nothing:\n%s", screenText(scr))
	}
	for y := range app.height {
		paneCleanAt(t, scr, app, edge, y)
	}
}

// TestSettingsOverlayStopsAtTheSidebar: the overlay is centred, and centred
// on the terminal it sat half under the panel.
func TestSettingsOverlayStopsAtTheSidebar(t *testing.T) {
	app, scr, edge := paneApp(t)
	app.SetSettingsOverlayOps(&SettingsOverlayOps{
		Path: "/tmp/x.yml",
		Read: func() []SettingsRow {
			return []SettingsRow{
				{Key: "showThinking", Label: "Show thinking", Value: "true", Editable: true, Kind: "toggle"},
				{Key: "theme", Label: "Theme", Value: "groknight", Editable: false, Kind: "text"},
			}
		},
		Write: func(string, string) error { return nil },
	})
	if !app.OpenSettingsOverlay() {
		t.Fatal("OpenSettingsOverlay returned false with rows wired")
	}
	app.draw()

	if !strings.Contains(screenText(scr), "SETTINGS") {
		t.Fatalf("the settings overlay painted nothing:\n%s", screenText(scr))
	}
	for y := range app.height {
		paneCleanAt(t, scr, app, edge, y)
	}
}

// TestMsgViewStopsAtTheSidebar: the read-only surface "jump" opens, plus the
// click menu beside it — both anchored to a transcript row, so both are the
// pane's surfaces.
func TestMsgViewStopsAtTheSidebar(t *testing.T) {
	app, scr, edge := paneApp(t)
	app.AddUserBlock("a message to open read-only")
	app.draw()

	app.mu.Lock()
	app.msgv = &msgView{ord: 0, blocks: 1}
	app.msgm = &msgMenu{ax: 10, ay: 6, sel: 0}
	app.mu.Unlock()
	app.draw()

	painted := screenText(scr)
	if !strings.Contains(painted, "message 1 of") {
		t.Fatalf("the read-only surface painted nothing:\n%s", painted)
	}
	for y := range app.height {
		paneCleanAt(t, scr, app, edge, y)
	}
}

// TestStatusPopupStopsAtTheSidebar: the pill's detail panel opens UP from the
// status row, whose own metrics already right-align to the pane's edge — a
// terminal-wide panel put its rows in the panel's columns.
func TestStatusPopupStopsAtTheSidebar(t *testing.T) {
	app, scr, edge := paneApp(t)
	app.AddUsage(1200, 340, 0, 0, 1540)
	app.draw()

	// The anchor is the pill's own cell, so the panel opens inside the pane.
	anchor := edge - 1
	app.mu.Lock()
	app.statusPop = &statusPopup{pill: "tokens", ax: anchor, ay: 39}
	app.mu.Unlock()
	app.draw()

	painted := screenText(scr)
	if !strings.Contains(painted, "cache") {
		t.Fatalf("the status panel painted nothing:\n%s", painted)
	}
	for y := range app.height {
		paneCleanAt(t, scr, app, edge, y)
	}
}

// TestDiffViewerStopsAtTheSidebar is the one with a second reason to be here:
// its panel is opened FROM the panel's changed-file rows, and those rows are
// how a reader points the viewer at another file. A terminal-wide viewer hid
// the very control it is driven by.
func TestDiffViewerStopsAtTheSidebar(t *testing.T) {
	app, scr, edge := paneApp(t)
	app.mu.Lock()
	app.dock = &dockState{mode: DockShow}
	app.dock.ops = DockOps{Session: func() (string, string) { return "s", "sess" }}
	app.blocks = append(app.blocks, &Block{
		Kind: KindToolDone, ToolName: "edit",
		Diff: "--- a/internal/tui/dock.go\n+++ b/internal/tui/dock.go\n@@ -1,2 +1,2 @@\n-\treturn oldValue\n+\treturn newValue\n",
	})
	app.DockBump()
	app.dockBuild()
	opened := app.openDiffOverlay("internal/tui/dock.go")
	inner := app.diffOverlayInner()
	app.mu.Unlock()
	if !opened {
		t.Fatal("the viewer did not open for the panel's changed file")
	}
	if want := app.rightEdge() - 2*diffPad - 3; inner != want {
		t.Fatalf("the viewer's interior = %d, want the pane's %d", inner, want)
	}
	app.draw()

	painted := screenText(scr)
	if !strings.Contains(painted, "return newValue") {
		t.Fatalf("the viewer painted no diff:\n%s", painted)
	}
	for y := range app.height {
		paneCleanAt(t, scr, app, edge, y)
	}
	// The panel's FILES row survives beside it: that is the control the
	// viewer is pointed with, and covering it is the defect.
	rows := strings.Split(painted, "\n")
	for _, row := range rows {
		if strings.Contains(row, "internal/tui/dock.go") && !strings.Contains(row, "diff ") {
			return // the panel's own changed-file row is still on screen
		}
	}
	t.Fatalf("the panel's changed-file row is gone with the viewer up:\n%s", painted)
}

// TestOverlaysDoNotChangeWithThePanelClosed is the other half of the rule:
// rightEdge IS the terminal when the panel is shut, so an overlay paints
// exactly where it did before this change. The dropdown is the witness — its
// border width is min(w-4, nameW+44), so any substitution that costs a column
// with the panel shut shows up as a shifted corner.
func TestOverlaysDoNotChangeWithThePanelClosed(t *testing.T) {
	app, scr := newTestApp(t, 160, 40)
	t.Cleanup(scr.Fini)
	app.SetDockMode(DockHide)
	app.AddUserBlock("a prompt")
	setDraft(&app.ed, "/", 0)
	app.mu.Lock()
	app.syncSlashMenu()
	app.mu.Unlock()
	app.draw()

	if app.rightEdge() != app.width {
		t.Fatalf("rightEdge %d with the panel closed, want the terminal's %d", app.rightEdge(), app.width)
	}
	rows := app.smenu.rows()
	composerTop := app.height - 1 - app.composerRows()
	top := composerTop - len(rows) - 2
	// The border is content-sized: the aligned name column plus 44 cells of
	// description, capped at the pane's own width. Both numbers come from the
	// menu, so this pins the width against the rule rather than a constant.
	nameW := 0
	for _, r := range rows {
		nameW = max(nameW, width(r.Name)+width(r.Tag))
	}
	nameW = min(nameW+2, 42)
	want := screenRow(scr, top)
	if border := strings.Count(want, "─"); border != min(app.width-4, nameW+44) {
		t.Fatalf("the closed-panel dropdown's top border is %d cells, want %d: %q",
			border, min(app.width-4, nameW+44), want)
	}
	// Open the panel and close it again: the same frame must come back. A
	// dropdown that only measured a.width would have kept its width, and a
	// row's [tag] column (drawn at w-6) is where that shows.
	app.SetDockMode(DockShow)
	app.SetDockOps(DockOps{Session: func() (string, string) { return "s", "sess" }})
	app.draw()
	if edge := app.width - dockCols; app.rightEdge() != edge {
		t.Fatalf("rightEdge %d with the panel open, want %d", app.rightEdge(), edge)
	}
	app.SetDockMode(DockHide)
	app.draw()
	if got := screenRow(scr, top); got != want {
		t.Fatalf("the dropdown is not where it was with the panel shut:\n%q\nvs\n%q", got, want)
	}
}
