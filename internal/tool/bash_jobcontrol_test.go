//go:build !windows

package tool

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A child spawned into its own process group but the SAME session keeps the
// TUI's terminal as its controlling terminal, which makes it a BACKGROUND
// process group on it. Job control then delivers SIGTTOU to any command that
// changes terminal attributes — an interactive `zsh -ic`, a `source ~/.zshrc`
// that reaches zle/stty, `stty`, any full-screen program — and STOP is the
// default disposition. Measured on this Mac through a pty, with the child
// environment internal/tool builds (TERM=dumb, NO_COLOR, LC_ALL=C):
//
//	zsh -ic true        Setpgid → STOP(SIGTTOU)   Setsid → exit 0
//	read x < /dev/tty   Setpgid → STOP(SIGTTIN)   Setsid → exit 1
//
// A stopped child is worse than a failed one: it holds the stdout/stderr pipes
// open, so runShell's copiers never see EOF and the tool call hangs until the
// abort path SIGKILLs it. That is the user report: "xdev session still meets
// the suspended issue when try to update bash in zshrc".
//
// ponytail: asserts the CAUSE — the child's own session id — not a pty
// reproduction. A controlling terminal belongs to a session and only setsid()
// detaches one, so "the child is in another session" IS "our terminal cannot
// stop it". One kernel fact, no pty package, no timing.
//
// The value comes from inside the child (a helper that re-execs this test
// binary) rather than from `ps -o sess` or Getsid on the pid afterwards: by
// then a foreground child is already reaped, and `ps -o sess` reports 0 for
// every process on macOS, which made an earlier version of this assertion
// compare against a constant and pass on the unfixed code.
func TestBashChildIsNotInOurSession(t *testing.T) {
	bt := NewBashTool(t.TempDir())
	res, err := bt.Execute(context.Background(), args(t, map[string]any{
		"command": fmt.Sprintf("%s -test.run=%s", os.Args[0], sessionHelperRun),
		"env":     map[string]string{sessionHelperEnv: "1"},
		"timeout": 60,
	}))
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(strings.SplitN(res.Text, "\n", 2)[0]))
	if err != nil {
		t.Fatalf("child session from %q: %v", res.Text, err)
	}
	ours := sessionOf(0)
	if child == ours {
		t.Fatalf("child session %d is ours (%d): our terminal is still its controlling terminal, so a `zsh -ic` in it takes SIGTTOU and stops", child, ours)
	}
}

const (
	// The helper arms itself through an env var, and it cannot be named XDEV_*:
	// the tool strips every such key from a child's environment (env.go).
	sessionHelperEnv = "JC_SESSION_HELPER"
	sessionHelperRun = "TestBashChildSessionHelper"
)

// TestBashChildSessionHelper is the child half of the assertion above: it
// reports the session it was spawned into, through the tool's own stdout. It is
func TestBashChildSessionHelper(t *testing.T) {
	if os.Getenv(sessionHelperEnv) != "1" {
		t.Skip("helper process only")
	}
	sid, err := unix.Getsid(0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: getsid: %v\n", err)
		os.Exit(2)
	}
	fmt.Println(sid)
}

// TestBashBackgroundChildIsNotInOurSession is the background path: a separate
// spawn (startBackground) and therefore a separate line to get right. Here the
// session is read from outside, while the process is provably still alive.
func TestBashBackgroundChildIsNotInOurSession(t *testing.T) {
	bt := NewBashTool(t.TempDir())
	bt.Jobs = NewBashJobs()
	res, err := bt.Execute(context.Background(), args(t, map[string]any{
		"command":           "sleep 30",
		"run_in_background": true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	d := res.Details.(*bashDetails)
	if !d.Backgrounded || d.PID == 0 {
		t.Fatalf("a background run must report its job: %+v", d)
	}
	if child, ours := sessionOf(d.PID), sessionOf(0); child == ours {
		t.Fatalf("background child session %d is ours (%d): same defect as the foreground path", child, ours)
	}
	// The behavioural half of the same claim, and what a stopped job looks
	// like from the registry: only SIGKILL and SIGCONT wake a stopped process,
	// so a job that stopped ignores SIGTERM and the stop verb's "stopped" is a
	// lie — the state has to stay "running" and then end by signal.
	if _, err := bt.Execute(context.Background(), args(t, map[string]any{"action": "stop", "job": d.JobID})); err != nil {
		t.Fatalf("stop: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		st, err := bt.Execute(context.Background(), args(t, map[string]any{"action": "status", "job": d.JobID}))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(st.Text, "running") {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if !strings.Contains(st.Text, "signal") {
			t.Fatalf("a stopped job reports %q; SIGTERM must have ended it, so it was never stopped", st.Text)
		}
		return
	}
	t.Fatal("the stopped job never reached a terminal state, so its process is stopped")
}

func sessionOf(pid int) int {
	sid, err := unix.Getsid(pid)
	if err != nil {
		return -1
	}
	return sid
}
