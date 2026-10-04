//go:build !windows

package lsp

import (
	"os/exec"
	"syscall"
)

// prepareProcessGroup puts the child in its own SESSION, so a shutdown reaches
// the server's descendants too (rust-analyzer spawns some) and the server's own
// commands cannot be stopped by the TUI's terminal.
//
// A new process group inside the same session keeps the TUI's terminal as the
// child's controlling terminal, which makes it a background process group on
// it: any command that changes terminal attributes — an interactive `zsh -ic`,
// a `source ~/.zshrc` reaching zle/stty, `stty` — takes SIGTTOU, whose default
// disposition is STOP, and a stopped process holds its pipes open, so the
// reader waiting on them never returns.
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
