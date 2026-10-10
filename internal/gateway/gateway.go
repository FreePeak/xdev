package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/memory"
	"github.com/FreePeak/xdev/internal/session"
)

// slashHelp is what /help answers. The set is the minimal useful one
// Hermes' gateway exposes to chat users (gateway/run_busy.py:907-922):
// status, help, stop, and a fresh session.
const slashHelp = `xdev gateway — Telegram bridge

  /help     this message
  /status   bot, workspace, and this chat's session
  /new      start a fresh session for this chat
  /stop     cancel the running turn
  /whoami   your Telegram user and chat id

Anything else is sent to the xdev agent as a prompt.`

// allowed reports whether a chat may drive the bridge. Empty allowlist with
// no allow-all denies everything (fail-closed): a bridge that runs code on
// the host must refuse what it was not told to accept.
func allowed(chat int64, allow []int64, allowAll bool) bool {
	if allowAll {
		return true
	}
	for _, id := range allow {
		if id == chat {
			return true
		}
	}
	return false
}

// Daemon is the long-poll loop. It owns the poller, the per-chat registry
// (lease + session pointer), and worker spawning — nothing else, because the
// agent itself runs as detached child processes of this binary (the
// isolation Prime Agent's Rust rewrite measured, which `xdev --bg` already
// implements).
type Daemon struct {
	tg        *Telegram
	cfg       *config.Settings
	chats     *Chats
	exe       string
	dataDir   string
	version   string
	logf      func(format string, args ...any)
	allow     []int64
	allowAll  bool
	workspace string
	chunk     int
	model     string
}

// Options configures a Daemon.
type Options struct {
	Settings *config.Settings
	Version  string
	DataDir  string
	Logf     func(format string, args ...any)
	// Exe overrides the binary that spawns workers (tests).
	Exe string
	// BaseURL overrides the Bot API root (tests).
	BaseURL string
}

// logFn routes lifecycle lines to the injected sink when the daemon is built
// with one (tests, `gateway run --log`), and to stderr otherwise.
func (d *Daemon) logFn(format string, args ...any) {
	if d.logf != nil {
		d.logf(format, args...)
		return
	}
	fmt.Fprintf(os.Stderr, "xdev gateway: "+format+"\n", args...)
}

// TokenPath is the 0600 file `xdev gateway setup` writes and the daemon reads
// when TELEGRAM_BOT_TOKEN is unset.
func TokenPath(dataDir string) string { return filepath.Join(dataDir, "gateway.token") }

// NewDaemon builds the daemon. The token is resolved from TELEGRAM_BOT_TOKEN
// first, then TokenPath; an absent token is an error, so a misconfigured
// start fails loudly instead of polling forever.
func NewDaemon(opts Options) (*Daemon, error) {
	s := opts.Settings
	if s == nil {
		s = &config.Settings{}
	}
	token := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	source := "TELEGRAM_BOT_TOKEN"
	if token == "" {
		token = loadToken(TokenPath(opts.DataDir))
		source = TokenPath(opts.DataDir)
	}
	if token == "" {
		return nil, fmt.Errorf("gateway: no bot token: set TELEGRAM_BOT_TOKEN or run `xdev gateway setup`")
	}
	if !ValidTokenShape(token) {
		return nil, fmt.Errorf("gateway: the token from %s does not look like a BotFather token (want <digits>:<35+ chars>)", source)
	}
	chats, err := NewChats(filepath.Join(opts.DataDir, "gateway", "sessions.json"))
	if err != nil {
		return nil, err
	}
	exe := opts.Exe
	if exe == "" {
		if exe, err = os.Executable(); err != nil {
			return nil, fmt.Errorf("gateway: resolve xdev binary: %w", err)
		}
	}
	ws := s.GatewayWorkspace()
	if ws == "" {
		ws = "."
	}
	return &Daemon{
		tg:        &Telegram{Token: token, BaseURL: opts.BaseURL, Logf: opts.Logf},
		cfg:       s,
		chats:     chats,
		exe:       exe,
		dataDir:   opts.DataDir,
		version:   opts.Version,
		logf:      opts.Logf,
		allow:     s.GatewayAllowedChats(),
		allowAll:  s.GatewayAllowAll(),
		workspace: ws,
		chunk:     s.GatewayReplyChunk(),
		model:     strings.TrimSpace(s.Gateway.Model),
	}, nil
}

