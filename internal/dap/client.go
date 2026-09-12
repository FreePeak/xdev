package dap

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// eventBuffer bounds the queued adapter events. A debuggee that floods output
// must not grow the client without bound; excess events are dropped and
// counted (Dropped).
const eventBuffer = 64

// clientID identifies xdev to the adapter's initialize handshake.
var clientID = fmt.Sprintf("xdev-%d", os.Getpid())

// Client is one running debug adapter.
type Client struct {
	name  string
	stdin io.WriteCloser
	out   *bufio.Reader
	proc  *os.Process
	// exited is closed when the adapter process has been reaped (nil for a
	// client that was never attached to a process, e.g. in tests).
	exited chan struct{}
	stderr *tailWriter

	writeMu sync.Mutex

	mu      sync.Mutex
	seq     int
	pending map[int]chan *message
	events  chan Event
	dropped int
	dead    error

	done      chan struct{}
	closeOnce sync.Once
}

// newClient wraps an already-connected stdio pair. stdout must be read by
// exactly one goroutine (readLoop).
func newClient(stdin io.WriteCloser, stdout io.Reader, name string) *Client {
	return &Client{
		name:    name,
		stdin:   stdin,
		out:     bufio.NewReader(stdout),
		stderr:  newTailWriter(4 << 10),
		pending: map[int]chan *message{},
		events:  make(chan Event, eventBuffer),
		done:    make(chan struct{}),
	}
}

// Name is the configured adapter id (e.g. "dlv").
func (c *Client) Name() string { return c.name }

// Done is closed when the client is unusable (adapter exited or Close ran).
func (c *Client) Done() <-chan struct{} { return c.done }

// start begins dispatching adapter messages.
func (c *Client) start() { go c.readLoop() }

func (c *Client) readLoop() {
	for {
		body, err := readFrame(c.out)
		if err != nil {
			c.finish(c.exitErr(err))
			return
		}
		var msg message
		if err := json.Unmarshal(body, &msg); err != nil {
			continue // a malformed frame must not kill the session
		}
		switch msg.Type {
		case typeResponse:
			c.mu.Lock()
			ch := c.pending[msg.RequestSeq]
			delete(c.pending, msg.RequestSeq)
			c.mu.Unlock()
			if ch != nil {
				ch <- &msg
			}
		case typeEvent:
			select {
			case c.events <- Event{Name: msg.Event, Body: msg.Body}:
			default:
				c.mu.Lock()
				c.dropped++
				c.mu.Unlock()
			}
		}
	}
}

// exitErr decorates a terminal read error with whatever the adapter last said
// on stderr, so "adapter exited" is actionable.
func (c *Client) exitErr(err error) error {
	if s := c.stderr.tail(); s != "" {
		return fmt.Errorf("debug/%s: adapter stopped: %w (%s)", c.name, err, s)
	}
	return fmt.Errorf("debug/%s: adapter stopped: %w", c.name, err)
}

// finish marks the client dead exactly once, fails every waiter and frees the
// process-wide session slot. Safe to call repeatedly (Close calls it too), so
// it must not go through closeOnce — that is Close's own guard and a nested
// Do on the same Once deadlocks.
func (c *Client) finish(err error) {
	c.mu.Lock()
	if c.dead != nil {
		c.mu.Unlock()
		return
	}
	c.dead = err
	close(c.done)
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	clearActive(c)
}

// Dead reports why the client is unusable.
func (c *Client) Dead() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dead != nil {
		return c.dead
	}
	return fmt.Errorf("debug/%s: adapter connection closed", c.name)
}

// Dropped counts events dropped because nothing consumed them in time.
func (c *Client) Dropped() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropped
}

func (c *Client) forget(id int) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// alive reports whether the adapter connection is still usable.
func (c *Client) alive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dead == nil
}

// send writes one message, marking the client dead when the pipe is gone.
func (c *Client) send(msg *message) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := writeFrame(c.stdin, msg); err != nil {
		c.finish(fmt.Errorf("debug/%s: write: %w", c.name, err))
		return c.Dead()
	}
	return nil
}

