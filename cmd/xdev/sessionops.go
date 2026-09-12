package main

import (
	"encoding/json"
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

// session-pins.json sidecar: {"pins": ["<shortid>", ...]} — pinned
// sessions sort first in the /resume picker. Keys are the 8-hex short ids
// the picker rows carry.

func pinsPath() string { return filepath.Join(config.DataDir(), "session-pins.json") }

// loadSessionPins reads the pin set; any error means "no pins".
func loadSessionPins() map[string]bool {
	out := map[string]bool{}
	b, err := os.ReadFile(pinsPath())
	if err != nil {
		return out
	}
	var v struct {
		Pins []string `json:"pins"`
	}
	if json.Unmarshal(b, &v) == nil {
		for _, id := range v.Pins {
			out[id] = true
		}
	}
	return out
}

func saveSessionPins(pins map[string]bool) error {
	ids := make([]string, 0, len(pins))
	for id, on := range pins {
		if on {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	b, err := json.MarshalIndent(struct {
		Pins []string `json:"pins"`
	}{ids}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(pinsPath(), append(b, '\n'), 0o644)
}

// toggleSessionPin flips one pin and persists the sidecar.
func toggleSessionPin(shortID string) error {
	pins := loadSessionPins()
	if pins[shortID] {
		delete(pins, shortID)
	} else {
		pins[shortID] = true
	}
	return saveSessionPins(pins)
}

// unpinSession drops a pin best-effort (delete cleanup).
func unpinSession(shortID string) {
	pins := loadSessionPins()
	if !pins[shortID] {
		return
	}
	delete(pins, shortID)
	_ = saveSessionPins(pins)
}

// deleteSessionByShortID removes the session's JSONL plus its artifacts
// dir (xdev spills no per-session artifacts today, but the omp layout
// keeps them under dataDir/artifacts/<id> — best-effort RemoveAll for
// forward compatibility) and drops its pin. Deleting the active session
// is refused: its store is live.
func deleteSessionByShortID(shortID, activePath string) error {
	metas, err := session.List(config.DataDir())
	if err != nil {
		return err
	}
	for _, m := range metas {
		if len(m.ID) < 8 || m.ID[:8] != shortID {
			continue
		}
		if m.Path == activePath {
			return fmt.Errorf("cannot delete the active session")
		}
		if err := os.Remove(m.Path); err != nil {
			return err
		}
		os.RemoveAll(filepath.Join(config.DataDir(), "artifacts", shortID))
		unpinSession(shortID)
		return nil
	}
	return fmt.Errorf("no session %s", shortID)
}
