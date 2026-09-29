package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadContextFilesNeverStarvesTheClosestFile is the correctness guard.
//
// The chain shared one 32 KiB budget, allocated strictly in load order: the
// global file first, then root→cwd. A developer with a fat personal rulebook
// therefore got the repository's own AGENTS.md silently deleted from the
// prompt — measured at 64 KiB of global rules: 32,871 bytes rendered, repo
// file absent, no error, no marker. The one file whose rules cannot be
// re-read on demand was the one a fat unrelated file could evict.
//
// The reserved floor is the fix; this test is what keeps it from being undone
// by a plausible-looking "simplify the budgeting" change.
func TestLoadContextFilesNeverStarvesTheClosestFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	agentDir := filepath.Join(home, ".xdev", "agent")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A global rulebook several times the whole chain budget.
	fat := strings.Repeat("# global\n\nA personal convention worth its bytes.\n", 2000)
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte(fat), 0o600); err != nil {
		t.Fatal(err)
	}

	repo := t.TempDir()
	const rule = "- Every exported function carries a one-line JSDoc."
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte(rule), 0o600); err != nil {
		t.Fatal(err)
	}

	got := LoadContextFiles(repo)
	if !strings.Contains(got, rule) {
		t.Fatalf("the cwd's own AGENTS.md was starved out of the prompt by the global file:\n%s",
			got[:min(len(got), 600)])
	}
	if !strings.Contains(got, "Global conventions") {
		t.Error("global file is missing entirely — the fix must be a share, not an exclusion")
	}
}

// TestLoadContextFilesAnnouncesTruncation guards the other half of the fix.
// A rule that is cut is a rule the model cannot see, so the cut has to be
// visible: the agent must be able to tell "this file says nothing about my
// task" from "this file says something I was not shown".
func TestLoadContextFilesAnnouncesTruncation(t *testing.T) {
	// HOME is redirected so the developer's real global AGENTS.md is not
	// measured inside this file's slice.
	t.Setenv("HOME", t.TempDir())
	repo := t.TempDir()
	huge := strings.Repeat("# section\n\nA line of convention text.\n", 4000)
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte(huge), 0o600); err != nil {
		t.Fatal(err)
	}

	got := LoadContextFiles(repo)
	if !strings.Contains(got, "truncated") {
		t.Fatal("a truncated file rendered with no marker — the omitted rules are now invisible")
	}
	if !strings.Contains(got, "not shown") {
		t.Error("the marker does not say that content was withheld")
	}
	// The path belongs in the marker: the agent has to be able to go read it.
	if !strings.Contains(got, filepath.Join(repo, "AGENTS.md")) {
		t.Error("the marker does not name the file, so the agent cannot act on it")
	}
	// The per-file cap is the bound. This is the cwd's own file, so the bound
	// is the reserved floor, not the generic share.
	bound := MaxContextFileKB + MinClosestFileBytes + 512
	if n := len([]rune(got)); n > bound {
		t.Errorf("rendered %d runes, past the %d closest-file share + marker", n, bound)
	}
	if strings.Contains(got, "A line of convention text\nA line of") {
		t.Error("the cut landed mid-line")
	}
}

// TestContextBytesForFile pins the allocation itself, including the two cases
// that were wrong in the first implementation: a small file must never be
// dropped just because the outer budget is spent, and the closest file must
// get more than a generic file.
func TestContextBytesForFile(t *testing.T) {
	t.Run("generic file", func(t *testing.T) {
		if got := contextBytesForFile(0, 1<<20, false); got != MaxContextFileKB {
			t.Errorf("share = %d, want %d", got, MaxContextFileKB)
		}
	})
	t.Run("closest file keeps a floor", func(t *testing.T) {
		got := contextBytesForFile(0, 1<<20, true)
		if got != MaxContextFileKB+MinClosestFileBytes {
			t.Errorf("share = %d, want %d", got, MaxContextFileKB+MinClosestFileBytes)
		}
	})
	t.Run("never more than the file has", func(t *testing.T) {
		if got := contextBytesForFile(0, 100, false); got != 100 {
			t.Errorf("share = %d, want the file's own 100", got)
		}
	})
	t.Run("never past the outer budget", func(t *testing.T) {
		if got := contextBytesForFile(MaxContextBytes-300, 1<<20, true); got != 300 {
			t.Errorf("share = %d, want the 300 bytes left in the chain budget", got)
		}
	})
}
