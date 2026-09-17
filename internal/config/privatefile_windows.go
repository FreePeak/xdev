//go:build windows

package config

import (
	"os"
)

// verifySecretFile is a no-op on windows: NTFS ACLs are the platform's
// mechanism, and Go's FileInfo reports a write-protected file as read-only and
// everything else as 0666 — so a POSIX mode check here would refuse every
// real credentials file while telling the user nothing about who can read it.
// The link count is not portable either.
func verifySecretFile(path string, fi os.FileInfo) error { return nil }

// enforceDirPrivacy creates the directory with the requested mode and leaves
// the ACLs to the parent (typically the user profile, already private).
func enforceDirPrivacy(path string, mode os.FileMode) error {
	return os.MkdirAll(path, mode)
}
