//go:build !windows

package eval

import (
	"os/exec"
	"syscall"
)

// prepareProcessGroup puts the kernel in its own process group so a cell
// timeout can interrupt or kill the cell together with its children.
func prepareProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// interruptGroup delivers SIGINT to the kernel group: python raises
// KeyboardInterrupt inside the running cell, so the kernel survives to serve
// the next cell.
func interruptGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
}

// killGroup delivers SIGKILL to the kernel group.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
