package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/FreePeak/xdev/internal/tool"
)

// mailboxCap bounds one inbox JSONL file; the oldest messages drop first.
// Package var so tests can shrink it.
var mailboxCap = 1000

// Message is one persisted mailbox message.
type Message struct {
	ID      string    `json:"id"`
	From    string    `json:"from"`
	To      string    `json:"to"`
	Subject string    `json:"subject"`
	Body    string    `json:"body"`
	SentAt  time.Time `json:"sentAt"`
	ReadAt  time.Time `json:"readAt"`
}

// Mailbox is the persisted cross-session message channel backing the
// send_message and inbox tools. Storage: one JSONL per inbox under
// <dataDir>/mail/<short-id>/inbox.jsonl, plus named-agent identity files
// under <dataDir>/agents/<name>.json.
type Mailbox struct {
	DataDir string // xdev agent data dir (config.DataDir())

	mu    sync.RWMutex
	owner string // current session id; "" until SetOwner
}

// NewMailbox returns an unbound mailbox; no directories are created until
// the first message or identity registration (newToolRegistry runs in tests
// with the real data dir).
func NewMailbox(dataDir string) *Mailbox {
	return &Mailbox{DataDir: dataDir}
}

// SetOwner binds the mailbox to a session id. wireTaskParent stamps it once
// the session opens, and re-stamps on session switches.
func (m *Mailbox) SetOwner(id string) {
	m.mu.Lock()
	m.owner = strings.TrimSpace(id)
	m.mu.Unlock()
}

// Owner returns the bound session id ("" = unbound).
func (m *Mailbox) Owner() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.owner
}

// mailID canonicalizes an address to the short-id mailbox namespace so full
// uuids and their 8-char short forms address the same inbox.
func mailID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) > 8 {
		id = id[:8]
	}
	return id
}

// mailboxSafe keeps a name usable as a single path component.
func mailboxSafe(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	if s == "" || s == "." || s == ".." {
		s = "_"
	}
	return s
}

func (m *Mailbox) inboxPath(to string) string {
	return filepath.Join(m.DataDir, "mail", mailboxSafe(mailID(to)), "inbox.jsonl")
}

// SendMessage persists a message from the bound owner into the recipient's
// inbox. `to` is a session id (full uuid or short form) or a named identity
// registered via RegisterIdentity.
func (m *Mailbox) SendMessage(to, subject, body string) (Message, error) {
	from := mailID(m.Owner())
	if from == "" {
		return Message{}, fmt.Errorf("no session identity bound (owner not set)")
	}
	to = m.Resolve(to)
	msg := Message{
		ID:      newMailboxID(),
		From:    from,
		To:      mailID(to),
		Subject: subject,
		Body:    body,
		SentAt:  time.Now().UTC(),
	}
	if err := appendMessage(m.inboxPath(to), msg); err != nil {
		return Message{}, err
	}
	return msg, nil
}

// Inbox returns the owner's messages, oldest first.
func (m *Mailbox) Inbox() ([]Message, error) {
	owner := m.Owner()
	if owner == "" {
		return nil, fmt.Errorf("no session identity bound (owner not set)")
	}
	return readInbox(m.inboxPath(owner))
}

// Unread returns the owner's not-yet-read messages, oldest first.
func (m *Mailbox) Unread() ([]Message, error) {
	msgs, err := m.Inbox()
	if err != nil {
		return nil, err
	}
	var out []Message
	for _, msg := range msgs {
		if msg.ReadAt.IsZero() {
			out = append(out, msg)
		}
	}
	return out, nil
}

// MarkRead stamps readAt on one message in the owner's inbox.
func (m *Mailbox) MarkRead(id string) error {
	owner := m.Owner()
	if owner == "" {
		return fmt.Errorf("no session identity bound (owner not set)")
	}
	path := m.inboxPath(owner)
	lock, err := acquireMailboxLock(path)
	if err != nil {
		return err
	}
	defer lock.release()
	msgs, err := readInbox(path)
	if err != nil {
		return err
	}
	changed := false
	for i := range msgs {
		if msgs[i].ID == id && msgs[i].ReadAt.IsZero() {
			msgs[i].ReadAt = time.Now().UTC()
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return writeInbox(path, msgs)
}

// mailboxLock is a portable exclusive lock for one inbox (os.O_CREATE|
// O_EXCL; syscall.Flock is Unix-only and would break the Windows cross
// build). Mutations (append+trim, mark-read) serialize under it so an
// append can never land inside another writer's rewrite window.
//
// ponytail: a crash between acquire and release strands the lock; holders
// older than mailboxLockStale are stolen. Two simultaneous stealers can
// both win the remove-race — upgrade path: mkdir-based lock or a Unix-only
// flock build tag.
type mailboxLock struct{ path string }

const mailboxLockStale = 10 * time.Second

func acquireMailboxLock(path string) (*mailboxLock, error) {
	lockPath := path + ".lock"
	deadline := time.Now().Add(2 * time.Second)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			f.Close()
			return &mailboxLock{path: lockPath}, nil
		}
		if fi, statErr := os.Stat(lockPath); statErr == nil && time.Since(fi.ModTime()) > mailboxLockStale {
			os.Remove(lockPath) // steal stale lock from a crashed holder
			continue
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("mailbox lock busy: %s", lockPath)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (l *mailboxLock) release() { os.Remove(l.path) }

func appendMessage(path string, msg Message) error {
	line, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("encode message: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create inbox dir: %w", err)
	}
	lock, err := acquireMailboxLock(path)
	if err != nil {
		return err
	}
	defer lock.release()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open inbox: %w", err)
	}
	// O_APPEND under the lock: the Write lands at the end as one
	// offset-protected operation (message lines are small).
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("append message: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	return trimInbox(path) // same lock covers the trim rewrite
}

func readInbox(path string) ([]Message, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open inbox: %w", err)
	}
	defer f.Close()
	var msgs []Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg Message
		if err := json.Unmarshal(line, &msg); err != nil {
			continue // skip corrupt lines rather than losing the inbox
		}
		msgs = append(msgs, msg)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read inbox: %w", err)
	}
	return msgs, nil
}

