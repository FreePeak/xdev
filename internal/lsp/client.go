// Package lsp is a minimal Language Server Protocol client (M13 #52):
// JSON-RPC 2.0 over stdio with Content-Length framing. The framing and the
// client are hand-rolled on bufio + encoding/json — xdev carries no LSP
// dependency, and a server is a plain subprocess.
//
// Only what the `lsp` tool needs is implemented: initialize (whose
// capabilities are kept for the `capabilities` op), didOpen / didChange,
// definition, references, hover, documentSymbol, workspace/symbol, rename and
// codeAction QUERIES, and the cached publishDiagnostics stream. Nothing here
// mutates the user's code: rename and code actions are reported, not applied,
// so every write keeps going through the edit/write tools where the approval
// gate, the freshness check and secret redaction live.
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxFrame caps one decoded JSON-RPC frame: a corrupt Content-Length must
// not make xdev allocate it.
const maxFrame = 16 << 20

// rpcMsg is one JSON-RPC 2.0 message in either direction.
type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return "lsp error"
	}
	return fmt.Sprintf("lsp error %d: %s", e.Code, e.Message)
}

// writeFrame writes one Content-Length framed message.
func writeFrame(w io.Writer, msg *rpcMsg) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

// readFrame reads one Content-Length framed message. Header names are matched
// case-insensitively (the LSP spec says so) and unknown headers are skipped.
func readFrame(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // end of headers
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return nil, fmt.Errorf("bad Content-Length %q", v)
			}
			length = n
		}
	}
	if length < 0 {
		return nil, errors.New("frame missing Content-Length")
	}
	if length > maxFrame {
		return nil, fmt.Errorf("frame of %d bytes exceeds the %d byte cap", length, maxFrame)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

// Position is a 0-based LSP position.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is an LSP range.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Diagnostic is one entry of a publishDiagnostics notification.
type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity"` // 1 error .. 4 hint
	Code     any    `json:"code"`
	Source   string `json:"source"`
	Message  string `json:"message"`
}

// docState tracks the version and the on-disk identity of an opened file.
type docState struct {
	version int
	size    int64
	mod     time.Time
}

// Client is one running language server.
type Client struct {
	name  string
	stdin io.WriteCloser
	out   *bufio.Reader
	proc  *os.Process

	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  int
	pending map[int]chan *rpcMsg
	fatal   error
	closed  bool

	done      chan struct{}
	exited    chan struct{} // closed when the process is reaped (nil = no process)
	closeOnce sync.Once

	docMu sync.Mutex
	docs  map[string]docState

	// caps is the server's `initialize` result capabilities, kept for the
	// `capabilities` op: without it, "why did that op do nothing?" has no
	// answer in a session that already paid for the handshake.
	caps json.RawMessage

	diagMu sync.Mutex
	diags  map[string][]Diagnostic
	seen   map[string]bool
	diagCh chan struct{} // closed and replaced on every publish

	stderr *tailWriter
}

// newClient wraps an already-connected stdio pair. stdout must be read by
// exactly one goroutine (readLoop).
func newClient(stdin io.WriteCloser, stdout io.Reader, name string) *Client {
	return &Client{
		name:    name,
		stdin:   stdin,
		out:     bufio.NewReader(stdout),
		pending: map[int]chan *rpcMsg{},
		done:    make(chan struct{}),
		docs:    map[string]docState{},
		diags:   map[string][]Diagnostic{},
		seen:    map[string]bool{},
		diagCh:  make(chan struct{}),
	}
}

// start begins dispatching server messages.
func (c *Client) start() { go c.readLoop() }

// Done is closed when the client is unusable (server exited or Close called).
func (c *Client) Done() <-chan struct{} { return c.done }

// Name is the configured server name (e.g. "go").
func (c *Client) Name() string { return c.name }

