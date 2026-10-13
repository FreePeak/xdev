package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/memory"
	"github.com/FreePeak/xdev/internal/session"
)

// botMenu is the command list the Telegram client shows when the user taps
// "/". Descriptions are short (Telegram caps each at 256 chars and the menu
// at 100) and must describe the action, not the mechanism.
var botMenu = []BotCommand{
	{"help", "What this bot does and how to talk to it"},
	{"model", "Show or switch the model for this chat"},
	{"status", "Bot, workspace, memory and this chat's session"},
	{"new", "Start a fresh session (the old one stays on disk)"},
	{"stop", "Cancel the turn running right now"},
	{"whoami", "Your Telegram user and chat id"},
}

// slashHelp is what /help answers. The set is the minimal useful one
// Hermes' gateway exposes to chat users (gateway/run_busy.py:907-922):
const slashHelp = `xdev gateway — Telegram bridge

  /help            this message
  /status          bot, workspace, and this chat's session
  /model           show this chat's model and what else is available
  /model <ref>     run this chat on another model (e.g. onegw/execution)
  /model default   go back to the gateway's configured model
  /new             start a fresh session for this chat
  /stop            cancel the running turn
  /whoami          your Telegram user and chat id

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
	// stream paints the answer into the chat while the turn runs
	// (gateway.stream; ON unless turned off).
	stream bool
	// streamEvery is how often a running turn repaints its preview (0 =
	// streamInterval). A field so a test drives the loop without waiting
	// the production 1.5s per paint.
	streamEvery time.Duration
	// model is the daemon-wide default ref for gateway turns ("" = the run's
	// configured default). A chat's own /model overrides it.
	model string
	// models loads the catalog /model resolves against (nil = read this
	// machine's models.yml). A field so a test hands in a fixed catalog.
	models func() (*config.Config, error)
	// badgeMu guards badgeLeg, the leg the bot's profile photo currently
	// names. Uploading is a network call, so it is never held across one.
	badgeMu  sync.Mutex
	badgeLeg string
}
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
		tg:          &Telegram{Token: token, BaseURL: opts.BaseURL, Logf: opts.Logf},
		cfg:         s,
		chats:       chats,
		exe:         exe,
		dataDir:     opts.DataDir,
		version:     opts.Version,
		logf:        opts.Logf,
		allow:       s.GatewayAllowedChats(),
		allowAll:    s.GatewayAllowAll(),
		workspace:   ws,
		chunk:       s.GatewayReplyChunk(),
		stream:      s.GatewayStream(),
		streamEvery: s.GatewayStreamInterval(),
		model:       strings.TrimSpace(s.Gateway.Model),
		models:      config.LoadModelsLayered,
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
	// Register the "/" menu. A failure here is a warning, never fatal: the
	// menu is a convenience, and a bridge that must see its commands typed
	// still works.
	if err := d.tg.SetMyCommands(ctx, botMenu); err != nil {
		d.logFn("gateway: command menu: %v", err)
	} else {
		d.logFn("gateway: registered %d command(s) in the client menu", len(botMenu))
	}
	offset := int64(0)
	conflicts := 0
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
				// A 409 means another poller holds this token's update
				// queue. It is NOT always a rival daemon: Telegram also
				// terminates an old long poll when a NEW one arrives, so any
				// client that pokes the API (a getMe, a stray getUpdates from
				// an operator checking the bot, a health probe) makes the
				// long poll 409 and the poll would then exit — and launchd's
				// KeepAlive would restart it, so a single probe is a restart.
				// Measured 2026-10-11: two `getUpdates` curls from a shell
				// caused two restarts of a live, healthy daemon.
				//
				// So: a 409 is tolerated, with backoff. What a 409 is NOT
				// allowed to do is spin — a real rival poller produces a 409
				// on every poll, which the backoff paces to once every 8s and
				// a counter reports, so a genuine conflict is still visible
				// (it is only no longer fatal, and no longer self-inflicted
				// by a health check).
				conflicts++
				if conflicts == 1 || conflicts%20 == 0 {
					d.logFn("gateway: poll conflict #%d: another getUpdates on this token (a rival daemon, or a check by hand); continuing", conflicts)
				}
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(conflictBackoff):
				}
				continue
			}
			conflicts = 0
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

// conflictBackoff paces the polls after a 409. Telegram terminates an
// existing long poll when another getUpdates arrives, so a 409 is expected
// whenever anything else touches the token; 8s is long enough that a real
// rival poller is visible (one log line per 20) and short enough that the
// daemon is back before the next message.
const conflictBackoff = 8 * time.Second

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
	if text == "" {
		// A photo, a sticker or an edit arrives as a message with no text:
		// spawning a worker for it would run a turn about nothing.
		_ = d.reply(ctx, chat, "xdev gateway reads text only — send /help for the commands.")
		return
	}
	switch {
	case text == "/start", strings.HasPrefix(text, "/help"):
		_ = d.reply(ctx, chat, slashHelp)
	case strings.HasPrefix(text, "/status"):
		_ = d.reply(ctx, chat, d.statusLine(chat))
	case strings.HasPrefix(text, "/whoami"):
		_ = d.reply(ctx, chat, whoami(m, chat))
	case text == "/model", strings.HasPrefix(text, "/model "):
		d.modelCommand(ctx, chat, text)
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
	// /status names the model too: "which model answered me?" is the question
	// /model exists for, and the answer belongs on the status card as well.
	if ref := d.modelFor(chat); ref != "" {
		fmt.Fprintf(&b, "model: %s\n", ref)
	} else {
		b.WriteString("model: the run's configured default\n")
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

// reply sends text as one or more Telegram messages. Each chunk carries the
// others' formatting: ToTelegramHTML maps markdown to Telegram's HTML subset,
// and Telegram.SendMessage falls back to plain text when the API rejects a
// parsed body — so a formatting quirk costs the italics, never the answer.
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

// startTyping keeps the chat's typing bubble alive while the turn runs, and
// returns a stop func the caller defers after w.wait() returns. Telegram
// expires the "typing" action after ~5s, so the loop re-posts it every
// typingInterval — Hermes' gateway does the same, and a long model call is
// exactly the window the bubble would otherwise go dark for. The first post
// is immediate (a phone user sees feedback right away, not after 2s).
// Failures are log lines: a chat action is a courtesy, never the reason to
// fail a turn.
func startTyping(ctx context.Context, tg *Telegram, chat int64, done <-chan struct{}, every time.Duration) func() {
	stop := make(chan struct{})
	go func() {
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			if err := tg.SendChatAction(ctx, chat); err != nil {
				tg.logf("gateway: typing for chat %d: %v", chat, err)
			}
			select {
			case <-stop:
				return
			case <-done:
				return
			case <-tick.C:
			}
		}
	}()
	return func() { close(stop) }
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

	stopTyping := startTyping(ctx, d.tg, chat, w.done, typingInterval)
	// The answer is painted into the chat while it is written, so a slow
	// turn shows its progress instead of a silent chat and a wall of
	// 4096-character bubbles at the end. The preview stops with the worker
	// (w.done), and the finished answer is delivered after it. With
	// gateway.stream off there is no preview at all: the answer arrives
	// whole, which is what an operator who turned it off asked for.
	var pv *preview
	stopPreview := make(chan struct{})
	var previewDone sync.WaitGroup
	if d.stream {
		pv = w.preview
	}
	if pv != nil {
		previewDone.Add(1)
		go func() {
			defer previewDone.Done()
			d.runPreview(ctx, chat, pv, stopPreview, w.done)
		}()
	}
	res := w.wait()
	close(stopPreview)
	previewDone.Wait()
	stopTyping()
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
	// The worker's own stderr carries the switch line when the gateway in
	// front of it fell the request over to another model
	// (ai.ModelSwitchNotice). Two things follow from it, in this order:
	// the BADGE first — the bot's profile photo is what the chat header
	// shows, and the user asked for the picture to change — then the line
	// itself, so the answer is never attributed to the model they pinned.
	if req, served, ok := ai.ParseModelSwitchNotice(res.logTail); ok {
		d.applyModelBadge(ctx, served)
		_ = d.deliver(ctx, chat, pv, switchCaption(req, served), res.answer)
		return
	}
	_ = d.deliver(ctx, chat, pv, "", res.answer)
}

// switchCaption is the one line a switched answer is prefixed with. The
// wording is ai.SwitchPhrase's, so the phone and the terminal cannot drift.
func switchCaption(requested, served string) string {
	return "⇄ " + ai.SwitchPhrase(requested, served)
}

// applyModelBadge points the bot's profile photo at the leg that just served,
// which is the picture the chat header shows. Best-effort by design: a bot the
// operator never granted photo rights must still answer the question, so a
// refusal is logged and the turn continues. The last leg is remembered so a
// chat that keeps landing on the same sibling does not re-upload on every
// turn (Telegram refuses a photo it already has).
func (d *Daemon) applyModelBadge(ctx context.Context, leg string) {
	d.badgeMu.Lock()
	same := d.badgeLeg == leg
	d.badgeMu.Unlock()
	if same {
		return
	}
	jpeg := ModelBadgeJPEG(leg)
	if jpeg == nil {
		return
	}
	if err := d.tg.SetMyProfilePhoto(ctx, jpeg); err != nil {
		d.logFn("gateway: profile photo for %s: %v", leg, err)
		return
	}
	d.badgeMu.Lock()
	d.badgeLeg = leg
	d.badgeMu.Unlock()
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

type worker struct {
	pid  int
	done chan struct{}
	mu   sync.Mutex
	res  workerResult
	// preview is the live view of this worker's stdout, painted into the
	// chat while the turn runs (nil only for a hand-built test worker).
	preview *preview
}

type workerResult struct {
	answer  string
	logTail string
	exit    int
}

// workerOutputCap bounds one turn's captured stdout+stderr. A phone turn is a
// short answer plus a log tail; anything past this is a loop, and dropping the
// oldest bytes keeps the answer (written last) while bounding memory.
const workerOutputCap = 256 << 10

// ringBuffer is an io.Writer that keeps only its last `limit` bytes. The
// answer a worker prints arrives after the preamble, so the tail is what
// matters and what survives.
type ringBuffer struct {
	mu      sync.Mutex
	buf     []byte
	limit   int
	dropped int
}

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if over := len(r.buf) - r.limit; over > 0 {
		r.buf = append(r.buf[:0], r.buf[over:]...)
		r.dropped += over
	}
	return len(p), nil
}

func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := string(r.buf)
	if r.dropped > 0 {
		out = "… (" + strconv.Itoa(r.dropped) + " bytes dropped)\n" + out
	}
	return out
}

// alive reports whether the worker is still running. The pid alone is not
// enough: once Wait returns the pid can be recycled, and /stop would then
// signal an innocent bystander. The closed done channel is what Wait left
// behind, so it is the honest signal (nil-safe: /status asks a chat that may
// never have run).
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

// spawn launches one print-mode turn. Its stdout is the answer and is
// painted into the chat while it is written (the preview); its stderr is
// narration for the failure message, never for the answer.
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
	// The chat's own /model wins over the daemon-wide gateway.model, which
	// wins over the run's configured default (no --model at all). Resolved
	// here rather than at /model time so a config edit or a /new cannot leave
	// a chat pinned to a ref the operator has since removed.
	if ref := d.modelFor(chat); ref != "" {
		args = append(args, "--model", ref)
	}
	// XDEV_MODEL carries the same choice to the worker's own subprocesses
	// (advisor, titles, memory) the way an interactive /model does, so a chat
	// pinned to a model does not quietly run its side passes on another.
	env := os.Environ()
	if ref := d.modelFor(chat); ref != "" {
		env = append(env, "XDEV_MODEL="+ref)
	}
	args = append(args, text)

	cmd := exec.Command(d.exe, args...)
	cmd.Dir = d.workspace
	cmd.Env = append(env,
		// Marks the child so `xdev ps` can classify it and a hook or an
		// extension can tell a chat turn from an interactive one.
		"XDEV_GATEWAY_CHAT="+strconv.FormatInt(chat, 10),
		// A detached worker must not inherit a TTY sense of "interactive".
		"TERM=dumb",
	)
	// Two bounded buffers, each capped the same way and each dropping the
	// OLDEST bytes: `all` is both streams (the failure message's window, and
	// where the model-switch notice on stderr is found) and `out` is stdout
	// alone — the answer, which is what the chat is shown. A worker that
	// prints without bound must not grow an unbounded buffer.
	var all, out ringBuffer
	all.limit, out.limit = workerOutputCap, workerOutputCap
	var pv *preview
	if d.stream {
		pv = newPreview()
		cmd.Stdout = io.MultiWriter(&out, previewWriter{pv})
	} else {
		cmd.Stdout = &out
	}
	cmd.Stderr = &all
	cmd.Stdin = nil
	cmd.SysProcAttr = sysProcAttr()

	w := &worker{pid: -1, done: make(chan struct{}), preview: pv}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	w.pid = cmd.Process.Pid
	go func() {
		werr := cmd.Wait()
		answer := strings.TrimSpace(out.String())
		res := workerResult{
			answer:  answer,
			logTail: tail(strings.TrimSpace(all.String()+"\n"+answer), workerLogTail),
		}
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

// previewWriter is the stdout half of a worker's output: it feeds the
// preview so the chat can be painted while the turn runs. It never fails a
// write — the answer also goes to the ring buffer, and a preview that cannot
// be painted costs progress, not the answer.
type previewWriter struct{ p *preview }

func (w previewWriter) Write(b []byte) (int, error) {
	w.p.write(string(b))
	return len(b), nil
}

// workerLogTail bounds how much of a failed turn's output is sent back: a
// phone message carries a window into the log, not the whole log.
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
