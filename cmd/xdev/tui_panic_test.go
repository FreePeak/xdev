//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A panic on a background goroutine is the one death tcell cannot survive: the
// runtime tears the process down where it stands, so no defer runs, `scr.Fini`
// never fires, and the shell comes back to a raw-mode alt screen (no echo, no
// prompt, Ctrl-C dead). That is the panic-path twin of what watchTerminalRoutes
// fixed for SIGTSTP/SIGHUP/SIGTERM (PR #460), from the same 2026-09-28 report —
// session 6917d52f ended mid-turn with a dead terminal and no session_exit.
//
// The contract, same shape as the signal guard's: the terminal is restored AND
// the process still dies loudly. A guard that swallowed the panic would trade a
// visible dead terminal for an invisible lost turn.
//
// A child process is required: the re-panic takes the test binary down with it
// (go test then reports the failure against whatever ran next), and the
// restore has to be observed from outside.
const (
	panicEnv    = "XDEV_PANIC_GUARD_TEST"
	panicMark   = "XDEV_PANIC_GUARD_MARK"
	panicChildT = "TestPanicGuardChildProcess"
)

func TestPanicOnBackgroundGoroutineRestoresTerminal(t *testing.T) {
	mark := filepath.Join(t.TempDir(), "restored")
	cmd := exec.Command(os.Args[0], "-test.run="+panicChildT)
	cmd.Env = append(os.Environ(), panicEnv+"=1", panicMark+"="+mark)
	out := &outBuf{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	waitForFile(t, mark+".ready", 20*time.Second, "the child to arm the guard")

	err := cmd.Wait()
	if err == nil {
		t.Fatalf("child exited cleanly: a panic must still be a crash. output:\n%s", out)
	}
	var ws *exec.ExitError
	if !errors.As(err, &ws) {
		t.Fatalf("child ended with %v, want a non-zero exit. output:\n%s", err, out)
	}
	if code := ws.ExitCode(); code != 2 {
		t.Fatalf("child exit code %d, want 2 (a Go panic). output:\n%s", code, out)
	}

	// The whole point: the restore ran BEFORE the process died, so the shell
	// behind it is usable. Without the guard this file does not exist at all.
	if _, err := os.Stat(mark); err != nil {
		t.Fatalf("the terminal was never restored: %v. output:\n%s", err, out)
	}
	if s := out.String(); !strings.Contains(s, "panic on a background goroutine") {
		t.Errorf("the crash is not reported on stderr, so it would read as a silent kill.\noutput:\n%s", s)
	}
}

// TestPanicGuardChildProcess is the child half: arm the restore, then panic on
// a goroutine. It writes .ready because the parent must not assume the guard is
// installed.
func TestPanicGuardChildProcess(t *testing.T) {
	if os.Getenv(panicEnv) != "1" {
		t.Skip("child process only")
	}
	mark := os.Getenv(panicMark)
	terminalRestore = func() {
		if err := os.WriteFile(mark, []byte("restored"), 0o600); err != nil {
			os.Exit(3)
		}
	}
	if err := os.WriteFile(mark+".ready", []byte("1"), 0o600); err != nil {
		os.Exit(5)
	}
	goGuarded(func() { panic("boom from a background goroutine") })
	// The guard re-panics, which takes the whole process down. Blocking here
	// makes sure the runtime, not this test, is what ends it.
	select {}
}

// outBuf collects the child's stdout and stderr at once; os/exec runs two copy
// goroutines, so the buffer needs a lock.
type outBuf struct {
	mu sync.Mutex
	b  []byte
}

func (o *outBuf) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.b = append(o.b, p...)
	return len(p), nil
}

func (o *outBuf) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return string(o.b)
}
