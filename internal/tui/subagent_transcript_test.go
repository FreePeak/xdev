package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// subAgentOps wires a hub whose transcript serves a foreground child's id as
// well as the roster's job, which is the shape cmd builds when a hub tracks
// both. The ops carry the ids they answer for, so a test can name the child it
// planted without duplicating the fixture's own table.
func subAgentOps(ids ...string) *HubOps {
	return &HubOps{
		Roster: func() []HubAgent { return rosterRows() },
		Transcript: func(id string, fromSeq int) ([]HubTranscriptLine, int, bool) {
			for _, want := range ids {
				if id == want {
					return []HubTranscriptLine{
						{Role: "user", Text: "map the app shell"},
						{Role: "assistant", Text: "reading src/boot.tsx"},
						{Role: "toolResult", Text: "src/hooks/useChat.ts"},
					}, 3, true
				}
			}
			return nil, 0, false
		},
	}
}

// subRowScreenY is the screen row the child row with the given label painted
// on, found by asking the app for every transcript row's child.
func subRowScreenY(t *testing.T, app *App, label string) int {
	t.Helper()
	app.mu.Lock()
	defer app.mu.Unlock()
	_, vp := app.selViewport()
	hdr := app.transcriptTop()
	for y := hdr; y < hdr+vp; y++ {
		if id, got := app.subChildAt(y); id != "" && got == label {
			return y
		}
	}
	t.Fatalf("no child row named %q is on screen", label)
	return -1
}

// subTranscriptApp is a session with one running `task` call whose child the
// host tracked, drawn so the child's row has a screen row to be clicked on.
func subTranscriptApp(t *testing.T) *App {
	t.Helper()
	app, _ := subTranscriptScreen(t)
	return app
}

// subTranscriptScreen is subTranscriptApp with the screen, for a test that has
// to read what was painted.
func subTranscriptScreen(t *testing.T) (*App, tcell.SimulationScreen) {
	t.Helper()
	app, scr := newTestApp(t, 100, 30)
	app.AddToolBlock("c1", "task", `{"name":"scout","prompt":"map the app shell"}`)
	app.AddTaskChild("c1", "scout", "scout", "free")
	app.SetTaskChildTranscript("c1", "scout", "fg-7")
	app.UpdateTaskChild("c1", "scout", "read", `{"path":"src/boot.tsx"}`, "ok")
	app.draw()
	return app, scr
}

// The headline: a click on a subagent's row opens THAT child's own transcript
// in the panel — not the roster, not the parent transcript — and the panel's
// title names the child. Without this the live child rows are narration the
// user can read but cannot open, which is what a `task` call always was.
func TestSubagentRowClickOpensThatChildTranscript(t *testing.T) {
	app := subTranscriptApp(t)
	app.SetHubOps(subAgentOps("fg-7"))

	y := subRowScreenY(t, app, "scout")
	app.mu.Lock()
	press(app, 6, y)
	release(app, 6, y)
	app.mu.Unlock()

	ui := hubUI(app)
	if !app.HubRosterOpen() {
		t.Fatal("the click did not open the agent panel")
	}
	if ui.viewID != "fg-7" {
		t.Fatalf("opened view = %q, want the clicked child's fg-7", ui.viewID)
	}
	if ui.viewName != "scout" || !strings.Contains(ui.viewTitle(), "scout") {
		t.Fatalf("view name = %q title = %q, want the child's own label", ui.viewName, ui.viewTitle())
	}
	// Its own rows, not the roster job's.
	if len(ui.view) != 3 || !strings.Contains(ui.view[0].Text, "map the app shell") {
		t.Fatalf("view rows = %+v, want the child's transcript", ui.view)
	}
}

