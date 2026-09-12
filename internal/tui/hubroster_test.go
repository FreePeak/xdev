package tui

import (
	"testing"

	"github.com/gdamore/tcell/v2"
)

// hubFixture wires a fake hub surface (roster + transcript + kill/park/
// revive) and records the calls the overlay makes.
type hubFixture struct {
	rows           []HubAgent
	transcriptReqs [][2]any // captured (id, fromSeq) requests
	killed         []string
	parked         []string
	revived        []string
	rosterCall     int
}

func (f *hubFixture) ops() *HubOps {
	return &HubOps{
		Roster: func() []HubAgent {
			f.rosterCall++
			return f.rows
		},
		Transcript: func(id string, fromSeq int) ([]HubTranscriptLine, int, bool) {
			f.transcriptReqs = append(f.transcriptReqs, [2]any{id, fromSeq})
			if id != "hub-1" {
				return nil, 0, false
			}
			return []HubTranscriptLine{
				{Role: "user", Text: "work on the parser"},
				{Role: "assistant", Text: "parser done"},
			}, 2, true
		},
		Kill: func(id string) bool {
			f.killed = append(f.killed, id)
			return true
		},
		Park: func(id string) bool {
			f.parked = append(f.parked, id)
			return true
		},
		Revive: func(id string) bool {
			f.revived = append(f.revived, id)
			return true
		},
	}
}

func rosterRows() []HubAgent {
	return []HubAgent{
		{ID: "hub-1", Name: "scout", Status: "running", Model: "free", Activity: "reading", Cost: "$0.0100"},
		{ID: "hub-2", Name: "fixer", Status: "parked", Model: "smol", Activity: "2 msgs", Cost: "-"},
	}
}

func pressKey(app *App, key tcell.Key, r rune) bool {
	return app.handleHubRosterKey(tcell.NewEventKey(key, r, tcell.ModNone))
}

// /hub opens the roster, Up/Down move the selection, Enter opens and closes
// the transcript view, Esc unwinds view → roster.
func TestHubRosterOverlayKeys(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	f := &hubFixture{rows: rosterRows()}
	app.SetHubOps(f.ops())

	if !dispatch(app, "/hub") {
		t.Fatal("/hub must be consumed by the command table")
	}
	if !app.HubRosterOpen() {
		t.Fatal("roster did not open")
	}
	if ui := hubUI(app); ui.sel != 0 || len(ui.rows) != 2 {
		t.Fatalf("opened state = %+v", ui)
	}

	if !pressKey(app, tcell.KeyDown, 0) {
		t.Fatal("Down must be handled")
	}
	if ui := hubUI(app); ui.sel != 1 {
		t.Fatalf("selection after Down = %d, want 1", ui.sel)
	}
	if !pressKey(app, tcell.KeyUp, 0) {
		t.Fatal("Up must be handled")
	}
	if ui := hubUI(app); ui.sel != 0 {
		t.Fatalf("selection after Up = %d, want 0", ui.sel)
	}

	// Enter opens the transcript of the selected agent (incremental fetch
	// starts at 0).
	if !pressKey(app, tcell.KeyEnter, 0) {
		t.Fatal("Enter must be handled")
	}
	ui := hubUI(app)
	if ui.viewID != "hub-1" || len(ui.view) != 2 {
		t.Fatalf("transcript view = %+v", ui)
	}
	if len(f.transcriptReqs) != 1 || f.transcriptReqs[0] != [2]any{"hub-1", 0} {
		t.Fatalf("transcript requests = %v", f.transcriptReqs)
	}
	// Esc leaves the view, a second Esc closes the roster.
	pressKey(app, tcell.KeyEsc, 0)
	if ui := hubUI(app); ui.viewID != "" || !app.HubRosterOpen() {
		t.Fatalf("after Esc from view: %+v open=%v", ui, app.HubRosterOpen())
	}
	pressKey(app, tcell.KeyEsc, 0)
	if app.HubRosterOpen() {
		t.Fatal("second Esc must close the roster")
	}
	// Closed roster no longer swallows keys.
	if pressKey(app, tcell.KeyDown, 0) || pressKey(app, tcell.KeyRune, 'k') {
		t.Fatal("keys must fall through while the roster is closed")
	}
}

