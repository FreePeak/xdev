package main

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/FreePeak/xdev/internal/session"
)

func restoreChanges(cwd string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	ok, err := isGitRepo(cwd)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("not a git repository — cannot restore changes")
	}
	args := []string{"checkout", "--"}
	args = append(args, paths...)
	out, err := exec.Command("git", append([]string{"-C", cwd}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git checkout failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func discardChanges(cwd string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	ok, err := isGitRepo(cwd)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("not a git repository — cannot discard changes")
	}
	st, serr := exec.Command("git", "-C", cwd, "diff", "--name-only").CombinedOutput()
	if serr != nil || len(strings.TrimSpace(string(st))) == 0 {
		return fmt.Errorf("no tracked file changes to discard")
	}
	args := []string{"checkout", "--"}
	args = append(args, paths...)
	out, err := exec.Command("git", append([]string{"-C", cwd}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git checkout failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func exitMenuText(store *session.Store, cwd string) string {
	if store == nil {
		return ""
	}
	hint := resumeHint(store, cwd)
	if hint == "" {
		return ""
	}
	var b strings.Builder
	fmt.Fprintln(&b, "─── resume ────────────────────────────────────────")
	fmt.Fprintf(&b, "  %s\n", hint)
	fmt.Fprintln(&b, "  restore <path> …  restore file(s) to committed state (git checkout --)")
	fmt.Fprintln(&b, "  discard <path> …  discard uncommitted changes in file(s) (git checkout --)")
	fmt.Fprintln(&b, "  claude          import from Claude Code and resume")
	fmt.Fprintln(&b, "  keep            keep working-tree changes, exit")
	fmt.Fprintln(&b, "──────────────────────────────────────────────────")
	return b.String()
}

func isGitRepo(cwd string) (bool, error) {
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "--is-inside-work-tree").CombinedOutput()
	if err != nil {
		return false, nil
	}
	return strings.TrimSpace(string(out)) == "true", nil
}
