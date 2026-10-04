//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

// prepareProcessGroup puts the child in its own SESSION, so signals reach a
// service as a tree (killing only the shell leaves the real process holding
// our stdout pipe open, which stalls the reaper) AND the service's commands
// cannot be stopped by the TUI's terminal.
//
// A new process group inside the same session keeps the TUI's terminal as the
// child's controlling terminal, which makes it a background process group on
// it: any command that changes terminal attributes — an interactive
// `zsh -ic`, a `source ~/.zshrc` reaching zle/stty, `stty` — takes SIGTTOU,
// whose default disposition is STOP, and a stopped process cannot act on
// SIGTERM, so Stop's "ask, then escalate" could never finish it.
//
// Setsid alone, never Setsid+Setpgid: after setsid(2) the child is already a
// process-group leader with pgid == pid, so killProcessGroup(-pid) still
// reaches its descendants; and asking for Setpgid as well makes exec fail on
// darwin, where setpgid(2) returns EPERM for a session leader.
func prepareProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// killProcessGroup sends sig to the child's process group (the child is its
// own group leader, so -pgid reaches its descendants too).
func killProcessGroup(pgid int, sig syscall.Signal) {
	_ = syscall.Kill(-pgid, sig)
}

var (
	procSignalTerm = syscall.SIGTERM
	procSignalInt  = syscall.SIGINT
	procSignalKill = syscall.SIGKILL
)
