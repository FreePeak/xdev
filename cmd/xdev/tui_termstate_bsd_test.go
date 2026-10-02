//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package main

import "golang.org/x/sys/unix"

// readTermios is the termios read ioctl on the BSDs, where it is TIOCGETA.
// x/term makes the same split for its own read (term_unix_bsd.go), so the
// test asks the tty the same way the code under test does.
func readTermios(fd int) (*unix.Termios, error) {
	return unix.IoctlGetTermios(fd, unix.TIOCGETA)
}
