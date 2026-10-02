//go:build !windows

package main

import "syscall"

// bgSysProcAttr puts the child in a new session so closing the parent's
// terminal (SIGHUP to the foreground process group) does not kill it.
func bgSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}