func (c *Client) readLoop() {
	for {
		body, err := readFrame(c.out)
		if err != nil {
			c.finish(c.exitErr(err))
			return
		}
		var msg rpcMsg
		if err := json.Unmarshal(body, &msg); err != nil {
			c.finish(fmt.Errorf("lsp/%s: malformed message: %w", c.name, err))
			return
		}
		switch {
		case msg.ID != nil:
			if ch := c.take(*msg.ID); ch != nil {
				ch <- &msg
			}
		case msg.Method == "textDocument/publishDiagnostics":
			c.cacheDiagnostics(msg.Params)
		}
	}
}

// exitErr decorates a terminal read error with whatever the server last said
// on stderr, so "server exited" is actionable.
func (c *Client) exitErr(err error) error {
	if errors.Is(err, io.EOF) {
		return fmt.Errorf("lsp/%s: server exited%s", c.name, c.stderr.tail())
	}
	return fmt.Errorf("lsp/%s: %w", c.name, err)
}

// finish marks the client dead exactly once and fails every waiter.
func (c *Client) finish(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	if c.fatal == nil {
		c.fatal = err
	}
	orphans := make([]chan *rpcMsg, 0, len(c.pending))
	for id, ch := range c.pending {
		orphans = append(orphans, ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	close(c.done)
	dead := &rpcMsg{Error: &rpcError{Code: -32000, Message: err.Error()}}
	for _, ch := range orphans {
		ch <- dead
	}
}

func (c *Client) take(id int) chan *rpcMsg {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := c.pending[id]
	delete(c.pending, id)
	return ch
}

// Dead reports why the client is unusable.
func (c *Client) Dead() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fatal != nil {
		return c.fatal
	}
	return fmt.Errorf("lsp/%s: client closed", c.name)
}

// send writes one message, marking the client dead when the pipe is gone.
func (c *Client) send(msg *rpcMsg) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := writeFrame(c.stdin, msg); err != nil {
		c.finish(fmt.Errorf("lsp/%s: write: %w", c.name, err))
		return err
	}
	return nil
}

// Call sends a request and waits for its response.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	raw, err := marshalParams(params)
	if err != nil {
		return nil, fmt.Errorf("lsp/%s: %s: %w", c.name, method, err)
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, c.Dead()
	}
	c.nextID++
	id := c.nextID
	ch := make(chan *rpcMsg, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.send(&rpcMsg{JSONRPC: "2.0", ID: &id, Method: method, Params: raw}); err != nil {
		c.take(id)
		return nil, err
	}
	select {
	case msg := <-ch:
		if msg.Error != nil {
			return nil, msg.Error
		}
		return msg.Result, nil
	case <-ctx.Done():
		c.take(id)
		return nil, fmt.Errorf("lsp/%s: %s: %w", c.name, method, ctx.Err())
	case <-c.done:
		c.take(id)
		return nil, c.Dead()
	}
}

// Notify sends a notification (no response expected).
func (c *Client) Notify(method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	return c.send(&rpcMsg{JSONRPC: "2.0", Method: method, Params: raw})
}

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return json.RawMessage("null"), nil
	}
	return json.Marshal(params)
}

// Close performs the LSP shutdown handshake then stops the process: graceful
// first, process group killed only if it does not exit.
// CapabilitiesSummary renders the stored initialize capabilities as a
// sorted, dot-path list of what the server offers ("renameProvider",
// "codeActionProvider", …). Empty when the server reported none.
func (c *Client) CapabilitiesSummary() string {
	c.mu.Lock()
	raw := c.caps
	c.mu.Unlock()
	if len(raw) == 0 {
		return ""
	}
	var caps map[string]any
	if err := json.Unmarshal(raw, &caps); err != nil {
		return capText(string(raw), 400)
	}
	var paths []string
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for k, v := range m {
			path := k
			if prefix != "" {
				path = prefix + "." + k
			}
			// `false` means "not offered" — the answer a caller needs is the
			// ABSENCE, so a declined provider is dropped.
			if b, ok := v.(bool); ok && !b {
				continue
			}
			// An options object ({"resolveProvider":true}) is itself proof
			// the capability exists: list it, and its sub-keys too. A nested
			// object whose every leaf is false would otherwise vanish and
			// read as "not supported".
			paths = append(paths, path)
			if sub, ok := v.(map[string]any); ok {
				walk(path, sub)
			}
		}
	}
	walk("", caps)
	sort.Strings(paths)
	return strings.Join(paths, "\n")
}

