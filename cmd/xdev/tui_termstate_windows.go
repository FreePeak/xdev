//go:build windows

package main

// Windows has no tcsetattr to capture and no alt screen for the console to be
// stuck on: scr.Fini is the whole restore, and it does not block the way a
// wedged tty write does. The bounded restore in tui_panic.go still applies —
// that file has no build tag, so the panic and signal paths get the grace
// period on this platform too — and the capture half degrades to nothing.
func captureTerminal() {}
func releaseTerminal() {}

// restoreTerminalFallback has nothing to do: no settings were captured, and
// Windows has no SIGTSTP for the suspend path to arrive on.
func restoreTerminalFallback(string) {}
