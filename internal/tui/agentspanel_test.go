package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// panelScreen is an app with the strip's surfaces ready and a screen to read.
func panelScreen(t *testing.T) (*App, tcell.SimulationScreen) {
	t.Helper()
	app, scr := newTestApp(t, 100, 30)
	app.SetHubOps(subAgentOps("fg-7"))
	return app, scr
}

// The headline: a child that starts appears on the strip ABOVE the composer,
// with its label and what it is doing — the view that did not exist, because
// the transcript rows hang under the `task` call and a grandchild has no call
// row of its own.
func TestAgentPanelShowsARunningChild(t *testing.T) {
	app, scr := panelScreen(t)
	app.AgentPanelStart("scout", "scout", "free", "fg-7")
	app.AgentPanelUpdate("scout", "read", `{"path":"internal/tui/app.go"}`)
	app.draw()

	out := screenText(scr)
	if !strings.Contains(out, "scout") {
		t.Fatalf("the strip does not name the running child:\n%s", out)
	}
	if !strings.Contains(out, "internal/tui/app.go") {
		t.Fatalf("the strip does not say what the child is doing:\n%s", out)
	}
	// It is CHROME: the row sits above the composer's top border, not inside
	// the transcript.
	app.mu.Lock()
	top := app.agentPanelTop(app.queueTop(app.height - 1 - app.composerRows()))
	app.mu.Unlock()
	if top <= 0 {
		t.Fatal("the strip has no row on screen")
	}
	composer := app.height - 1 - app.composerRows()
	if top >= composer {
		t.Fatalf("strip top %d is not above the composer top %d", top, composer)
	}
}

// A grandchild is exactly the case the transcript cannot render: it arrives
// with a label and no call row of its own. The strip is flat, so it shows it.
func TestAgentPanelShowsAChainFlat(t *testing.T) {
	app, scr := panelScreen(t)
	app.AgentPanelStart("task", "task", "", "")
	app.AgentPanelStart("scout", "scout", "", "")
	app.AgentPanelStart("leaf", "sonic", "", "")
	app.draw()

	out := screenText(scr)
	for _, want := range []string{"task", "scout", "leaf"} {
		if !strings.Contains(out, want) {
			t.Fatalf("level %q is missing from the flat strip:\n%s", want, out)
		}
	}
}

// A settled child leaves the strip: it lists what is RUNNING, and a finished
// child's row is the transcript's business.
func TestAgentPanelDropsASettledChild(t *testing.T) {
	app, _ := panelScreen(t)
	app.AgentPanelStart("scout", "scout", "", "")
	if _, n := app.AgentPanelState(); n != 1 {
		t.Fatalf("running = %d, want 1", n)
	}
	app.AgentPanelSettle("scout")
	if _, n := app.AgentPanelState(); n != 0 {
		t.Fatalf("running = %d after settle, want 0", n)
	}
}

// A click on a strip row opens THAT child's own transcript in the panel —
// the same surface /hub's Enter uses, so Esc means one thing.
func TestAgentPanelRowClickOpensTheChildTranscript(t *testing.T) {
	app, _ := panelScreen(t)
	app.AgentPanelStart("scout", "scout", "", "fg-7")
	app.draw()

	app.mu.Lock()
	hits := append([]agentPanelHit(nil), app.agentHits...)
	app.mu.Unlock()
	if len(hits) != 1 {
		t.Fatalf("strip hits = %+v, want the tracked child", hits)
	}
	y := hits[0].row.y
	// The real path: the press goes through the UI loop's mouse branch, which
	// arms the open and runs it after the lock — the same two steps the queue
	// uses for send-now.
	// The real path: the press goes through the UI loop's mouse branch, which
	// arms the open and runs it after the lock — the same two steps the queue
	// uses for send-now.
	app.handleKey(tcell.NewEventMouse(6, y, tcell.Button1, tcell.ModNone))
	app.draw()
	ui := hubUI(app)
	if !app.HubRosterOpen() || ui.viewID != "fg-7" {
		t.Fatalf("panel = %+v, want the clicked child's fg-7", ui)
	}
}

// A child no hub tracked has no transcript to open, so its row is not
// clickable — a row that offered an open it cannot perform is worse than a
// row that offers nothing.
func TestAgentPanelRowWithoutTranscriptIsNotClickable(t *testing.T) {
	app, _ := panelScreen(t)
	app.AgentPanelStart("scout", "scout", "", "")
	app.draw()
	app.mu.Lock()
	hits := len(app.agentHits)
	app.mu.Unlock()
	if hits != 0 {
		t.Fatalf("strip hits = %d, want 0 for an untracked child", hits)
	}
}

