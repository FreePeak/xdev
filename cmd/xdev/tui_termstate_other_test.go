//go:build !windows && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package main

import "golang.org/x/sys/unix"

// readTermios is the termios read ioctl everywhere else: TCGETS. The BSDs
// have their own file, named in the build tag, because the constant differs
// and does not exist under the other's name.
func readTermios(fd int) (*unix.Termios, error) {
	return unix.IoctlGetTermios(fd, unix.TCGETS)
}
