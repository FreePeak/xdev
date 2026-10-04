//go:build !windows

package tool

import (
	"os/exec"
	"syscall"
)

// prepareProcessGroup detaches the child into its own SESSION, not just its
// own process group, and keeps it a group leader so signals reach its
// descendants.
//
// Setpgid alone is not enough, and the gap is the whole "the session
// suspended" report: a new process group inside the SAME session keeps the
// TUI's terminal as the child's controlling terminal, which makes the child a
// BACKGROUND process group on it. Job control then delivers SIGTTOU to any
// command that changes terminal attributes — an interactive `zsh -ic`, a
// `source ~/.zshrc` that reaches zle/stty, `stty`, any full-screen program —
// and STOP is the default disposition. Measured on macOS: `zsh -ic true`
// stopped with SIGTTOU under Setpgid alone and exited 0 under Setsid.
//
// A stopped child is worse than a failed one: it holds the stdout/stderr
// pipes open forever, so runShell's copiers never see EOF, `done` never
// closes, and the tool call hangs until the abort path SIGKILLs it. The
// session looks suspended because the turn is waiting on a process the kernel
// has stopped.
//
// Setsid alone, never Setsid+Setpgid. After setsid(2) the child is already a
// process-group leader with pgid == pid, so killProcessGroup(-pid) still
// reaches its descendants; and asking for Setpgid as well makes exec fail on
// darwin, where setpgid(2) returns EPERM for a session leader (measured: every
// spawn came back "fork/exec /bin/bash: operation not permitted").
func prepareProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// killProcessGroup sends sig to the child's process group (the child is its
// own group leader, so -pgid reaches its descendants too).
func killProcessGroup(pgid int, sig syscall.Signal) {
	_ = syscall.Kill(-pgid, sig)
}

// signalTerm / signalKill are the portable aliases bash.go uses.
var (
	signalTerm = syscall.SIGTERM
	signalKill = syscall.SIGKILL
)
