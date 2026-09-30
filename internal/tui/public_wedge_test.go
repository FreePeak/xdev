package tui

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/theme"
	"github.com/gdamore/tcell/v2"
)

// Public-API proof of the report, so it compiles and runs on main as well as
// on the fix: a screen whose Show() blocks (tcell's synchronous tty write
// against a terminal that stopped draining), a Ctrl+C, and the question the
// user asked — did the session end?
//
// No unexported field is touched, so the same file builds on both trees.
type pubWedgeScreen struct {
	tcell.SimulationScreen
	inShow  chan struct{}
	release chan struct{}
}

func (w *pubWedgeScreen) Show() {
	select {
	case <-w.inShow:
	default:
		close(w.inShow)
	}
	<-w.release
}

func TestPublicCtrlCEnitsASessionWedgedInsideShow(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatal(err)
	}
	scr := &pubWedgeScreen{SimulationScreen: sim, inShow: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-scr.release:
		default:
			close(scr.release)
		}
	})

	app := New(scr, theme.Load("groknight"), "test/free", "sess-pub")
	app.width, app.height = 80, 24
	app.AddUserBlock("hi")
	quit := make(chan struct{}, 1)
	app.SetHandlers(func(string) {}, func() {}, func() { quit <- struct{}{}; app.Quit() })
	app.SetRunning(true) // a live turn, the shape the report had
	// Production arms this (cmd/xdev/tui.go), and the watchdog is what marks
	// the loop wedged — without it the chord path is never reached, which is
	// also why it fails on main.
	app.SetStallDumpDir(filepath.Join(t.TempDir(), "dumps"))

	loopDone := make(chan struct{})
	go func() { defer close(loopDone); app.Run() }()
	<-scr.inShow // the UI loop is now inside the blocked Show

	// The watchdog condemns a loop after stallAfter (5s in production). Give it
	// that long plus slack, then send the chord, the way a terminal would.
	sim.InjectKey(tcell.KeyCtrlC, 0, tcell.ModCtrl)
	sim.InjectKey(tcell.KeyCtrlC, 0, tcell.ModCtrl)

	deadline := time.After(20 * time.Second)
	for {
		select {
		case <-quit:
			select {
			case <-app.quitCh:
			case <-time.After(5 * time.Second):
				t.Fatal("the quit handler ran but quitCh was never closed")
			}
			return
		case <-deadline:
			t.Fatal("Ctrl+C did not end a session wedged inside Show: the loop is the only reader of keyq, so the chord had no way out")
		case <-time.After(500 * time.Millisecond):
			sim.InjectKey(tcell.KeyCtrlC, 0, tcell.ModCtrl)
		}
	}
}
