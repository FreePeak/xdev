//go:build windows

package main

import "syscall"

// Windows has no new-session concept: syscall.SysProcAttr there has no
// Setsid field, so the whole concept — and with it the reason spawnBg
// refuses --bg on this platform — is Unix-only. nil means "default
// attributes", which is the closest a Windows build can get and is never
// reached: spawnBg returns before calling this.
func bgSysProcAttr() *syscall.SysProcAttr { return nil }