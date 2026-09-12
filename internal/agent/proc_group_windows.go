//go:build windows

package agent

import (
	"os/exec"
	"syscall"
)

// prepareProcessGroup is a no-op on windows: there is no Setpgid, and the
// child is terminated directly.
func prepareProcessGroup(cmd *exec.Cmd) { _ = cmd }

// killProcessGroup is a no-op on windows; Stop falls back to
// Process.Kill, which terminates the child directly.
func killProcessGroup(pgid int, sig syscall.Signal) {
	_ = pgid
	_ = sig
}

var (
	procSignalTerm = syscall.Signal(0)
	procSignalInt  = syscall.Signal(0)
	procSignalKill = syscall.Signal(0)
)
