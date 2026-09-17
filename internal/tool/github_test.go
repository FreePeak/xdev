package tool

// Fixture strategy for the github tool: a stub `gh` (and `git`) script goes
// on PATH. Each stub appends "name@cwd" followed by one line per argument and
// a "--" terminator to $GH_LOG, so argv construction is asserted exactly —
// including arguments that contain spaces. Responses come from
// <PREFIX>_RESPONSES/<n> (slot n, or slot 0 as the every-call default), and
// <PREFIX>_EXIT sets the exit status.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const stubTemplate = `#!/bin/sh
log="${GH_LOG:-/dev/null}"
printf '%s\n' "NAME@$PWD" >> "$log"
for a in "$@"; do printf '%s\n' "$a" >> "$log"; done
printf '%s\n' '--' >> "$log"
count_file="$log.NAME.count"
n=1
if [ -f "$count_file" ]; then n=$(cat "$count_file"); n=$((n + 1)); fi
printf '%s\n' "$n" > "$count_file"
dir="${PREFIX_RESPONSES:-}"
if [ -n "$dir" ]; then
  if [ -f "$dir/$n" ]; then cat "$dir/$n"; elif [ -f "$dir/0" ]; then cat "$dir/0"; fi
fi
cap="${PREFIX_CAPTURE:-}"
if [ -n "$cap" ] && [ -d "$cap" ]; then
  i=0
  for a in "$@"; do i=$((i + 1)); if [ -f "$a" ]; then cp "$a" "$cap/$i"; fi; done
fi
failmatch="${PREFIX_FAIL_MATCH:-}"
if [ -n "$failmatch" ]; then
  case "$*" in *"$failmatch"*) exit 1;; esac
