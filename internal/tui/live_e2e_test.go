//go:build e2e

// Live-driver tests: the whole App on a real screen, driven the way a human
// drives it — through handleKey, the UI loop's own entry point, with the lock
// discipline app.go imposes (the lock is taken around handleMouse, taken again
// by the key/modal handlers that take it themselves).
//
// Why this file exists separately: the session tests call handleMouse directly
// because that is the unit under test, and a bug in the WRAPPING (the lock, the
// modal ordering, the event type the handler tests inject by hand) is invisible
// to them. A trajectory click that only works when the report is synthesised by
// tcell.NewEventMouse is not a click.
//
// Build-tagged `e2e` so `go test ./...` stays hermetic: these tests need no
// network, but they do exercise the full event path, so they are slower and
// separate. Run them with:
//
//	go test -tags e2e ./internal/tui/ -run TestLive
package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// liveTurns is a transcript with two user prompts, a real event-loop run
// underneath them, and the app painted once.
func liveTurns(t *testing.T) (*App, tcell.SimulationScreen) {
	t.Helper()
	app, scr := newTestApp(t, 160, 40)
	app.SetDockMode(DockShow)
	app.SetDockOps(DockOps{
		Session:    func() (string, string) { return "sidebar window", "sess1234" },
		Tasks:      func() string { return "TASKS · 1/2 done\n[x] wire the panel\n[ ] write tests" },
		Trajectory: func() string { return "TRAJECTORY · 2 records" },
	})
	app.SetTrajectoryOps(&TrajectoryOps{
		Records: func() []TrajectoryRecord {
			return []TrajectoryRecord{
				{Index: 1, Kind: "user", Text: "make a file", Detail: "make a file"},
				{Index: 2, Kind: "tool", Text: "edit foo.txt", Detail: "+hello"},
			}
		},
	})
	app.AddUserBlock("first prompt")
	app.AddAssistantBlock("first answer")
	app.AddUserBlock("second prompt")
	app.AddSystemBlock("trailing system row")
	app.draw()
	return app, scr
}

// liveClick is the whole event path for one click, with the lock discipline the
// UI loop uses: handleKey takes a.mu around handleMouse, exactly as app.go does
// for a non-wheel button, and the unlocked action runs after.
func liveClick(app *App, x, y int) {
	app.handleKey(tcell.NewEventMouse(x, y, tcell.Button1, tcell.ModNone))
	app.handleKey(tcell.NewEventMouse(x, y, tcell.ButtonNone, tcell.ModNone))
}

// liveDrag is a press, motion and release through the same entry point — the
// shape a terminal sends for a selection.
func liveDrag(app *App, x0, y0, x1, y1 int) {
	app.handleKey(tcell.NewEventMouse(x0, y0, tcell.Button1, tcell.ModNone))
	for i := 1; i <= 4; i++ {
		x := x0 + (x1-x0)*i/4
		y := y0 + (y1-y0)*i/4
		app.handleKey(tcell.NewEventMouse(x, y, tcell.Button1, tcell.ModNone))
	}
	app.handleKey(tcell.NewEventMouse(x1, y1, tcell.ButtonNone, tcell.ModNone))
}

// TestLiveSidebarTrajectoryClickOpensTheLedger is the dead click, driven the way
// a terminal drives it. The session test calls handleMouse; this one goes
// through handleKey, so a wrapping bug — the lock, the modal ordering, the
// report shape — fails here even when the unit test passes.
func TestLiveSidebarTrajectoryClickOpensTheLedger(t *testing.T) {
	app, scr := liveTurns(t)
	defer scr.Fini()

	x, y, ok := dockCell(t, app, "click to open the ledger")
	if !ok {
		t.Skip("panel layout exposed no trajectory row")
	}
	liveClick(app, x, y)
	if !app.TrajectoryOpen() {
		t.Fatalf("a click on the panel's trajectory row did not open the ledger:\n%s", screenText(scr))
	}
	// The ledger is an overlay, so the frame after the click has to be painted
	// to see it: OpenTrajectory only pokes, and the caller draws.
	app.draw()
	painted := screenText(scr)
	if !strings.Contains(painted, "TRAJECTORY — ledger") {
		t.Fatalf("the ledger is open but did not paint its panel:\n%s", painted)
	}
	if !strings.Contains(painted, "make a file") {
		t.Fatalf("the ledger painted no record:\n%s", painted)
	}
}

// TestLiveSidebarDragCopiesPanelText is the same path for the copy half: a drag
// over the panel's rows, through handleKey, must land the panel's text on the
// clipboard.
func TestLiveSidebarDragCopiesPanelText(t *testing.T) {
	app, scr := liveTurns(t)
	defer scr.Fini()

	x, y, ok := dockCell(t, app, "wire the panel")
	if !ok {
		t.Skip("panel layout exposed no task row")
	}

	liveDrag(app, x, y, x+4, y)

	got := string(scr.GetClipboardData())
	if !strings.Contains(got, "wir") {
		t.Fatalf("clipboard = %q, want the panel's own row text", got)
	}
	if strings.Contains(got, "prompt") || strings.Contains(got, "answer") {
		t.Fatalf("a drag over the panel copied the transcript beneath it: %q", got)
	}
}

