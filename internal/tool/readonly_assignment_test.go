package tool

import "testing"

// TestAnAssignmentOnlySegmentChangesNothing pins the assignment case.
//
// The bash tool gives every call a fresh process, so a variable set by a
// call dies with that call — the same argument that puts `cd` in the read
// set. An assignment-only segment changes nothing.
//
// It used to be a WRITE, on the reasoning that an assignment exports state.
// But the splitter cuts `d=$(cmd)` in half, and the half it leaves — "d=$" —
// is not a command at all. It read as an unknown command, so the loop
//
//	for b in $(git for-each-ref --format='%(refname:short)' refs/heads); do
//	  d=$(git rev-list --count main..$b 2>/dev/null)
//	  echo "$b ahead=$d"
//	done
//
// was a mutation on a turn that had only read — the loop's own scaffolding
// is transparent, and the two commands inside it are reads.
//
// The command substitution inside an assignment is a segment of its own (the
// splitter cuts at `(`), so the command inside it is still judged.
func TestAnAssignmentOnlySegmentChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want bool
		why  string
	}{
		{"FOO=1", true, "a variable set by a call dies with the call"},
		{"FOO=bar", true, "same"},
		{"for b in $(git for-each-ref --format='%(refname:short)' refs/heads); do d=$(git rev-list --count main..$b 2>/dev/null); echo \"$b ahead=$d\"; done", true, "a loop of reads is a read"},
		{"d=$(git rev-list --count main..HEAD)", true, "the assignment is neutral, the substitution is judged"},
		{"FOO=1 ls", true, "an assignment prefix does not change the command"},
		{"FOO=1 rm -rf /tmp/x", false, "the command after the assignment still writes"},
		{"d=$(rm -rf /tmp/x)", false, "the command inside the substitution still writes"},
		{"FOO=$(git commit -m x)", false, "the command inside the substitution still writes"},
	} {
		if got := ReadOnlyCommand(tc.cmd); got != tc.want {
			t.Errorf("ReadOnlyCommand(%q) = %v, want %v — %s", tc.cmd, got, tc.want, tc.why)
		}
	}
}