// The chord toggles the strip and says which way it went; a second press
// brings it back.
func TestAgentPanelToggle(t *testing.T) {
	app, _ := panelScreen(t)
	app.SetAgentPanelOps(&AgentPanelOps{
		OpenChild: func(string, string) bool { return true },
		Set:       func(bool) error { return nil },
	})
	app.AgentPanelStart("scout", "scout", "", "")
	app.draw()

	app.handleKey(tcell.NewEventKey(tcell.KeyRune, 'p', tcell.ModAlt))
	on, _ := app.AgentPanelState()
	if on {
		t.Fatal("Alt+P did not turn the strip off")
	}
	app.handleKey(tcell.NewEventKey(tcell.KeyRune, 'p', tcell.ModAlt))
	if on, _ := app.AgentPanelState(); !on {
		t.Fatal("Alt+P did not turn the strip back on")
	}
}

// An unwired toggle says so in the transcript rather than swallowing the
// chord — the same rule the hub roster's chord follows.
func TestAgentPanelToggleUnwiredSaysSo(t *testing.T) {
	app, scr := panelScreen(t)
	app.draw()
	app.handleKey(tcell.NewEventKey(tcell.KeyRune, 'p', tcell.ModAlt))
	app.draw()
	if !strings.Contains(screenText(scr), "not wired") {
		t.Fatalf("an unwired toggle must say so:\n%s", screenText(scr))
	}
}

// The strip never paints over the queue or the composer: with the queue full
// and children running, the strip is pushed up and stays a chrome band.
func TestAgentPanelSitsAboveTheQueue(t *testing.T) {
	app, scr := panelScreen(t)
	app.AgentPanelStart("scout", "scout", "", "")
	app.mu.Lock()
	app.queue = append(app.queue, queuedEntry{text: "a pending prompt", kind: queueKindText, session: app.st.SessionID})
	app.mu.Unlock()
	app.draw()

	out := screenText(scr)
	if !strings.Contains(out, "scout") || !strings.Contains(out, "a pending prompt") {
		t.Fatalf("both chrome bands must be on screen:\n%s", out)
	}
	app.mu.Lock()
	strip := app.agentPanelTop(app.queueTop(app.height - 1 - app.composerRows()))
	queue := app.queueTop(app.height - 1 - app.composerRows())
	app.mu.Unlock()
	if strip >= queue {
		t.Fatalf("strip top %d must sit above the queue top %d", strip, queue)
	}
}

// A settled child STAYS on the strip, dim and still: the strip is this
// session's record of what ran, and a row that vanished the moment its
// child did would read as a bug. It is marked done, so the count of
// RUNNING children drops to zero while the row itself remains.
func TestAgentPanelKeepsASettledChild(t *testing.T) {
	app, _ := panelScreen(t)
	app.AgentPanelStart("scout", "scout", "", "")
	if _, n := app.AgentPanelState(); n != 1 {
		t.Fatalf("running = %d, want 1", n)
	}
	app.AgentPanelSettle("scout")
	app.mu.Lock()
	rows := app.agents.snapshot()
	app.mu.Unlock()
	if len(rows) != 1 {
		t.Fatalf("rows after settle = %d, want 1 (settled rows persist)", len(rows))
	}
	if !rows[0].Done {
		t.Fatal("settled row is not marked done")
	}
	if rows[0].End.IsZero() {
		t.Fatal("settled row has no end time")
	}
}

// The keyboard can enter the strip with Alt+F: the newest row is the one a
// person watching would open, and from there arrows walk the list, Enter
// opens a child, and Esc leaves. The strip's own focus state is what the
// painter marks, so the test reads it directly rather than the painted
// frame.
func TestAgentPanelAltFFocusesTheNewestRow(t *testing.T) {
	app, _ := panelScreen(t)
	app.AgentPanelStart("scout", "scout", "", "fg-7")
	app.AgentPanelStart("leaf", "sonic", "", "fg-8")
	app.draw()

	app.handleKey(tcell.NewEventKey(tcell.KeyRune, 'f', tcell.ModAlt))
	if !app.agentPanelFocused {
		t.Fatal("Alt+F did not put the keyboard on the strip")
	}
	app.mu.Lock()
	focus := app.agents.focused()
	app.mu.Unlock()
	if focus != "leaf" {
		t.Fatalf("focused row = %q, want the newest child \"leaf\"", focus)
	}
}

// Alt+F on a strip with nothing to focus is silence — the chord must not
// claim to have done something, the same rule ctrl+o uses on a transcript
// with no box.
func TestAgentPanelAltFWithNothingToFocusIsSilence(t *testing.T) {
	app, scr := panelScreen(t)
	app.draw()
	app.handleKey(tcell.NewEventKey(tcell.KeyRune, 'f', tcell.ModAlt))
	app.draw()
	if strings.Contains(screenText(scr), "not wired") {
		t.Fatalf("Alt+F with no running child must be silent:\n%s", screenText(scr))
	}
}

