package tui

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/theme"
	"github.com/gdamore/tcell/v2"
)

// The reported dead end (session 00b1c5a0, 2026-09-30): the UI loop blocked
// 17 minutes inside a tty write, and because handleKey runs ON that loop, the
// quit chord had no reader — Ctrl+C and Ctrl+D were delivered to a keyq nobody
// was draining, and the session could not be exited at all. A dump is not an
// exit: it names the hang, then leaves the user with the same dead terminal.
//
// Two halves, each failing against the line it guards: the chord is served
// where a key still lands (the poll goroutine in Run) while the loop is
// wedged, and a loop still wedged when the exit budget runs out restores the
// terminal and ends the session (stall.go).

// wedgedScreen below stands in for the real thing: tcell's Show is the
// synchronous tty write, and a terminal whose reader stopped makes it block
// (dumps/tui-stall-20260930-202555.txt, 17 minutes in devTty.Write).
//
// It blocks inside Show until released, so the UI loop is inside it for the
// whole test.
type wedgedScreen struct {
	tcell.SimulationScreen
	inShow  chan struct{}
	release chan struct{}
	// unblock releases the wedge exactly once; nil in a screen a test
	// never wedges, where the default (block forever) is right.
	unblock func()
}

func (w *wedgedScreen) Show() {
	select {
	case <-w.inShow:
	default:
		close(w.inShow)
	}
	<-w.release
}

func wedgedApp(t *testing.T) (*App, *wedgedScreen) {
	t.Helper()
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatal(err)
	}
	scr := &wedgedScreen{SimulationScreen: sim, inShow: make(chan struct{}), release: make(chan struct{})}
	// Release the wedge once: a Show that unblocks must not double-close if
	// the loop happens to paint again on its way out.
	var once sync.Once
	scr.unblock = func() { once.Do(func() { close(scr.release) }) }
	t.Cleanup(scr.unblock)
	app := New(scr, theme.Load("groknight"), "test/free", "sess1234")
	app.width, app.height = 80, 24
	app.AddUserBlock("hi") // non-empty transcript: the main paint path
	return app, scr
}

// TestQuitChordQuitsWhileTheUILoopIsWedged is the exact report: the loop is
// stuck in a frame, so the chord cannot be applied there, and it must still
// end the session. onQuit is app.Quit() here, which is what cmd wires.
func TestQuitChordQuitsWhileTheUILoopIsWedged(t *testing.T) {
	app, scr := wedgedApp(t)
	// Channels, not bools: the handlers run on the poll goroutine.
	canceled, quit := make(chan struct{}, 1), make(chan struct{}, 1)
	app.SetHandlers(func(string) {}, func() { canceled <- struct{}{} }, func() { quit <- struct{}{}; app.Quit() })
	app.SetRunning(true) // a live turn: the chord aborts it and exits

	// Run on its own goroutine and wedge its very first frame.
	loopDone := make(chan struct{})
	go func() { defer close(loopDone); app.Run() }()
	<-scr.inShow

	// The watchdog sets this in one poll (a second) in production; the test
	// sets it directly so the assertion is about the chord, not the timer.
	app.wedged.Store(true)
	scr.InjectKey(tcell.KeyCtrlC, 0, tcell.ModCtrl)

	select {
	case <-quit:
	case <-time.After(5 * time.Second):
		t.Fatal("Ctrl+C did not quit: the wedged loop is the only reader of keyq, so the chord had no way out")
	}
	select {
	case <-canceled:
	default:
		t.Error("the chord quit without aborting the live turn")
	}
	// The session is really over: quitCh closed, so the loop returns the
	// instant its frame comes back and runTUI's defers restore the terminal.
	select {
	case <-app.quitCh:
	case <-time.After(5 * time.Second):
		t.Fatal("the quit handler ran but quitCh was never closed")
	}
	scr.unblock()
	select {
	case <-loopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the wedged frame came back and quitCh was closed")
	}
}

// Esc must NOT be taken as the exit chord here: it is cancel-only, and a
// wedged loop serving every key would turn "stop this turn" into "exit".
func TestOnlyTheQuitChordIsServedWhileWedged(t *testing.T) {
	app, scr := wedgedApp(t)
	app.SetHandlers(func(string) {}, func() {}, func() { t.Error("a wedged loop quit on a non-quit key") })

	go app.Run()
	<-scr.inShow
	app.wedged.Store(true)

	scr.InjectKey(tcell.KeyEscape, 0, tcell.ModNone)
	scr.InjectKey(tcell.KeyRune, 'x', tcell.ModNone)
	time.Sleep(300 * time.Millisecond)
	select {
	case <-app.quitCh:
		t.Fatal("the wedged-loop path quit on a key that is not the quit chord")
	default:
	}
}