// The pty drive (scripts/tui-subagent-transcript-drive.py) found this one: a
// child transcript opened under the panel's EMPTY-ROSTER panel ("no background
// agents yet"), because the panel chose that branch on a roster list that was
// still empty — the click had just tracked the child, and nothing refilled the
// list before the next frame. A view with rows hidden behind a panel that says
// nothing is running reads exactly like the app ignoring the click.
//
// The offset is the other half of the same drive: the hit recorded the child's
// row as offset 0 — the CALL row — so in any transcript with a row above the
// child, the click resolved to the call. Both passed a simulation-screen test
// whose call row happened to be the first row on screen.
func TestSubagentHitIsTheChildRowNotTheCallRow(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	app.AddToolBlock("c1", "task", `{"name":"scout","prompt":"map the app shell"}`)
	app.AddTaskChild("c1", "scout", "", "free")
	app.SetTaskChildTranscript("c1", "scout", "fg-7")
	app.draw()

	app.mu.Lock()
	hits := app.rowIdx.rend[0].subs
	app.mu.Unlock()
	if len(hits) != 1 {
		t.Fatalf("subs = %+v, want one child hit", hits)
	}
	// The child is the block's SECOND row: row 0 is the `task` call row.
	if hits[0].offset != 1 {
		t.Fatalf("child hit offset = %d, want 1 (the row after the call row)", hits[0].offset)
	}
	// And the click must land on the CHILD's screen row, not the call's.
	y := subRowScreenY(t, app, "scout")
	app.mu.Lock()
	id, _ := app.subChildAt(y - 1)
	app.mu.Unlock()
	if id != "" {
		t.Fatalf("screen row %d (the call row) resolved to child %q; the offset points at the call", y-1, id)
	}
}

// The panel must show the child's rows rather than its empty-roster panel: the
// ops can read the child even when the roster it was opened from is empty, and
// the empty-roster branch is chosen on len(rows).
func TestSubagentPanelShowsRowsOverAnEmptyRoster(t *testing.T) {
	app, scr := subTranscriptScreen(t)
	ops := subAgentOps("fg-7")
	ops.Roster = func() []HubAgent { return nil } // the state the drive found
	app.SetHubOps(ops)

	y := subRowScreenY(t, app, "scout")
	app.mu.Lock()
	press(app, 8, y)
	release(app, 8, y)
	app.mu.Unlock()
	app.draw()

	ui := hubUI(app)
	if !ui.open || ui.viewID != "fg-7" || len(ui.view) != 3 {
		t.Fatalf("panel = %+v, want the child's three rows over an empty roster", ui)
	}
	out := screenText(scr)
	if strings.Contains(out, "no background agents yet") {
		t.Fatalf("the empty-roster panel covered the child's transcript:\n%s", out)
	}
	if !strings.Contains(out, "map the app shell") {
		t.Fatalf("the child's own rows are not on screen:\n%s", out)
	}
}

// Esc is the way back: one press leaves the child's transcript for the roster,
// the next closes the panel, and the session transcript underneath never moved
// (the panel never touches the viewport, the same discipline the diff overlay
// uses).
func TestSubagentTranscriptEscReturnsToTheSession(t *testing.T) {
	app := subTranscriptApp(t)
	app.SetHubOps(subAgentOps("fg-7"))

	y := subRowScreenY(t, app, "scout")
	app.mu.Lock()
	before, vp := app.selViewport()
	press(app, 6, y)
	release(app, 6, y)
	app.mu.Unlock()

	if !pressRosterKey(app, tcell.KeyEsc, 0) {
		t.Fatal("Esc must be owned by the panel while the child's transcript is up")
	}
	if ui := hubUI(app); ui.viewID != "" || !app.HubRosterOpen() {
		t.Fatalf("after one Esc: view=%q open=%v, want the roster still open", ui.viewID, app.HubRosterOpen())
	}
	pressRosterKey(app, tcell.KeyEsc, 0)
	if app.HubRosterOpen() {
		t.Fatal("the second Esc must close the panel entirely")
	}
	app.mu.Lock()
	after, _ := app.selViewport()
	app.mu.Unlock()
	if before != after || vp == 0 {
		t.Fatalf("the session transcript moved while the panel was open: top %d -> %d (vp %d)", before, after, vp)
	}
}

