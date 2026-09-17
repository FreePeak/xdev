package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/tool"
)

// commitTestRepo initializes a throwaway repository with one staged file and
// returns its directory plus the staged file's content.
func commitTestRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	// A hermetic identity: the host's git config must not decide whether the
	// test can commit.
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_AUTHOR_NAME", "xdev test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "xdev test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.invalid")
	ctx := context.Background()
	if _, err := tool.GitCLI(ctx, dir, "init", "-b", "main"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	content := "package widget\n\n// Widget is the thing under test.\nfunc Widget() string { return \"widget\" }\n"
	if err := os.WriteFile(filepath.Join(dir, "widget.go"), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := tool.GitCLI(ctx, dir, "add", "widget.go"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	return dir, content
}

// TestCommitCmdGeneratesFromStagedDiff is the core contract: the prompt the
// model sees carries the staged diff, and the generated message is printed
// without committing anything.
func TestCommitCmdGeneratesFromStagedDiff(t *testing.T) {
	dir, _ := commitTestRepo(t)

	var gotPrompt, gotModel string
	complete := func(_ context.Context, ref, prompt string, _ int) (string, error) {
		gotPrompt, gotModel = prompt, ref
		return "feat(widget): add the Widget helper\n\nIt gives the package a single entry point.", nil
	}

	var out, errOut bytes.Buffer
	if code := commitCmd([]string{}, dir, &out, &errOut, complete, tool.GitCLI); code != 0 {
		t.Fatalf("commitCmd = %d (%s)", code, errOut.String())
	}
	if gotModel != "" {
		t.Fatalf("model = %q, want the empty ref (the session model resolves it)", gotModel)
	}
	if !strings.Contains(gotPrompt, "func Widget() string") {
		t.Fatalf("the staged diff never reached the prompt:\n%s", gotPrompt)
	}
	if !strings.Contains(gotPrompt, "widget.go") {
		t.Fatal("the diff summary is missing from the prompt")
	}
	if !strings.Contains(out.String(), "feat(widget): add the Widget helper") {
		t.Fatalf("message not printed:\n%s", out.String())
	}
	if _, err := tool.GitCLI(context.Background(), dir, "rev-parse", "HEAD"); err == nil {
		t.Fatal("a message-only run created a commit")
	}

	// The --model flag is what gets resolved.
	out.Reset()
	if code := commitCmd([]string{"--model", "onegw/dev"}, dir, &out, &errOut, complete, tool.GitCLI); code != 0 {
		t.Fatalf("commitCmd --model = %d", code)
	}
	if gotModel != "onegw/dev" {
		t.Fatalf("model = %q, want onegw/dev", gotModel)
	}
}

// TestCommitCmdApplyCommitsTheMessage runs the real git path: --apply must
// leave a commit whose subject is the generated message, and re-running with
// an empty index must refuse.
func TestCommitCmdApplyCommitsTheMessage(t *testing.T) {
	dir, _ := commitTestRepo(t)
	complete := func(context.Context, string, string, int) (string, error) {
		return "feat(widget): add the Widget helper\n\nBody line.", nil
	}

	var out, errOut bytes.Buffer
	if code := commitCmd([]string{"--apply"}, dir, &out, &errOut, complete, tool.GitCLI); code != 0 {
		t.Fatalf("commitCmd --apply = %d (%s)", code, errOut.String())
	}
	subject, err := tool.GitCLI(context.Background(), dir, "log", "-1", "--format=%s")
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if strings.TrimSpace(subject) != "feat(widget): add the Widget helper" {
		t.Fatalf("commit subject = %q", strings.TrimSpace(subject))
	}
	body, err := tool.GitCLI(context.Background(), dir, "log", "-1", "--format=%b")
	if err != nil || !strings.Contains(body, "Body line.") {
		t.Fatalf("commit body = %q (%v)", body, err)
	}

	// Nothing staged now: the command refuses instead of committing again.
	called := false
	out.Reset()
	code := commitCmd([]string{"--apply"}, dir, &out, &errOut, func(context.Context, string, string, int) (string, error) {
		called = true
		return "should not run", nil
	}, tool.GitCLI)
	if code != 2 {
		t.Fatalf("empty index exit = %d, want 2", code)
	}
	if called {
		t.Fatal("the model was called with an empty index")
	}
	if !strings.Contains(errOut.String(), "nothing staged") || !strings.Contains(errOut.String(), "git add") {
		t.Fatalf("empty-index error:\n%s", errOut.String())
	}
}

// TestCommitCleanMessageUnwrapsFences keeps --apply from committing a stray
// fence line when the model ignores the "no fences" instruction.
func TestCommitCleanMessageUnwrapsFences(t *testing.T) {
	cases := map[string]string{
		"```\nfix: a thing\n```":            "fix: a thing",
		"```markdown\nfix: a thing\n```\n":  "fix: a thing",
		"fix: plain subject":                "fix: plain subject",
		"  fix: padded subject\n\nbody\n  ": "fix: padded subject\n\nbody",
	}
	for in, want := range cases {
		if got := cleanCommitMessage(in); got != want {
			t.Fatalf("cleanCommitMessage(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCommitPromptTruncatesHugeDiffs keeps a giant diff from becoming a giant
// request.
func TestCommitPromptTruncatesHugeDiffs(t *testing.T) {
	huge := strings.Repeat("+added line\n", 20000) // ~240 KiB
	prompt := commitPrompt(huge, " 1 file changed")
	if !strings.Contains(prompt, "diff truncated") {
		t.Fatal("huge diff was not truncated")
	}
	if len(prompt) > 200<<10 {
		t.Fatalf("prompt is %d bytes, want the cap to hold", len(prompt))
	}
}
