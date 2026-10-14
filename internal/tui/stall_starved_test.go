package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The decision that keeps a starved session alive, pinned against the exact
// stacks the dumps hold. These two strings are the headers of real files under
// ~/.xdev/agent/dumps (tui-stall-20261013-072623.txt is the 1029s one that
// lines up with `pmset`'s Sleep 07:09:11 → DarkWake 07:26:23).
func TestStackLooksStarvedOnRealDumpHeaders(t *testing.T) {
	starved := []string{
		"goroutine 1 [select]:\ngithub.com/FreePeak/xdev/internal/tui.(*App).Run(0x14000fe5608)\n\t.../app.go:3202 +0x140\n",
		"goroutine 1 [runnable]:\ngithub.com/gdamore/tcell/v2.(*tScreen).drawCell(...)\n\t.../tscreen.go:497 +0xbb0\n",
	}
	wedged := []string{
		// A loop parked in the tty write: only a kill ends this one.
		"goroutine 1 [syscall]:\nsyscall.write(...)\n\t.../syscall.syscall:16\n",
		// A loop holding a mutex the watchdog itself wants.
		"goroutine 1 [sync.Mutex.Lock]:\nsync.(*Mutex).lockSlow(0x14000d9c1a0)\n",
	}
	for _, s := range starved {
		if !stackLooksStarved(s) {
			t.Errorf("a starved loop read as wedged (it would be killed):\n%s", s)
		}
	}
	for _, s := range wedged {
		if stackLooksStarved(s) {
			t.Errorf("a wedged loop read as starved (it would never be exited):\n%s", s)
		}
	}
}

// An idle loop is starved, and the real capture must agree with the real
// process. Asked from another goroutine, because that is the only way the
// answer exists: a stack captured by the loop itself shows it `[running]`.
// Here the test's own goroutine 1 is parked in a `select`, which is the state
// 105 of the long dumps show for App.Run.
func TestLoopStarvedSeesAnIdleLoop(t *testing.T) {
	answer := make(chan bool, 1)
	go func() {
		time.Sleep(50 * time.Millisecond) // let goroutine 1 reach its select
		answer <- loopStarved()
	}()
	select {
	case got := <-answer:
		if !got {
			t.Fatal("an idle goroutine 1 did not read as starved: the exit would fire on a healthy session")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("loopStarved did not answer")
	}
}

// A starved loop must NOT be exited, however long it has been condemned: this
// is the resume loop the fix exists for. The watchdog runs to the exit
// threshold and past it, and the exit stub must never be reached — while the
// dump still lands, because holding the exit must not hold the evidence.
func TestWatchdogDoesNotExitAStarvedLoop(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dumps")
	app, _ := loopApp(80, 24)
	app.SetStallDumpDir(dir)
	app.SetStallExitAfter(30 * time.Millisecond)
	app.SetStallRestore(func() {})
	app.beat() // one beat, then silence: the loop looks stalled
	exited := stubStallExit(t)

	prev := loopStarvedFn
	loopStarvedFn = func() bool { return true } // this test's own loop is idle
	t.Cleanup(func() { loopStarvedFn = prev })

	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go app.watchStall(20*time.Millisecond, 5*time.Millisecond, time.Now, stop)

	select {
	case <-exited:
		t.Fatal("a starved loop was exited: the watchdog resume chain starts here")
	case <-time.After(400 * time.Millisecond):
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "tui-stall-") {
			found = true
		}
	}
	if !found {
		t.Fatal("a starved stall wrote no dump; the evidence is the point")
	}
}

// The wedged path still exits: the other half of the same branch, with the
// starvation check reporting "blocked" as it would for a loop in a syscall.
func TestWatchdogStillExitsAWedge(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dumps")
	app, _ := loopApp(80, 24)
	app.SetStallDumpDir(dir)
	app.SetStallExitAfter(30 * time.Millisecond)
	app.SetStallRestore(func() {})
	app.beat()
	exited := stubStallExit(t)

	prev := loopStarvedFn
	loopStarvedFn = func() bool { return false } // a genuinely blocked loop
	t.Cleanup(func() { loopStarvedFn = prev })

	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go app.watchStall(20*time.Millisecond, 5*time.Millisecond, time.Now, stop)

	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("a wedged loop was not exited: the escape hatch stopped working")
	}
}
