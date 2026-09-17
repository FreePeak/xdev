//go:build windows

package tool

import (
	"os/exec"
	"syscall"
)

// killProcessGroup is a no-op on windows: cmd /c children have no
// portable process-group signal; ctx cancellation terminates the child
// directly via CommandContext.
func killProcessGroup(pgid int, sig syscall.Signal) {
	_ = pgid
	_ = sig
}

// prepareProcessGroup is a no-op on windows: there is no Setpgid, and
// CommandContext terminates the child directly.
func prepareProcessGroup(cmd *exec.Cmd) { _ = cmd }

// signalTerm / signalKill: windows cannot deliver these POSIX signals, so
// the values exist only to satisfy the shared call sites (killProcessGroup
// ignores them and the child dies via CommandContext).
var (
	signalTerm = syscall.Signal(0)
	signalKill = syscall.Signal(0)
)