func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = c.Call(ctx, "shutdown", nil)
		cancel()
		_ = c.Notify("exit", nil)
		_ = c.stdin.Close()
		if c.exited != nil {
			select {
			case <-c.exited:
			case <-time.After(2 * time.Second):
				if c.proc != nil {
					killProcessGroup(c.proc.Pid)
				}
				select {
				case <-c.exited:
				case <-time.After(2 * time.Second):
				}
			}
		}
		c.finish(fmt.Errorf("lsp/%s: client closed", c.name))
	})
	return nil
}

// EnsureOpen makes the server aware of the file's current content: didOpen
// the first time, didChange whenever the on-disk text moved on. Without it a
// file edited earlier in the session answers from stale text.
func (c *Client) EnsureOpen(path, languageID string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	c.docMu.Lock()
	defer c.docMu.Unlock()
	uri := uriFromPath(path)
	st, open := c.docs[path]
	if open && st.size == fi.Size() && st.mod.Equal(fi.ModTime()) {
		return nil
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !open {
		c.docs[path] = docState{version: 1, size: fi.Size(), mod: fi.ModTime()}
		return c.Notify("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{
				"uri": uri, "languageId": languageID, "version": 1, "text": string(text),
			},
		})
	}
	st.version++
	st.size, st.mod = fi.Size(), fi.ModTime()
	c.docs[path] = st
	return c.Notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": uri, "version": st.version},
		"contentChanges": []map[string]any{{"text": string(text)}},
	})
}

func (c *Client) cacheDiagnostics(params json.RawMessage) {
	var p struct {
		URI         string       `json:"uri"`
		Diagnostics []Diagnostic `json:"diagnostics"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	c.diagMu.Lock()
	c.diags[p.URI] = p.Diagnostics
	c.seen[p.URI] = true
	prev := c.diagCh
	c.diagCh = make(chan struct{})
	c.diagMu.Unlock()
	close(prev)
}

// DiagnosticsFor returns the cached diagnostics for path. A server publishes
// asynchronously, so a file that was never analyzed waits up to wait for the
// first publish instead of being reported as clean.
func (c *Client) DiagnosticsFor(ctx context.Context, path string, wait time.Duration) ([]Diagnostic, bool) {
	uri := uriFromPath(path)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		c.diagMu.Lock()
		ds, seen, ch := c.diags[uri], c.seen[uri], c.diagCh
		c.diagMu.Unlock()
		if seen {
			return ds, true
		}
		select {
		case <-ch:
		case <-timer.C:
			return nil, false
		case <-ctx.Done():
			return nil, false
		case <-c.done:
			return nil, false
		}
	}
}

// CachedDiagnostics returns every uri with a published diagnostic set.
func (c *Client) CachedDiagnostics() map[string][]Diagnostic {
	c.diagMu.Lock()
	defer c.diagMu.Unlock()
	out := make(map[string][]Diagnostic, len(c.diags))
	for uri, ds := range c.diags {
		out[uri] = ds
	}
	return out
}

// tailWriter keeps the last max bytes a server wrote to stderr.
type tailWriter struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newTailWriter(max int) *tailWriter { return &tailWriter{max: max} }

func (w *tailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.max {
		w.buf = w.buf[len(w.buf)-w.max:]
	}
	return len(p), nil
}

// tail renders the captured stderr as ", stderr: ..." ("" when silent).
func (w *tailWriter) tail() string {
	if w == nil {
		return ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	s := strings.TrimSpace(string(w.buf))
	if s == "" {
		return ""
	}
	const maxTail = 300
	if len(s) > maxTail {
		s = "…" + s[len(s)-maxTail:]
	}
	return "; stderr: " + strings.ReplaceAll(s, "\n", " | ")
}
