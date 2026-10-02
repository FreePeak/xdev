//go:build !windows

package tui

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/theme"
	"github.com/gdamore/tcell/v2"
)

// The reported dead end (session 00b1c5a0, and 12 more dumps in
// ~/.xdev/agent/dumps): the UI loop blocked inside tcell's frame write —
// goroutine 1 [syscall] → devTty.Write at tty_unix.go:53 — because the process
// above the pane stopped draining it. Keys, Ctrl+C and output all stop while
// the process stays alive, and only the stall watchdog ended the session, by
// restoring the terminal and exiting: a session that died mid-turn.
//
// Two halves, each failing against the line it guards: the frame write is
// bounded (deadlineTty.Write), and a dropped frame is repaired in full rather
// than left half-painted (App.draw's Sync).

// deadWriter is a tty whose write does not complete on its own: a pty slave
// whose master nobody reads. block is the pre-fix shape (a write with no
// deadline at all); err is the bounded shape (what a deadline returns).
type deadWriter struct {
	mu       sync.Mutex
	deadline time.Time
	block    chan struct{}
	err      error
	// fails is how many more writes time out before the terminal counts as
	// draining again; err alone would fail every frame forever, which is a
	// stall that never recovers rather than one that repairs itself.
	fails int
}

func (w *deadWriter) SetWriteDeadline(t time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deadline = t
	return nil
}

func (w *deadWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	blk, err := w.block, w.err
	if err != nil {
		if w.fails > 0 {
			w.fails--
		} else {
			err = nil // the terminal drained: this frame lands
		}
	}
	w.mu.Unlock()
	if blk != nil {
		<-blk // reachable only when nothing bounded the write
	}
	return 0, err
}

func (w *deadWriter) Close() error { return nil }

func (w *deadWriter) bounded() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return !w.deadline.IsZero()
}

// countingScreen is the simulation screen with tcell's synchronous frame
// write kept in the loop: Show() routes the frame through the bounded writer,
// and Sync() is counted so the repair a dropped frame needs is observable.
type countingScreen struct {
	tcell.SimulationScreen
	syncs int
	tty   *deadlineTty
}

// Show is where the frame lands: tScreen.draw ends in one buf.WriteTo(tty),
// which is exactly the write that wedged. Here it goes through deadlineTty.
func (c *countingScreen) Show() {
	if c.tty != nil {
		_, _ = c.tty.Write([]byte(strings.Repeat("x", 4096)))
	}
	c.SimulationScreen.Show()
}

func (c *countingScreen) Sync() {
	c.syncs++
	c.SimulationScreen.Sync()
}

// deadlineApp is a screen whose frame writes go through a deadlineTty, wired
// exactly as cmd/xdev/tui.go wires it.
func deadlineApp(t *testing.T, w writeDeadliner) (*App, *countingScreen, *deadlineTty) {
	t.Helper()
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatal(err)
	}
	d := &deadlineTty{w: w}
	scr := &countingScreen{SimulationScreen: sim, tty: d}
	app := New(scr, theme.Load("groknight"), "test/free", "sess1234")
	app.width, app.height = 80, 24
	app.AddUserBlock("hi") // non-empty transcript: the main paint path
	app.SetFrameDropped(&d.dropped)
	return app, scr, d
}

// The fix: a frame write to a terminal that never drains returns on the
// deadline instead of blocking the UI loop for as long as the pty stays full.
func TestFrameWriteToAStalledTtyIsBounded(t *testing.T) {
	w := &deadWriter{err: os.ErrDeadlineExceeded, fails: 1} // this write fails
	d := &deadlineTty{w: w}

	if _, err := d.Write([]byte(strings.Repeat("x", 4096))); err == nil {
		t.Fatal("a write that could not land reported success")
	}
	if !w.bounded() {
		t.Error("the write ran without a deadline; nothing bounds it")
	}
	if !d.dropped.Load() {
		t.Error("a write that gave up did not record the drop, so draw() cannot repair it")
	}
}

// The loop keeps running: a terminal that stopped draining costs a frame, not
// a session. This is the reported crash, end to end over the draw path.

