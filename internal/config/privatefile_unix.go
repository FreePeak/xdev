//go:build !windows

package config

import (
	"fmt"
	"os"
	"syscall"
)

// verifySecretFile re-checks the guarantees a secret file has to hold between
// writes, at every read (#123, the pattern fx checks at use time rather than
// at write time). Writing 0600 once proves nothing about the bytes now: an
// editor that round-trips the file, a sync tool, a restore from a tarball or a
// careless chmod all change the answer.
//
//   - mode 0600: owner read/write only.
//   - nlink == 1: a hard link is a second path to the same secret with its own
//     permissions, and it reads exactly like the original — so the count is
//     what says "only the path I checked can reach these bytes".
//
// A refusal is deliberate and names the repair: the alternative is trusting a
// secret an unknown number of other paths can reach.
func verifySecretFile(path string, fi os.FileInfo) error {
	if perm := fi.Mode().Perm(); perm != 0o600 {
		return fmt.Errorf("%s is mode %04o, not 0600 — repair with: chmod 600 %s", path, perm, path)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
		return fmt.Errorf("%s has %d hard links, so another path reaches the same secret — remove the extra link and re-create the file", path, st.Nlink)
	}
	return nil
}

// enforceDirPrivacy tightens the data directory to 0700. It is not part of the
// read-time refusal: a 0755 directory leaks the *names* inside (which
// providers you are authenticated to), not the contents, and existing installs
// were created 0755, so this repairs on write instead of bricking on read.
func enforceDirPrivacy(path string, mode os.FileMode) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return os.Chmod(path, 0o700)
	}
	return nil
}
