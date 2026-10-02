//go:build !windows

package main

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// A TUI owns the tty in raw mode (x/term's MakeRaw clears ISIG/ICANON/ECHO)
// plus the alt screen and mouse reporting. runTUI restores all of it exactly
// once, on `defer scr.Fini()`. Every other way out skips that defer — SIGTSTP,
// SIGHUP, SIGTERM, a panic on a non-main goroutine — so the shell comes back to
// a dead terminal: no echo, no prompt, Ctrl-C dead. That is the 2026-09-28
// report (session 6917d52f: the process sat in state T mid-turn, the pane was
// reaped, the session file never got its session_exit).
//
// A caught signal is treated as a normal exit rather than a default death:
// restore the terminal first, then die on the signal it was sent, so the shell
// still reports a job-control stop instead of a silent disappearance.
//
// SIGTTOU and SIGTTIN are two more ways out, and the pair a background launch
// reaches. They are not the SUSP character: a background process group gets
// SIGTTOU from the kernel the moment it calls tcsetattr, and tcell calls that
// inside Init() — so a TUI started as a background job (`xdev &`, a stray
// `&` in a script) is stopped during Init, before this guard is even armed.
// The default disposition is to STOP, and stopping runs no defer, so the
// terminal stays raw and the alt screen stays up. SIGTTIN is the read-side
// twin. Both are handled exactly like SIGTSTP: restore, then die on the
// signal.
//
// SIGWINCH is deliberately absent: tcell installs its own handler for it
// (tty_unix.go) and a second Notify would starve the screen of resize
// events.
func watchTerminalRoutes(fini func()) (stop func()) {
	sigs := make(chan os.Signal, 4)
	// On a platform without these (Windows has no SIGTSTP/SIGHUP) Notify
	// accepts them silently and simply never fires, so the guard degrades to
	// inert rather than fake: the terminal stays as-is, as it did before.
	signal.Notify(sigs, syscall.SIGTSTP, syscall.SIGHUP, syscall.SIGTERM,
		syscall.SIGTTOU, syscall.SIGTTIN)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case sig := <-sigs:
				// Bounded, because fini() can block: the UI loop holds
				// tcell's screen mutex across the whole flush, so a loop
				// wedged in a write to a terminal that has stopped draining
				// holds it too and fini() waits on the very fd it is
				// rescuing. Unbounded, this handler IS the suspended
				// terminal: Ctrl-Z arrives, the restore never returns, the
				// re-raise below never runs, and the process sits in a
				// raw-mode alt screen with no way out — 2026-10-02, the
				// herdr pane dead on arrival. Bounded, the worst case is
				// restoreGrace spent restoring the tty through the settings
				// captured before tcell took it.
				restoreTerminalBounded(fini, "signal "+sig.String())
				if s, ok := sig.(syscall.Signal); ok {
					// Re-raise after un-notifying, so the process still ends on
					// the signal it was sent (a stopped/hung-up job) — with a
					// terminal behind it that works.
					signal.Stop(sigs)
					_ = syscall.Kill(os.Getpid(), s)
				}
				return
			case <-done:
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			signal.Stop(sigs)
			close(done)
		})
	}
}