// BotName reports the bot identity (also the token check setup runs).
func (d *Daemon) BotName(ctx context.Context) (*Me, error) { return d.tg.GetMe(ctx) }

// TokenMask names the token source without exposing it (status output).
func (d *Daemon) TokenMask() string { return MaskToken(d.tg.Token) }

// Run polls until ctx is done.
func (d *Daemon) Run(ctx context.Context) error {
	me, err := d.tg.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("gateway: %w (is the bot token valid?)", err)
	}
	d.logFn("gateway %s: connected as @%s (%d); %d allowed chat(s)%s; workspace %s",
		d.version, me.Username, me.ID, len(d.allow), allowAllNote(d.allowAll), d.workspace)
	// Record the daemon so `gateway status` can answer without asking the
	// supervisor. Best-effort: a read-only data dir costs a status line, not
	// the bridge, and WriteUnit guards the rest of the daemon's needs.
	if err := WriteStatus(d.dataDir, Status{PID: os.Getpid(), Version: d.version, BotName: me.Username, Workspace: d.workspace, Started: time.Now().UTC()}); err != nil {
		d.logFn("gateway: status file: %v", err)
	}
	offset := int64(0)
	for {
		if ctx.Err() != nil {
			return nil
		}
		updates, err := d.tg.GetUpdates(ctx, offset, d.cfg.GatewayPollTimeout())
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if IsConflict(err) {
				// Two pollers on one token: Telegram answers 409 to the
				// loser. Retrying loses messages forever, so stop and say why.
				return fmt.Errorf("gateway: another process is polling this bot token (409 Conflict) — stop the other gateway first")
			}
			d.logFn("gateway: poll error: %v", err)
			select {
			case <-time.After(pollBackoff):
			case <-ctx.Done():
				return nil
			}
			continue
		}
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			if u.Message == nil || u.Message.Text == "" {
				continue
			}
			d.handle(ctx, u)
		}
	}
}

// pollBackoff is the fixed wait between failed polls: a dead endpoint is
// retried at this cadence rather than killing a background daemon.
const pollBackoff = 2 * time.Second

func allowAllNote(all bool) string {
	if all {
		return " (allow-all: TELEGRAM_ALLOW_ALL_CHATS)"
	}
	return ""
}

// handle authorizes and routes one update.
func (d *Daemon) handle(ctx context.Context, u Update) {
	m := u.Message
	chat := m.Chat.ID
	if !allowed(chat, d.allow, d.allowAll) {
		d.logFn("gateway: denying chat %d (not in the allowlist)", chat)
		_ = d.tg.SendMessage(ctx, chat, "Not authorized. Ask the operator to allow this chat id:\n"+strconv.FormatInt(chat, 10))
		return
	}
	text := strings.TrimSpace(m.Text)
	switch {
	case text == "/start", strings.HasPrefix(text, "/help"):
		_ = d.reply(ctx, chat, slashHelp)
	case strings.HasPrefix(text, "/status"):
		_ = d.reply(ctx, chat, d.statusLine(chat))
	case strings.HasPrefix(text, "/whoami"):
		_ = d.reply(ctx, chat, whoami(m, chat))
	case strings.HasPrefix(text, "/new"):
		if err := d.chats.Forget(chat); err != nil {
			d.logFn("gateway: forget chat %d: %v", chat, err)
		}
		_ = d.reply(ctx, chat, "New session. The next message starts a fresh conversation.")
	case strings.HasPrefix(text, "/stop"):
		w := d.chats.Worker(chat)
		if w == nil || !w.alive() {
			_ = d.reply(ctx, chat, "Nothing is running for this chat.")
			return
		}
		if err := stopWorker(w); err != nil {
			d.logFn("gateway: stop chat %d: %v", chat, err)
			_ = d.reply(ctx, chat, "Could not stop the turn: "+err.Error())
			return
		}
		_ = d.reply(ctx, chat, "Stopped.")
	default:
		d.turn(ctx, chat, text)
	}
}