func TestStalledTtyDoesNotStopTheUILoop(t *testing.T) {
	// fails:1 = one frame the terminal cannot take, then it drains again.
	w := &deadWriter{err: os.ErrDeadlineExceeded, fails: 1}
	app, scr, d := deadlineApp(t, w)

	done := make(chan struct{})
	go func() {
		defer close(done)
		app.draw() // the frame the terminal cannot take
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("draw() never returned with a stalled tty (dropped=%v)", d.dropped.Load())
	}
	if scr.syncs != 1 {
		t.Fatalf("the dropped frame was not repaired in full: %d Syncs, want 1", scr.syncs)
	}
	// Repaired once, not on every later frame: the flag is consumed.
	app.draw()
	if scr.syncs != 1 {
		t.Fatalf("the drop was repaired on a frame that dropped nothing: %d Syncs, want 1", scr.syncs)
	}
}

// Without the deadline the same write is the reported crash, and the test
// proves the bound is what releases it: the writer stays wedged until the
// deadline fires, and never before.
func TestFrameWriteStaysWedgedUntilTheDeadline(t *testing.T) {
	blocked := make(chan struct{})
	w := &deadWriter{block: blocked}
	d := &deadlineTty{w: w}

	// A real deadline is on the order of frameWriteTimeout; this test
	// observes the pre-deadline half only, so it must stay short.
	stuck := make(chan struct{})
	go func() {
		defer close(stuck)
		_, _ = d.Write([]byte("x"))
	}()
	select {
	case <-stuck:
		t.Fatal("the write returned with nothing to receive it: the shape under test is not exercised")
	case <-time.After(100 * time.Millisecond):
	}
	if !w.bounded() {
		t.Fatal("Write did not set a deadline, so nothing releases a stalled write")
	}
	// What the deadline buys: an unblock ends the write at once.
	close(blocked)
	select {
	case <-stuck:
	case <-time.After(5 * time.Second):
		t.Fatal("the write did not return once the terminal drained")
	}
}

// A screen with no deadline (no /dev/tty, or a platform build without one) must
// not be repainted on every frame: nil is the wired default and means "nothing
// was dropped", not "something was dropped".
func TestNoDeadlineFlagMeansNoRepair(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatal(err)
	}
	scr := &countingScreen{SimulationScreen: sim}
	app := New(scr, theme.Load("groknight"), "test/free", "sess1234")
	app.width, app.height = 80, 24
	app.AddUserBlock("hi")

	app.draw()
	app.draw()
	if scr.syncs != 0 {
		t.Fatalf("an app with no dropped-frame flag repainted %d times", scr.syncs)
	}
}

// A terminal that cannot carry a deadline must still receive its frame. This is
// the shipped macOS shape: /dev/tty is a character device, SetWriteDeadline
// answers os.ErrNoDeadline, and returning that error instead of writing left
// the TUI blank — the screen never entered the alt screen, so no key, no
// prompt and no output ever appeared. A frame lost is worse than the
// unbounded write the deadline was added to bound.
type deadlineLessWriter struct {
	wrote []byte
	err   error
}

func (w *deadlineLessWriter) SetWriteDeadline(time.Time) error { return os.ErrNoDeadline }

func (w *deadlineLessWriter) Write(p []byte) (int, error) {
	w.wrote = append(w.wrote, p...)
	return len(p), w.err
}

func TestFrameWriteLandsOnATtyThatCannotCarryADeadline(t *testing.T) {
	w := &deadlineLessWriter{}
	d := &deadlineTty{w: w}

	n, err := d.Write([]byte("frame"))
	if err != nil {
		t.Fatalf("a frame write to a deadline-less tty failed instead of writing: %v", err)
	}
	if n != len("frame") {
		t.Fatalf("short write: %d of %d bytes", n, len("frame"))
	}
	if string(w.wrote) != "frame" {
		t.Fatalf("the frame never reached the terminal, wrote %q", w.wrote)
	}
	if d.dropped.Load() {
		t.Error("a frame that landed was recorded as dropped, so draw() repaints for nothing")
	}
}

// The same end to end over the draw path: an app on a deadline-less tty must
// actually paint. This is the shipped symptom — the loop ran and every frame
// was refused, so the screen stayed blank with the process alive.
func TestDeadlineLessTtyStillPaints(t *testing.T) {
	w := &deadlineLessWriter{}
	app, scr, _ := deadlineApp(t, w)

	app.draw()
	if len(w.wrote) == 0 {
		t.Fatal("draw() wrote no frame at all: the screen would stay blank")
	}
	if scr.syncs != 0 {
		t.Fatalf("a frame that landed asked for %d repairs", scr.syncs)
	}
}
