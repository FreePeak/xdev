package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/share"
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

// resumeHint is the line the TUI prints as it exits: the one command that
// reopens the session just used. It asks resolveResumeID the same question the
// pasted line will ask it — does `--resume <id>` land on THIS file, from this
// directory? — so a store with nothing to resume (--no-session, a chat that got
// no assistant reply, the file /drop deleted, a --continue'd session owned by
// another directory) prints nothing instead of a command that errors.
func resumeHint(store *session.Store, cwd string) string {
	if store == nil {
		return ""
	}
	path, err := resolveResumeID(cwd, store.ID())
	if err != nil || path != store.Path() {
		return ""
	}
	return resumeCommand(store.ID())
}

// resumeCommand spells the hint with the name xdev was invoked as, so a shell
// that installs the binary under another name (an `alias omp=xdev`, a renamed
// copy) prints a line that pastes back.
func resumeCommand(id string) string {
	return resumeProg(os.Args[0]) + " --resume " + id
}

// resumeProg is the command name for a resume line: argv0's basename without
// the Windows suffix, falling back to "xdev" when argv0 says nothing usable.
func resumeProg(argv0 string) string {
	prog := strings.TrimSuffix(filepath.Base(argv0), ".exe")
	if prog == "" || prog == "." || prog == ".." || prog == string(filepath.Separator) {
		return "xdev"
	}
	return prog
}

// resolveResumeID finds a session by case-insensitive id prefix (mtime
// desc, omp parity). "" query returns the most recent in cwd.
func resolveResumeID(cwd, query string) (string, error) {
	metas, err := session.List(sessionDataDir())
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

// --- /export + /share + --export (issue #61) ---

// exportSession renders the transcript as one self-contained HTML file and
// returns the path written ("" = ~/.xdev/agent/exports/<shortid>.html). The
// walk is internal/share's FromStore, which keeps EVERY entry — /dump's
// BuildContext path drops the entries the model no longer sees (compaction
// summaries, branch markers, custom records), which an archive must keep.
// systemPrompt and model come from the live session when the caller runs one
// ("" falls back to what the store records: the last model_change, else the
// model of the last assistant turn).
func exportSession(store *session.Store, systemPrompt, model, outPath string) (string, error) {
	return share.Export(store, share.Options{SystemPrompt: systemPrompt, Model: model}, outPath)
}

// runExport implements --export <file>: resolve the session to export, write
// its HTML, and report the path. An export only reads: with no session
// selector it takes the newest session of this directory, and none existing
// is an error rather than a fresh empty session.
func runExport(outPath string, opts printOptions) error {
	if strings.TrimSpace(outPath) == "" {
		return errors.New("export needs a file path (xdev -export session.html)")
	}
	// omp's --export takes the SESSION (id or .jsonl path) as its first
	// argument; xdev's takes the OUTPUT path. A user following the omp docs
	// would therefore point the HTML writer at the transcript itself and
	// destroy it — HTML over JSONL, exit 0. Refuse to write over anything
	// that is not already an HTML export: re-exporting over a previous
	// export stays allowed.
	if data, rerr := os.ReadFile(outPath); rerr == nil {
		head := data
		if len(head) > 256 {
			head = head[:256]
		}
		trimmed := strings.ToLower(strings.TrimSpace(string(head)))
		// xdev's own exports emit lowercase `<!doctype html>` — the case
		// check must fold, or a legitimate re-export is refused.
		isHTML := strings.HasPrefix(trimmed, "<!doctype html") || strings.HasPrefix(trimmed, "<html")
		if !isHTML {
			return fmt.Errorf("refusing to overwrite %s: it exists and is not an HTML export (point -export at a new .html path, and select the session with -resume/-continue/-fork)", outPath)
		}
	}
	cwd := mustGetwd()
	store, err := exportSourceSession(cwd, opts)
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			logx.Errorf("session close: %v", cerr)
		}
	}()
	path, err := exportSession(store, "", "", outPath)
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	fmt.Println("Exported to: " + path)
	return nil
}

