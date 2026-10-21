//go:build !windows

package session

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// openLeaseFile opens (creating it if needed) the 0-byte lease file.
//
// The lease file records WHO may write a session; it is not a transcript,
// so it is 0600 and never carries content.
func openLeaseFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	// O_CREATE applies the umask, so the mode is set explicitly.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// lockLeaseFile takes an exclusive, non-blocking lock on the lease file.
//
// flock(2) is the kernel's serialisation point: there is no check-then-act
// window, and the kernel drops the lock when the descriptor closes —
// including on SIGKILL — so a crashed holder leaves nothing to reclaim.
func lockLeaseFile(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return ErrSessionOwned
	}
	return err
}

// unlockLeaseFile releases the lock. The lease file stays on disk.
func unlockLeaseFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}

// checkLeaseDirPrivate refuses a lease directory another user could reach:
// it must carry no group/other bits and must belong to this user.
func checkLeaseDirPrivate(path string, st os.FileInfo) error {
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("session: lease dir %s is readable by other users (mode %04o) — a lease another user could take is not a lease", path, perm)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return nil // a filesystem that reports no owner is not claiming one
	}
	if int(sys.Uid) != os.Getuid() {
		return fmt.Errorf("session: lease dir %s is owned by uid %d, not by this user — a lease another user could take is not a lease", path, sys.Uid)
	}
	return nil
}
