package tool

import (
	"context"
	"fmt"
	"strings"
)

// GitCLI runs git for the xdev CLI subcommands (worktree, commit) through the
// same plumbing the github tool's pr_checkout uses: the same lookPath
// resolution, discrete argv (never a shell), the same capped capture and the
// same timeout. The only difference is the error shape — a CLI invocation
// reports "git <sub>: <stderr>" instead of the github tool's branded message.
func GitCLI(ctx context.Context, dir string, args ...string) (string, error) {
	path, err := lookPath("git")
	if err != nil {
		return "", fmt.Errorf("git not found on PATH: %w", err)
	}
	cctx, cancel := context.WithTimeout(ctx, ghCommandTimeout)
	defer cancel()
	stdout, stderr, err := execCapped(cctx, path, args, dir)
	if err != nil {
		sub := "git"
		if len(args) > 0 {
			sub = strings.Join(args[:min(2, len(args))], " ")
		}
		switch {
		case cctx.Err() == context.DeadlineExceeded:
			return stdout, fmt.Errorf("git %s timed out after %s", sub, ghCommandTimeout)
		case ctx.Err() != nil:
			return stdout, fmt.Errorf("git %s cancelled", sub)
		}
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			msg = err.Error()
		}
		return stdout, fmt.Errorf("git %s: %s", sub, capText(msg, 4096))
	}
	return stdout, nil
}