// A drag that starts on a child row must still select text: the click opens a
// transcript, and a selection that cannot be made across the row that names
// the subagent would be a worse trade than the transcript.
func TestSubagentRowDragStillSelects(t *testing.T) {
	app := subTranscriptApp(t)
	app.SetHubOps(subAgentOps("fg-7"))

	y := subRowScreenY(t, app, "scout")
	app.mu.Lock()
	press(app, 6, y)
	app.mu.Unlock()
	if got := hubUI(app); got.open {
		t.Fatal("the press alone opened the panel: it must wait for a release")
	}
	app.mu.Lock()
	press(app, 6, y) // a second report at a new point: the drag
	release(app, 40, y)
	opened := app.HubRosterOpen()
	app.mu.Unlock()
	if opened {
		t.Fatal("a drag across a child row opened the transcript instead of selecting")
	}
}

// A child the host tracked nothing for has no transcript to open, so its row
// must not be clickable at all — a click there falls through to selection,
// and the chord stays silent rather than claiming to have opened something.
func TestUntrackedSubagentRowIsNotOpenable(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	app.AddToolBlock("c1", "task", `{"prompt":"work"}`)
	app.AddTaskChild("c1", "reader", "", "free")
	app.SetHubOps(subAgentOps("fg-7"))
	app.draw()

	// The row itself resolves to nothing, so the click test cannot even name
	// a screen row for it — subRowScreenY would fail the lookup.
	app.mu.Lock()
	_, vp := app.selViewport()
	hdr := app.transcriptTop()
	openable := false
	for y := hdr; y < hdr+vp; y++ {
		if id, _ := app.subChildAt(y); id != "" {
			openable = true
		}
	}
	app.mu.Unlock()
	if openable {
		t.Fatal("an untracked child row is openable; there is no transcript behind it")
	}
	if app.openNewestSubChildTranscript() {
		t.Fatal("the chord opened a transcript for a session with no tracked child")
	}
}

// Alt+B is the keyboard twin: the newest child a `task` call spawned opens in
// the same panel, so the mouse and the chord can never disagree about what
// "open this subagent" means.
func TestSubagentTranscriptChordOpensTheNewestChild(t *testing.T) {
	app := subTranscriptApp(t)
	app.SetHubOps(subAgentOps("fg-7"))

	action := app.keyMap.Resolve(tcell.NewEventKey(tcell.KeyRune, 'b', tcell.ModAlt))
	if action != "app.subagent.transcript" {
		t.Fatalf("Alt+B resolved to %q, want app.subagent.transcript", action)
	}
	app.handleKey(tcell.NewEventKey(tcell.KeyRune, 'b', tcell.ModAlt))

	ui := hubUI(app)
	if ui.viewID != "fg-7" || ui.viewName != "scout" {
		t.Fatalf("chord opened %+v, want the newest child's transcript", ui)
	}
}

// With two children under one call, the chord opens the newest — the one a
// tail-following transcript shows last — while the click still reaches any of
// them. Both paths go through one activation, so the batch is not a special
// case in either.
func TestSubagentChordPicksTheNewestOfABatch(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	app.AddToolBlock("c1", "task", `{"context":"shared","tasks":[{"prompt":"a"},{"prompt":"b"}]}`)
	app.AddTaskChild("c1", "first", "", "free")
	app.SetTaskChildTranscript("c1", "first", "fg-1")
	app.AddTaskChild("c1", "second", "", "free")
	app.SetTaskChildTranscript("c1", "second", "fg-2")
	app.draw()
	app.SetHubOps(subAgentOps("fg-1", "fg-2"))

	app.handleKey(tcell.NewEventKey(tcell.KeyRune, 'b', tcell.ModAlt))
	if ui := hubUI(app); ui.viewID != "fg-2" || ui.viewName != "second" {
		t.Fatalf("chord opened %+v, want the newest child (fg-2/second)", ui)
	}

	// And a click still reaches the older sibling.
	pressRosterKey(app, tcell.KeyEsc, 0)
	y := subRowScreenY(t, app, "first")
	app.mu.Lock()
	press(app, 6, y)
	release(app, 6, y)
	app.mu.Unlock()
	if ui := hubUI(app); ui.viewID != "fg-1" {
		t.Fatalf("click opened %q, want the clicked sibling fg-1", ui.viewID)
	}
}