// A healthy loop keeps the chord to itself. The poll goroutine must not become
// a second, racing exit path, or a healthy UI would quit on Ctrl+C before
// handleKey could abort the live turn first. onQuit is app.Quit() here, which
// is what cmd wires (tui.go) — the chord is only an exit because something
// closes quitCh.
func TestQuitChordIsNotServedOffTheLoopWhileItIsHealthy(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	quit := make(chan struct{}, 1)
	app.SetHandlers(func(string) {}, func() {}, func() { quit <- struct{}{}; app.Quit() })

	done := make(chan struct{})
	go func() { defer close(done); app.Run() }()
	scr.InjectKey(tcell.KeyCtrlC, 0, tcell.ModCtrl)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit after the quit chord")
	}
	select {
	case <-quit:
	default:
		t.Fatal("the chord did not reach the quit handler on a healthy loop")
	}
}

// The exit budget is the half that ends a session nobody can walk out of. The
// watchdog restores the terminal and exits; the exit is stubbed to a panic
// here so the decision is assertable in-process. The restore must be the one
// the app was given — the same scr.Fini the signal and panic guards use — or
// the shell is left raw with the alt screen up.
func TestWatchdogGivesUpOnALoopThatNeverRecovers(t *testing.T) {
	app, _ := loopApp(80, 24)
	app.SetStallDumpDir(filepath.Join(t.TempDir(), "dumps"))
	app.SetStallExitAfter(150 * time.Millisecond)
	app.beat()

	var restored bool
	app.SetStallRestore(func() { restored = true })
	exited := stubStallExit(t)

	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go app.watchStall(30*time.Millisecond, 10*time.Millisecond, time.Now, stop)

	// The loop is stuck for good: no beat after this one. The stub reports
	// the decision on this channel instead of exiting the test binary.
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the watchdog never gave up on a loop that never beat again")
	}
	if !restored {
		t.Fatal("it gave up without restoring the terminal: the shell would come back raw with the alt screen up")
	}
}

// The give-up clock is per episode: a loop that recovers and hangs again is
// measured from the second hang, and the first must not carry an exit with it.
func TestWatchdogExitClockRestartsAfterRecovery(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dumps")
	app, _ := loopApp(80, 24)
	app.SetStallDumpDir(dir)
	app.SetStallExitAfter(200 * time.Millisecond)
	app.beat()

	exited := stubStallExit(t)
	app.SetStallRestore(func() {})

	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go app.watchStall(30*time.Millisecond, 10*time.Millisecond, time.Now, stop)

	// Hang, then recover inside the budget — and keep beating, which is what
	// a recovered loop does. This must be reported and must NOT end the
	// session: the first hang may not carry its own exit into the next one.
	time.Sleep(120 * time.Millisecond)
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		app.beat()
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-exited:
		t.Fatal("a loop that recovered inside the budget ended the session anyway")
	default:
	}
	// The hang was still reported, or the fix traded the report the whole
	// mechanism exists for.
	entries, err := readDirNames(dir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("a recovered stall was not reported: %v %v", entries, err)
	}
}

// No budget, no exit. stallExitAfter is 0 until cmd arms it, so every other
// caller (tests, other embedders) keeps exactly the old behaviour: dumps, no
// exits.
func TestWatchdogWithoutAnExitBudgetNeverEndsTheSession(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dumps")
	app, _ := loopApp(80, 24)
	app.SetStallDumpDir(dir)
	app.beat() // SetStallExitAfter deliberately not called

	exited := stubStallExit(t)

	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go app.watchStall(30*time.Millisecond, 10*time.Millisecond, time.Now, stop)

	app.mu.Lock()
	time.Sleep(600 * time.Millisecond)
	app.mu.Unlock()
	entries, err := readDirNames(dir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("the dump the watchdog has always written is gone: %v %v", entries, err)
	}
	select {
	case <-exited:
		t.Fatal("an unarmed watchdog ended the session")
	default:
	}
}

// stubStallExit swaps the watchdog's exit for one that reports on the returned
// channel, so a test can assert that the watchdog DECIDED to end a wedged
// session without ending the test binary. A channel rather than a panic: the
// exit runs on the watchdog's goroutine, and a panic there would take the
// whole test binary down instead of failing one test.
func stubStallExit(t *testing.T) <-chan struct{} {
	t.Helper()
	exited := make(chan struct{}, 1)
	orig := exitProcess
	exitProcess = func(int) {
		select {
		case exited <- struct{}{}:
		default:
		}
	}
	t.Cleanup(func() { exitProcess = orig })
	return exited
}
