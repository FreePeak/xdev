//go:build !windows

package gateway

import "syscall"

// sysProcAttr puts a worker in its own session, so the daemon exiting (or
// receiving a SIGHUP) does not take a running turn with it. The same
// posture `xdev --bg` uses.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
