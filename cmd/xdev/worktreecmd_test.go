package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/tool"
)

// Every test below drives the real git binary through the same seam the CLI
// uses (tool.GitCLI) in a throwaway repository: git's porcelain format is the
// contract, so a hand-written fixture alone would not prove the parser.

// worktreeTestRepo creates a repository with one commit on `main` and returns
// its symlink-resolved path. Git reports realpath'd worktree paths, and on
// macOS t.TempDir() hands out /var/... (a symlink to /private/var), so every
// path assertion has to compare resolved forms.
func worktreeTestRepo(t *testing.T) string {
	t.Helper()
	// A developer's global config must not decide these tests: gpg signing,
	// hooksPath or an odd init.defaultBranch would otherwise leak in.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	dir := worktreeTestReal(t, t.TempDir())
	worktreeTestGit(t, dir, "init", "-b", "main")
	worktreeTestGit(t, dir,
		"-c", "user.email=worktree@example.test",
		"-c", "user.name=worktree test",
		"-c", "commit.gpgsign=false",
		"commit", "--allow-empty", "-m", "init")
	return dir
}

// worktreeTestGit runs git for the fixtures; a failure is fatal because every
// later assertion would be meaningless.
func worktreeTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	res, err := tool.GitCLI(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return res
}

func worktreeTestReal(t *testing.T, p string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", p, err)
	}
	return real
}

// worktreeTestRun invokes the CLI seam and keeps stdout and stderr apart, so
// the tests can assert that usage goes to stderr and data to stdout.
func worktreeTestRun(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := worktreeCmd(args, dir, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// worktreeTestLine returns the one output line containing substr.
func worktreeTestLine(t *testing.T, out, substr string) string {
	t.Helper()
	var hits []string
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, substr) {
			hits = append(hits, ln)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("want exactly one line containing %q, got %d in:\n%s", substr, len(hits), out)
	}
	return hits[0]
}

// worktreeTestRows decodes `list --json` the way a script would: generically,
// so the test fails if a field disappears from the documented shape.
func worktreeTestRows(t *testing.T, dir string) []map[string]any {
	t.Helper()
	code, stdout, stderr := worktreeTestRun(t, dir, "list", "--json")
	if code != 0 {
		t.Fatalf("list --json exit %d: %s", code, stderr)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("list --json is not a JSON array: %v\n%s", err, stdout)
	}
	return rows
}

// worktreeTestRowFor finds the row for a worktree path, resolved.
func worktreeTestRowFor(t *testing.T, rows []map[string]any, path string) map[string]any {
	t.Helper()
	want := worktreeTestReal(t, path)
	for _, r := range rows {
		if r["path"] == want {
			return r
		}
	}
	t.Fatalf("no worktree row for %s in %v", want, rows)
	return nil
}

// worktreeTestPath returns a not-yet-existing directory beside a fresh temp
// dir, resolved the way git will report it after creating the worktree.
func worktreeTestPath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(worktreeTestReal(t, t.TempDir()), name)
}

func TestWorktreeListReportsRepositoryRow(t *testing.T) {
	repo := worktreeTestRepo(t)
	code, stdout, stderr := worktreeTestRun(t, repo, "list")
	if code != 0 {
		t.Fatalf("list exit %d: %s", code, stderr)
	}
	header := worktreeTestLine(t, stdout, "PATH")
	for _, col := range []string{"BRANCH", "HEAD"} {
		if !strings.Contains(header, col) {
			t.Fatalf("table header %q is missing %s", header, col)
		}
	}
	fields := strings.Fields(worktreeTestLine(t, stdout, repo))
	if len(fields) != 3 {
		t.Fatalf("row = %v, want PATH BRANCH HEAD", fields)
	}
	if fields[0] != repo {
		t.Fatalf("path = %q, want %q", fields[0], repo)
	}
	if fields[1] != "main" {
		t.Fatalf("branch = %q, want main", fields[1])
	}
	if len(fields[2]) != 7 {
		t.Fatalf("head = %q, want a 7-char short sha", fields[2])
	}
	if _, err := strconv.ParseUint(fields[2], 16, 32); err != nil {
		t.Fatalf("head %q is not hex: %v", fields[2], err)
	}

	// No subcommand means list: the default must be the useful one.
	code, def, stderr := worktreeTestRun(t, repo)
	if code != 0 {
		t.Fatalf("bare `worktree` exit %d: %s", code, stderr)
	}
	if def != stdout {
		t.Fatalf("bare `worktree` is not the list output:\n%s", def)
	}
}

