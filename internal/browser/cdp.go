package browser

// CDP client + target discovery (M13 #50).
//
// xdev discovers pages through Chrome's HTTP discovery endpoint (/json/list)
// and then drives the page's own websocket. It prefers a browser the user
// already started with --remote-debugging-port — the real logged-in session,
// which the tool never touches beyond the pages it drives. When nothing is
// listening it starts its own browser on a private profile (launch.go);
// browser.autolaunch: false keeps attach-only.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
)

// maxDiscoveryBytes caps the /json/list response: a browser with a hundred
// tabs is still far below this.
const maxDiscoveryBytes = 8 << 20

// Target is one CDP page/tab as reported by /json/list.
type Target struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	Attached             bool   `json:"attached"`
}

// cdpError is the CDP error object of a failed command.
type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data"`
}

func (e *cdpError) Error() string {
	if e.Data != "" {
		return fmt.Sprintf("CDP error %d: %s (%s)", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("CDP error %d: %s", e.Code, e.Message)
}

// cdpConn is one CDP websocket connection with request/response correlation.
type cdpConn struct {
	ws *wsConn

	next int64

	mu      sync.Mutex
	pending map[int64]chan cdpReply
	closed  bool
	err     error
}

type cdpReply struct {
	result json.RawMessage
	err    error
}

// dialCDP connects to a page's webSocketDebuggerUrl and starts the read loop.
func dialCDP(ctx context.Context, wsURL string) (*cdpConn, error) {
	ws, err := dialWS(ctx, wsURL)
	if err != nil {
		return nil, err
	}
	c := &cdpConn{ws: ws, pending: map[int64]chan cdpReply{}}
	go c.read()
	return c, nil
}

// alive reports whether the connection is still usable (the read loop has not
// failed it).
func (c *cdpConn) alive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed
}

// Close shuts the connection down and fails any in-flight call.
func (c *cdpConn) Close() error {
	c.fail(io.EOF)
	return c.ws.Close()
}

// Call sends one CDP command and waits for its reply. Events (messages
// without an id) are dropped: v1 drives the page by command result, not by
// event stream, so no event subscription state exists to get stale.
func (c *cdpConn) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("browser: %s params: %w", method, err)
	}
	if params == nil {
		raw = []byte("{}")
	}
	c.mu.Lock()
	if c.closed {
		err := c.err
		c.mu.Unlock()
		if err == nil {
			err = io.EOF
		}
		return nil, fmt.Errorf("browser: %s: connection closed: %w", method, err)
	}
	c.next++
	id := c.next
	ch := make(chan cdpReply, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	msg := struct {
		ID     int64           `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}{ID: id, Method: method, Params: raw}
	payload, err := json.Marshal(msg)
	if err != nil {
		c.done(id)
		return nil, fmt.Errorf("browser: %s marshal: %w", method, err)
	}
	if err := c.ws.write(opText, payload); err != nil {
		c.done(id)
		return nil, fmt.Errorf("browser: %s: %w", method, err)
	}

	select {
	case r := <-ch:
		return r.result, r.err
	case <-ctx.Done():
		c.done(id)
		return nil, fmt.Errorf("browser: %s: %w", method, ctx.Err())
	}
}

// done removes a pending call from the dispatch table.
func (c *cdpConn) done(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// fail marks the connection dead and releases every waiter.
func (c *cdpConn) fail(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed, c.err = true, err
	pending := c.pending
	c.pending = map[int64]chan cdpReply{}
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- cdpReply{err: err}
	}
}

// read is the single reader goroutine: it frames messages, answers pending
// calls by id, and drops events.
func (c *cdpConn) read() {
	for {
		data, err := c.ws.readMessage()
		if err != nil {
			c.fail(err)
			return
		}
		var msg struct {
			ID     *int64          `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *cdpError       `json:"error"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue // unknown frame shape: not ours to interpret
		}
		if msg.ID == nil {
			continue // event (no subscriber in v1)
		}
		c.mu.Lock()
		ch, ok := c.pending[*msg.ID]
		delete(c.pending, *msg.ID)
		c.mu.Unlock()
		if !ok {
			continue // a call that already timed out
		}
		if msg.Error != nil {
			ch <- cdpReply{err: msg.Error}
			continue
		}
		ch <- cdpReply{result: msg.Result}
	}
}

// defaultEndpoint is Chrome's default --remote-debugging-port.
const defaultEndpoint = "http://127.0.0.1:9222"

// envEndpoint overrides the endpoint for a session without editing config.
const envEndpoint = "XDEV_BROWSER_CDP_URL"

// resolveEndpoint applies the browser.cdpUrl → $XDEV_BROWSER_CDP_URL →
// default precedence and normalizes the value to a discovery base URL.
func resolveEndpoint(configured string) string {
	base := strings.TrimSpace(configured)
	if base == "" {
		base = strings.TrimSpace(os.Getenv(envEndpoint))
	}
	if base == "" {
		return defaultEndpoint
	}
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	return strings.TrimRight(base, "/")
}

// discoveryURL appends the /json/list path unless the caller already gave a
// discovery URL (either /json or /json/list, both of which Chrome serves).
func discoveryURL(endpoint string) string {
	switch {
	case strings.HasSuffix(endpoint, "/json/list"), strings.HasSuffix(endpoint, "/json"):
		return endpoint
	default:
		return endpoint + "/json/list"
	}
}

// ListTargets reads the browser's target list from the discovery endpoint.
// A connection failure is turned into the "no browser here" error the caller
// needs to act on; autolaunch selects which remedy that error names.
//
// ponytail: no client timeout — the caller's context carries the per-op
// deadline (the tool always passes one), so a hung endpoint cannot outlive
// the call. A bare-Endpoint caller (tests) relies on its own context.
func ListTargets(ctx context.Context, endpoint string, autolaunch bool) ([]Target, error) {
	url := discoveryURL(endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("browser: discovery request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, unreachableErr(endpoint, err, autolaunch)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDiscoveryBytes))
	if err != nil {
		return nil, fmt.Errorf("browser: reading %s: %w", url, err)
	}
	var targets []Target
	if err := json.Unmarshal(body, &targets); err != nil {
		return nil, fmt.Errorf("browser: %s is not a CDP target list: %w", url, err)
	}
	return targets, nil
}

// unreachableErr names the fix, not just the failure: "nothing on that port"
// means the endpoint is not a running browser, and what to do about it
// depends on whether xdev is allowed to start one itself.
func unreachableErr(endpoint string, err error, autolaunch bool) error {
	if autolaunch {
		return fmt.Errorf("browser: no Chrome DevTools endpoint at %s (%v); could not auto-launch one — set $%s to a Chrome/Chromium binary, or start one yourself with "+
			"--remote-debugging-port=9222 (Chrome 136+ also needs a non-default --user-data-dir=<dir>), "+
			"then retry — browser.autolaunch: false turns the launch off", endpoint, err, envBinary)
	}
	return fmt.Errorf("browser: no Chrome DevTools endpoint at %s (%v); start Chrome/Chromium with "+
		"--remote-debugging-port=9222 (Chrome 136+ also needs a non-default --user-data-dir=<dir>), "+
		"then retry — or set browser.cdpUrl / $%s to the right endpoint", endpoint, err, envEndpoint)
}

// pickPage chooses an unclaimed page target. Tabs that are already driven by
// this session are skipped so two names never fight over one page.
func pickPage(targets []Target, claimed map[string]string) (Target, bool) {
	for _, t := range targets {
		if t.Type != "page" || t.WebSocketDebuggerURL == "" {
			continue
		}
		if _, taken := claimed[t.ID]; taken {
			continue
		}
		return t, true
	}
	return Target{}, false
}

// noPageErr explains a page list the tool cannot attach to: either the
// browser has no page target at all, or this session already drives every
// one of them (attach-only mode cannot create a page).
func noPageErr(endpoint string, targets []Target) error {
	for _, t := range targets {
		if t.Type == "page" && t.WebSocketDebuggerURL != "" {
			return fmt.Errorf("browser: every page target at %s is already driven by this session; "+
				"close a tab handle (op \"close\") or open a new tab in the browser", endpoint)
		}
	}
	return fmt.Errorf("browser: %s has no page target with a websocket URL; open a tab first "+
		"(e.g. navigate to chrome://newtab) — xdev attaches to existing tabs and never opens a browser", endpoint)
}

// evalResult is the shape of a Runtime.evaluate reply.
type evalResult struct {
	Result struct {
		Type        string          `json:"type"`
		Value       json.RawMessage `json:"value"`
		Description string          `json:"description"`
	} `json:"result"`
	ExceptionDetails *struct {
		Text      string `json:"text"`
		Exception *struct {
			Description string `json:"description"`
		} `json:"exception"`
	} `json:"exceptionDetails"`
}

// evaluate runs one expression in the page and returns its value. The
// second return value is the value as text (strings unquoted, everything
// else as compact JSON).
func (t *tab) evaluate(ctx context.Context, expr string) (string, error) {
	raw, err := t.c.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    expr,
		"returnByValue": true,
		"awaitPromise":  true,
	})
	if err != nil {
		if strings.Contains(err.Error(), "returned by value") {
			return "", fmt.Errorf("%w — the result is not JSON-serializable; return JSON.stringify(value)", err)
		}
		return "", err
	}
	var out evalResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("browser: evaluate reply: %w", err)
	}
	if out.ExceptionDetails != nil {
		desc := out.ExceptionDetails.Text
		if out.ExceptionDetails.Exception != nil && out.ExceptionDetails.Exception.Description != "" {
			desc = out.ExceptionDetails.Exception.Description
		}
		return "", fmt.Errorf("browser: page exception: %s", desc)
	}
	return evalValueText(&out)
}

// evalValueText renders the JSON value of an evaluate reply.
func evalValueText(out *evalResult) (string, error) {
	if len(out.Result.Value) == 0 {
		if out.Result.Description != "" {
			return out.Result.Description, nil
		}
		return out.Result.Type, nil
	}
	if out.Result.Type == "string" {
		var s string
		if err := json.Unmarshal(out.Result.Value, &s); err != nil {
			return string(out.Result.Value), nil
		}
		return s, nil
	}
	return string(out.Result.Value), nil
}
