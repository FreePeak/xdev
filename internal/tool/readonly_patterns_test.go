package tool

import "testing"

// TestReadOnlyPatternsAreReads pins the shell patterns a session uses
// constantly, every one of which is a READ, and every one of which used to
// be classified as a mutation.
//
// The cost of that misclassification is the keep-going gate: a read
// reported as a write arms the gate, and the gate pushes the model past the
// answer it has already given.
//
// Live 2026-10-22, a TUI run of "list me all pr urls opening xdev": three
// of the run's calls were reads, and each one armed the gate — so the run
// was pushed into a fourth turn of work it did not need.
func TestReadOnlyPatternsAreReads(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want bool
		why  string
	}{
		// A shell TEST looks at values and returns a status.
		{`[ "$n" != "0" ]`, true, "a test is a read"},
		{`[[ -f "$f" ]]`, true, "the double-bracket test is a read"},
		{`test -d /tmp`, true, "test is a read"},

		// The shell's CONTROL WORDS name no program.
		{"continue", true, "continue names no program"},
		{"break", true, "break names no program"},
		{"read b", true, "read consumes a line, it changes no file"},
		{"exit 0", true, "exit ends the shell, it changes no file"},

		// A LIST form takes a PATTERN, not a branch name to create.
		{"git branch --list 'origin/*'", true, "the list form is a query"},
		{"git branch -r --list 'origin/*'", true, "the list form is a query"},
		{"git branch --list", true, "the list form with no pattern is a query"},
		{"git branch --list 'feat/*' | wc -l", true, "the list form is a query"},

		// The whole shapes the live run used.
		{`echo "=== all FreePeak repos ==="; for r in $(gh repo list FreePeak --limit 100 --json nameWithOwner --jq '.[].nameWithOwner' 2>/dev/null); do n=$(gh api "repos/$r/pulls?state=open&per_page=100" --jq 'length' 2>/dev/null); if [ "$n" != "0" ]; then echo "$r -> $n"; fi; done`, true, "a read-only loop over a read-only query is a read"},
		{`cd /repo && git branch -r --list 'origin/*' | wc -l; echo "--- remote branches ---"; gh api "repos/FreePeak/xdev/branches?per_page=100" --jq '.[].name' 2>&1 | head -50`, true, "a branch listing and a branch query are reads"},
		{`cd /repo && git status -sb | head -5; echo "--- ahead of main ---"; git for-each-ref --format='%(refname:short)' refs/remotes/origin/ | sed 's|^origin/||' | while read b; do [ "$b" = "main" ] && continue; done`, true, "a status and a ref walk are reads"},

		// A BRANCH is a command, not scaffolding. `if COND; then CMD; else
		// CMD; fi` — the branch IS the command, and treating it as
		// scaffolding hid a real `git worktree remove` from the
		// classifier. The store-wide destructive-git guard caught it.
		{`if [ "$d" = "0" ]; then git worktree remove "$p"; else echo skipped; fi`, false, "the branch removes a worktree"},
		{`if [ "$d" = "0" ]; then git worktree list; else echo skipped; fi`, true, "a branch that only lists is a read"},
		{`for w in a b; do git worktree remove "$w"; done`, false, "the body removes a worktree"},

		// The other direction, unchanged: a branch NAME still creates.
		{"git branch feat/x", false, "a bare name creates a branch"},
		{"git branch -d feat/x", false, "-d deletes"},
		{"git branch -m a b", false, "-m renames"},
		{"git branch --list; git branch feat/x", false, "the create still counts"},
	} {
		if got := ReadOnlyCommand(tc.cmd); got != tc.want {
			t.Errorf("ReadOnlyCommand(%q) = %v, want %v — %s", tc.cmd, got, tc.want, tc.why)
		}
	}
}
