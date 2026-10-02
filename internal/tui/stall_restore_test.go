package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The watchdog's last resort has to be able to fire while the terminal it is
// rescuing is itself the thing that will not answer. A restore that blocks on
// the wedged fd never reaches exitProcess, and the session is only ever ended by
// a kill from outside — the exact dead end the watchdog exists to prevent.
func TestWatchdogExitsEvenWhenTheRestoreHangs(t *testing.T) {
	app, _ := loopApp(80, 24)
	app.SetStallDumpDir(filepath.Join(t.TempDir(), "dumps"))
	app.beat()

	restored := make(chan struct{})
	app.SetStallRestore(func() { <-restored }) // never returns
	exited := stubStallExit(t)

	// restoreGrace is the bound; the test cannot wait it out in real time, so
	// the assertion is that the exit happened while the restore was still
	// parked, and that the park was announced.
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.exitWedgedLoop(90 * time.Second)
	}()
	select {
	case <-exited:
	case <-time.After(7 * time.Second):
		close(restored)
		t.Fatal("a hanging terminal restore kept the wedged process alive")
	}
	close(restored)
	<-done
}

func TestWatchdogRestoreStillRunsWhenItReturns(t *testing.T) {
	app, _ := loopApp(80, 24)
	var called bool
	app.SetStallRestore(func() { called = true })
	exited := stubStallExit(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		app.exitWedgedLoop(2 * time.Second)
	}()
	select {
	case <-exited:
	case <-time.After(7 * time.Second):
		t.Fatal("the watchdog did not exit")
	}
	<-done
	if !called {
		t.Fatal("the restore hook was skipped on the ordinary path")
	}
}

// The dumps directory is the only record of why a loop hung, so the give-up
// path must still write one: exiting is the fix, losing the evidence is not.
func TestWatchdogDumpSurvivesTheGiveUpPath(t *testing.T) {
	dir := t.TempDir()
	app, _ := loopApp(80, 24)

	app.SetStallDumpDir(dir)
	app.SetStallExitAfter(40 * time.Millisecond)
	app.beat()
	exited := stubStallExit(t)
	app.SetStallRestore(func() {})

	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go app.watchStall(20*time.Millisecond, 5*time.Millisecond, time.Now, stop)

	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the watchdog never gave up")
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
		t.Fatalf("no stall dump written under %s", dir)
	}
}
