//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// The signal guard's restore is scr.Fini, and scr.Fini takes tcell's screen
// mutex — the same one the UI loop holds for the whole flush (tcell's Show
// holds t.Lock across draw, and draw is where the syscall.write is). So a UI
// loop wedged in a write to a terminal that has stopped draining takes the
// normal restore down with it, and the guard deadlocks inside the one thing it
// called to clean up. That is the 2026-10-02 report: the signal arrives
// (Ctrl-Z, or the kernel's SIGTTOU on a backgrounded launch), the handler
// blocks inside fini(), the re-raise below it never runs, and the session is
// left in a raw-mode alt screen with no way out — a suspended terminal, and a
// herdr pane dead on arrival.
//
// The fix is restoreTerminalBounded (tui_panic.go): scr.Fini gets restoreGrace
// to finish, and the settings captured before tcell took the tty are restored
// with an ioctl afterwards, which cannot block on a write. Both halves are
// pinned here: the fast path is unchanged, and the fallback survives a real
// parked write on a real pty.
const (
	termEnv    = "XDEV_TERM_GUARD_TEST"
	termChildT = "TestTerminalFallbackChild"
	termDir    = "XDEV_TERM_GUARD_DIR" // the child's evidence goes here
)

// TestBoundedRestoreKeepsTheFastPath is the half that must NOT change: when
// scr.Fini returns promptly the fallback never runs. This is the normal exit
// and the common signal — the case every existing restore test covers.
// The fallback reads preTcell, so a test that wants to see whether it ran
// has to arm or disarm that state — there is no seam to stub. Nothing
// captured is the disarmed case.
func TestBoundedRestoreKeepsTheFastPath(t *testing.T) {
	releaseTerminal()
	if preTcell.state != nil {
		t.Fatal("test hygiene: a previous test left captured tty state behind")
	}

	called := 0
	restoreTerminalBounded(func() { called++ }, "test: fast path")
	if called != 1 {
		t.Fatalf("the tcell-owned restore ran %d times, want 1", called)
	}
	// A nil restore is the shape a panic before the screen existed has: it
	// must not panic, and must not try to restore what it never captured.
	restoreTerminalBounded(nil, "test: no screen")
}

// TestTerminalFallbackSurvivesAWedgedWrite is the regression for the deadlock.
// It runs in a child process, on a real pty, with a real write parked in the
// kernel: the child's output is never drained, the pty buffer fills, and the
// write blocks exactly as a wedged pane's does. The tcell-owned restore is
// then a func that never returns — what scr.Fini does while the UI loop holds
// the screen mutex in that write — and the child must still come back with
// ECHO/ICANON/ISIG restored.
//
// The child's diagnostics go to a file, never to the pty: a stderr write to a
// terminal whose buffer is full is itself blocked, which is precisely the
// failure under test, and a test that hangs on its own logging is worse than
// no test.
func TestTerminalFallbackSurvivesAWedgedWrite(t *testing.T) {
	dir := t.TempDir()
	runner := filepath.Join(dir, "child.sh")
	script := "#!/bin/sh\nexec " + os.Args[0] + " -test.run=" + termChildT + "\n"
	if err := os.WriteFile(runner, []byte(script), 0o700); err != nil {
		t.Fatalf("write runner: %v", err)
	}

	// script allocates a pty and makes it the child's controlling terminal,
	// which is what captureTerminal needs to find. Its own output is a real
	// file, so nothing drains the pty while the child is parked: that is the
	// wedge.
	logPath := filepath.Join(dir, "child.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create log: %v", err)
	}
	defer logFile.Close()

	cmd := exec.Command("/usr/bin/script", "-q", "/dev/null", "/bin/sh", runner)
	cmd.Env = append(os.Environ(), termEnv+"=1", termDir+"="+dir)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Skipf("no pty on this host (%v): the fallback needs a real terminal", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	select {
	case err := <-exited:
		out, _ := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("child ended with %v; log:\n%s", err, out)
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
		out, _ := os.ReadFile(logPath)
		t.Fatalf("the child never finished: the terminal restore blocked on the wedged write.\nlog:\n%s", out)
	}

	// The "restoring the tty without it" line is deliberately NOT asserted:
	// it goes to stderr, and stderr in this test IS the wedged pty, so the
	// child cannot get it out. That is the honest limit of that message on a
	// genuinely wedged terminal — it is best effort. What IS guaranteed, and
	// asserted: the child returns from the bounded restore, and the tty is
	// cooked when it does (it exits non-zero otherwise).
	out, _ := os.ReadFile(filepath.Join(dir, "log"))
	for _, want := range []string{
		"premise: tty raw before the wedge",
		"premise: still raw with a write parked",
		"the tcell-owned restore was entered",
		"restoreTerminalBounded returned",
		"after the fallback the tty is cooked again",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("child log is missing %q; the fallback did not do its whole job.\nlog:\n%s", want, out)
		}
	}
}

