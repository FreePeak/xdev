//go:build windows

package session

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// openLeaseFile opens (creating it if needed) the 0-byte lease file.
//
// The file is opened with FILE_SHARE_READ|FILE_SHARE_WRITE on purpose: Go's
// os.OpenFile asks for FILE_SHARE_READ only for a read/write handle, so a
// second xdev would fail at OPEN with a sharing violation instead of
// discovering the lock. A sharing violation says nothing about WHO owns the
// session; the lock does.
func openLeaseFile(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

// lockLeaseFile takes an exclusive, non-blocking lock on the first byte of
// the lease file.
//
// LockFileEx is windows' counterpart of flock: the kernel drops the lock
// when the handle closes — including when the process is killed — so a
// crashed holder leaves nothing to reclaim.
func lockLeaseFile(f *os.File) error {
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, ol)
	if err == nil {
		return nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return ErrSessionOwned
	}
	return err
}

// unlockLeaseFile releases the lock. The lease file stays on disk.
func unlockLeaseFile(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
}

// checkLeaseDirPrivate cannot do its job on windows: NTFS has no unix
// owner/mode pair Go exposes, and inventing one would refuse every lease
// over a fact the platform never told us. The lock itself is still
// exclusive, and the lease directory still has to exist.
func checkLeaseDirPrivate(path string, st os.FileInfo) error {
	return nil
}
