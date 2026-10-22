package tool

import "testing"

// TestCanonicalCommandGroupsTheSameWork pins the canonical form.
//
// It is the key the keep-going gate's REPEATED-WORK notice uses, and only
// that: a notice is text, so a coarse key cannot hand the model a wrong
// answer. The tool-result cache keeps its own precise key for exactly that
// reason.
//
// The live failure this exists for (2026-10-22, session adf5029b): a model
// answering "list me all pr urls opening xdev" asked the same question in ten
// spellings — `gh pr list`, `gh api repos/…/pulls`, `gh api graphql`,
// `gh search prs` — each with a different projection, a different page size
// and a different pipe. The exact key saw 28 tool calls and 28 distinct
// keys, and not one repeat.
func TestCanonicalCommandGroupsTheSameWork(t *testing.T) {
	same := [][2]string{
		// The same command at two depths.
		{"gh pr list --repo FreePeak/xdev --state open --limit 100", "gh pr list --repo FreePeak/xdev --state open --limit 2000"},
		// The same command with a different projection.
		{`gh api "repos/FreePeak/xdev/pulls?state=open&per_page=100" --jq 'length'`, `gh api "repos/FreePeak/xdev/pulls?state=open&per_page=100" --jq '.[] | .url'`},
		// The same command with a different pipe target.
		{"gh pr list --state open | head -40", "gh pr list --state open | head -50"},
		// The model's own LABELS and the shaping of its output are not the
		// work: a call that echoes a heading and pipes into `head` is one
		// question, and the noise must not be part of its identity.
		{`echo "=== open PRs ==="; gh pr list --state open | head -40`, `gh pr list --state open`},
		{`gh pr list --state open | head -40`, `gh pr list --state open | head -50`},
		{"echo hi; echo bye; gh pr list --state open", "gh pr list --state open"},

		// The same command with the flag order changed.
		{"gh pr list --state open --author linhdmn", "gh pr list --author linhdmn --state open"},
	}
	for _, p := range same {
		a, b := CanonicalCommand(p[0]), CanonicalCommand(p[1])
		if a != b {
			t.Errorf("the same work must canonicalize alike:\n  %s\n  %s\n  got %q vs %q", p[0], p[1], a, b)
		}
	}

	different := [][2]string{
		// A different TARGET is a different question.
		{"gh pr list --repo FreePeak/xdev", "gh pr list --repo FreePeak/xdev-private2"},
		// A different STATE is a different question.
		{`gh api "repos/FreePeak/xdev/pulls?state=open"`, `gh api "repos/FreePeak/xdev/pulls?state=closed"`},
		// A different PROGRAM is different work.
		{"gh pr list", "gh api repos/FreePeak/xdev"},
		// A RANGE is not a magnitude: the same file, different lines.
		{"sed -n '1,50p' internal/agent/loop.go", "sed -n '200,250p' internal/agent/loop.go"},
		// A different file is a different target.
		{"sed -n '1,50p' internal/agent/loop.go", "sed -n '1,50p' internal/agent/task.go"},
		// A different DIRECTORY is a different context, even for the same
		// command: the cd stays in the key.
		{`cd /tmp && gh pr list --state open`, `gh pr list --state open`},
		{`cd /a && gh pr list --state open`, `cd /b && gh pr list --state open`},
	}
	for _, p := range different {
		a, b := CanonicalCommand(p[0]), CanonicalCommand(p[1])
		if a == b {
			t.Errorf("different work must NOT canonicalize alike:\n  %s\n  %s\n  both got %q", p[0], p[1], a)
		}
	}
}

// TestCanonicalCommandKeepsAQuotedWordWhole pins the word splitter.
//
// The repo's own splitter is quote-aware about OPERATORS; this is the same
// care applied to WORDS, so `--jq '.[] | "x"'` is one word and not three.
func TestCanonicalCommandKeepsAQuotedWordWhole(t *testing.T) {
	got := splitShellWords(`gh api X --jq '.[] | "y"' --limit 5`)
	want := []string{"gh", "api", "X", "--jq", `'.[] | "y"'`, "--limit", "5"}
	if len(got) != len(want) {
		t.Fatalf("splitShellWords = %q, want %q", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("splitShellWords = %q, want %q", got, want)
		}
	}
}