func whoami(m *Message, chat int64) string {
	var b strings.Builder
	if m.From != nil {
		fmt.Fprintf(&b, "user id: %d\n", m.From.ID)
		if m.From.Username != "" {
			fmt.Fprintf(&b, "username: @%s\n", m.From.Username)
		}
	}
	fmt.Fprintf(&b, "chat id: %d", chat)
	return b.String()
}

func (d *Daemon) statusLine(chat int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "xdev gateway %s\nworkspace: %s\n", d.version, d.workspace)
	if id, ok := d.chats.Session(chat); ok {
		fmt.Fprintf(&b, "session: %s\n", shortID(id))
	} else {
		b.WriteString("session: (none yet)\n")
	}
	if w := d.chats.Worker(chat); w != nil && w.alive() {
		fmt.Fprintf(&b, "turn: running (pid %d)\n", w.pid)
	} else {
		b.WriteString("turn: idle\n")
	}
	fmt.Fprintf(&b, "memory: %s\n", memoryNote(d.cfg))
	return b.String()
}

// memoryNote names the configured memory backend, so /status answers "is my
// agent still learning?" without reading config files.
func memoryNote(s *config.Settings) string {
	if s == nil || s.Memory == "" || s.Memory == "off" {
		return "off"
	}
	if s.Memory == "hindsight" {
		url := s.Hindsight.APIURL
		if url == "" {
			url = memory.DefaultHindsightURL
		}
		return "hindsight (LeanKG) " + url
	}
	return s.Memory
}

// reply sends text as one or more Telegram messages.
func (d *Daemon) reply(ctx context.Context, chat int64, text string) error {
	text = strings.TrimRight(text, "\n")
	if strings.TrimSpace(text) == "" {
		text = "(no answer)"
	}
	for _, part := range ChunkText(text, d.chunk) {
		if err := d.tg.SendMessage(ctx, chat, part); err != nil {
			return err
		}
	}
	return nil
}

// turn runs one chat message end to end: lease, spawn, wait, reply.
func (d *Daemon) turn(ctx context.Context, chat int64, text string) {
	release, err := d.chats.Acquire(chat, d.cfg.GatewayLeaseWait())
	if err != nil {
		_ = d.reply(ctx, chat, err.Error())
		return
	}
	defer release()

	sessionID, _ := d.chats.Session(chat)
	started := time.Now()
	w, err := d.spawn(sessionID, chat, text)
	if err != nil {
		d.logFn("gateway: spawn failed: %v", err)
		_ = d.reply(ctx, chat, "Could not start a turn: "+err.Error())
		return
	}
	d.chats.AttachWorker(chat, w)
	d.logFn("gateway: chat %d turn started (pid %d, session %s)", chat, w.pid, orNew(sessionID))

	res := w.wait()
	d.chats.AttachWorker(chat, nil)
	if sessionID == "" {
		// A fresh conversation: the worker just created the session file.
		// Discover it from the store rather than asking the child to report
		// it — that keeps the worker an ordinary `xdev print` run.
		if id, title := d.newestSessionSince(started); id != "" {
			if err := d.chats.SetSession(chat, id, title); err != nil {
				d.logFn("gateway: record session for chat %d: %v", chat, err)
			}
			d.logFn("gateway: chat %d session %s", chat, shortID(id))
		}
	}
	if res.exit != 0 {
		msg := strings.TrimSpace(res.logTail)
		if msg == "" {
			msg = fmt.Sprintf("The turn failed (exit %d).", res.exit)
		}
		_ = d.reply(ctx, chat, msg)
		return
	}
	_ = d.reply(ctx, chat, res.answer)
}

