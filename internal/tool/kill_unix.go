//go:build !windows

package tool

import "syscall"

// killProcessGroup sends sig to the child's process group (Setpgid makes
// the child its own group leader, so -pgid reaches its descendants too).
func killProcessGroup(pgid int, sig syscall.Signal) {
	_ = syscall.Kill(-pgid, sig)
}
