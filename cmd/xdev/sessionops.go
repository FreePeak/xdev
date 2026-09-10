package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/session"
)

// terminalKey identifies the terminal/pane + directory for the
// --continue breadcrumb: pane env vars first, then the TTY device,
// always suffixed with the cwd bucket so a terminal can never adopt
// another directory's session (omp re-roots per cwd the same way).
func terminalKey() string {
	term := "unknown"
	for _, env := range []string{"ZELLIJ_PANE", "TMUX_PANE", "KITTY_WINDOW_ID", "WEZTERM_PANE", "TERM_SESSION_ID", "WT_SESSION"} {
		if v := os.Getenv(env); v != "" {
			term = strings.ReplaceAll(strings.ToLower(env+"_"+v), "/", "_")
			break
		}
	}
	if term == "unknown" {
		if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			if tty, err := os.Readlink("/dev/fd/0"); err == nil {
				term = strings.ReplaceAll(tty, "/", "_")
			}
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		if real, err := filepath.EvalSymlinks(cwd); err == nil {
			cwd = real
		}
		term += "__" + strings.ReplaceAll(cwd, "/", "_")
	}
	return term
}

// breadcrumbDir is ~/.xdev/agent/terminal-sessions.
func breadcrumbDir() string {
	return filepath.Join(config.DataDir(), "terminal-sessions")
}

// saveBreadcrumb records the active session path for this terminal.
func saveBreadcrumb(sessionPath string) {
	if sessionPath == "" {
		return
	}
	dir := breadcrumbDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, terminalKey()), []byte(sessionPath), 0o644)
}

// readBreadcrumb returns the recorded session path for this terminal, or
// "" when absent.
func readBreadcrumb() string {
	b, err := os.ReadFile(filepath.Join(breadcrumbDir(), terminalKey()))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// resolveResumeID finds a session by case-insensitive id prefix (mtime
// desc, omp parity). "" query returns the most recent in cwd.
func resolveResumeID(cwd, query string) (string, error) {
	metas, err := session.List(config.DataDir())
	if err != nil {
		return "", err
	}
	sort.Slice(metas, func(a, b int) bool { return metas[a].ModTime.After(metas[b].ModTime) })
	q := strings.ToLower(query)
	for _, m := range metas {
		if m.CWD != cwd {
			continue
		}
		// The "" (newest) form must resolve a user session, never a
		// subagent child; an explicit prefix may still address one.
		if q == "" && m.TitleSource == session.TitleSourceSubagent {
			continue
		}
		if q == "" || strings.HasPrefix(strings.ToLower(m.ID), q) {
			return m.Path, nil
		}
	}
	return "", fmt.Errorf("no session in %s matching %q", cwd, query)
}

// dumpSession renders the transcript at leaf as markdown, writes it to
// ~/.xdev/agent/dumps/<shortid>.md, and returns the path.
func dumpSession(store *session.Store) (string, error) {
	res, err := session.BuildContext(store.Entries(), store.LeafID(), session.SystemPrompt{})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# xdev transcript — %s\n\n- session: `%s`\n- cwd: `%s`\n- dumped: %s\n\n---\n\n",
		store.Title(), store.ID(), store.CWD(), time.Now().Format("2006-01-02 15:04"))
	for _, m := range res.Messages {
		r := string(m.Role)
		role := strings.ToUpper(r[:1]) + r[1:]
		fmt.Fprintf(&b, "## %s\n\n%s\n\n", role, strings.TrimSpace(m.Text()))
	}

	dir := filepath.Join(config.DataDir(), "dumps")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, store.ID()[:8]+".md")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return path, nil
}
