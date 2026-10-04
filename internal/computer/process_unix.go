//go:build !windows

package computer

import (
	"os/exec"
	"syscall"
)

// prepareProcessGroup puts the child in its own SESSION, so a cancelled op
// reaches its descendants too: the platform tools are shells and scripting
// hosts (osascript, xdotool) that fork the real work — and so those tools
// cannot be stopped by the TUI's terminal. A new process group inside the same
// session keeps the TUI's terminal as the child's controlling terminal, which
// makes it a background process group on it, and any command that changes
// terminal attributes (an interactive `zsh -ic`, a `source ~/.zshrc` reaching
// zle/stty, `stty`) takes SIGTTOU — whose default disposition is STOP.
//
// Setsid alone, never Setsid+Setpgid: after setsid(2) the child is already a
// group leader with pgid == pid, so killProcessGroup(-pid) still reaches its
// descendants; asking for Setpgid as well fails on darwin with EPERM.
func prepareProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// killProcessGroup SIGKILLs the group and the child itself, so the op dies
// whether or not the group took effect.
func killProcessGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
