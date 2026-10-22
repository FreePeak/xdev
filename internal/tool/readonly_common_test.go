package tool

import "testing"

// TestCommonReadCommandsAreReads pins the read commands a research turn
// reaches for and the classifier did not know.
//
// The keep-going gate's verdict decides whether the nudge fires, and a read
// reported as a mutation arms it. These are the shapes that showed up in the
// store: a `timeout` around a slow call, a `gofmt -l` to see what is
// unformatted, a `sleep` between two polls, an `expr` to compute a number.
//
// `awk` is deliberately NOT here: an awk program can write a file
// (`awk '{print > "out"}'`), and a `>` inside an awk program is also a
// comparison (`awk '$1 > 5'`), so the two cannot be told apart without
// parsing the program. It stays unknown, which is the safe direction.
func TestCommonReadCommandsAreReads(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want bool
		why  string
	}{
		{"timeout 5 gh pr list --state open", true, "a bounded read is a read"},
		{"timeout 5s gh pr list", true, "the duration may carry a suffix"},
		{"timeout --foreground 5 gh pr list", true, "a flag before the duration"},
		{"timeout -k 2 5 gh pr list", true, "a flag that takes a value"},
		{"timeout 5", true, "a timeout with no command does nothing"},
		{"timeout 5 wc -c file", true, "the wrapped command is judged on its own"},
		{"gofmt -l internal/agent/loop.go", true, "listing what needs formatting is a read"},
		{"gofmt -d internal/agent/loop.go", true, "a diff is a read"},
		{"gofmt internal/agent/loop.go", true, "printing the formatted source is a read"},
		{"gofmt -w internal/agent/loop.go", false, "-w rewrites the file"},
		{"sleep 2", true, "sleeping does nothing"},
		{"expr 1 + 1", true, "evaluating an expression is a read"},
		{"expr 1 + 1 > out.txt", false, "the redirect still writes"},

		// The wrapped command keeps its own verdict.
		{"timeout 5 rm -rf /tmp/x", false, "the wrapped command writes"},
		{"timeout 5 git commit -m x", false, "the wrapped command writes"},
		{"timeout 5 gh pr merge 123", false, "the wrapped command writes"},
	} {
		if got := ReadOnlyCommand(tc.cmd); got != tc.want {
			t.Errorf("ReadOnlyCommand(%q) = %v, want %v — %s", tc.cmd, got, tc.want, tc.why)
		}
	}
}