// TestLiveSidebarClickOverUserRowOpensNoPopup: the panel is painted over the
// transcript, so a click on its columns must not arm the transcript's own
// affordances — a message menu anchored over the sidebar is the symptom.
func TestLiveSidebarClickOverUserRowOpensNoPopup(t *testing.T) {
	app, scr := liveTurns(t)
	defer scr.Fini()

	x := app.width - dockCols + dockPad + 4
	promptY := userRow(t, app, 1)
	app.mu.Lock()
	covered := app.dockAt(x, promptY)
	app.mu.Unlock()
	if !covered {
		t.Skipf("the panel does not cover the prompt row (%d)", promptY)
	}

	liveClick(app, x, promptY)
	app.draw()

	if app.MsgMenuOpen() {
		t.Fatal("a click inside the sidebar opened the user-message menu")
	}
}

// TestLiveUserMessageRevertIsTheTreeRewind drives the whole menu through the
// event loop: a click on a ❯ row opens the menu, the digit for the revert row
// runs it, and the session is rewound with the prompt back in the composer —
// the /tree user-row behaviour the user asked for.
func TestLiveUserMessageRevertIsTheTreeRewind(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	defer scr.Fini()
	threeTurns(app)

	var rewound string
	var summarize bool
	app.SetSessionOps(&SessionOps{
		NavigateTree: func(entryID string, s bool) (string, error) {
			rewound, summarize = entryID, s
			return "second prompt", nil
		},
		UserEntryID: func(i int) string { return "entry" + string(rune('A'+i)) },
	})
	app.draw()

	y := userRow(t, app, 1)
	liveClick(app, 6, y)
	if !app.MsgMenuOpen() {
		t.Fatalf("the click did not open the menu:\n%s", screenText(scr))
	}
	app.draw()

	// The revert row is the second one, so the digit is "2".
	app.handleKey(tcell.NewEventKey(tcell.KeyRune, '2', tcell.ModNone))
	app.draw()

	if rewound != "entryB" {
		t.Fatalf("reverted entry = %q, want %q", rewound, "entryB")
	}
	if summarize {
		t.Fatal("revert asked for a branch summary; that costs a model round trip on the UI thread")
	}
	app.mu.Lock()
	draft := app.ed.Text()
	notices := ""
	for _, b := range app.blocks {
		if b.Kind == KindSystem {
			notices += b.Text + "\n"
		}
	}
	app.mu.Unlock()
	if draft != "second prompt" {
		t.Fatalf("composer = %q, want the rewound prompt back for editing", draft)
	}
	if !strings.Contains(notices, "files were NOT restored") {
		t.Fatalf("revert did not warn about files:\n%s", notices)
	}
}

// TestLiveAskCardIsAModalInTheMainPane drives the blocking ask card through the
// event loop: the click goes through handleKey — the UI loop's own entry point,
// with the modal ordering and the lock discipline app.go imposes — not the
// card's own mouse handler, so a wrapping bug fails here even when the unit
// tests pass. It is also the pane assertion on a live frame: the card ends at
// the panel's left edge, which is the report this card was restyled for.
func TestLiveAskCardIsAModalInTheMainPane(t *testing.T) {
	app, scr := liveTurns(t)
	defer scr.Fini()

	answered := make(chan AskAnswer, 1)
	go func() {
		ans, _ := app.AskCard(context.Background(), askOptions(), 20*time.Second)
		answered <- ans
	}()
	waitAsk(t, app, true)
	app.draw()

	painted := strings.Split(screenText(scr), "\n")
	app.mu.Lock()
	edge := app.rightEdge()
	app.mu.Unlock()
	if edge >= app.width {
		t.Fatalf("the panel is not reserving columns (rightEdge=%d), nothing to prove", edge)
	}
	top := -1
	for y, ln := range painted {
		if strings.Contains(ln, "◆ ask") {
			top = y
			break
		}
	}
	if top < 0 {
		t.Fatalf("the card is not on screen:\n%s", strings.Join(painted, "\n"))
	}
	// The card's own rows only: screenText flattens every row of the terminal
	// and a row past the height is one zero rune long, not a painted cell.
	bottom := top
	for y := top; y < app.height; y++ {
		for x := edge; x < app.width; x++ {
			if ch, _, _, _ := scr.GetContent(x, y); ch != ' ' {
				t.Fatalf("the card painted %q at x=%d y=%d, inside the panel:\n%s",
					string(ch), x, y, painted[y])
			}
		}
		if ch, _, _, _ := scr.GetContent(1, y); ch == '┃' {
			bottom = y
		}
	}
	if bottom <= top {
		t.Fatalf("the rail spans one row only (top=%d bottom=%d):\n%s",
			top, bottom, strings.Join(painted, "\n"))
	}
	// A modal owns every key: the click has to reach the card, and nothing may
	// leak into the composer behind it.
	x, y := 0, -1
	for row, ln := range painted {
		if at := strings.Index(ln, "mysql"); at > 0 {
			x, y = width(ln[:at]), row
			break
		}
	}
	if y < 0 {
		t.Fatalf("no mysql row on screen:\n%s", strings.Join(painted, "\n"))
	}
	liveClick(app, x, y)
	select {
	case ans := <-answered:
		if len(ans.Labels) != 1 || ans.Labels[0] != "mysql" {
			t.Fatalf("the click answered %+v, want mysql", ans)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("a click through the UI loop did not answer the card:\n%s", screenText(scr))
	}
	if app.AskPending() {
		t.Fatal("the card must close once the click answered it")
	}
}