// trimInbox enforces the cap by keeping only the newest mailboxCap messages.
func trimInbox(path string) error {
	msgs, err := readInbox(path)
	if err != nil {
		return err
	}
	if len(msgs) <= mailboxCap {
		return nil
	}
	return writeInbox(path, msgs[len(msgs)-mailboxCap:])
}

// writeInbox atomically replaces the inbox with msgs (temp file + rename).
// Callers must hold the inbox lock: the read-modify-write window is exactly
// what the lock exists to close.
func writeInbox(path string, msgs []Message) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".inbox-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	w := bufio.NewWriter(tmp)
	for _, msg := range msgs {
		line, err := json.Marshal(msg)
		if err != nil {
			tmp.Close()
			return err
		}
		w.Write(line)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func newMailboxID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is unrecoverable in practice; degrade to a
		// time-derived id rather than returning a colliding empty string.
		return fmt.Sprintf("%08x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// agentIdentity is one named-agent binding file under <dataDir>/agents/.
type agentIdentity struct {
	Name      string `json:"name"`
	SessionID string `json:"sessionID"`
}

// RegisterIdentity binds a named agent identity to a session id; SendMessage
// resolves the name via Resolve before addressing the inbox.
func (m *Mailbox) RegisterIdentity(name, sessionID string) error {
	name = strings.TrimSpace(name)
	sessionID = strings.TrimSpace(sessionID)
	if name == "" || sessionID == "" {
		return fmt.Errorf("identity name and session id are required")
	}
	dir := filepath.Join(m.DataDir, "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create agents dir: %w", err)
	}
	raw, err := json.MarshalIndent(agentIdentity{Name: name, SessionID: sessionID}, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, mailboxSafe(name)+".json")
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

// Resolve maps a named identity to its session id; anything else passes
// through unchanged so raw session ids address inboxes directly.
func (m *Mailbox) Resolve(id string) string {
	id = strings.TrimSpace(id)
	raw, err := os.ReadFile(filepath.Join(m.DataDir, "agents", mailboxSafe(id)+".json"))
	if err != nil {
		return id
	}
	var ident agentIdentity
	if json.Unmarshal(raw, &ident) != nil || ident.SessionID == "" {
		return id
	}
	return ident.SessionID
}

// InboxPoller watches the owner's inbox and pushes newly arrived messages to
// OnMessage and to the channel consumed by Wait. The agent loop integration
// (start on session open, stop on close) lives in the loop wiring.
type InboxPoller struct {
	MB        *Mailbox
	Interval  time.Duration // default 2s
	OnMessage func(Message)

	mu        sync.Mutex
	running   bool
	cancel    context.CancelFunc
	done      chan struct{} // closed when the loop goroutine has exited
	delivered map[string]bool
	ch        chan Message
}

// NewInboxPoller constructs a stopped poller.
func NewInboxPoller(mb *Mailbox) *InboxPoller {
	return &InboxPoller{
		MB:        mb,
		delivered: map[string]bool{},
		ch:        make(chan Message, 256),
	}
}

// Start begins polling; a first sweep runs immediately. Idempotent.
func (p *InboxPoller) Start() {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.running = true
	p.cancel = cancel
	p.done = make(chan struct{})
	done := p.done
	p.mu.Unlock()
	go func() {
		defer close(done)
		p.loop(ctx)
	}()
}

func (p *InboxPoller) loop(ctx context.Context) {
	p.poll(ctx)
	interval := p.Interval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.poll(ctx)
		}
	}
}

// poll sweeps once. Delivered messages are marked read so the poller and the
// inbox tool agree on "new"; the in-memory delivered map additionally dedupes
// when mark-read fails transiently.
func (p *InboxPoller) poll(ctx context.Context) {
	unread, err := p.MB.Unread()
	if err != nil {
		return // unbound owner or transient fs error; retry next tick
	}
	for _, msg := range unread {
		select {
		case <-ctx.Done():
			return
		default:
		}
		p.mu.Lock()
		already := p.delivered[msg.ID]
		p.delivered[msg.ID] = true
		p.mu.Unlock()
		if already {
			continue
		}
		p.deliver(msg)
		_ = p.MB.MarkRead(msg.ID) // best effort; delivered map covers retry
	}
}

