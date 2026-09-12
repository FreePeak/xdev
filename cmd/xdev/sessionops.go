package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/logx"
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
	// session.List already returns newest-first with a header-timestamp
	// tiebreak; the scan is ordered, so no second sort here.
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

// --- foreign-session import + --fork startup (issues #28, #11) ---

// splitForeignQuery recognizes the /resume @claude|@codex picker variants:
// "@claude" (bare list) or "@claude <id-or-path>" (import + switch).
func splitForeignQuery(query string) (kind, ref string, ok bool) {
	q := strings.TrimSpace(query)
	for _, k := range []string{"claude", "codex"} {
		prefix := "@" + k
		if q == prefix {
			return k, "", true
		}
		if strings.HasPrefix(q, prefix+" ") {
			return k, strings.TrimSpace(q[len(prefix)+1:]), true
		}
	}
	return "", "", false
}

// foreignRoot returns the on-disk transcript root for a foreign source.
func foreignRoot(kind string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("import %s: cannot locate home directory: %w", kind, err)
	}
	switch kind {
	case "claude":
		return session.ClaudeProjectsRoot(home), nil
	case "codex":
		return session.CodexSessionsRoot(home), nil
	}
	return "", fmt.Errorf("import: unknown foreign source %q", kind)
}

// listForeignTranscripts lists one kind of foreign transcript, newest
// first, filtered to cwd where the source layout encodes it.
func listForeignTranscripts(kind, cwd string) ([]session.ForeignTranscript, error) {
	root, err := foreignRoot(kind)
	if err != nil {
		return nil, err
	}
	return session.ListForeignRoot(kind, root, cwd)
}

// importForeignSession resolves a --from-claude/--from-codex (or
// /resume @kind) reference, imports the transcript read-only, and returns
// the NEW on-disk xdev session.
func importForeignSession(kind, ref, cwd string) (*session.Store, error) {
	root, err := foreignRoot(kind)
	if err != nil {
		return nil, err
	}
	path, err := session.ResolveForeign(kind, ref, root)
	if err != nil {
		return nil, err
	}
	var res *session.ImportResult
	switch kind {
	case "claude":
		res, err = session.ImportClaude(path, config.DataDir(), cwd)
	case "codex":
		res, err = session.ImportCodex(path, config.DataDir(), cwd)
	default:
		err = fmt.Errorf("import: unknown foreign source %q", kind)
	}
	if err != nil {
		return nil, err
	}
	logx.Debugf("import %s: %s → %s (%d turns, %d dropped)",
		kind, path, res.Store.Path(), res.Turns, res.Dropped)
	return res.Store, nil
}

// forkSessionByID implements --fork <id|path>: open the resolved session
// and fork it — new file, parentSession header (session.ForkSession).
func forkSessionByID(cwd, query string) (*session.Store, error) {
	src := query
	if st, err := os.Stat(query); err != nil || st.IsDir() {
		p, rerr := resolveResumeID(cwd, query)
		if rerr != nil {
			return nil, fmt.Errorf("fork: %q is neither a session file nor an id prefix (%v)", query, rerr)
		}
		src = p
	}
	return session.ForkSession(src,
		session.SessionFilePath(config.DataDir(), cwd, time.Now(), session.NewSessionID()), "")
}

// openStartupSession resolves the startup session for print/TUI runs:
// a foreign import (--from-claude/--from-codex) wins over --resume, and
// --fork wins over both (forking an import would need a source id anyway).
func openStartupSession(cwd string, opts printOptions) (*session.Store, error) {
	if opts.FromClaude != "" && opts.FromCodex != "" {
		return nil, fmt.Errorf("--from-claude and --from-codex are mutually exclusive")
	}
	if opts.ForkID != "" {
		return forkSessionByID(cwd, opts.ForkID)
	}
	if opts.FromClaude != "" {
		return importForeignSession("claude", opts.FromClaude, cwd)
	}
	if opts.FromCodex != "" {
		return importForeignSession("codex", opts.FromCodex, cwd)
	}
	return openSession(cwd, opts.ContinueLast, opts.ResumePrefix)
}
