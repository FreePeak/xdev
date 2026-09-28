package main

import (
	"fmt"
	"os"
	"runtime/debug"
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
				restoreTerminal()
				fmt.Fprintf(os.Stderr, "xdev: panic on a background goroutine: %v\n%s\n", r, debug.Stack())
				panic(r)
			}
		}()
		f()
	}()
}

// restoreTerminal puts the tty back the way the shell expects. runTUI arms it
// with scr.Fini; a panic before the screen exists leaves it nil and there is
// nothing to restore.
func restoreTerminal() {
	if terminalRestore != nil {
		terminalRestore()
	}
}
