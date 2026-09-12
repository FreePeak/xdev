//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

// prepareProcessGroup puts the child in its own process group so a
// service launched through a shell (npm run dev, a watcher) can be
// signalled as a tree: killing only the shell leaves the real process
// holding our stdout pipe open, which stalls the reaper.
func prepareProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup sends sig to the child's process group (Setpgid makes
// the child its own group leader, so -pgid reaches its descendants too).
func killProcessGroup(pgid int, sig syscall.Signal) {
	_ = syscall.Kill(-pgid, sig)
}

var (
	procSignalTerm = syscall.SIGTERM
	procSignalInt  = syscall.SIGINT
	procSignalKill = syscall.SIGKILL
)
