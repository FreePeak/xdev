//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A TUI owns the tty in raw mode (x/term's MakeRaw clears ISIG/ICANON/ECHO)
// plus the alt screen and mouse reporting. runTUI restores all of it exactly
// once, on `defer scr.Fini()`. Every other way out skips that defer — SIGTSTP,
// SIGHUP, SIGTERM, a panic on a non-main goroutine — so the shell comes back to
// a dead terminal: no echo, no prompt, Ctrl-C dead. That is the 2026-09-28
// report (session 6917d52f: the process sat in state T mid-turn, the pane was
// reaped, the session file never got its session_exit).
//
// watchTerminalRoutes answers that by restoring first and then dying on the
// signal, so the death is the point: these run in a child process, because a
// re-raised signal would otherwise take the test binary down with it (go test
// surfaces that later as "signal: hangup" against whatever ran next).
const (
	guardEnv    = "XDEV_GUARD_TEST"
	guardMode   = "XDEV_GUARD_MODE" // "" = guard armed, "stopped" = guard released
	guardMark   = "XDEV_GUARD_MARK" // written by the restore
	guardChildT = "TestGuardChild"
)

// guardHold pins the child's blocking pipe's write end. An *os.File finalizer
// closes an unreferenced fd, so a writer held only in a discarded local would
// be GC'd and the child's read would return EOF before any signal arrived.
var guardHold *os.File

// The whole contract per signal: the terminal is restored, and the process
// still ends on the signal it was sent (a stopped/hung-up job the shell can
// report) rather than vanishing. The released-guard case is the other half: a
// late signal belongs to the process, not to a screen already restored.
func TestGuardRestoresThenDiesOnSignal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sig   syscall.Signal
		stop  bool // the child is stopped, not killed; cmd.Wait never returns
		never bool // the restore must NOT run
		mode  string
	}{
		{name: "stop", sig: syscall.SIGTSTP, stop: true},
		{name: "hangup", sig: syscall.SIGHUP},
		{name: "terminate", sig: syscall.SIGTERM},
		{name: "after-stop", sig: syscall.SIGTERM, never: true, mode: "stopped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mark := filepath.Join(t.TempDir(), "restored")
			child := startGuardChild(t, mark, tc.mode)
			defer child.kill()

			child.signal(t, tc.sig)

			if tc.stop {
				// Before the fix the child stopped inside its own signal
				// handler's absence: the restore never ran, which is the whole
				// bug. Afterwards the restore always completes, and the
				// re-raised stop may or may not land — POSIX discards a stop
				// for an orphaned process group, and go test's child is in
				// one. So the assertion is the restore plus "did not die",
				// which holds either way and is what the user sees.
				child.expectRestore(t, mark, !tc.never)
				child.expectAlive(t, 500*time.Millisecond)
				return
			}
			err := child.wait(10 * time.Second)
			var ws *exec.ExitError
			if !errors.As(err, &ws) {
				t.Fatalf("child ended with %v, want death by %v", err, tc.sig)
			}
			st := ws.Sys().(syscall.WaitStatus)
			if !st.Signaled() || st.Signal() != tc.sig {
				t.Fatalf("child died by %v, want %v", st, tc.sig)
			}
			child.expectRestore(t, mark, !tc.never)
		})
	}
}

// TestGuardChildProcess is the child half: install the guard with a restore
// that records itself, then block for the signal. It writes a .ready file
// because the parent must not signal before Notify is armed.
func TestGuardChildProcess(t *testing.T) {
	if os.Getenv(guardEnv) != "1" {
		t.Skip("child process only")
	}
	mark := os.Getenv(guardMark)
	stop := watchTerminalRoutes(func() {
		if err := os.WriteFile(mark, []byte("restored"), 0o600); err != nil {
			os.Exit(3)
		}
	})
	if stop == nil {
		os.Exit(4)
	}
	if os.Getenv(guardMode) == "stopped" {
		stop()
		stop() // idempotent: runTUI's defer must be safe to call twice
	}
	if err := os.WriteFile(mark+".ready", []byte("1"), 0o600); err != nil {
		os.Exit(5)
	}
	// Block on a pipe nobody writes to, not on stdin (go test hands the child
	// /dev/null, where a read returns at once) and not on a sleep (the child
	// must be alive and idle when the signal lands).
	pr, pw, _ := os.Pipe()
	if pr == nil || pw == nil {
		os.Exit(6)
	}
	guardHold = pw
	_, _ = pr.Read(make([]byte, 1))
	os.Exit(0)
}

// guardChild is one running instance of TestGuardChildProcess.
type guardChild struct {
	cmd    *exec.Cmd
	exited chan error
	done   bool
}

func startGuardChild(t *testing.T, mark, mode string) *guardChild {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run="+guardChildT)
	cmd.Env = append(os.Environ(), guardEnv+"=1", guardMode+"="+mode, guardMark+"="+mark)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	c := &guardChild{cmd: cmd, exited: make(chan error, 1)}
	go func() { c.exited <- cmd.Wait() }()
	// The child must not be signalled before Notify is armed, and .ready is
	// written after arming.
	waitForFile(t, mark+".ready", 20*time.Second, "the child to arm the guard")
	return c
}

func (c *guardChild) signal(t *testing.T, sig syscall.Signal) {
	t.Helper()
	if err := c.cmd.Process.Signal(sig); err != nil {
		t.Fatalf("signal child: %v", err)
	}
}

// wait reaps the child, marking it so kill() does not wait twice.
func (c *guardChild) wait(d time.Duration) error {
	if c.done {
		return nil
	}
	select {
	case err := <-c.exited:
		c.done = true
		return err
	case <-time.After(d):
		return nil
	}
}

// expectAlive fails when the child exits inside d: a stop the shell can report
// is a job still running.
func (c *guardChild) expectAlive(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case err := <-c.exited:
		c.done = true
		t.Fatalf("child exited (%v) instead of stopping: job control would report nothing", err)
	case <-time.After(d):
	}
}

func (c *guardChild) kill() {
	if c.done {
		return
	}
	// A stopped child never reaps itself; wake it, then take it down.
	_ = c.cmd.Process.Signal(syscall.SIGCONT)
	_ = c.cmd.Process.Kill()
	<-c.exited
	c.done = true
}

// expectRestore asserts the restore marker within a bounded wait. The restore
// and the re-raise race inside the handler, so a negative case is only
// conclusive after the child is gone — which the caller has arranged.
func (c *guardChild) expectRestore(t *testing.T, mark string, want bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(mark); err == nil {
			if !want {
				t.Fatal("the restore ran after stop(): the terminal was already restored")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if want {
		t.Fatal("the terminal was never restored")
	}
}

func waitForFile(t *testing.T, path string, d time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s (%s)", d, what, filepath.Base(path))
}