// Arrows walk the strip while it is focused, wrapping at the ends, and Enter
// opens the focused child's transcript — the same surface a click opens, so
// the keyboard and the mouse cannot drift into opening different children.
func TestAgentPanelArrowsWalkAndEnterOpens(t *testing.T) {
	app, _ := panelScreen(t)
	app.AgentPanelStart("task", "task", "", "fg-7")
	app.AgentPanelStart("scout", "scout", "", "fg-8")
	app.AgentPanelStart("leaf", "sonic", "", "fg-9")
	app.draw()

	app.handleKey(tcell.NewEventKey(tcell.KeyRune, 'f', tcell.ModAlt))
	app.mu.Lock()
	focus := app.agents.focused()
	app.mu.Unlock()
	if focus != "leaf" {
		t.Fatalf("initial focus = %q, want \"leaf\" (newest)", focus)
	}

	// Down moves toward the oldest and clamps there: the strip is a short
	// band, so an arrow that runs off the end stays at the last row rather
	// than wrapping to the other end.
	for i := 0; i < 5; i++ {
		app.handleKey(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	}
	app.mu.Lock()
	focus = app.agents.focused()
	app.mu.Unlock()
	if focus != "task" {
		t.Fatalf("after 5 Down (clamp) focus = %q, want the oldest \"task\"", focus)
	}

	// Up moves toward the newest and clamps there: from "task" two presses
	// reach "leaf".
	for i := 0; i < 5; i++ {
		app.handleKey(tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone))
	}
	app.mu.Lock()
	focus = app.agents.focused()
	app.mu.Unlock()
	if focus != "leaf" {
		t.Fatalf("after 5 Up (clamp) focus = %q, want the newest \"leaf\"", focus)
	}

	// Up moves toward the newest; from "leaf" one Up stays at "leaf".
	app.handleKey(tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone))
	app.mu.Lock()
	focus = app.agents.focused()
	app.mu.Unlock()
	if focus != "leaf" {
		t.Fatalf("after Up from the newest focus = %q, want \"leaf\"", focus)
	}

	// Enter opens the focused child's transcript in the panel surface.
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	app.draw()
	ui := hubUI(app)
	if !app.HubRosterOpen() || ui.viewID != "fg-9" {
		t.Fatalf("panel = %+v, want the focused child's fg-9", ui)
	}
}

// Esc leaves the strip: the editor's arrows then do their usual work, and the
// strip can be re-entered with the chord. One Esc is "I am done looking at
// the strip", not "close the whole app".
func TestAgentPanelEscLeavesTheStrip(t *testing.T) {
	app, _ := panelScreen(t)
	app.AgentPanelStart("scout", "scout", "", "fg-7")
	app.draw()

	app.handleKey(tcell.NewEventKey(tcell.KeyRune, 'f', tcell.ModAlt))
	if !app.agentPanelFocused {
		t.Fatal("Alt+F did not focus the strip")
	}
	app.handleKey(tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone))
	if app.agentPanelFocused {
		t.Fatal("Esc did not leave the strip")
	}
	app.mu.Lock()
	if app.agents.focused() != "" {
		t.Fatal("Esc did not blur the strip")
	}
	app.mu.Unlock()
}

// Arrow-down from an empty composer with nothing to recall enters the strip
// instead of scrolling the transcript past its tail — the opencode shape,
// where the strip is a list the composer's arrow walks into. A session that
// never spawns a child is unaffected: the strip has no rows and the block
// falls through to the editor as before.
func TestAgentPanelDownFromComposerEntersTheStrip(t *testing.T) {
	app, _ := panelScreen(t)
	app.AgentPanelStart("scout", "scout", "", "fg-7")
	app.draw()

	app.handleKey(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	if !app.agentPanelFocused {
		t.Fatal("Down from an empty composer did not enter the strip")
	}
	app.mu.Lock()
	focus := app.agents.focused()
	app.mu.Unlock()
	if focus != "scout" {
		t.Fatalf("focused row = %q, want \"scout\"", focus)
	}
}

// D1: a tracked child — one the host has a transcript for — wears the ⎿ tick
// next to its model column, so the row reads as openable before the mouse
// moves. An untracked child wears nothing, because offering an open it cannot
// perform is worse than offering nothing.
func TestAgentPanelOpenableRowWearsATick(t *testing.T) {
	app, scr := panelScreen(t)
	app.AgentPanelStart("scout", "scout", "free", "fg-7")
	app.draw()

	if !strings.Contains(screenText(scr), "⎿") {
		t.Fatalf("an openable strip row must wear the ⎿ tick:\n%s", screenText(scr))
	}

	app2, scr2 := panelScreen(t)
	app2.AgentPanelStart("scout", "scout", "", "")
	app2.draw()
	if strings.Contains(screenText(scr2), "⎿") {
		t.Fatalf("an untracked row must not wear the ⎿ tick:\n%s", screenText(scr2))
	}
}