func (p *InboxPoller) deliver(msg Message) {
	if p.OnMessage != nil {
		p.OnMessage(msg)
	}
	select {
	case p.ch <- msg:
	default:
		// ponytail: bounded 256-message buffer; overflow drops the
		// delivery (the message stays persisted — and already marked
		// read — in the inbox file). Upgrade path: spill to disk or
		// drain-blocking semantics with backpressure into the loop.
	}
}

// Stop halts polling and blocks until the in-flight sweep has finished (so
// callers can stop and then safely read the inbox). Idempotent. ponytail:
// calling Stop from inside OnMessage deadlocks — the callback runs on the
// loop goroutine; the upgrade path is an async stop with a done signal.
func (p *InboxPoller) Stop() {
	p.mu.Lock()
	if !p.running {
		p.mu.Unlock()
		return
	}
	p.cancel()
	done := p.done
	p.running = false
	p.mu.Unlock()
	<-done
}

// Wait blocks up to timeout for the first-arrived delivered message.
func (p *InboxPoller) Wait(timeout time.Duration) (Message, bool) {
	select {
	case msg := <-p.ch:
		return msg, true
	case <-time.After(timeout):
		return Message{}, false
	}
}

const SendMessageToolName = "send_message"
const InboxToolName = "inbox"

// SendMessageTool is the model-facing send half of the mailbox.
type SendMessageTool struct{ Mailbox *Mailbox }

func (t *SendMessageTool) Name() string { return SendMessageToolName }

func (t *SendMessageTool) Description() string {
	return "send a persisted message to another session's or a named agent's inbox. " +
		"Works across sessions (unlike hub). The recipient reads it with the inbox tool."
}

func (t *SendMessageTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "to": {"type": "string", "description": "recipient: session short-id (first 8 chars) or a registered agent name"},
    "subject": {"type": "string", "description": "one-line subject"},
    "body": {"type": "string", "description": "message body"}
  },
  "required": ["to", "body"]
}`)
}

func (t *SendMessageTool) Execute(_ context.Context, args json.RawMessage) (tool.Result, error) {
	if t.Mailbox == nil {
		return tool.Result{Text: "send_message: not configured", IsError: true}, nil
	}
	var a struct {
		To      string `json:"to"`
		Subject string `json:"subject"`
		Body    string `json:"body"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return tool.Result{Text: "send_message: malformed arguments: " + err.Error(), IsError: true}, nil
	}
	if strings.TrimSpace(a.To) == "" || strings.TrimSpace(a.Body) == "" {
		return tool.Result{Text: "send_message: to and body are required", IsError: true}, nil
	}
	msg, err := t.Mailbox.SendMessage(a.To, a.Subject, a.Body)
	if err != nil {
		return tool.Result{Text: "send_message: " + err.Error(), IsError: true}, nil
	}
	return tool.Result{
		Text:    fmt.Sprintf("sent to %s (message %s)", msg.To, msg.ID),
		Details: msg,
	}, nil
}

// InboxTool is the model-facing read half: this session's inbox only.
type InboxTool struct{ Mailbox *Mailbox }

func (t *InboxTool) Name() string { return InboxToolName }

func (t *InboxTool) Description() string {
	return "read this session's mailbox inbox (never another agent's). " +
		"Lists messages oldest-first and marks them read; use send_message to reply."
}

func (t *InboxTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (t *InboxTool) Execute(_ context.Context, _ json.RawMessage) (tool.Result, error) {
	if t.Mailbox == nil {
		return tool.Result{Text: "inbox: not configured", IsError: true}, nil
	}
	msgs, err := t.Mailbox.Inbox()
	if err != nil {
		return tool.Result{Text: "inbox: " + err.Error(), IsError: true}, nil
	}
	if len(msgs) == 0 {
		return tool.Result{Text: "inbox: empty", Details: []Message{}}, nil
	}
	for _, msg := range msgs {
		if msg.ReadAt.IsZero() {
			_ = t.Mailbox.MarkRead(msg.ID) // best effort; listing is the contract
		}
	}
	return tool.Result{Text: renderInbox(msgs), Details: msgs}, nil
}

func renderInbox(msgs []Message) string {
	var b strings.Builder
	for _, msg := range msgs {
		status := "read"
		if msg.ReadAt.IsZero() {
			status = "new"
		}
		subj := msg.Subject
		if subj == "" {
			subj = "(no subject)"
		}
		fmt.Fprintf(&b, "%s  [%s]  from %s  %s\n",
			msg.SentAt.Local().Format("2006-01-02 15:04:05"), status, msg.From, subj)
		if body := msg.Body; body != "" {
			if runes := []rune(body); len(runes) > 300 {
				body = string(runes[:300]) + "…"
			}
			for _, line := range strings.Split(body, "\n") {
				fmt.Fprintf(&b, "    %s\n", line)
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
