//go:build !windows

package eval

import (
	"os/exec"
	"syscall"
)

// prepareProcessGroup puts the kernel in its own SESSION, so a cell timeout can
// interrupt or kill the cell together with its children — and so a cell that
// changes terminal attributes (an interactive `zsh -ic`, a `source ~/.zshrc`
// reaching zle/stty, `stty`) cannot be stopped by the TUI's terminal. A new
// process group inside the same session keeps that terminal as the child's
// controlling terminal, which makes it a background group on it, and the
// kernel delivers SIGTTOU, whose default disposition is STOP.
//
// Setsid alone, never Setsid+Setpgid: after setsid(2) the child is already a
// group leader with pgid == pid, so the -pid signals below still reach it and
// its children; asking for Setpgid as well fails on darwin with EPERM.
func prepareProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
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