// k kills, p parks, r revives the selected agent; every action refreshes
// the roster snapshot and leaves feedback in the panel.
func TestHubRosterKillParkReviveKeys(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	f := &hubFixture{rows: rosterRows()}
	app.SetHubOps(f.ops())
	if err := app.HubRoster(); err != nil {
		t.Fatal(err)
	}
	pressKey(app, tcell.KeyDown, 0) // select hub-2 (parked)
	before := f.rosterCall

	if !pressKey(app, tcell.KeyRune, 'k') {
		t.Fatal("k must be handled")
	}
	if len(f.killed) != 1 || f.killed[0] != "hub-2" {
		t.Fatalf("killed = %v", f.killed)
	}
	if !pressKey(app, tcell.KeyRune, 'p') {
		t.Fatal("p must be handled")
	}
	if len(f.parked) != 1 || f.parked[0] != "hub-2" {
		t.Fatalf("parked = %v", f.parked)
	}
	if !pressKey(app, tcell.KeyRune, 'r') {
		t.Fatal("r must be handled")
	}
	if len(f.revived) != 1 || f.revived[0] != "hub-2" {
		t.Fatalf("revived = %v", f.revived)
	}
	if f.rosterCall <= before {
		t.Fatal("actions must refresh the roster snapshot")
	}
	if ui := hubUI(app); ui.msg == "" {
		t.Fatal("actions must leave feedback in the panel")
	}
	// Unmatched runes fall through to the editor.
	if pressKey(app, tcell.KeyRune, 'x') {
		t.Fatal("plain runes must not be swallowed")
	}
}

// The overlay renders the roster rows and the transcript panel.
func TestHubRosterOverlayRenders(t *testing.T) {
	app, scr := newTestApp(t, 90, 24)
	f := &hubFixture{rows: rosterRows()}
	app.SetHubOps(f.ops())
	if err := app.HubRoster(); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	app.drawHubRoster(20)
	app.mu.Unlock()
	scr.Show()

	for _, want := range []string{"AGENT HUB", "hub-1", "scout", "running", "hub-2", "parked"} {
		if !gridContains(scr, want) {
			t.Fatalf("roster panel missing %q", want)
		}
	}
	pressKey(app, tcell.KeyEnter, 0) // transcript of hub-1
	app.mu.Lock()
	app.drawHubRoster(20)
	app.mu.Unlock()
	scr.Show()
	if !gridContains(scr, "transcript") || !gridContains(scr, "parser done") {
		t.Fatal("transcript panel did not render")
	}
}

// /hub without a wired hub degrades to a notice instead of machinery.
func TestHubRosterNotWired(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	if err := app.HubRoster(); err != nil {
		t.Fatal(err)
	}
	if app.HubRosterOpen() {
		t.Fatal("roster must not open without ops")
	}
	app.mu.Lock()
	n, text := len(app.blocks), app.blocks[len(app.blocks)-1].Text
	app.mu.Unlock()
	if n != 1 || text == "" {
		t.Fatalf("missing notice block: n=%d text=%q", n, text)
	}

	// An empty roster reports no agents rather than opening an empty panel.
	app.SetHubOps(&HubOps{Roster: func() []HubAgent { return nil }})
	if err := app.HubRoster(); err != nil {
		t.Fatal(err)
	}
	if app.HubRosterOpen() {
		t.Fatal("empty roster must not open the overlay")
	}
	app.mu.Lock()
	text = app.blocks[len(app.blocks)-1].Text
	app.mu.Unlock()
	if text == "" {
		t.Fatal("empty roster must leave a notice")
	}
}

// hubUI snapshots the overlay state (tests are single-threaded).
func hubUI(a *App) hubRosterUI {
	st := a.hubState()
	if st == nil {
		return hubRosterUI{}
	}
	return st.ui
}