fi
err="${PREFIX_STDERR:-}"
if [ -n "$err" ] && [ -f "$err" ]; then cat "$err" 1>&2; fi
exit "${PREFIX_EXIT:-0}"
`

func writeStub(t *testing.T, dir, name, prefix string) {
	t.Helper()
	script := strings.ReplaceAll(strings.ReplaceAll(stubTemplate, "NAME", name), "PREFIX", prefix)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

type stubCall struct {
	bin  string
	dir  string
	args []string
}

func readStubCalls(t *testing.T, log string) []stubCall {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	// One call is "marker@cwd", its args, then a "--" terminator; walk the
	// lines so the trailing terminator never leaks into argv.
	var calls []stubCall
	var cur []string
	flush := func() {
		if len(cur) == 0 {
			return
		}
		bin, dir, _ := strings.Cut(cur[0], "@")
		calls = append(calls, stubCall{bin: bin, dir: dir, args: cur[1:]})
		cur = nil
	}
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if line == "--" {
			flush()
			continue
		}
		cur = append(cur, line)
	}
	flush()
	return calls
}

type githubStub struct {
	t    *testing.T
	dir  string
	log  string
	repo string // the tool's CWD: os/exec refuses a cmd.Dir that does not exist
}

// newGithubStub installs stubs and points PATH at them. The real PATH stays
// afterwards so /bin/sh and friends still resolve.
func newGithubStub(t *testing.T) *githubStub {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("github stubs are POSIX shell scripts")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	writeStub(t, dir, "gh", "GH")
	writeStub(t, dir, "git", "GIT")
	for _, sub := range []string{"gh-responses", "git-responses", "captures"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_LOG", log)
	t.Setenv("GH_RESPONSES", filepath.Join(dir, "gh-responses"))
	t.Setenv("GIT_RESPONSES", filepath.Join(dir, "git-responses"))
	t.Setenv("GH_CAPTURE", filepath.Join(dir, "captures"))
	return &githubStub{t: t, dir: dir, log: log, repo: repo}
}

// respond sets the default gh stdout (used for every gh call without its own
// slot).
func (s *githubStub) respond(text string) { s.writeResponse("gh-responses", "0", text) }

// respondSeq sets gh stdout per call (slot 1, 2, ...).
func (s *githubStub) respondSeq(texts ...string) {
	for i, text := range texts {
		s.writeResponse("gh-responses", strconv.Itoa(i+1), text)
	}
}

func (s *githubStub) respondGit(text string) { s.writeResponse("git-responses", "0", text) }

func (s *githubStub) writeResponse(sub, slot, text string) {
	s.t.Helper()
	if err := os.WriteFile(filepath.Join(s.dir, sub, slot), []byte(text), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

func (s *githubStub) fail(code int, stderr string) {
	s.t.Helper()
	path := filepath.Join(s.dir, "gh-stderr")
	if err := os.WriteFile(path, []byte(stderr), 0o644); err != nil {
		s.t.Fatal(err)
	}
	s.t.Setenv("GH_STDERR", path)
	s.t.Setenv("GH_EXIT", strconv.Itoa(code))
}

func (s *githubStub) calls() []stubCall { return readStubCalls(s.t, s.log) }

// reset forgets the recorded calls and the response cursor (so a follow-up
// call starts at slot 1 again).
func (s *githubStub) reset() {
	for _, p := range []string{s.log, s.log + ".gh.count", s.log + ".git.count"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			s.t.Fatal(err)
		}
	}
}

func runGithub(t *testing.T, tool *GithubTool, args string) Result {
	t.Helper()
	res, err := tool.Execute(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("github(%s) returned a harness error: %v", args, err)
	}
	return res
}

func assertArgs(t *testing.T, got stubCall, wantBin, wantDir string, want ...string) {
	t.Helper()
	if got.bin != wantBin {
		t.Fatalf("binary = %q, want %q", got.bin, wantBin)
	}
	if wantDir != "" && !sameDir(got.dir, wantDir) {
		t.Fatalf("cwd = %q, want %q", got.dir, wantDir)
	}
	if len(got.args) != len(want) {
		t.Fatalf("argv = %q, want %q", got.args, want)
	}
	for i := range want {
		if got.args[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q (argv = %q)", i, got.args[i], want[i], got.args)
		}
	}
}

// sameDir compares two working directories, resolving symlinks: the stub
// reports the kernel's $PWD, and on macOS the temp dir is reached through
// /private/tmp.
func sameDir(a, b string) bool {
	if a == b {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

const prMetaJSON = `{"number":7,"title":"add widget","url":"https://github.com/FreePeak/xdev/pull/7","state":"OPEN","isDraft":false,"headRefName":"feat/widget","headRefOid":"abc1234567890def","baseRefName":"main","isCrossRepository":false}`

func TestGithubRepoViewArgs(t *testing.T) {
	stub := newGithubStub(t)
	stub.respond(`{"nameWithOwner":"FreePeak/xdev","description":"agent harness","url":"https://github.com/FreePeak/xdev","defaultBranchRef":{"name":"main"},"isPrivate":true,"stargazerCount":3,"forkCount":1,"primaryLanguage":{"name":"Go"},"pushedAt":"2026-09-11T00:00:00Z","homepageUrl":"https://x.dev","repositoryTopics":[{"name":"agent"}],"viewerPermission":"ADMIN"}`)
	tool := NewGithubTool(stub.repo)

	res := runGithub(t, tool, `{"op":"repo_view","repo":"https://github.com/FreePeak/xdev.git","branch":"main"}`)
	if res.IsError {
		t.Fatalf("repo_view errored: %s", res.Text)
	}
	calls := stub.calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	assertArgs(t, calls[0], "gh", stub.repo, "repo", "view", "github.com/FreePeak/xdev", "--branch", "main", "--json", ghRepoFields)
	for _, want := range []string{"# FreePeak/xdev", "agent harness", "default branch: main", "visibility: private", "language: Go", "topics: agent", "permission: ADMIN"} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("repo_view text missing %q:\n%s", want, res.Text)
		}
	}
}

func TestGithubFileReadArgsAndRawBody(t *testing.T) {
	stub := newGithubStub(t)
	body := "package main\n\nfunc main() {}\n"
	stub.respond(body)
	tool := NewGithubTool(stub.repo)

	res := runGithub(t, tool, `{"op":"file_read","repo":"github.com/FreePeak/xdev","path":"cmd/x dev/main.go","branch":"main"}`)
	if res.IsError {
		t.Fatalf("file_read errored: %s", res.Text)
	}
	if res.Text != body {
		t.Fatalf("file_read must return raw bytes, got %q", res.Text)
	}
	assertArgs(t, stub.calls()[0], "gh", stub.repo,
		"api", "/repos/FreePeak/xdev/contents/cmd/x%20dev/main.go",
		"--method", "GET", "-H", "Accept: application/vnd.github.raw+json", "-f", "ref=main")
}

func TestGithubFileReadResolvesCurrentRepo(t *testing.T) {
	stub := newGithubStub(t)
	// `--jq .nameWithOwner` prints the bare value, not the JSON envelope.
	stub.respondSeq("FreePeak/xdev\n", "README body\n")
	tool := NewGithubTool(stub.repo)

	res := runGithub(t, tool, `{"op":"file_read","path":"README.md"}`)
	if res.IsError {
		t.Fatalf("file_read errored: %s", res.Text)
	}
	if res.Text != "README body\n" {
		t.Fatalf("text = %q", res.Text)
	}
	calls := stub.calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %v", calls)
	}
	assertArgs(t, calls[0], "gh", stub.repo, "repo", "view", "--json", "nameWithOwner", "--jq", ".nameWithOwner")
	assertArgs(t, calls[1], "gh", stub.repo, "api", "/repos/FreePeak/xdev/contents/README.md",
		"--method", "GET", "-H", "Accept: application/vnd.github.raw+json")
}

func TestGithubFileReadRejectsAbsolutePath(t *testing.T) {
	stub := newGithubStub(t)
	res := runGithub(t, NewGithubTool(stub.repo), `{"op":"file_read","path":"/etc/passwd"}`)
	if !res.IsError || !strings.Contains(res.Text, "repository-relative") {
		t.Fatalf("absolute path must be rejected, got %+v", res)
	}
	if len(stub.calls()) != 0 {
		t.Fatalf("no gh call expected, got %v", stub.calls())
	}
}

func TestGithubPrCreateArgs(t *testing.T) {
	stub := newGithubStub(t)
	stub.respond("https://github.com/FreePeak/xdev/pull/99\n")
	tool := NewGithubTool(stub.repo)

	res := runGithub(t, tool, `{"op":"pr_create","title":"add widget","body":"body text","base":"main","head":"feat/widget","draft":true,"reviewer":["ann","bob"],"label":"enhancement","assignee":"me","repo":"FreePeak/xdev"}`)
	if res.IsError {
		t.Fatalf("pr_create errored: %s", res.Text)
	}
	if !strings.Contains(res.Text, "https://github.com/FreePeak/xdev/pull/99") {
		t.Fatalf("pr_create text = %q", res.Text)
	}
	argv := stub.calls()[0].args
	want := []string{"pr", "create", "--title", "add widget", "--body-file"}
	if len(argv) < len(want)+1 {
		t.Fatalf("argv = %q", argv)
	}
	for i, w := range want {
		if argv[i] != w {
			t.Fatalf("argv[%d] = %q, want %q (argv = %q)", i, argv[i], w, argv)
		}
	}
	bodyFile := argv[len(want)]
	// The staged body is read while gh runs (the stub copies it), because
	// the tool removes the temp file as soon as the call returns.
	raw, err := os.ReadFile(filepath.Join(stub.dir, "captures", strconv.Itoa(len(want)+1)))
	if err != nil {
		t.Fatalf("body file %s not staged for gh: %v", bodyFile, err)
	}
	if string(raw) != "body text" {
		t.Fatalf("body file = %q", raw)
	}
	rest := strings.Join(argv[len(want)+1:], " ")
	for _, w := range []string{"--base main", "--head feat/widget", "--draft", "--reviewer ann", "--reviewer bob", "--label enhancement", "--assignee me", "--repo FreePeak/xdev"} {
		if !strings.Contains(rest, w) {
			t.Fatalf("argv missing %q: %q", w, argv)
		}
	}
	if _, err := os.Stat(bodyFile); !os.IsNotExist(err) {
		t.Fatalf("staged body file %s must be removed after the call", bodyFile)
	}
}

func TestGithubPrCreateEmptyBodySuppressesEditor(t *testing.T) {
	stub := newGithubStub(t)
	stub.respond("https://github.com/FreePeak/xdev/pull/1\n")
	runGithub(t, NewGithubTool(stub.repo), `{"op":"pr_create","title":"t"}`)
	assertArgs(t, stub.calls()[0], "gh", stub.repo, "pr", "create", "--title", "t", "--body", "")
}

func TestGithubPrCreateValidation(t *testing.T) {
	stub := newGithubStub(t)
	tool := NewGithubTool(stub.repo)
	for _, args := range []string{
		`{"op":"pr_create"}`,
		`{"op":"pr_create","fill":true,"title":"both"}`,
	} {
		if res := runGithub(t, tool, args); !res.IsError {
			t.Fatalf("%s must be rejected, got %q", args, res.Text)
		}
	}
	if len(stub.calls()) != 0 {
		t.Fatalf("no gh call expected, got %v", stub.calls())
	}
}

func TestGithubPrCheckoutCreatesWorktree(t *testing.T) {
	stub := newGithubStub(t)
	stub.respond(prMetaJSON)
	stub.respondGit("origin\n")
	scratch := t.TempDir()
	tool := NewGithubTool(stub.repo)
	tool.ScratchDir = scratch

	res := runGithub(t, tool, `{"op":"pr_checkout","pr":"7"}`)
	if res.IsError {
		t.Fatalf("pr_checkout errored: %s", res.Text)
	}
	worktree := filepath.Join(scratch, "pr-7")
	if !strings.Contains(res.Text, worktree) {
		t.Fatalf("checkout text = %q, want the worktree path", res.Text)
	}
	details, ok := res.Details.(map[string]any)
	if !ok || details["worktreePath"] != worktree {
		t.Fatalf("details = %#v", res.Details)
	}
	calls := stub.calls()
	if len(calls) != 4 {
		t.Fatalf("calls = %v", calls)
	}
	assertArgs(t, calls[0], "gh", stub.repo, "pr", "view", "7", "--json", ghPRMetaFields)
	assertArgs(t, calls[1], "git", stub.repo, "remote")
	assertArgs(t, calls[2], "git", stub.repo, "fetch", "origin", "refs/pull/7/head:refs/heads/pr-7")
	assertArgs(t, calls[3], "git", stub.repo, "worktree", "add", worktree, "pr-7")
	for _, c := range calls {
		if c.bin == "git" && (strings.Contains(strings.Join(c.args, " "), "checkout") || strings.Contains(strings.Join(c.args, " "), "switch")) {
			t.Fatalf("pr_checkout must never switch the working tree: %q", c.args)
		}
	}
}

func TestGithubPrPushAfterCheckout(t *testing.T) {
	stub := newGithubStub(t)
	stub.respond(prMetaJSON)
	stub.respondGit("origin\n")
	scratch := t.TempDir()
	tool := NewGithubTool(stub.repo)
	tool.ScratchDir = scratch

	if res := runGithub(t, tool, `{"op":"pr_checkout","pr":"7"}`); res.IsError {
		t.Fatalf("pr_checkout errored: %s", res.Text)
	}
	worktree := filepath.Join(scratch, "pr-7")
	if err := os.MkdirAll(worktree, 0o755); err != nil { // the real git creates it
		t.Fatal(err)
	}
	stub.reset()

	if res := runGithub(t, tool, `{"op":"pr_push","pr":"7","forceWithLease":true}`); res.IsError {
		t.Fatalf("pr_push errored: %s", res.Text)
	}
	push := stub.calls()
	if len(push) != 1 {
		t.Fatalf("push calls = %v", push)
	}
	assertArgs(t, push[0], "git", worktree, "push", "--force-with-lease", "origin", "HEAD:feat/widget")
}

func TestGithubPrPushSelectsCheckoutByBranch(t *testing.T) {
	stub := newGithubStub(t)
	stub.respond(prMetaJSON)
	stub.respondGit("origin\n")
	scratch := t.TempDir()
	tool := NewGithubTool(stub.repo)
	tool.ScratchDir = scratch
	if res := runGithub(t, tool, `{"op":"pr_checkout","pr":"7"}`); res.IsError {
		t.Fatalf("pr_checkout errored: %s", res.Text)
	}
	worktree := filepath.Join(scratch, "pr-7")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	stub.reset()
	if res := runGithub(t, tool, `{"op":"pr_push","branch":"pr-7"}`); res.IsError {
		t.Fatalf("pr_push errored: %s", res.Text)
	}
	assertArgs(t, stub.calls()[0], "git", worktree, "push", "origin", "HEAD:feat/widget")
}

func TestGithubPrPushUsesTheOnlyCheckout(t *testing.T) {
	stub := newGithubStub(t)
	stub.respond(prMetaJSON)
	stub.respondGit("origin\n")
	scratch := t.TempDir()
	tool := NewGithubTool(stub.repo)
	tool.ScratchDir = scratch
	if res := runGithub(t, tool, `{"op":"pr_checkout","pr":"7"}`); res.IsError {
		t.Fatalf("pr_checkout errored: %s", res.Text)
	}
	worktree := filepath.Join(scratch, "pr-7")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	stub.reset()
	if res := runGithub(t, tool, `{"op":"pr_push"}`); res.IsError {
		t.Fatalf("pr_push errored: %s", res.Text)
	}
	assertArgs(t, stub.calls()[0], "git", worktree, "push", "origin", "HEAD:feat/widget")
}

func TestGithubPrPushAmbiguousWithoutIdentifier(t *testing.T) {
	stub := newGithubStub(t)
	stub.respondSeq(prMetaJSON, `{"number":8,"title":"other","url":"https://github.com/FreePeak/xdev/pull/8","state":"OPEN","headRefName":"other","headRefOid":"deadbeefcafe0001","baseRefName":"main"}`)
	stub.respondGit("origin\n")
	tool := NewGithubTool(stub.repo)
	tool.ScratchDir = t.TempDir()
	for _, args := range []string{`{"op":"pr_checkout","pr":"7"}`, `{"op":"pr_checkout","pr":"8"}`} {
		if res := runGithub(t, tool, args); res.IsError {
			t.Fatalf("%s errored: %s", args, res.Text)
		}
	}
	stub.reset()
	res := runGithub(t, tool, `{"op":"pr_push"}`)
	if !res.IsError || !strings.Contains(res.Text, "pr_checkout") {
		t.Fatalf("an ambiguous push must be rejected, got %+v", res)
	}
	if len(stub.calls()) != 0 {
		t.Fatalf("no git call expected, got %v", stub.calls())
	}
}

func TestGithubPrCheckoutExistingWorktreeRequiresForce(t *testing.T) {
	stub := newGithubStub(t)
	stub.respond(prMetaJSON)
	stub.respondGit("origin\n")
	scratch := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scratch, "pr-7"), 0o755); err != nil {
		t.Fatal(err)
	}
	tool := NewGithubTool(stub.repo)
	tool.ScratchDir = scratch

	res := runGithub(t, tool, `{"op":"pr_checkout","pr":"7"}`)
	if !res.IsError || !strings.Contains(res.Text, "force") {
		t.Fatalf("existing worktree must require force, got %+v", res)
	}
	stub.reset()
	if res := runGithub(t, tool, `{"op":"pr_checkout","pr":"7","force":true}`); res.IsError {
		t.Fatalf("forced checkout errored: %s", res.Text)
	}
	calls := stub.calls()
	assertArgs(t, calls[1], "git", stub.repo, "worktree", "remove", "--force", filepath.Join(scratch, "pr-7"))
	assertArgs(t, calls[2], "git", stub.repo, "remote")
	assertArgs(t, calls[3], "git", stub.repo, "fetch", "origin", "+refs/pull/7/head:refs/heads/pr-7")
	assertArgs(t, calls[4], "git", stub.repo, "worktree", "add", "--force", filepath.Join(scratch, "pr-7"), "pr-7")
}

func TestGithubPrPushWithoutCheckoutRejected(t *testing.T) {
	stub := newGithubStub(t)
	res := runGithub(t, NewGithubTool(stub.repo), `{"op":"pr_push"}`)
	if !res.IsError || !strings.Contains(res.Text, "pr_checkout") {
		t.Fatalf("push without checkout must be rejected, got %+v", res)
	}
	if len(stub.calls()) != 0 {
		t.Fatalf("no git/gh call expected, got %v", stub.calls())
	}
}

func TestGithubPrCheckoutCrossRepositoryFetchTarget(t *testing.T) {
	stub := newGithubStub(t)
	stub.respond(`{"number":8,"title":"fork pr","url":"https://github.com/FreePeak/xdev/pull/8","state":"OPEN","headRefName":"widget","headRefOid":"deadbeefcafe0001","baseRefName":"main","isCrossRepository":true,"headRepository":{"name":"xdev"},"headRepositoryOwner":{"login":"someone"}}`)
	tool := NewGithubTool(stub.repo)
	tool.ScratchDir = t.TempDir()

	if res := runGithub(t, tool, `{"op":"pr_checkout","pr":"8"}`); res.IsError {
		t.Fatalf("pr_checkout errored: %s", res.Text)
	}
	scratch := tool.ScratchDir
	calls := stub.calls()
	if len(calls) != 3 {
		t.Fatalf("calls = %v (a fork PR fetches straight from the fork)", calls)
	}
	assertArgs(t, calls[1], "git", stub.repo, "fetch", "https://github.com/someone/xdev.git", "refs/pull/8/head:refs/heads/pr-8")
	assertArgs(t, calls[2], "git", stub.repo, "worktree", "add", filepath.Join(scratch, "pr-8"), "pr-8")
}

func TestGithubSearchArgs(t *testing.T) {
	cases := []struct {
		name string
		args string
		want []string
	}{
		{
			name: "issues with dates",
			args: `{"op":"search_issues","query":"flaky test","repo":"FreePeak/xdev","since":"3d","until":"2026-09-12","dateField":"updated","limit":3}`,
			want: []string{"search", "issues", "flaky test updated:>=3d updated:<=2026-09-12 is:issue", "--limit", "3", "--json", ghSearchIssueFields, "--repo", "FreePeak/xdev"},
		},
		{
			name: "prs",
			args: `{"op":"search_prs","query":"widget"}`,
			want: []string{"search", "prs", "widget is:pr", "--limit", "10", "--json", ghSearchPRFields},
		},
		{
			name: "code",
			args: `{"op":"search_code","query":"func main"}`,
			want: []string{"search", "code", "func main", "--limit", "10", "--json", ghSearchCodeFields},
		},
		{
			name: "commits use committer-date",
			args: `{"op":"search_commits","query":"fix","since":"2026-01-01","dateField":"updated","repo":"FreePeak/xdev"}`,
			want: []string{"search", "commits", "fix committer-date:>=2026-01-01", "--limit", "10", "--json", ghSearchCommitFields, "--repo", "FreePeak/xdev"},
		},
		{
			name: "repos never forward repo",
			args: `{"op":"search_repos","query":"cli","repo":"FreePeak/xdev","dateField":"updated","since":"2w"}`,
			want: []string{"search", "repos", "cli pushed:>=2w", "--limit", "10", "--json", ghSearchRepoFields},
		},
		{
			name: "a qualifier-only search needs no query",
			args: `{"op":"search_issues"}`,
			want: []string{"search", "issues", "is:issue", "--limit", "10", "--json", ghSearchIssueFields},
		},
		{
			name: "limit clamps to 50",
			args: `{"op":"search_issues","limit":999}`,
			want: []string{"search", "issues", "is:issue", "--limit", "50", "--json", ghSearchIssueFields},
		},
		{
			name: "limit below one falls back to the default",
			args: `{"op":"search_issues","limit":-4}`,
			want: []string{"search", "issues", "is:issue", "--limit", "10", "--json", ghSearchIssueFields},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newGithubStub(t)
			stub.respond("[]")
			res := runGithub(t, NewGithubTool(stub.repo), tc.args)
			if res.IsError {
				t.Fatalf("search errored: %s", res.Text)
			}
			if res.Text != "no results" {
				t.Fatalf("empty search text = %q", res.Text)
			}
			calls := stub.calls()
			if len(calls) != 1 {
				t.Fatalf("calls = %v", calls)
			}
			assertArgs(t, calls[0], "gh", stub.repo, tc.want...)
		})
	}
}

func TestGithubSearchValidation(t *testing.T) {
	stub := newGithubStub(t)
	tool := NewGithubTool(stub.repo)
	for _, args := range []string{
		`{"op":"search_code"}`,
		`{"op":"search_code","query":"x","since":"3d"}`,
	} {
		if res := runGithub(t, tool, args); !res.IsError {
			t.Fatalf("%s must be rejected, got %q", args, res.Text)
		}
	}
	if len(stub.calls()) != 0 {
		t.Fatalf("no gh call expected, got %v", stub.calls())
	}
}

func TestGithubSearchRendersResults(t *testing.T) {
	stub := newGithubStub(t)
	stub.respond(`[{"number":49,"title":"github tool","state":"OPEN","url":"https://github.com/FreePeak/xdev/issues/49","repository":{"nameWithOwner":"FreePeak/xdev"}}]`)
	res := runGithub(t, NewGithubTool(stub.repo), `{"op":"search_issues","query":"github"}`)
	if res.IsError {
		t.Fatalf("search errored: %s", res.Text)
	}
	if !strings.Contains(res.Text, "#49 [open] github tool — https://github.com/FreePeak/xdev/issues/49 (FreePeak/xdev)") {
		t.Fatalf("search text = %q", res.Text)
	}
}

func TestGithubRunWatchArgsAndRender(t *testing.T) {
	stub := newGithubStub(t)
	stub.respond(`{"databaseId":12345,"status":"completed","conclusion":"success","url":"https://github.com/FreePeak/xdev/actions/runs/12345","headSha":"8655781f556defb80f20de939fa852585947c506","headBranch":"main","displayTitle":"CI","workflowName":"CI","jobs":[{"name":"build","status":"completed","conclusion":"success"}]}`)
	tool := NewGithubTool(stub.repo)

	res := runGithub(t, tool, `{"op":"run_watch","run":"https://github.com/FreePeak/xdev/actions/runs/12345"}`)
	if res.IsError {
		t.Fatalf("run_watch errored: %s", res.Text)
	}
	calls := stub.calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %v (a completed run needs one poll)", calls)
	}
	assertArgs(t, calls[0], "gh", stub.repo, "run", "view", "12345", "--json", ghRunViewFields)
	for _, want := range []string{"run 12345 CI [completed/success]", "head: 8655781f (main)", "url: https://github.com/FreePeak/xdev/actions/runs/12345"} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("run_watch text missing %q:\n%s", want, res.Text)
		}
	}
}

func TestGithubRunWatchCapturesFailedLog(t *testing.T) {
	stub := newGithubStub(t)
	stub.respondSeq(
		`{"databaseId":12345,"status":"completed","conclusion":"failure","url":"https://github.com/FreePeak/xdev/actions/runs/12345","headSha":"8655781f556defb80f20de939fa852585947c506","headBranch":"main","workflowName":"CI","jobs":[{"name":"build","status":"completed","conclusion":"failure"},{"name":"lint","status":"completed","conclusion":"success"}]}`,
		"step 1 ok\nstep 2 boom\n",
	)
	tool := NewGithubTool(stub.repo)

	res := runGithub(t, tool, `{"op":"run_watch","run":"12345","tail":5}`)
	if res.IsError {
		t.Fatalf("run_watch errored: %s", res.Text)
	}
	calls := stub.calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %v, want a poll plus the failed-log fetch", calls)
	}
	assertArgs(t, calls[1], "gh", stub.repo, "run", "view", "12345", "--log-failed")
	for _, want := range []string{"failed jobs:", "- build", "failed log (last 5 lines):", "step 2 boom"} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("run_watch text missing %q:\n%s", want, res.Text)
		}
	}
	if strings.Contains(res.Text, "- lint") {
		t.Fatalf("passing jobs must not be listed as failed:\n%s", res.Text)
	}
}

func TestGithubRunWatchResolvesLatestRunOnBranch(t *testing.T) {
	stub := newGithubStub(t)
	stub.respondSeq(
		`[{"databaseId":777,"status":"completed","conclusion":"success","url":"https://github.com/FreePeak/xdev/actions/runs/777","headSha":"abc1234567890def","workflowName":"CI"}]`,
		`{"databaseId":777,"status":"completed","conclusion":"success","url":"https://github.com/FreePeak/xdev/actions/runs/777","headSha":"abc1234567890def","headBranch":"main","workflowName":"CI"}`,
	)
	stub.respondGit("main\n")
	tool := NewGithubTool(stub.repo)

	if res := runGithub(t, tool, `{"op":"run_watch"}`); res.IsError {
		t.Fatalf("run_watch errored: %s", res.Text)
	}
	calls := stub.calls()
	if len(calls) != 3 {
		t.Fatalf("calls = %v", calls)
	}
	assertArgs(t, calls[0], "git", stub.repo, "rev-parse", "--abbrev-ref", "HEAD")
	assertArgs(t, calls[1], "gh", stub.repo, "run", "list", "--branch", "main", "--limit", "1", "--json", "databaseId,status,conclusion,url,headSha,workflowName")
	assertArgs(t, calls[2], "gh", stub.repo, "run", "view", "777", "--json", ghRunViewFields)
}

func TestGithubRunWatchRejectsBadRunID(t *testing.T) {
	stub := newGithubStub(t)
	res := runGithub(t, NewGithubTool(stub.repo), `{"op":"run_watch","run":"not-a-run"}`)
	if !res.IsError || !strings.Contains(res.Text, "numeric run id") {
		t.Fatalf("bad run id must be rejected, got %+v", res)
	}
	if len(stub.calls()) != 0 {
		t.Fatalf("no gh call expected, got %v", stub.calls())
	}
}

func TestGithubMissingCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH handling differs on windows")
	}
	t.Setenv("PATH", t.TempDir()) // an empty PATH: no gh anywhere
	res := runGithub(t, NewGithubTool(t.TempDir()), `{"op":"repo_view"}`)
	if !res.IsError {
		t.Fatalf("missing gh must error, got %q", res.Text)
	}
	for _, want := range []string{"gh", "cli.github.com", "gh auth login"} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("missing-gh text missing %q: %q", want, res.Text)
		}
	}
}

func TestGithub401Hint(t *testing.T) {
	stub := newGithubStub(t)
	stub.fail(1, "gh: Bad credentials (HTTP 401)\n")
	res := runGithub(t, NewGithubTool(stub.repo), `{"op":"repo_view"}`)
	if !res.IsError {
		t.Fatalf("401 must error, got %q", res.Text)
	}
	for _, want := range []string{"Bad credentials", "GITHUB_TOKEN", "env -u GITHUB_TOKEN"} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("401 text missing %q: %q", want, res.Text)
		}
	}
}

// TestGithubPrCreateUnpushedBranchHint pins the fix for the live failure a
// session hit: gh in the session cwd aborted with "or use the --head flag"
// (GH_PROMPT_DISABLED makes its push prompt impossible) while the commits sat
// on a branch in a sibling worktree. The error must name the branch and the
// directory gh ran in, and must never claim to have pushed anything.
func TestGithubPrCreateUnpushedBranchHint(t *testing.T) {
	stub := newGithubStub(t)
	stub.fail(1, "Warning: 1 uncommitted change\naborted: you must first push the current branch to a remote, or use the --head flag\n")
	stub.respondGit("fix/thing\n")
	res := runGithub(t, NewGithubTool(stub.repo), `{"op":"pr_create","title":"t"}`)
	if !res.IsError {
		t.Fatalf("unpushed branch must error, got %q", res.Text)
	}
	for _, want := range []string{"aborted:", "never pushes your working tree", `branch "fix/thing"`, stub.repo} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("hint missing %q: %q", want, res.Text)
		}
	}
	// The gh argv stays untouched: reading the branch is a git call.
	argv := stub.calls()[0].args
	for _, a := range argv {
		if a == "--head" {
			t.Fatalf("pr_create must not invent --head: %q", argv)
		}
	}
}

func TestGithubOpValidation(t *testing.T) {
	stub := newGithubStub(t)
	tool := NewGithubTool(stub.repo)
	if res := runGithub(t, tool, `{}`); !res.IsError || !strings.Contains(res.Text, "op is required") {
		t.Fatalf("missing op: %+v", res)
	}
	if res := runGithub(t, tool, `{"op":"nope"}`); !res.IsError || !strings.Contains(res.Text, "unknown op") {
		t.Fatalf("unknown op: %+v", res)
	}
	if len(stub.calls()) != 0 {
		t.Fatalf("no gh call expected, got %v", stub.calls())
	}
}

func TestGithubArgsAcceptStringOrArray(t *testing.T) {
	var a githubArgs
	if err := json.Unmarshal([]byte(`{"op":"pr_checkout","pr":7,"reviewer":"ann"}`), &a); err != nil {
		t.Fatal(err)
	}
	if len(a.PR) != 1 || a.PR[0] != "7" {
		t.Fatalf("scalar pr = %#v", a.PR)
	}
	if len(a.Reviewer) != 1 || a.Reviewer[0] != "ann" {
		t.Fatalf("scalar reviewer = %#v", a.Reviewer)
	}
	if err := json.Unmarshal([]byte(`{"pr":[1,2],"reviewer":["a","b"]}`), &a); err != nil {
		t.Fatal(err)
	}
	if len(a.PR) != 2 || a.PR[1] != "2" || len(a.Reviewer) != 2 {
		t.Fatalf("array form = %#v / %#v", a.PR, a.Reviewer)
	}
}

func TestGithubHelpers(t *testing.T) {
	if got := trailingNumber("https://github.com/o/r/actions/runs/42/"); got != "42" {
		t.Fatalf("trailingNumber = %q", got)
	}
	if got := trailingNumber("42x"); got != "" {
		t.Fatalf("trailingNumber(42x) = %q", got)
	}
	if got := normalizeRepo("https://github.com/FreePeak/xdev"); got != "github.com/FreePeak/xdev" {
		t.Fatalf("normalizeRepo = %q", got)
	}
	if got := splitRepoRef("github.com/FreePeak/xdev"); got != "FreePeak/xdev" {
		t.Fatalf("splitRepoRef = %q", got)
	}
	if got := splitRepoRef("ghe.example.com/FreePeak/xdev"); got != "ghe.example.com/FreePeak/xdev" {
		t.Fatalf("enterprise repo must keep its host: %q", got)
	}
	if got := tailLines("a\nb\nc\nd\n", 2); got != "c\nd" {
		t.Fatalf("tailLines = %q", got)
	}
	if got := pollInterval(30 * time.Second); got != 3*time.Second {
		t.Fatalf("pollInterval(<1m) = %s", got)
	}
	if got := pollInterval(2 * time.Minute); got != 15*time.Second {
		t.Fatalf("pollInterval(>1m) = %s", got)
	}
	if got := capText("héllo wörld", 6); !strings.HasSuffix(got, "[truncated]") || !strings.HasPrefix(got, "héll") {
		t.Fatalf("capText = %q", got)
	}
	if got := firstGithubURL("Creating pull request for x\nhttps://github.com/o/r/pull/9\n"); got != "https://github.com/o/r/pull/9" {
		t.Fatalf("firstGithubURL = %q", got)
	}
}
