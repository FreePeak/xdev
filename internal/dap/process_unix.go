//go:build !windows

package dap

import (
	"os/exec"
	"syscall"
)

// prepareProcessGroup puts the adapter in its own SESSION, so a teardown reaches
// the debuggee too (debugpy and lldb-dap spawn the inferior as a child; killing
// only the adapter would leave it running) and the adapter's own commands
// cannot be stopped by the TUI's terminal.
//
// A new process group inside the same session keeps the TUI's terminal as the
// child's controlling terminal, which makes it a background process group on
// it: any command that changes terminal attributes — an interactive `zsh -ic`,
// a `source ~/.zshrc` reaching zle/stty, `stty` — takes SIGTTOU, whose default
// disposition is STOP, and a stopped process holds its pipes open.
//
// Setsid alone, never Setsid+Setpgid: after setsid(2) the child is already a
// group leader with pgid == pid, so killProcessGroup(-pid) still reaches its
// descendants; asking for Setpgid as well fails on darwin with EPERM.
func prepareProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func killProcessGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