func orNew(id string) string {
	if id == "" {
		return "(new)"
	}
	return shortID(id)
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// newestSessionSince finds the newest session in the workspace created at or
// after since — the session the worker just started.
func (d *Daemon) newestSessionSince(since time.Time) (id, title string) {
	metas, err := session.List(d.dataDirOf())
	if err != nil {
		return "", ""
	}
	wsAbs, _ := filepath.Abs(d.workspace)
	for _, m := range metas {
		if m.CWD != wsAbs && m.CWD != d.workspace {
			continue
		}
		if m.TitleSource == session.TitleSourceSubagent || m.Status == session.StatusEmpty {
			continue
		}
		if m.Timestamp.Before(since.Add(-2*time.Second)) && m.ModTime.Before(since) {
			continue
		}
		return m.ID, m.Title
	}
	return "", ""
}

// dataDirOf is the root the session store reads: the resolved data dir (the
// daemon was handed the same one config.DataDir() reports).
func (d *Daemon) dataDirOf() string { return d.dataDir }

// --- worker -----------------------------------------------------------------

// worker is a detached `xdev print` child running one chat turn.
type worker struct {
	pid  int
	done chan struct{}
	mu   sync.Mutex
	res  workerResult
}

type workerResult struct {
	answer  string
	logTail string
	exit    int
}

func (w *worker) alive() bool {
	if w == nil || w.pid <= 0 {
		return false
	}
	select {
	case <-w.done:
		return false
	default:
		return true
	}
}

func (w *worker) wait() workerResult {
	<-w.done
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.res
}

func (w *worker) finish(res workerResult) {
	w.mu.Lock()
	w.res = res
	w.mu.Unlock()
	close(w.done)
}

// spawn launches one print-mode turn. Output is captured, not streamed: a
// chat wants the finished answer, and one send cannot interleave two turns'
// partial text.
func (d *Daemon) spawn(sessionID string, chat int64, text string) (*worker, error) {
	// The model's answer must be the ONLY thing on stdout: the gateway
	// forwards that stream verbatim to the chat, so thinking, banner lines
	// and notices would be read as the answer. --hide-thinking keeps the
	// reasoning off stdout (it still lands in the worker's stderr log), and
	// --allow-home keeps a turn whose workspace IS $HOME (the common
	// gateway.workspace default) in $HOME instead of a temp dir — the temp
	// switch exists so a human's interactive run does not scatter files in
	// $HOME, which a configured bridge has already opted into.
	// The daemon falls back to $HOME when gateway.workspace is unset; the
	// temp-dir switch is for a human run, not for a configured bridge, so it
	// is opted out of (the gateway turn is the un-attended case that switch
	// was never meant for).
	// Flags BEFORE the positional: Go's flag package stops at the first
	// non-flag argument, so `xdev print --allow-home X` would parse
	// "--allow-home" as part of the prompt and never see the switch.
	args := []string{"--print", "--no-title", "--hide-thinking", "--allow-home"}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	if d.model != "" {
		args = append(args, "--model", d.model)
	}
	args = append(args, text)

	cmd := exec.Command(d.exe, args...)
	cmd.Dir = d.workspace
	cmd.Env = append(os.Environ(),
		// Marks the child so `xdev ps` can classify it and a hook or an
		// extension can tell a chat turn from an interactive one.
		"XDEV_GATEWAY_CHAT="+strconv.FormatInt(chat, 10),
		// A detached worker must not inherit a TTY sense of "interactive".
		"TERM=dumb",
	)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr, cmd.Stdin = &buf, &buf, nil
	cmd.SysProcAttr = sysProcAttr()

	w := &worker{pid: -1, done: make(chan struct{})}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	w.pid = cmd.Process.Pid
	go func() {
		werr := cmd.Wait()
		out := strings.TrimSpace(buf.String())
		res := workerResult{answer: out, logTail: tail(out, workerLogTail)}
		if werr != nil {
			res.exit = -1
			var ee *exec.ExitError
			if errors.As(werr, &ee) {
				res.exit = ee.ExitCode()
			}
		}
		w.finish(res)
	}()
	return w, nil
}

// workerLogTail bounds how much of a failed turn's output is sent back.
const workerLogTail = 2000

// stopWorker asks the turn to end the way `xdev bg stop` does.
func stopWorker(w *worker) error {
	if w == nil || w.pid <= 0 {
		return nil
	}
	p, err := os.FindProcess(w.pid)
	if err != nil {
		return err
	}
	if err := p.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// tail keeps the last n bytes of s, starting at a line boundary when one is
// available (a truncated line reads as corruption).
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	start := len(s) - n
	if i := strings.IndexByte(s[start:], '\n'); i >= 0 {
		start += i + 1
	}
	return s[start:]
}

// loadToken reads a token file, trimming whitespace.
func loadToken(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}