// TestTerminalFallbackChild is the child half. It is skipped unless the parent
// set the env, and it exits by code: 0 means the tty came back, anything else
// names the step that failed.
func TestTerminalFallbackChild(t *testing.T) {
	if os.Getenv(termEnv) != "1" {
		t.Skip("child process only")
	}
	dir := os.Getenv(termDir)
	if dir == "" {
		os.Exit(2)
	}
	// Diagnostics off the pty. A write to a full tty buffer blocks, so the
	// child must not be reporting through one.
	logPath := filepath.Join(dir, "log")
	logFile, err := os.Create(logPath)
	if err != nil {
		os.Exit(3)
	}
	say := func(format string, args ...any) {
		fmt.Fprintf(logFile, format+"\n", args...)
		_ = logFile.Sync()
	}

	captureTerminal()
	if preTcell.state == nil || preTcell.f == nil {
		say("no controlling tty: the child has no terminal to wedge")
		os.Exit(4)
	}
	fd := int(preTcell.f.Fd())

	// Put the tty in raw mode the way tcell's Init does.
	if _, err := term.MakeRaw(fd); err != nil {
		say("MakeRaw: %v", err)
		os.Exit(5)
	}
	if !rawStillInEffect(fd) {
		say("premise: tty raw before the wedge")
		os.Exit(6)
	}
	say("premise: tty raw before the wedge")

	// Park writes nobody drains. 64 KiB per write is far past any tty
	// buffer, so this cannot complete by accident, and the loop keeps going
	// in case a write lands partially.
	go func() {
		block := make([]byte, 64<<10)
		for i := range block {
			block[i] = 'x'
		}
		for {
			if _, err := preTcell.f.Write(block); err != nil {
				return
			}
		}
	}()

	// Let the pty buffer fill. Short: the writes are the wedge, not the wait.
	time.Sleep(500 * time.Millisecond)
	if !rawStillInEffect(fd) {
		say("premise: still raw with a write parked")
		os.Exit(7)
	}
	say("premise: still raw with a write parked")

	// What scr.Fini does while the UI loop holds tcell's screen mutex in a
	// blocking write: entered, and never returned from.
	terminalRestore = func() {
		say("the tcell-owned restore was entered")
		_ = logFile.Sync()
		select {}
	}

	// This is the call under test. Before the fix the equivalent code called
	// terminalRestore() inline and the child hung here forever.
	restoreTerminalBounded(terminalRestore, "test: wedged tcell restore")
	say("restoreTerminalBounded returned")

	if rawStillInEffect(fd) {
		say("after the fallback the tty is STILL raw")
		os.Exit(8)
	}
	say("after the fallback the tty is cooked again")

	// Let the fallback's escape write take its grace, so the child does not
	// exit while that goroutine is mid-write to a terminal it is about to
	// lose. Exit 0 is the parent's only pass condition.
	time.Sleep(escapeGrace + 200*time.Millisecond)
	os.Exit(0)
}

// rawStillInEffect reports whether the tty is still in raw mode: ECHO, ICANON
// and ISIG cleared, which is exactly what the shell needs back. Read through
// an ioctl, so it answers while a write is parked — term.State is opaque, and
// the point is to inspect the tty from a goroutine that is not the one
// writing to it.
func rawStillInEffect(fd int) bool {
	t, err := readTermios(fd)
	if err != nil {
		return false
	}
	const cooked = unix.ECHO | unix.ICANON | unix.ISIG
	return t.Lflag&cooked == 0 // raw: none of them set
}
