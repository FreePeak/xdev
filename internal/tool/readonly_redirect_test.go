package tool

import "testing"

// TestSplitCompoundKeepsARedirectIntact pins the splitter's treatment of the
// '&' that belongs to a REDIRECT rather than to a control operator.
//
//	>&2    fd duplication: stderr onto stdout
//	2>&1   fd duplication: stdout onto stderr
//	&>f    both streams onto a file
//	&>>f   both streams appended to a file
//
// The splitter used to cut at every '&', so `cmd 2>&1` became two segments:
// "cmd 2>" and "1". The second one is not a command, so it could not be
// classified, and the WHOLE line stopped being a read — and `2>&1` is how a
// session captures stderr, so it appears in a large share of the calls a
// research turn makes.
//
// Live 2026-10-21 (session 4c4addab): the answer to "list me all pr urls
// openningin xdev" was delivered, and the keep-going gate then pushed the
// model past it into two unrelated worktrees. Twelve of that turn's calls
// ended in `2>&1`, and every one of them read as a mutation for this reason.
func TestSplitCompoundKeepsARedirectIntact(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want []string
		why  string
	}{
		{"gh pr list 2>&1 | head -50", []string{"gh pr list 2>&1", "head -50"}, "the fd shuffle stays with its command"},
		{"git status 2>&1", []string{"git status 2>&1"}, "a trailing fd shuffle is one segment"},
		{"cmd >&2", []string{"cmd >&2"}, "the >& form is a redirect too"},
		{"cmd &>out", []string{"cmd &>out"}, "the &> form is a redirect too"},
		{"cmd &>>out", []string{"cmd &>>out"}, "the &>> form is a redirect too"},
		{"cmd 2>&1 && other", []string{"cmd 2>&1", "other"}, "a redirect does not swallow the &&"},
		{"cmd 2>&1 & other", []string{"cmd 2>&1", "other"}, "a background & is still a control operator"},
		{"cmd && other", []string{"cmd", "other"}, "&& still splits"},
		{"cmd & other", []string{"cmd", "other"}, "a background & still splits"},
		{"echo 'a & b'", []string{"echo 'a & b'"}, "a quoted & is data"},
	} {
		got := splitCompound(tc.cmd)
		if len(got) != len(tc.want) {
			t.Errorf("splitCompound(%q) = %q, want %q — %s", tc.cmd, got, tc.want, tc.why)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitCompound(%q) = %q, want %q — %s", tc.cmd, got, tc.want, tc.why)
				break
			}
		}
	}
}

// The other direction, which the splitter change makes reachable: a line
// that writes a file is not a read, even when the file write is preceded by
// an fd shuffle.
//
// "cmd 2>&1 > out" writes out. The redirect scan used to stop at the FIRST
// '>', which was the one in "2>&1", and report the line as a read — a write
// reported as a read, the one direction that must be impossible.
func TestARedirectAfterAnFdShuffleStillWrites(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want bool
		why  string
	}{
		{"git status 2>&1", true, "an fd shuffle alone writes no file"},
		{"git status 2>&1 > out", false, "the later redirect writes out"},
		{"git status > out 2>&1", false, "the redirect writes out"},
		{"git status &>out", false, "the &> form writes out"},
		{"git status &>>out", false, "the &>> form writes out"},
		{"git status 2>/dev/null", true, "stderr to null is not a mutation"},
		{"git status 2>&1 >/dev/null", true, "a write to null is not a mutation"},

		// A redirect is not an argument to the command. `git branch
		// --contains sha 2>&1` passes --contains and sha to git, and
		// 2>&1 to the shell — but the classifier saw the tail as a third
		// argument, and a bare argument after `git branch` is a branch
		// name to CREATE, so a query read as a write.
		{"git branch --contains 01671eb", true, "a query is a read"},
		{"git branch --contains 01671eb 2>&1", true, "a query with an fd shuffle is a read"},
		{"git branch -a --contains 01671eb 2>/dev/null | head", true, "the query survives a pipe"},
		{"git branch 01671eb", false, "a bare name after git branch CREATES a branch"},
		{"git branch -d feat/x 2>&1", false, "a delete is a delete, redirect or not"},
	} {
		if got := ReadOnlyCommand(tc.cmd); got != tc.want {
			t.Errorf("ReadOnlyCommand(%q) = %v, want %v — %s", tc.cmd, got, tc.want, tc.why)
		}
	}
}
