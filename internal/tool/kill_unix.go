//go:build !windows

package tool

import (
	"os/exec"
	"syscall"
)

// prepareProcessGroup makes the child its own process-group leader so
// signals reach its descendants.
func prepareProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup sends sig to the child's process group (Setpgid makes
// the child its own group leader, so -pgid reaches its descendants too).
func killProcessGroup(pgid int, sig syscall.Signal) {
	_ = syscall.Kill(-pgid, sig)
}

// signalTerm / signalKill are the portable aliases bash.go uses.
var (
	signalTerm = syscall.SIGTERM
	signalKill = syscall.SIGKILL
)
