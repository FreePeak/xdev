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
	// The global file does not have to SURVIVE to be accounted for: with a
	// rulebook 3x the pool, the right outcome is that it is dropped and named
	// in the budget marker, so the agent can go read it. What must never
	// happen is it being dropped silently — that is the pre-fix failure.
	if !strings.Contains(got, "Global conventions") {
		if !strings.Contains(got, "Rules budget reached") || !strings.Contains(got, agentDir) {
			t.Errorf("the global file is gone and nothing says so — the model cannot know to read it\n%s",
				got[:min(len(got), 600)])
		}
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
	if !strings.Contains(got, "left out") {
		t.Fatal("a truncated file rendered with no marker — the omitted rules are now invisible")
	}
	// The path belongs in the marker: the agent has to be able to go read it.
	if !strings.Contains(got, filepath.Join(repo, "AGENTS.md")) {
		t.Error("the marker does not name the file, so the agent cannot act on it")
	}
	// The per-file cap is the bound on a file's own content; the block adds
	// its own heading and marker on top. This is the cwd's file, so the
	// pool is what bounds it.
	bound := MaxContextBytes + 2048
	if n := len([]rune(got)); n > bound {
		t.Errorf("rendered %d runes, past the %d pool plus framing", n, bound)
	}
	if strings.Contains(got, "A line of convention text\nA line of") {
		t.Error("the cut landed mid-line")
	}
}

// TestContextBytesForFile pins the allocation itself, including the case
// that was wrong in the first implementation: a small file must never be
// dropped just because the outer budget is spent, and the closest file must
// get more than an ancestor's share.
func TestContextBytesForFile(t *testing.T) {
	t.Run("generic file", func(t *testing.T) {
		if got := contextBytesForFile(0, 1<<20, false); got != MaxContextFileKB {
			t.Errorf("share = %d, want %d", got, MaxContextFileKB)
		}
	})
	t.Run("closest file may claim the whole pool", func(t *testing.T) {
		// The cwd's rules are the ones a session cannot afford to lose, so
		// they are bounded by the chain budget, not by an ancestor's share.
		if got := contextBytesForFile(0, 1<<20, true); got != MaxContextBytes {
			t.Errorf("share = %d, want the full %d pool", got, MaxContextBytes)
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