// exportSourceSession resolves the session an --export run reads. --fork
// resolves to its SOURCE: an export must not leave a fork behind.
func exportSourceSession(cwd string, opts printOptions) (*session.Store, error) {
	if opts.ForkID != "" {
		if st, err := os.Stat(opts.ForkID); err == nil && !st.IsDir() {
			return session.Open(opts.ForkID)
		}
		path, err := resolveResumeID(cwd, opts.ForkID)
		if err != nil {
			return nil, err
		}
		return session.Open(path)
	}
	if opts.ResumePrefix != "" || opts.ContinueLast || opts.FromClaude != "" || opts.FromCodex != "" {
		return openStartupSession(cwd, opts)
	}
	path, err := resolveResumeID(cwd, "")
	if err != nil {
		return nil, err
	}
	return session.Open(path)
}

// liveShare is the process's one live share server (nil = nothing shared).
var (
	shareMu   sync.Mutex
	liveShare *share.Server
)

// shareLive seals the session as an E2E-encrypted snapshot and serves it on
// loopback, returning the view-only link (the AES-256-GCM key rides in the
// URL fragment, so the server never holds the plaintext in a response). A
// second /share replaces the first: one live link per process, so a stale
// snapshot is never left serving.
// ponytail: loopback only — the link dies with this process. The upgrade
// path is a POST of the same sealed blob to share.serverUrl.
func shareLive(store *session.Store, systemPrompt, model string) (string, error) {
	srv, err := share.Publish(store, share.Options{SystemPrompt: systemPrompt, Model: model}, 0)
	if err != nil {
		return "", err
	}
	shareMu.Lock()
	prev := liveShare
	liveShare = srv
	shareMu.Unlock()
	if prev != nil {
		_ = prev.Close()
	}
	return srv.Link(), nil
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
		res, err = session.ImportClaude(path, sessionDataDir(), cwd)
	case "codex":
		res, err = session.ImportCodex(path, sessionDataDir(), cwd)
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
		session.SessionFilePath(sessionDataDir(), cwd, time.Now(), session.NewSessionID()), "")
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
	metas, err := session.List(sessionDataDir())
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

// summarizeAndBranch moves the leaf to targetID and records the branch being
// left as a branch_summary entry on the new branch (tree selector
// Shift+Enter; navigateTree computes the target — a user row rewinds to its
// parent). The branch_summary is generated on the session model when the branch
// carries enough context to be worth summarizing (the engine's own
// threshold); every failure path — no model resolvable, provider error, empty
// answer, budget off — falls back to the fixed marker rather than failing the
// switch (#83: the seam existed with no caller, so every summary was the
// marker).
func summarizeAndBranch(store *session.Store, targetID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), branchSummaryBudget)
	defer cancel()
	return agent.SummarizeBranchStore(ctx, store, targetID, branchSummarizer())
}

// branchSummaryBudget bounds the side request; a slow provider must not hang
// a tree switch the user just asked for.
const branchSummaryBudget = 20 * time.Second

// branchSummarizer resolves the session model into a callable, or nil when
// the feature is off or that model cannot be reached (nil = marker only).
func branchSummarizer() agent.BranchSummarizer {
	settings := lastSettings()
	if settings != nil && !settings.BranchSummaryOn() {
		return nil
	}
	cfg, err := config.LoadModelsLayered()
	if err != nil {
		logx.Debugf("branch summary: config unavailable: %v", err)
		return nil
	}
	resolved, _, err := resolveModel("", cfg, settings)
	if err != nil {
		logx.Debugf("branch summary: no model resolvable (%v), recording markers", err)
		return nil
	}
	pName, mName, err := config.ParseModelRef(resolved)
	if err != nil {
		logx.Debugf("branch summary: %v, recording markers", err)
		return nil
	}
	pc, ok := cfg.Providers[pName]
	if !ok {
		logx.Debugf("branch summary: unknown provider %s, recording markers", pName)
		return nil
	}
	prov, err := buildProvider(pName, pc, mName, cfg)
	if err != nil {
		logx.Debugf("branch summary: provider %s unavailable: %v, recording markers", pName, err)
		return nil
	}
	maxTokens := settings.BranchSummaryReserveTokens()
	return func(ctx context.Context, prompt string) (string, error) {
		msg, err := ai.Complete(ctx, prov, mName, branchSummarySystem, prompt, maxTokens)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(msg.Text()), nil
	}
}

// branchSummarySystem is the writer instruction for a branch note.
const branchSummarySystem = "You summarize the conversation branch a user is abandoning in a coding session. Write 2-4 sentences in the past tense: what was attempted, what was learned or changed, and anything left unfinished. Plain prose, no bullets, no preamble."
