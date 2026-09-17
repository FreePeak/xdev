//go:build windows

package session

import "os"

// checkSessionLinks cannot do its job on windows: NTFS has hard links, but Go's
// FileInfo exposes no link count there, and inventing one would refuse sessions
// over a fact the platform never told us. The append is still guarded by the
// committed-length check in the store, which is platform-independent.
func checkSessionLinks(path string) error {
	if _, err := os.Stat(path); err != nil {
		return err
	}
	return nil
}
