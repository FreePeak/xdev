package tool

import "testing"

// TestReadOnlyCommandClassifiesGh pins the `gh` half of the keep-going gate's
// classifier.
//
// Live 2026-10-21, session 4c4addab: the user asked "list me all pr urls
// openningin xdev". The model ran `gh pr list` twelve times and the github
// tool once, and at 22:44:46.566 it DELIVERED the answer — the open PR list,
// with the URLs. In the same millisecond the harness injected a keep-going
// nudge ("continue the unfinished work: take the next concrete step with
// tools"), and the model obeyed it by spending the next three minutes reading
// the diffs of two unrelated worktrees.
//
// The cause was here: `gh` was not in the read-only command table at all, so
// `readOnlySegment` fell through to its default and every `gh` line — a read
// or a write — was reported as a mutation. The gate then read a research turn
// as a mutating one and pushed the model past the answer it had just given.
//
// This is the same shape commit 7c1c481 fixed for `git branch`, `git worktree`,
// `git remote`, `git stash`, `git tag` and `git config`: the read form of a
// verb with a destructive spelling has to be modelled, or the gate arms on
// the commands a session runs to ORIENT ITSELF.
//
// Both directions are load-bearing. A read reported as a write is the bug
// above; a write reported as a read would stop the gate from ever nudging a
// run that really was mid-task.
func TestReadOnlyCommandClassifiesGh(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want bool
		why  string
	}{
		// The reads. These are the calls a session makes when it is asked
		// what is open, what is merged, what the CI is doing.
		{"gh pr list", true, "listing open PRs is a read"},
		{"gh pr list --state open --limit 100 --json number,url", true, "flags do not change the verb"},
		{"gh pr view 123", true, "reading a PR is a read"},
		{"gh pr status", true, "the read form of status"},
		{"gh pr diff 123", true, "reading a diff is not changing one"},
		{"gh pr checks 123", true, "reading checks is a read"},
		{"gh issue list --state open", true, "listing issues is a read"},
		{"gh issue view 42", true, "reading an issue is a read"},
		{"gh repo list", true, "listing repos is a read"},
		{"gh repo view FreePeak/xdev", true, "reading a repo is a read"},
		{"gh search prs --owner FreePeak", true, "searching is a read"},
		{"gh search issues --owner FreePeak --state open", true, "searching is a read"},
		{"gh run list", true, "listing runs is a read"},
		{"gh run view 99", true, "reading a run is a read"},
		{"gh release list", true, "listing releases is a read"},
		{"gh label list", true, "listing labels is a read"},
		{"gh secret list", true, "listing secret names is a read"},
		{"gh auth status", true, "reading the auth state is a read"},
		{"gh api repos/FreePeak/xdev", true, "gh api defaults to GET"},
		{"gh api -X GET repos/FreePeak/xdev", true, "an explicit GET is a read"},
		{"gh api --method GET repos/FreePeak/xdev", true, "the long method flag is a read"},
		{"gh status", true, "the bare status noun is a read"},
		{"gh pr list | head -20", true, "a pipe is not a write"},
		{"gh pr list 2>/dev/null", true, "stderr to null is not a mutation"},

		// The writes. Each must stay mutating or the gate stops nudging runs
		// that really were mid-task.
		{"gh pr create --title x --body y", false, "creating a PR writes"},
		{"gh pr merge 123 --squash", false, "merging writes"},
		{"gh pr close 123", false, "closing writes"},
		{"gh pr comment 123 --body x", false, "commenting writes"},
		{"gh pr edit 123 --title x", false, "editing writes"},
		{"gh pr ready 123", false, "marking ready writes"},
		{"gh issue create --title x", false, "creating an issue writes"},
		{"gh issue close 42", false, "closing writes"},
		{"gh issue comment 42 --body x", false, "commenting writes"},
		{"gh repo create x", false, "creating a repo writes"},
		{"gh repo delete x --yes", false, "deleting writes"},
		{"gh repo fork FreePeak/xdev", false, "forking writes"},
		{"gh release create v1", false, "creating a release writes"},
		{"gh release delete v1", false, "deleting writes"},
		{"gh label create bug", false, "creating a label writes"},
		{"gh label delete bug", false, "deleting writes"},
		{"gh secret set FOO", false, "setting a secret writes"},
		{"gh secret delete FOO", false, "deleting writes"},
		{"gh run rerun 99", false, "rerunning writes"},
		{"gh run cancel 99", false, "cancelling writes"},
		{"gh workflow run build.yml", false, "dispatching a workflow writes"},
		{"gh gist create", false, "creating a gist writes"},
		{"gh cache delete", false, "deleting writes"},
		{"gh alias set x '!echo'", false, "setting an alias writes"},
		{"gh auth login", false, "logging in writes"},
		{"gh auth logout", false, "logging out writes"},
		{"gh extension install x", false, "installing writes"},
		{"gh api -X POST repos/FreePeak/xdev/issues", false, "a POST writes"},
		{"gh api --method DELETE repos/FreePeak/xdev", false, "a DELETE writes"},
		{"gh api -XPOST repos/FreePeak/xdev/issues", false, "the joined short flag writes"},
		{"gh config set x y", false, "setting config writes"},
		{"gh pr", false, "a bare noun with no subcommand is not a read"},
		{"gh", false, "a bare program name is not a read"},
	} {
		if got := ReadOnlyCommand(tc.cmd); got != tc.want {
			t.Errorf("ReadOnlyCommand(%q) = %v, want %v — %s", tc.cmd, got, tc.want, tc.why)
		}
	}
}
