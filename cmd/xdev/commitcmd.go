package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/tool"
)

// runCommit implements `xdev commit` (issue #34): generate a commit message
// from the staged diff on the session model and print it; --apply commits.
// The message is a real model turn over the same diff git itself reports, not
// a template — the model decides subject and body.
func runCommit(args []string) int {
	return commitCmd(args, mustGetwd(), os.Stdout, os.Stderr, defaultCommitCompleter, tool.GitCLI)
}

// commitGit is the git runner seam (tool.GitCLI in production).
type commitGit func(ctx context.Context, dir string, args ...string) (string, error)

// commitCompleter runs one model turn (production: the resolved model ref
// through the same resolver a run uses).
type commitCompleter func(ctx context.Context, ref, prompt string, maxTokens int) (string, error)

// commitStats reports the inputs and model behind a generated message.
type commitStats struct {
	Model     string `json:"model"`
	DiffBytes int    `json:"diffBytes"`
	Elapsed   string `json:"elapsed"`
}

func commitCmd(args []string, cwd string, out, errOut io.Writer, complete commitCompleter, git commitGit) int {
	fs := flag.NewFlagSet("commit", flag.ContinueOnError)
	fs.SetOutput(errOut)
	apply := fs.Bool("apply", false, "commit with the generated message")
	plain := fs.Bool("plain", false, "print only the message (pipes into `git commit -F -`)")
	model := fs.String("model", "", "model ref to generate with (default: the session model)")
	maxTokens := fs.Int("max-tokens", 1024, "cap on the generated message")
	fs.Usage = func() {
		fmt.Fprint(errOut, `usage: xdev commit [flags]

  xdev commit               generate a message for the staged diff and print it
  xdev commit --apply       commit with the generated message
  xdev commit --plain       print only the message (git commit -F - friendly)

The diff is exactly `+"`git diff --cached`"+`: nothing is committed without
--apply, and an empty index is an error with the hint to stage first.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	diff, stat, err := commitStagedDiff(ctx, cwd, git)
	if err != nil {
		fmt.Fprintln(errOut, "xdev commit:", err)
		if strings.Contains(err.Error(), "nothing staged") {
			fmt.Fprintln(errOut, "  hint: stage the change first (`git add <file>`), or commit through your editor as usual")
			return 2
		}
		return 1
	}

	start := time.Now()
	msg, err := complete(ctx, *model, commitPrompt(diff, stat), *maxTokens)
	if err != nil {
		fmt.Fprintln(errOut, "xdev commit:", err)
		return 1
	}
	msg = cleanCommitMessage(msg)
	if msg == "" {
		fmt.Fprintln(errOut, "xdev commit: the model returned an empty message")
		return 1
	}
	stats := &commitStats{DiffBytes: len(diff) + len(stat), Elapsed: time.Since(start).Round(time.Millisecond).String()}

	if *apply {
		if _, err := git(ctx, cwd, "commit", "-m", msg); err != nil {
			fmt.Fprintln(errOut, "xdev commit:", err)
			return 1
		}
	}
	if *plain {
		fmt.Fprint(out, msg)
		return 0
	}
	verb := "message"
	if *apply {
		verb = "committed with"
	}
	fmt.Fprintf(out, "%s:\n%s\n\n— %s, %s of staged diff, %s\n", verb, msg, stats.Model, commitBytes(stats.DiffBytes), stats.Elapsed)
	if !*apply {
		fmt.Fprintln(out, "  apply with: xdev commit --apply   (or: xdev commit --plain | git commit -F -)")
	}
	return 0
}

// commitStagedDiff reads the index through git itself: the staged diff and its
// stat are the model's only input, so a wrong index is impossible.
func commitStagedDiff(ctx context.Context, cwd string, git commitGit) (string, string, error) {
	diff, err := git(ctx, cwd, "diff", "--cached")
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(diff) == "" {
		return "", "", fmt.Errorf("nothing staged: `git diff --cached` is empty")
	}
	stat, err := git(ctx, cwd, "diff", "--cached", "--stat")
	if err != nil {
		return "", "", err
	}
	return diff, stat, nil
}

// commitPrompt builds the one-shot instruction. The diff is capped: a message
// is written from the shape of a change, and a runaway diff must not become a
// runaway request.
func commitPrompt(diff, stat string) string {
	const cap = 128 << 10
	if len(diff) > cap {
		diff = diff[:cap] + "\n…[diff truncated at 128 KiB]"
	}
	return "Write a git commit message for the staged diff below.\n" +
		"Format: an imperative subject line of at most 72 characters, then a blank line, then a short body saying why.\n" +
		"Output only the commit message: no explanation, no fences, no signature.\n\n" +
		"Summary:\n" + stat + "\nDiff:\n" + diff
}

// cleanCommitMessage unwraps the fences a model sometimes adds, so --apply
// never commits a stray ``` line.
func cleanCommitMessage(msg string) string {
	text := strings.TrimSpace(msg)
	if strings.HasPrefix(text, "```") {
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		} else {
			text = ""
		}
		if i := strings.LastIndex(text, "```"); i >= 0 {
			text = text[:i]
		}
	}
	text = strings.TrimSpace(text)
	// Drop a leading "commit message:" style preamble only when a blank line
	// separates it from a fenced-off body: anything else is real content.
	return strings.TrimRight(text, "\n")
}

// defaultCommitCompleter resolves the model ref the way a run does (flag →
// settings defaultModel → models.yml) and runs one streaming turn.
func defaultCommitCompleter(ctx context.Context, ref, prompt string, maxTokens int) (string, error) {
	cfg, err := config.LoadModelsLayered()
	if err != nil {
		cfg = &config.Config{} // still allow an explicit -model ref
	}
	ref, _, err = resolveModel(ref, cfg, lastSettings())
	if err != nil {
		return "", err
	}
	name, model, err := config.ParseModelRef(ref)
	if err != nil {
		return "", err
	}
	pc, ok := cfg.Providers[name]
	if !ok {
		return "", fmt.Errorf("unknown provider %q", name)
	}
	prov, err := buildProvider(name, pc, model, cfg)
	if err != nil {
		return "", err
	}
	if maxTokens <= 0 {
		maxTokens = 1024
	}
	ch, err := prov.Stream(ctx, ai.StreamRequest{
		Messages:  []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: prompt}}}},
		Model:     model,
		MaxTokens: maxTokens,
	})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for ev := range ch {
		switch ev.Type {
		case ai.EventTextDelta:
			b.WriteString(ev.Delta)
		case ai.EventError:
			return "", ev.Err
		case ai.EventDone:
			if ev.Message != nil && ev.Message.Text() != "" {
				return ev.Message.Text(), nil
			}
		}
	}
	return b.String(), nil
}

func commitBytes(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KiB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1024*1024))
	}
}
