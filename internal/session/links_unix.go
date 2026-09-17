//go:build !windows

package session

import (
	"fmt"
	"os"
	"syscall"
)

// checkSessionLinks refuses to resume a session file reachable by more than one
// path (#122). A hard link is a second name for the same bytes: the append-only
// invariant "this file is the history of one session" stops meaning anything,
// because writing through either name mutates the other, and a rename or a
// branch taken in one tree silently appears in the other.
//
// The check runs at open rather than at write so the diagnostic names the
// session the user asked for instead of arriving later as a persistence error,
// and it refuses rather than warns, because the alternative is appending to a
// tree whose identity two paths can disagree about. Listing is unaffected — the
// session still appears among --continue candidates with its header intact — so
// nothing is lost, only not appended to through this handle.
func checkSessionLinks(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("session: stat %s: %w", path, err)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return nil // a filesystem that reports no link count is not claiming one
	}
	if sys.Nlink > 1 {
		return fmt.Errorf("session: %s is reachable by %d paths — a hard-linked session file is not a trustworthy append target, because writing through either name mutates the other; remove the extra link, or resume a private copy",
			path, sys.Nlink)
	}
	return nil
}
