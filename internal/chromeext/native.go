// Package chromeext is the Claude-in-Chrome-shaped bridge for xdev:
// a Chrome MV3 extension drives the user's real profile via chrome.debugger,
// and this package exposes a CDP-compatible loopback endpoint so the existing
// browser tool can attach without new ops.
//
// Wire:
//
//	browser tool  --CDP HTTP/WS-->  Host (this package)  --native messaging-->  extension
//
// Native messaging frames are uint32-le length + JSON (Chrome's host protocol).
package chromeext

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Native host identity — must match extensions/chrome/background.js and the
// installed com.freepeak.xdev_browser.json allowed_origins.
const (
	NativeHostName = "com.freepeak.xdev_browser"
	// ExtensionID is the stable id produced by the public key in
	// extensions/chrome/manifest.json (sha256 of SPKI → a-p alphabet).
	ExtensionID = "gblmengjnbglfmkmfbpbninolffmecdp"
	// DefaultListen is the CDP discovery base the browser tool attaches to
	// when using the extension bridge.
	DefaultListen = "127.0.0.1:9224"
)

// maxNativeFrame caps one native-messaging payload (Chrome itself caps at 1 MiB).
const maxNativeFrame = 1 << 20

// Bridge is the in-process state shared by the native-messaging reader and the
// CDP HTTP/WS server: one extension peer, pending RPC, and the target list.
type Bridge struct {
	mu sync.Mutex

	out   io.Writer
	outMu sync.Mutex

	// nextID is the next request id sent to the extension.
	nextID atomic.Int64
	// pending maps request id → waiter.
	pending map[int64]chan nativeReply

	// connected is set when the extension has said hello (or any message).
	connected atomic.Bool
	// lastHello holds the last hello payload for status.
	lastHello json.RawMessage

	// logf is optional diagnostics (tests leave it nil).
	logf func(string, ...any)
}

type nativeReply struct {
	raw json.RawMessage
	err error
}

// NewBridge builds a bridge that writes native frames to out (Chrome's stdin
// of the host is our stdout).
func NewBridge(out io.Writer) *Bridge {
	return &Bridge{
		out:     out,
		pending: map[int64]chan nativeReply{},
	}
}

// Connected reports whether the extension has spoken at least once.
func (b *Bridge) Connected() bool { return b.connected.Load() }

// Send writes one native-messaging frame to the extension.
func (b *Bridge) Send(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(raw) > maxNativeFrame {
		return fmt.Errorf("chromeext: native frame %d exceeds %d", len(raw), maxNativeFrame)
	}
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(raw)))
	b.outMu.Lock()
	defer b.outMu.Unlock()
	if _, err := b.out.Write(hdr[:]); err != nil {
		return err
	}
	_, err = b.out.Write(raw)
	return err
}

// ReadLoop consumes length-prefixed frames from r until EOF or a hard error.
// It is the host's main goroutine when launched as --chrome-native-host.
func (b *Bridge) ReadLoop(r io.Reader) error {
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				b.failAll(io.EOF)
				return nil
			}
			b.failAll(err)
			return err
		}
		n := binary.LittleEndian.Uint32(hdr[:])
		if n == 0 || n > maxNativeFrame {
			err := fmt.Errorf("chromeext: bad native frame length %d", n)
			b.failAll(err)
			return err
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			b.failAll(err)
			return err
		}
		b.handleFrame(buf)
	}
}

func (b *Bridge) failAll(err error) {
	b.connected.Store(false)
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, ch := range b.pending {
		ch <- nativeReply{err: err}
		delete(b.pending, id)
	}
}

