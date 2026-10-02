//go:build !windows

package main

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/term"
)

// The tty's own settings, captured before tcell takes it, plus an fd to
// restore them through. This is the escape hatch for the exits tcell cannot
// make on its own.
//
// scr.Fini takes tcell's screen mutex, and the UI loop holds that same mutex
// for the whole flush (tcell's Show holds t.Lock across draw, which is where
// the syscall.write happens). So a loop wedged in a write to a terminal that
// has stopped draining — the 2026-10-02 case, 24 of the stall dumps under the
// data dir — also wedges every restore that goes through scr.Fini. On the
// signal path that is what turns a Ctrl-Z into a suspended terminal: the
// handler blocks inside fini() and never re-raises, so the process stays in a
// raw-mode alt screen it can never leave.
//
// tcsetattr is an ioctl: it does not block, does not need tcell's mutex, and
// does not need tcell to be alive. Restoring the settings captured before
// tcell took the tty therefore puts echo, canonical mode and ISIG back even
// when nothing else can run. The alt-screen and cursor modes are emitted as
// writes and go on top as best effort, because the same terminal that refused
// the escape sequences is the one that refused the write.
//
// One fd, captured once and held for the session, which is also why this works
// at all: a restore that had to open the tty at the last moment could fail to.
// tcell holds a second /dev/tty open for the same reason (tty_unix.go Start).
type terminalState struct {
	f     *os.File
	state *term.State
}

// preTcell is armed by captureTerminal before tcell's Init, and read only by
// restoreTerminalFallback.
var preTcell terminalState

// captureTerminal records the terminal's settings and keeps an fd on the
// device. Call it before scr.Init, and before anything else that puts the tty
// into raw mode. Best effort throughout: with nothing recorded there is no
// fallback and every other path (scr.Fini) behaves exactly as before.
func captureTerminal() {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		// No controlling tty of our own (a detached launch, a pipe under a
		// supervisor). tcell would find the terminal on the std streams in
		// that case, so look there too.
		for _, c := range []*os.File{os.Stdout, os.Stdin, os.Stderr} {
			if !term.IsTerminal(int(c.Fd())) {
				continue
			}
			if f, err = os.OpenFile(c.Name(), os.O_RDWR, 0); err == nil {
				break
			}
		}
	}
	if f == nil || err != nil {
		return
	}
	state, err := term.GetState(int(f.Fd()))
	if err != nil {
		_ = f.Close()
		return
	}
	preTcell = terminalState{f: f, state: state}
}

// restoreTerminalFallback puts the tty back without asking tcell. It is the
// second half of restoreTerminalBounded, and it runs only after the tcell-owned
// restore failed to return — reason goes on stderr so the one line the user
// can still read names what it was rescuing.
func restoreTerminalFallback(reason string) {
	if preTcell.f == nil || preTcell.state == nil {
		return
	}
	// Settings first. This is the part that gives the shell its echo, its
	// line editing and its working Ctrl-C, and an ioctl cannot block.
	if err := term.Restore(int(preTcell.f.Fd()), preTcell.state); err != nil {
		fmt.Fprintf(os.Stderr, "xdev: tty settings restore failed: %v\n", err)
		return
	}
	// The terminal modes that only exist as output: leave the alt screen,
	// show the cursor, drop attributes. Best effort, and bounded — this is
	// a write to the terminal that was just refusing writes.
	const escape = "\x1b[?1049l\x1b[?25h\x1b[m\x1b[0m\r\n"
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = preTcell.f.WriteString(escape)
	}()
	select {
	case <-done:
	case <-time.After(escapeGrace):
		fmt.Fprintf(os.Stderr,
			"xdev: alt screen exit did not reach the terminal (%s) — the tty is usable, redraw with `reset`\n",
			reason)
	}
}

// releaseTerminal hands back the fd captured by captureTerminal. Called when
// the TUI is done with the tty, on the same path that runs scr.Fini normally:
// a fallback restore still needs it, so it outlives every early return that
// does not restore.
func releaseTerminal() {
	if preTcell.f != nil {
		_ = preTcell.f.Close()
		preTcell = terminalState{}
	}
}
