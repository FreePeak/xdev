package main

import (
	"fmt"
	"os"
	"runtime/debug"
	"time"
)

// A panic on a background goroutine is unrecoverable by a recover() in main():
// the runtime tears the process down where it stands, running no defer, so
// tcell's Fini never fires and the shell comes back to a raw-mode alt screen —
// no echo, no prompt, Ctrl-C dead. A panic on the MAIN goroutine unwinds
// through runTUI's own `defer scr.Fini()` and is already restored, which is why
// the guard belongs here and not in main.
//
// That is the half of the 2026-09-28 report (session 6917d52f) that PR #460
// did not cover: #460 owns the signal paths, this owns the panic path. Both
// runTUI goroutines that a turn owns — the turn itself, the title pass, hub
// and schedule delivery, the collab guest, the bang runner — are the way a
// panic reaches a place the UI loop cannot recover from.
//
// The guard restores and then RE-PANICS, so a crash still reads as a crash:
// the stack is printed and the exit is non-zero. Swallowing it, or turning it
// into a tidy error, would trade a visible dead terminal for an invisible lost
// turn.
var terminalRestore func() // set by runTUI while tcell owns the tty

// goGuarded runs f on its own goroutine, restoring the terminal before a
// panic takes the process down. One call site replacement, no new machinery:
// every `go func() { … }()` in the TUI's session wiring becomes
// `goGuarded(func() { … })`.
//
// ponytail: this restores the tty but cannot resume the UI loop, so the
// session is still lost — a panic is not a survivable turn. The ceiling is
// deliberate; the upgrade path is per-goroutine recovery at the panic site
// once a caller that can genuinely continue exists.
func goGuarded(f func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				restoreTerminal("panic on a background goroutine")
				fmt.Fprintf(os.Stderr, "xdev: panic on a background goroutine: %v\n%s\n", r, debug.Stack())
				panic(r)
			}
		}()
		f()
	}()
}

// restoreGrace bounds the tcell-owned restore. The reason it must be bounded
// is the 2026-10-02 failure: the UI loop is usually wedged inside the
// terminal write itself (24 of the dumps under the data dir name
// tcell's draw → syscall.write), and scr.Fini writes to that same fd under
// the same screen mutex. An unbounded restore blocks on exactly what it is
// rescuing the terminal from, so on the signal path the handler never
// re-raises and the process is left in a raw-mode alt screen with no way out —
// a suspended terminal, which is what the user reported.
const restoreGrace = 2 * time.Second

// escapeGrace bounds the fallback's alt-screen write. Same reasoning, one
// layer down: the terminal that is refusing writes is the one being asked to
// leave the alt screen, so the settings restore stands on its own and only the
// cosmetic escape sequences are allowed to fail.
const escapeGrace = 500 * time.Millisecond

// restoreTerminalBounded puts the tty back through fini, giving it
// restoreGrace to finish. Two attempts, in order, because either one can be
// the thing that blocks: scr.Fini normally (it also leaves the alt screen and
// the mouse reporting the way tcell set them up), then — if that did not
// return in time — the settings captured before tcell took the tty, restored
// with an ioctl that cannot block (tui_termstate_unix.go). The second attempt
// is what makes a wedged terminal usable again; the first is what keeps the
// normal exit exactly as it was.
//
// reason names what was being rescued, for the one stderr line the user can
// read once the screen is back.
func restoreTerminalBounded(fini func(), reason string) {
	if fini == nil {
		return
	}
	restored := make(chan struct{})
	go func() {
		defer close(restored)
		fini()
	}()
	select {
	case <-restored:
		return
	case <-time.After(restoreGrace):
		fmt.Fprintf(os.Stderr,
			"xdev: terminal restore did not return in %s (%s); restoring the tty without it\n",
			restoreGrace, reason)
	}
	restoreTerminalFallback(reason)
}

// restoreTerminal is the panic path's name for it: the restore runTUI armed
// while tcell owns the tty, or nothing at all when the panic arrived before
// the screen existed.
func restoreTerminal(reason string) { restoreTerminalBounded(terminalRestore, reason) }
