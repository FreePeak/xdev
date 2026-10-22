package tool

import (
	"context"
	"encoding/json"
	"testing"
)

// multiOpStub is a tool whose ops split into reads and writes, the shape the
// github tool and the hub tool have.
type multiOpStub struct{ name string }

func (s multiOpStub) Name() string        { return s.name }
func (s multiOpStub) Description() string { return "stub" }
func (s multiOpStub) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"op":{"type":"string"}}}`)
}
func (s multiOpStub) Execute(context.Context, json.RawMessage) (Result, error) {
	return Result{}, nil
}
func (s multiOpStub) ReadOnlyOps() []string { return []string{"search", "view"} }

// TestCallIsReadOnlyJudgesAMultiOpToolByItsOp pins the per-op verdict.
//
// Live 2026-10-21 (session 4c4addab): the model called the github tool's
// search_prs op while answering "list me all pr urls openningin xdev", and
// the keep-going gate counted the call as a mutation — the tool had no caps
// entry, so an undeclared name failed closed. The gate armed, the harness
// pushed the model past the answer it had just delivered, and the model went
// to work on two unrelated worktrees instead.
//
// A whole-tool cap cannot fix this honestly: the same tool also creates PRs
// and pushes branches, so "read-only" would be a lie for those ops. The
// verdict has to be per op.
func TestCallIsReadOnlyJudgesAMultiOpToolByItsOp(t *testing.T) {
	reg := NewRegistry()
	reg.Register(multiOpStub{name: "multi"})

	for _, tc := range []struct {
		tool string
		args string
		want bool
		why  string
	}{
		{"multi", `{"op":"search"}`, true, "a read op is a read"},
		{"multi", `{"op":"view"}`, true, "a read op is a read"},
		{"multi", `{"op":"create"}`, false, "a write op is a write"},
		{"multi", `{"op":""}`, false, "no op is not a read"},
		{"multi", `{}`, false, "no op is not a read"},
		{"multi", `not json`, false, "an unparseable call is not a read"},
		{"multi", ``, false, "no arguments is not a read"},
		// A tool that declares no read-only ops keeps the conservative
		// default: every op of it is a write.
		{"bash", `{"command":"git status"}`, true, "bash is judged by its command"},
		{"echo", ``, false, "an undeclared tool may write anything"},
	} {
		var args json.RawMessage
		if tc.args != "" {
			args = json.RawMessage(tc.args)
		}
		if got := CallIsReadOnly(reg, tc.tool, args); got != tc.want {
			t.Errorf("CallIsReadOnly(%s %s) = %v, want %v — %s", tc.tool, tc.args, got, tc.want, tc.why)
		}
	}
}

// The real tools' op sets are pinned here, because a typo in them is
// invisible: an op that should read would fail closed and arm the gate, and
// an op that should write would read as a read and stop the gate from ever
// nudging a run that was mid-task.
func TestTheGithubToolDeclaresItsReadOnlyOps(t *testing.T) {
	ops := NewGithubTool(t.TempDir()).ReadOnlyOps()
	read := map[string]bool{}
	for _, o := range ops {
		read[o] = true
	}
	for _, o := range []string{"repo_view", "file_read", "search_issues", "search_prs", "search_code", "search_commits", "search_repos"} {
		if !read[o] {
			t.Errorf("github op %q is a read but is not declared read-only", o)
		}
	}
	for _, o := range []string{"pr_create", "pr_checkout", "pr_push", "run_watch"} {
		if read[o] {
			t.Errorf("github op %q writes but is declared read-only", o)
		}
	}
}
