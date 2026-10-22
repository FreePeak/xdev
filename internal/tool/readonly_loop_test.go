package tool

import "testing"

// TestShellLoopScaffoldingIsTransparent pins the loop constructs.
//
// A loop's scaffolding names no program: "for x in a b", "do", "done" are
// the shell's own words, and a segment that is one of them cannot change
// anything. The classifier used to read them as unknown commands, so a
// research turn that enumerated a list with a `for` loop was a mutation —
// and the keep-going gate armed on it.
//
// Live 2026-10-21 (session 4c4addab): three of the turn's calls were `for`
// loops, and the answer had already been delivered when the gate pushed the
// model past it.
//
// The BODY is a command and is judged on its own: "do rm -rf /" is still a
// write. The header of a for loop cannot write either — the shell expands
// its word list, and a command substitution inside it is a segment of its
// own.
func TestShellLoopScaffoldingIsTransparent(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want bool
		why  string
	}{
		{"for r in a b; do echo $r; done", true, "a read-only loop is a read"},
		{"for r in a b; do gh pr list --repo x/$r; done", true, "a read-only loop body is a read"},
		{"cd /tmp && for b in feat/x fix/y; do git log --oneline origin/main..$b; done", true, "the loop is one part of the line"},
		{"for r in a b; do echo $r; done; wc -l file", true, "a loop next to a read is a read"},

		// The body is a command, and a write in it is a write.
		{"for r in a b; do rm -rf $r; done", false, "the body writes"},
		{"for r in a b; do git commit -m x; done", false, "the body commits"},
		{"for r in a b; do gh pr merge $r; done", false, "the body merges"},
		{"for r in a b; do echo $r > out.txt; done", false, "the body redirects to a file"},
		{"for r in a b; do git branch -d $r; done", false, "the body deletes a branch"},
		// A while/until condition that only READS is a read: `read` takes a
		// line of stdin and `[ ... ]` is a test. Both changed when the
		// shell's own words were classified as what they are.
		{"while read line; do echo $line; done", true, "reading a line is a read"},
		{"until [ -f x ]; do sleep 1; done", true, "a test is a read"},
		// A condition that WRITES is still a write.
		{"while git fetch origin; do sleep 1; done", false, "a while condition that writes is a write"},
	} {
		if got := ReadOnlyCommand(tc.cmd); got != tc.want {
			t.Errorf("ReadOnlyCommand(%q) = %v, want %v — %s", tc.cmd, got, tc.want, tc.why)
		}
	}
}