func TestWorktreeAddCreatesListedWorktree(t *testing.T) {
	repo := worktreeTestRepo(t)
	wt := worktreeTestPath(t, "wt")

	code, stdout, stderr := worktreeTestRun(t, repo, "add", wt)
	if code != 0 {
		t.Fatalf("add exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, wt) {
		t.Fatalf("add did not name the created path %s:\n%s", wt, stdout)
	}
	if fi, err := os.Stat(wt); err != nil || !fi.IsDir() {
		t.Fatalf("worktree not on disk at %s: %v", wt, err)
	}

	row := worktreeTestRowFor(t, worktreeTestRows(t, repo), wt)
	// Without -b git derives the branch from the last path component.
	if row["branch"] != "wt" {
		t.Fatalf("branch = %v, want wt (derived from the path)", row["branch"])
	}
	if row["detached"] != false || row["bare"] != false {
		t.Fatalf("fresh worktree row = %v", row)
	}
	if head, _ := row["head"].(string); len(head) != 40 {
		t.Fatalf("head = %v, want a full sha", row["head"])
	}

	// -b names the branch instead of deriving it from the path.
	wt2 := worktreeTestPath(t, "feature")
	if code, _, stderr := worktreeTestRun(t, repo, "add", "-b", "xdev-feature", wt2); code != 0 {
		t.Fatalf("add -b exit %d: %s", code, stderr)
	}
	if row := worktreeTestRowFor(t, worktreeTestRows(t, repo), wt2); row["branch"] != "xdev-feature" {
		t.Fatalf("branch = %v, want xdev-feature", row["branch"])
	}
}

func TestWorktreeRemoveDeletesWorktree(t *testing.T) {
	repo := worktreeTestRepo(t)
	wt := worktreeTestPath(t, "gone")
	if code, _, stderr := worktreeTestRun(t, repo, "add", wt); code != 0 {
		t.Fatalf("add exit %d: %s", code, stderr)
	}

	// git refuses a dirty worktree without --force; that refusal must reach
	// the user as a failure rather than a silent deletion.
	if err := os.WriteFile(filepath.Join(wt, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	code, _, stderr := worktreeTestRun(t, repo, "remove", wt)
	if code != 1 {
		t.Fatalf("dirty remove exit %d, want 1: %s", code, stderr)
	}
	if !strings.Contains(stderr, "xdev worktree remove:") {
		t.Fatalf("dirty remove did not report a git failure: %s", stderr)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("refused remove deleted the worktree anyway: %v", err)
	}

	code, stdout, stderr := worktreeTestRun(t, repo, "remove", "--force", wt)
	if code != 0 {
		t.Fatalf("remove --force exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, wt) {
		t.Fatalf("remove did not name the removed path:\n%s", stdout)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree still on disk: %v", err)
	}
	for _, r := range worktreeTestRows(t, repo) {
		if r["path"] == wt {
			t.Fatalf("removed worktree still registered: %v", r)
		}
	}
}

func TestWorktreePruneDropsStaleRegistration(t *testing.T) {
	repo := worktreeTestRepo(t)
	wt := worktreeTestPath(t, "stale")
	if code, _, stderr := worktreeTestRun(t, repo, "add", wt); code != 0 {
		t.Fatalf("add exit %d: %s", code, stderr)
	}
	if err := os.RemoveAll(wt); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	// A registration whose directory vanished is reported, not forgotten.
	code, stdout, stderr := worktreeTestRun(t, repo, "list")
	if code != 0 {
		t.Fatalf("list exit %d: %s", code, stderr)
	}
	if line := worktreeTestLine(t, stdout, wt); !strings.Contains(line, "(prunable)") {
		t.Fatalf("row does not flag the stale worktree: %q", line)
	}

	// --dry-run reports without pruning.
	if code, _, stderr := worktreeTestRun(t, repo, "prune", "--dry-run"); code != 0 {
		t.Fatalf("prune --dry-run exit %d: %s", code, stderr)
	}
	if !worktreeTestHasPath(t, worktreeTestRows(t, repo), wt) {
		t.Fatalf("prune --dry-run removed the registration")
	}

	if code, _, stderr := worktreeTestRun(t, repo, "prune"); code != 0 {
		t.Fatalf("prune exit %d: %s", code, stderr)
	}
	if worktreeTestHasPath(t, worktreeTestRows(t, repo), wt) {
		t.Fatalf("prune left the stale registration in place")
	}
}

// worktreeTestHasPath reports whether a JSON row exists for path.
func worktreeTestHasPath(t *testing.T, rows []map[string]any, path string) bool {
	t.Helper()
	for _, r := range rows {
		if r["path"] == path {
			return true
		}
	}
	return false
}

func TestWorktreeListJSONShape(t *testing.T) {
	repo := worktreeTestRepo(t)
	rows := worktreeTestRows(t, repo)
	if len(rows) != 1 {
		t.Fatalf("fresh repository has %d worktrees, want 1: %v", len(rows), rows)
	}
	row := worktreeTestRowFor(t, rows, repo)
	for _, key := range []string{"path", "head", "branch", "detached", "bare", "locked", "prunable"} {
		if _, ok := row[key]; !ok {
			t.Fatalf("row is missing field %q: %v", key, row)
		}
	}
	if row["branch"] != "main" {
		t.Fatalf("branch = %v, want main", row["branch"])
	}
	for _, key := range []string{"detached", "bare", "locked", "prunable"} {
		if _, ok := row[key].(bool); !ok {
			t.Fatalf("field %q = %v, want a bool", key, row[key])
		}
	}
}

func TestWorktreeUsageErrors(t *testing.T) {
	repo := worktreeTestRepo(t)
	cases := []struct {
		name string
		args []string
	}{
		{"unknown subcommand", []string{"bogus"}},
		{"add without a path", []string{"add"}},
		{"remove without a path", []string{"remove"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := worktreeTestRun(t, repo, tc.args...)
			if code != 2 {
				t.Fatalf("exit %d, want 2 (stderr: %s)", code, stderr)
			}
			if !strings.Contains(stderr, "usage: xdev worktree") {
				t.Fatalf("no usage block for %v: %s", tc.args, stderr)
			}
			if stdout != "" {
				t.Fatalf("usage errors must not write to stdout: %q", stdout)
			}
		})
	}

	// An explicit help request is not an error.
	code, _, stderr := worktreeTestRun(t, repo, "help")
	if code != 0 || !strings.Contains(stderr, "usage: xdev worktree") {
		t.Fatalf("help exit %d: %s", code, stderr)
	}
}

// TestWorktreeParsePorcelain pins the parser against the shapes real git
// emits that a fresh repository never produces: a detached HEAD, a locked
// entry, a bare repository and a prunable one — plus the trailing blank line
// git always writes.
func TestWorktreeParsePorcelain(t *testing.T) {
	raw := "worktree /repo/main\nHEAD 0123456789abcdef0123456789abcdef01234567\nbranch refs/heads/main\n\n" +
		"worktree /repo/detached\nHEAD fedcba9876543210fedcba9876543210fedcba98\ndetached\n\n" +
		"worktree /repo/locked tree\nHEAD 1111111111111111111111111111111111111111\nbranch refs/heads/topic\nlocked wrong checkout\n\n" +
		"worktree /repo/bare\nHEAD 2222222222222222222222222222222222222222\nbare\nprunable gitdir file points to non-existent location\n\n"
	rows := parseWorktreePorcelain(raw)
	if len(rows) != 4 {
		t.Fatalf("parsed %d rows, want 4: %+v", len(rows), rows)
	}
	want := []worktreeRow{
		{Path: "/repo/main", Head: "0123456789abcdef0123456789abcdef01234567", Branch: "main"},
		{Path: "/repo/detached", Head: "fedcba9876543210fedcba9876543210fedcba98", Detached: true},
		{Path: "/repo/locked tree", Head: "1111111111111111111111111111111111111111", Branch: "topic", Locked: true},
		{Path: "/repo/bare", Head: "2222222222222222222222222222222222222222", Bare: true, Prunable: true},
	}
	for i, w := range want {
		if rows[i] != w {
			t.Fatalf("row %d = %+v, want %+v", i, rows[i], w)
		}
	}
	if got := worktreeBranchText(rows[1]); got != "(detached)" {
		t.Fatalf("detached branch cell = %q", got)
	}
	if got := worktreeMarkerText(rows[3]); got != "(bare) (prunable)" {
		t.Fatalf("bare/prunable marker = %q", got)
	}
	if got := worktreeMarkerText(rows[0]); got != "" {
		t.Fatalf("healthy worktree marker = %q, want empty", got)
	}
	if got := worktreeShortHead(rows[0].Head); got != "0123456" {
		t.Fatalf("short head = %q", got)
	}
}
