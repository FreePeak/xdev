package tool

import (
	"encoding/json"
	"testing"
)

// TestReadOnlyCommandClassifiesShellLines pins the classifier the keep-going
// gate leans on. Both directions are load-bearing and they fail differently:
//
//   - a pure read reported as mutating is the bug this exists to fix (every
//     session shells out for `git status`, so the gate never let a finished
//     run stop: 649 continuations over 180 user turns in the 80 newest
//     session files);
//   - a write reported as read-only is the dangerous direction, so it gets
//     the table below rather than a spot check. `git branch` and `git tag`
//     are deliberately absent from the read set for exactly this reason:
//     `git branch -D` and `git tag -d` are the same first word.
func TestReadOnlyCommandClassifiesShellLines(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want bool
		why  string
	}{
		// The reads that caused the bug.
		{"git status", true, "the single most common call in a session"},
		{"git status --short", true, "flags do not change the verb"},
		{"git -C /tmp/repo status", true, "a global flag takes a value"},
		{"git --no-pager log --oneline -20", true, "a valueless global flag"},
		{"git diff HEAD~1 --stat", true, "reading a diff is not changing one"},
		{"git rev-parse --show-toplevel", true, "resolving a path"},
		{"wc -c internal/agent/prompt.go", true, "measuring a file"},
		{"ls -la", true, "listing"},
		{"pwd && ls docs/", true, "every segment must be a read"},
		{"rg -n 'foo' internal | head -20", true, "a pipe is not a write"},
		{"grep -rn foo . ; cat README.md", true, "sequencing reads"},
		{"cat file | jq .name", true, "a read pipeline"},
		{"find . -name '*.go' -type f", true, "find without -exec/-delete"},
		{"sed -n '1,20p' file.go", true, "sed without -i is a filter"},
		{"sort file.txt", true, "sort without -o writes only to stdout"},
		{"FOO=1 ls", true, "a leading assignment does not make it a write"},

		// The writes. Each must stay mutating or the gate stops nudging runs
		// that really were mid-task.
		{"git commit -m x", false, "a commit is the definition of a mutation"},
		{"git add -A", false, "staging changes the index"},
		{"git push", false, "publishing"},
		{"git checkout main", false, "checkout rewrites the worktree"},
		{"git worktree add .worktrees/x -b y", false, "the exact command 44 sessions ran"},
		{"git branch -D feat/x", false, "branch can delete — not in the read set"},
		{"git tag -d v1", false, "tag can delete — not in the read set"},
		{"git stash", false, "stash writes and clears the worktree"},
		{"git config user.name x", false, "config writes"},
		{"sed -i 's/a/b/' file.go", false, "in-place edit"},
		{"sed -ni p file.go", false, "a flag cluster still carries the i"},
		{"sort -o out.txt in.txt", false, "output to a file"},
		{"wc -c file > out.txt", false, "a redirect is a write, whatever the verb"},
		{"cat a > b", false, "same, for cat"},
		{"echo hi >> log", false, "append"},
		{"rm -rf build", false, "unmodelled commands fail closed"},
		{"go build ./...", false, "a build writes artifacts"},
		{"npm install", false, "a package manager writes"},
		{"bash -c 'rm -rf /'", false, "a nested shell is opaque"},
		{"sh -c 'git status'", false, "even a nested read stays opaque"},
		{"", false, "an empty command is not a read"},
		{"FOO=1", false, "a bare assignment exports state"},

		// Quoting and substitution: the splitter is policy.go's, and these are
		// the cases where a naive `strings.Contains(cmd, ">")` is wrong.
		{`grep "a>b" file`, true, "a quoted > is data"},
		{"echo 'git status'", true, "quoted text is not a command"},
		{"git status; rm -rf /", false, "a write hidden behind a read"},
		// The splitter judges a substitution's contents as their own segment
		// (policy.go splitOnOperators), so the verdict is the same as if the
		// inner command had been written out: a read inside stays a read, a
		// write inside fails the whole line.
		{"cat $(git rev-parse HEAD)", true, "the substitution's own segment is a read"},
		{"cat $(touch x)", false, "the substitution's own segment is a write"},
		{"git status $(touch x)", false, "a read with a write substitution is not a read"},
		{"cd /tmp && git status", true, "cd persists nothing — the tool gives every call a fresh process"},
		{"cd /tmp && rm -rf x", false, "cd does not launder a write after it"},
		// /dev/null redirects are not workspace writes (session 724e3fbb:
		// every research probe used 2>/dev/null and the gate treated them as
		// mutations, so the keep-going nudge fired after the report).
		// Note: `2>&1` is still fail-closed — policy.go's splitter cuts on
		// bare `&`, so the segment never reaches hasWriteRedirect intact.
		{"ls docs 2>/dev/null", true, "stderr to null is not a write"},
		{"pwd; ls 2>/dev/null; head -5 f", true, "compound of reads with null redirects"},
		{"cat a >/dev/null", true, "stdout to null is not a write"},
		{"wc -c f > out.txt", false, "a real file redirect stays a write"},
		{"echo hi >> log", false, "append stays a write"},

		// curl GET probes are the live-test surface; -o/-d stay mutating.
		{"curl -s http://127.0.0.1:8080/health", true, "GET probe"},
		{"curl -sS -H 'X-Admin-Password: x' http://127.0.0.1:8080/admin", true, "GET with headers"},
		{"curl -s -X GET http://127.0.0.1:8080/x", true, "explicit GET"},
		{"curl -s -o /tmp/out http://x", false, "output file is a write"},
		{"curl -s -d 'a=1' http://x", false, "POST body"},
		{"curl -s --data-urlencode a=1 http://x", false, "urlencoded POST"},

		// python -c is the visible research one-liner; a heredoc/script path
		// is opaque and fails closed (body may open files for write).
		{"python3 -c 'print(1)'", true, "inline -c program is visible"},
		{`python3 -c "import json; print(1)"`, true, "double-quoted -c"},
		{"python3 - <<'PY'\nprint(1)\nPY", false, "heredoc body is opaque"},
		{"python3 script.py", false, "script path is opaque"},
	} {
		if got := ReadOnlyCommand(tc.cmd); got != tc.want {
			t.Errorf("ReadOnlyCommand(%q) = %v, want %v — %s", tc.cmd, got, tc.want, tc.why)
		}
	}
}

// TestBashCallIsReadOnlyDecodesTheToolArguments pins the seam the agent loop
// calls: the JSON shape stays in this package (bash.go and policy.go already
// decode it), so no second package re-guesses `{"command": ...}`.
func TestBashCallIsReadOnlyDecodesTheToolArguments(t *testing.T) {
	args, err := json.Marshal(map[string]string{"command": "git status"})
	if err != nil {
		t.Fatal(err)
	}
	if !BashCallIsReadOnly(args) {
		t.Fatal("a git status call was not read as a read")
	}
	// workdir is irrelevant to the verdict, but a call carrying one must still
	// decode.
	args, err = json.Marshal(map[string]any{"command": "git status", "workdir": "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	if !BashCallIsReadOnly(args) {
		t.Fatal("workdir changed the verdict")
	}
	// Malformed or absent arguments must not be read as a read.
	for _, raw := range []string{"", "null", "{}", "not json"} {
		if BashCallIsReadOnly(json.RawMessage(raw)) {
			t.Errorf("BashCallIsReadOnly(%q) = true, want false", raw)
		}
	}
}