func (b *Bridge) handleFrame(raw []byte) {
	var env struct {
		Type string          `json:"type"`
		ID   int64           `json:"id"`
		Raw  json.RawMessage `json:"-"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		b.log("chromeext: bad frame: %v", err)
		return
	}
	env.Raw = append(json.RawMessage(nil), raw...)
	b.connected.Store(true)

	switch env.Type {
	case "hello":
		b.lastHello = env.Raw
		_ = b.Send(map[string]any{"type": "hello_ack", "host": "xdev", "listen": DefaultListen})
	case "ping":
		_ = b.Send(map[string]any{"type": "pong", "t": time.Now().UnixMilli()})
	case "pong":
		// keepalive ack — nothing to do
	case "list_targets_result", "cdp_result", "attach_result", "detach_result", "error":
		b.deliver(env.ID, env.Raw, nil)
	case "detached":
		// extension dropped a tab; no waiter
		b.log("chromeext: tab detached: %s", string(raw))
	default:
		// If it carries an id, deliver as a generic reply.
		if env.ID != 0 {
			b.deliver(env.ID, env.Raw, nil)
		} else {
			b.log("chromeext: unhandled type %q", env.Type)
		}
	}
}

func (b *Bridge) deliver(id int64, raw json.RawMessage, err error) {
	if id == 0 {
		return
	}
	b.mu.Lock()
	ch, ok := b.pending[id]
	if ok {
		delete(b.pending, id)
	}
	b.mu.Unlock()
	if !ok {
		return
	}
	ch <- nativeReply{raw: raw, err: err}
}

func (b *Bridge) log(format string, args ...any) {
	if b.logf != nil {
		b.logf(format, args...)
	}
}

// call sends a typed request with a fresh id and waits for the matching reply.
func (b *Bridge) call(ctx context.Context, msg map[string]any) (json.RawMessage, error) {
	id := b.nextID.Add(1)
	msg["id"] = id
	ch := make(chan nativeReply, 1)
	b.mu.Lock()
	b.pending[id] = ch
	b.mu.Unlock()

	if err := b.Send(msg); err != nil {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return nil, err
	}
	select {
	case <-ctx.Done():
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return nil, ctx.Err()
	case rep := <-ch:
		if rep.err != nil {
			return nil, rep.err
		}
		return rep.raw, nil
	}
}

// Target is one page as the extension reports it (CDP /json/list shape).
type Target struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	FavIconURL           string `json:"favIconUrl,omitempty"`
	Attached             bool   `json:"attached,omitempty"`
}

// ListTargets asks the extension for open pages and stamps each with a
// websocket URL pointing at this host's /devtools/page/<id>.
func (b *Bridge) ListTargets(ctx context.Context, wsBase string) ([]Target, error) {
	raw, err := b.call(ctx, map[string]any{"type": "list_targets"})
	if err != nil {
		return nil, err
	}
	var rep struct {
		Type    string   `json:"type"`
		Error   string   `json:"error"`
		Targets []Target `json:"targets"`
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil, fmt.Errorf("chromeext: list_targets: %w", err)
	}
	if rep.Error != "" {
		return nil, fmt.Errorf("chromeext: list_targets: %s", rep.Error)
	}
	for i := range rep.Targets {
		if rep.Targets[i].Type == "" {
			rep.Targets[i].Type = "page"
		}
		rep.Targets[i].WebSocketDebuggerURL = fmt.Sprintf("%s/devtools/page/%s", wsBase, rep.Targets[i].ID)
	}
	return rep.Targets, nil
}

// CDP sends one chrome.debugger command against tabId and returns the result object.
func (b *Bridge) CDP(ctx context.Context, tabID, method string, params json.RawMessage) (json.RawMessage, error) {
	msg := map[string]any{
		"type":   "cdp",
		"tabId":  tabID,
		"method": method,
	}
	if len(params) > 0 && string(params) != "null" {
		var v any
		if err := json.Unmarshal(params, &v); err != nil {
			return nil, fmt.Errorf("chromeext: cdp params: %w", err)
		}
		msg["params"] = v
	}
	raw, err := b.call(ctx, msg)
	if err != nil {
		return nil, err
	}
	var rep struct {
		Type   string          `json:"type"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil, fmt.Errorf("chromeext: cdp reply: %w", err)
	}
	if rep.Error != nil {
		return nil, fmt.Errorf("CDP error %d: %s", rep.Error.Code, rep.Error.Message)
	}
	if len(rep.Result) == 0 {
		return json.RawMessage(`{}`), nil
	}
	return rep.Result, nil
}

// RunNativeHost is the process entry for `xdev --chrome-native-host` /
// `xdev chrome host`: native messaging on stdin/stdout + CDP on listen.
func RunNativeHost(ctx context.Context, listen string) error {
	if listen == "" {
		listen = DefaultListen
	}
	b := NewBridge(os.Stdout)
	b.logf = func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}
	srv, err := NewServer(b, listen)
	if err != nil {
		return err
	}
	errCh := make(chan error, 2)
	go func() { errCh <- srv.Serve(ctx) }()
	go func() { errCh <- b.ReadLoop(os.Stdin) }()
	select {
	case <-ctx.Done():
		_ = srv.Close()
		return ctx.Err()
	case err := <-errCh:
		_ = srv.Close()
		return err
	}
}