// Call sends one request and waits for its response, returning the response
// body. A DAP-level failure (success=false) is an error carrying the
// adapter's message.
func (c *Client) Call(ctx context.Context, command string, args any) (json.RawMessage, error) {
	var raw json.RawMessage
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			return nil, fmt.Errorf("debug: %s arguments: %w", command, err)
		}
		raw = b
	}
	c.mu.Lock()
	if c.dead != nil {
		err := c.dead
		c.mu.Unlock()
		return nil, err
	}
	c.seq++
	id := c.seq
	ch := make(chan *message, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	msg := &message{Seq: id, Type: typeRequest, Command: command, Arguments: raw}
	if err := c.send(msg); err != nil {
		c.forget(id)
		return nil, err
	}
	select {
	case resp, ok := <-ch:
		if !ok { // adapter died while the request was in flight
			return nil, c.Dead()
		}
		if !resp.Success {
			if resp.Message != "" {
				return nil, fmt.Errorf("debug: %s: %s", command, resp.Message)
			}
			return nil, fmt.Errorf("debug: %s failed", command)
		}
		return resp.Body, nil
	case <-ctx.Done():
		c.forget(id)
		return nil, fmt.Errorf("debug: %s: %w", command, ctx.Err())
	case <-c.done:
		c.forget(id)
		return nil, c.Dead()
	}
}

// Capabilities is the subset of the adapter's initialize response the client
// acts on.
type Capabilities struct {
	SupportsConfigurationDoneRequest bool `json:"supportsConfigurationDoneRequest"`
}

// Initialize performs the DAP initialize exchange: what xdev can do is
// declared honestly (no runInTerminal, no variable paging), so an adapter
// never waits for a reverse request that will not come.
func (c *Client) Initialize(ctx context.Context, adapterID string) (Capabilities, error) {
	var caps Capabilities
	body, err := c.Call(ctx, "initialize", map[string]any{
		"clientID":                     clientID,
		"clientName":                   "xdev",
		"adapterID":                    adapterID,
		"pathFormat":                   "path",
		"linesStartAt1":                true,
		"columnsStartAt1":              true,
		"supportsVariableType":         true,
		"supportsVariablePaging":       false,
		"supportsRunInTerminalRequest": false,
		"supportsTerminateRequest":     true,
	})
	if err != nil {
		return caps, err
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &caps); err != nil {
			return caps, fmt.Errorf("debug/%s: initialize capabilities: %w", c.name, err)
		}
	}
	return caps, nil
}

// NextEvent returns the next adapter event, blocking until one arrives, ctx
// expires or the adapter exits. Events buffered before an exit are still
// delivered (the stopped event of a finished debuggee must not be lost).
func (c *Client) NextEvent(ctx context.Context) (Event, error) {
	select {
	case ev := <-c.events:
		return ev, nil
	case <-ctx.Done():
		return Event{}, ctx.Err()
	case <-c.done:
		select {
		case ev := <-c.events:
			return ev, nil
		default:
		}
		return Event{}, c.Dead()
	}
}

// WaitEvent blocks until one of names arrives (other events are discarded) or
// the timeout/cancel fires.
func (c *Client) WaitEvent(ctx context.Context, timeout time.Duration, names ...string) (Event, error) {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		ev, err := c.NextEvent(wctx)
		if err != nil {
			return Event{}, err
		}
		for _, n := range names {
			if ev.Name == n {
				return ev, nil
			}
		}
	}
}

// Terminate ends the debuggee: the DAP terminate request, then disconnect and
// process teardown. An adapter that does not implement terminate still gets
// torn down — the error says why the request failed.
func (c *Client) Terminate(ctx context.Context) error {
	_, err := c.Call(ctx, "terminate", map[string]any{"restart": false})
	_ = c.Close()
	return err
}

// Close ends the session: DAP disconnect (terminateDebuggee false, so a
// detach never kills a process the user is debugging) and then the adapter's
// process group is stopped — gracefully first, killed only if it lingers.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = c.Call(ctx, "disconnect", map[string]any{"terminateDebuggee": false})
		cancel()
		_ = c.stdin.Close()
		c.stopProcess(2 * time.Second)
		c.finish(fmt.Errorf("debug/%s: adapter closed", c.name))
	})
	return nil
}

// stopProcess reaps the adapter: grace to exit on its own, then the whole
// process group is killed (a debug adapter spawns the inferior process).
func (c *Client) stopProcess(grace time.Duration) {
	if c.proc == nil {
		return
	}
	if c.exited != nil {
		select {
		case <-c.exited:
			return
		case <-time.After(grace):
		}
	}
	killProcessGroup(c.proc.Pid)
	if c.exited != nil {
		select {
		case <-c.exited:
		case <-time.After(2 * time.Second):
		}
	}
}

// tailWriter keeps the last max bytes an adapter wrote to stderr.
type tailWriter struct {
	mu  sync.Mutex
	max int
	buf []byte
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

// tail renders the captured stderr trimmed to one line ("" when silent).
func (w *tailWriter) tail() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return lastLine(string(w.buf))
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 200 {
		s = s[len(s)-200:]
	}
	return strings.TrimSpace(s)
}
