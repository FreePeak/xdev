package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoConflictMarkersInTrackedFiles closes the other half of the gap
// TestNoMergeMarkersInSource leaves open. That test guards the strings this
// package prints; it globs cmd/xdev/*.go and knows nothing about the rest of
// the tree. So a real unresolved-conflict artifact shipped in docs/PRD.md on
// 2026-09-17 — a `>>>>>>> eee9219 (feat(tui): …)` line introduced by #311's
// merge and carried by every commit for two days — and nothing in the suite
// could see it. This scans what Git actually tracks, docs included.
//
// Only the three prefixed forms are checked. A bare `=======` is deliberately
// NOT a marker here: it is also a legal setext H1 underline, so flagging it
// would fail on a valid document. An unresolved conflict always writes the
// `<<<<<<<` side too, so the prefixed forms cover the real case without the
// false positive.
func TestNoConflictMarkersInTrackedFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	root := repoTopLevel(t)
	cmd := exec.Command("git", "-C", root, "ls-files", "-z")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("git ls-files: %v", err)
	}

	// Built, not spelled: the literals in this file would otherwise be the
	// first hit the scan reports.
	markers := []string{
		strings.Repeat("<", 7) + " ",
		strings.Repeat("|", 7) + " ",
		strings.Repeat(">", 7) + " ",
	}

	var files int
	for _, rel := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if rel == "" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if bytes.IndexByte(b, 0) >= 0 {
			continue // binary
		}
		files++
		for i, line := range strings.Split(string(b), "\n") {
			for _, m := range markers {
				if strings.HasPrefix(line, m) {
					t.Errorf("%s:%d: unresolved conflict marker %q (%s)", rel, i+1, strings.TrimRight(line, "\r"), m[:7])
				}
			}
		}
	}
	if files == 0 {
		t.Fatal("git ls-files returned nothing to scan; the guard would pass vacuously")
	}
}

// repoTopLevel is the worktree root, not the package directory: `ls-files` run
// from cmd/xdev lists only cmd/xdev, so the scan would miss docs/ entirely —
// the very place the artifact shipped.
func repoTopLevel(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("git rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}
