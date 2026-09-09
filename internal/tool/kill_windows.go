//go:build windows

package tool

// killProcessGroup is a no-op on windows: cmd /c children have no
// portable process-group signal; ctx cancellation terminates the child
// directly via CommandContext.
func killProcessGroup(pgid int, sig syscall.Signal) {
	_ = pgid
	_ = sig
}
