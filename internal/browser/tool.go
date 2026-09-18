package browser

// The `browser` tool (M13 #50, research parity-tools-providers §D): a
// minimal CDP client for an already-running Chrome.
//
// Attach-first: when a browser is already listening on the configured
// endpoint (a Chrome the user started with --remote-debugging-port) xdev
// attaches to it and never touches the window, the profile or the user's
// tabs — `close` releases xdev's handle. When nothing answers, xdev starts a
// throwaway Chrome on that endpoint with its own profile (see launch.go)
// instead of failing with instructions the model cannot carry out. An
// explicit browser.autolaunch: false restores attach-only.
//
// Every op is bounded: a per-op timeout, a page-text cap, a JS-result cap,
// and a screenshot byte cap. Page text is sanitized before it reaches the
// model's context (control characters dropped, blank runs folded).

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/tool"
)

// Bounds (M13 #50): a browser op must never dump a whole page, a JS heap, or
// an unbounded image into the model's context.
const (
	// DefaultTimeout bounds one op when the caller omits timeout.
	DefaultTimeout = 30 * time.Second
	// MinTimeout / MaxTimeout clamp the per-op timeout argument. MinTimeout
	// also bounds a cold attach: a launch that cannot open the port fails
	// quickly instead of hanging the op.
	MinTimeout = time.Second
	MaxTimeout = 300 * time.Second

	// defaultSnapshotChars / maxSnapshotChars bound snapshot text.
	defaultSnapshotChars = 20000
	maxSnapshotChars     = 200000
	// defaultEvalChars / maxEvalChars bound eval output.
	defaultEvalChars = 8000
	maxEvalChars     = 64000
	// maxScreenshotBytes bounds a decoded PNG before it reaches the blob store.
	maxScreenshotBytes = 16 << 20

	// loadWait / loadPoll bound the post-navigate wait for readyState.
	loadWait = 15 * time.Second
	loadPoll = 100 * time.Millisecond
	// defaultTabName is the handle used when the caller omits name.
	defaultTabName = "main"

	// profileDirName is the launched browser's profile directory under the
	// tool's ProfileDir: a private profile, never the user's.
	profileDirName = "profile"
)

// pngMagic identifies a PNG payload; a screenshot must be one.
// Settings is the browser config block (`browser:` in config.yml).
type Settings struct {
	// CDPURL is the DevTools HTTP discovery base of the browser to drive
	// ("http://127.0.0.1:9222", or the /json/list URL itself). Empty means
	// $XDEV_BROWSER_CDP_URL, then Chrome's default port.
	CDPURL string `yaml:"cdpUrl"`
	// Timeout is the per-op limit in seconds (default 30, clamped 1..300).
	Timeout int `yaml:"timeout"`
	// Autolaunch, when set, starts a browser on CDPURL if nothing answers
	// there: false is attach-only (every call reports the dead endpoint),
	// true is the explicit form of the default. A pointer because the
	// default is ON and an absent key must not read as "off".
	Autolaunch *bool `yaml:"autolaunch"`
	// NoAutolaunch is the resolved inverse of Autolaunch, filled in by
	// config.BrowserConfig.
	NoAutolaunch bool `yaml:"-"`
	// ProfileDir is filled in by config.BrowserConfig with the private
	// --user-data-dir a launched browser gets. It is not a config key:
	// empty means the tool never launches one.
	ProfileDir string `yaml:"-"`
}

// AutolaunchOn reports whether xdev may start a browser itself: the default
// is on, so only an explicit `autolaunch: false` turns it off.
func (s Settings) AutolaunchOn() bool {
	return s.Autolaunch == nil || *s.Autolaunch
}

// pngMagic identifies a PNG payload; a screenshot must be one.
var pngMagic = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

// Tool is the `browser` tool. One instance per session: named tabs and their
// CDP connections are per-instance state.
type Tool struct {
	// Cfg is the browser: settings block (zero value = defaults).
	Cfg Settings
	// Blobs receives screenshots. nil means a store under the system temp dir.
	Blobs *session.BlobStore
	// ProfileDir is the private --user-data-dir of a browser xdev launches
	// itself (a profileDir under the data dir). Empty means the tool only
	// ever attaches to a browser already running.
	ProfileDir string
	// launchErr records a failed auto-launch, so the next attach reports
	// that failure instead of repeating the launch.
	launchErr error
	// ponytail: one mutex guards the tab map for the whole attach sequence,
	// so the first two concurrent ops on a cold/slow endpoint serialize. The
	// endpoint is loopback and Chrome answers instantly or refuses instantly;
	// an upgrade path is a per-name in-flight placeholder if that ever bites.
	mu   sync.Mutex
	tabs map[string]*tab
}

// tab is one attached page.
type tab struct {
	name     string
	targetID string
	url      string
	title    string
	c        *cdpConn
	// opMu serializes ops on one page: click/type/navigate against a page
	// that is mid-navigation is a race the model cannot debug.
	opMu sync.Mutex
}

// NewTool returns the browser tool. blobs holds screenshots; nil uses a
// store under the system temp dir (tests, harnesses).
func NewTool(cfg Settings, blobs *session.BlobStore) *Tool {
	if blobs == nil {
		blobs = session.NewBlobStore(filepath.Join(os.TempDir(), "xdev-browser"))
	}
	return &Tool{Cfg: cfg, Blobs: blobs, tabs: map[string]*tab{}}
}

// Close releases every attached connection. The browser tabs stay open.
func (t *Tool) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for name, tb := range t.tabs {
		_ = tb.c.Close()
		delete(t.tabs, name)
	}
	return nil
}

// Name implements tool.Tool.
func (t *Tool) Name() string { return "browser" }

// Description implements tool.Tool.
func (t *Tool) Description() string {
	return "Drive a running Chrome over CDP (started with --remote-debugging-port): open, snapshot, screenshot, click, type, eval, close."
}

// Parameters implements tool.Tool.
func (t *Tool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "op": {"type": "string", "enum": ["open", "snapshot", "screenshot", "click", "type", "eval", "close"], "description": "operation to run"},
    "name": {"type": "string", "description": "tab handle (default \"main\"); open attaches a page to it, later ops address it"},
    "url": {"type": "string", "description": "open: navigate the tab here (http(s)://, file://, or data:)"},
    "selector": {"type": "string", "description": "click/type: CSS selector of the target element"},
    "text": {"type": "string", "description": "type: text to put in the field (replaces its value)"},
    "expression": {"type": "string", "description": "eval: JavaScript expression; awaitPromise is on, the value must be JSON-serializable (wrap in JSON.stringify otherwise)"},
    "maxChars": {"type": "integer", "description": "snapshot/eval output cap (snapshot default 20000, max 200000; eval default 8000, max 64000)"},
    "timeout": {"type": "number", "description": "per-op limit in seconds (default 30, clamped 1..300)"},
    "all": {"type": "boolean", "description": "close: release every tab this session attached"}
  },
  "required": ["op"]
}`)
}

// args is the input shape; keep in sync with Parameters.
type args struct {
	Op         string  `json:"op"`
	Name       string  `json:"name,omitempty"`
	URL        string  `json:"url,omitempty"`
	Selector   string  `json:"selector,omitempty"`
	Text       string  `json:"text,omitempty"`
	Expression string  `json:"expression,omitempty"`
	MaxChars   int     `json:"maxChars,omitempty"`
	Timeout    float64 `json:"timeout,omitempty"`
	All        bool    `json:"all,omitempty"`
}

// Execute implements tool.Tool.
func (t *Tool) Execute(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var in args
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult("browser: invalid arguments: " + err.Error()), nil
	}
	op := strings.ToLower(strings.TrimSpace(in.Op))
	if op == "" {
		return errResult("browser: op is required (open|snapshot|screenshot|click|type|eval|close)"), nil
	}
	if op == "close" {
		return t.opClose(in), nil
	}
	ctx, cancel := context.WithTimeout(ctx, t.opTimeout(in.Timeout))
	defer cancel()

	var res tool.Result
	var err error
	switch op {
	case "open":
		res, err = t.opOpen(ctx, in)
	case "snapshot":
		res, err = t.opSnapshot(ctx, in)
	case "screenshot":
		res, err = t.opScreenshot(ctx, in)
	case "click":
		res, err = t.opClick(ctx, in)
	case "type":
		res, err = t.opType(ctx, in)
	case "eval":
		res, err = t.opEval(ctx, in)
	default:
		return errResult(fmt.Sprintf("browser: unknown op %q (open|snapshot|screenshot|click|type|eval|close)", in.Op)), nil
	}
	if err != nil {
		return errResult(err.Error()), nil
	}
	return res, nil
}

// opOpen attaches (or re-attaches) a named tab and optionally navigates it.
func (t *Tool) opOpen(ctx context.Context, in args) (tool.Result, error) {
	name := tabName(in.Name)
	tb, err := t.ensure(ctx, name)
	if err != nil {
		return tool.Result{}, err
	}
	tb.opMu.Lock()
	defer tb.opMu.Unlock()

	state := ""
	if url := strings.TrimSpace(in.URL); url != "" {
		state, err = tb.navigate(ctx, url)
		if err != nil {
			return tool.Result{}, err
		}
	}
	info, err := tb.info(ctx)
	if err != nil {
		return tool.Result{}, err
	}
	if state == "" {
		state = info.ReadyState
	}
	text := fmt.Sprintf("tab %q attached: %s (%s, readyState=%s)", name, info.URL, info.Title, state)
	if in.URL != "" && state != "complete" {
		text += fmt.Sprintf("\nwarning: the page had not finished loading after %s; snapshot/click may see a partial DOM", loadWait)
	}
	return tool.Result{Text: text, Details: map[string]any{
		"name": name, "targetId": tb.targetID, "url": info.URL, "title": info.Title, "readyState": state,
	}}, nil
}

// opSnapshot returns the page's title/url plus bounded, sanitized DOM text.
func (t *Tool) opSnapshot(ctx context.Context, in args) (tool.Result, error) {
	tb, err := t.tab(ctx, in)
	if err != nil {
		return tool.Result{}, err
	}
	tb.opMu.Lock()
	defer tb.opMu.Unlock()

	text, err := tb.evaluate(ctx, snapshotExpr)
	if err != nil {
		return tool.Result{}, err
	}
	var snap struct {
		URL   string `json:"url"`
		Title string `json:"title"`
		Text  string `json:"text"`
	}
	if err := json.Unmarshal([]byte(text), &snap); err != nil {
		return tool.Result{}, fmt.Errorf("browser: snapshot reply: %w", err)
	}
	max := clampChars(in.MaxChars, defaultSnapshotChars, maxSnapshotChars)
	body := sanitizeText(snap.Text, max)
	out := fmt.Sprintf("URL: %s\nTITLE: %s\n---\n%s", snap.URL, snap.Title, body)
	return tool.Result{Text: out, Details: map[string]any{
		"name": tb.name, "url": snap.URL, "title": snap.Title,
		"textChars": len(body), "truncated": len(body) >= max,
	}}, nil
}

// opScreenshot captures a PNG into the blob store and returns its path.
func (t *Tool) opScreenshot(ctx context.Context, in args) (tool.Result, error) {
	tb, err := t.tab(ctx, in)
	if err != nil {
		return tool.Result{}, err
	}
	tb.opMu.Lock()
	defer tb.opMu.Unlock()

	raw, err := tb.c.Call(ctx, "Page.captureScreenshot", map[string]any{"format": "png"})
	if err != nil {
		return tool.Result{}, err
	}
	var shot struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &shot); err != nil {
		return tool.Result{}, fmt.Errorf("browser: screenshot reply: %w", err)
	}
	if base64.StdEncoding.DecodedLen(len(shot.Data)) > maxScreenshotBytes {
		return tool.Result{}, fmt.Errorf("browser: screenshot exceeds the %d byte cap (encoded %d bytes)", maxScreenshotBytes, len(shot.Data))
	}
	png, err := base64.StdEncoding.DecodeString(shot.Data)
	if err != nil {
		return tool.Result{}, fmt.Errorf("browser: screenshot is not valid base64: %w", err)
	}
	if !bytes.HasPrefix(png, pngMagic) {
		return tool.Result{}, fmt.Errorf("browser: screenshot is not a PNG (%d bytes, missing the PNG signature)", len(png))
	}
	ref, err := t.Blobs.Put(png)
	if err != nil {
		return tool.Result{}, fmt.Errorf("browser: storing screenshot: %w", err)
	}
	sum, err := session.ParseBlobRef(ref)
	if err != nil {
		return tool.Result{}, fmt.Errorf("browser: %w", err)
	}
	path := filepath.Join(t.Blobs.Root(), sum)
	return tool.Result{
		Text: fmt.Sprintf("screenshot: %s (%d bytes, %s)", path, len(png), ref),
		Details: map[string]any{
			"name": tb.name, "path": path, "blob": ref, "bytes": len(png),
		},
	}, nil
}

// opClick clicks the first element matching a CSS selector.
func (t *Tool) opClick(ctx context.Context, in args) (tool.Result, error) {
	sel, err := requireSelector(in.Selector)
	if err != nil {
		return tool.Result{}, err
	}
	tb, err := t.tab(ctx, in)
	if err != nil {
		return tool.Result{}, err
	}
	tb.opMu.Lock()
	defer tb.opMu.Unlock()

	out, err := tb.evaluate(ctx, clickExpr(sel))
	if err != nil {
		return tool.Result{}, err
	}
	return domResult("clicked", sel, out)
}

// opType sets the value of the first element matching a CSS selector.
func (t *Tool) opType(ctx context.Context, in args) (tool.Result, error) {
	sel, err := requireSelector(in.Selector)
	if err != nil {
		return tool.Result{}, err
	}
	tb, err := t.tab(ctx, in)
	if err != nil {
		return tool.Result{}, err
	}
	tb.opMu.Lock()
	defer tb.opMu.Unlock()

	out, err := tb.evaluate(ctx, typeExpr(sel, in.Text))
	if err != nil {
		return tool.Result{}, err
	}
	return domResult("typed into", sel, out)
}

// opEval runs a JavaScript expression in the page and returns its value.
func (t *Tool) opEval(ctx context.Context, in args) (tool.Result, error) {
	expr := strings.TrimSpace(in.Expression)
	if expr == "" {
		return tool.Result{}, errors.New("browser: eval needs expression")
	}
	tb, err := t.tab(ctx, in)
	if err != nil {
		return tool.Result{}, err
	}
	tb.opMu.Lock()
	defer tb.opMu.Unlock()

	out, err := tb.evaluate(ctx, expr)
	if err != nil {
		return tool.Result{}, err
	}
	max := clampChars(in.MaxChars, defaultEvalChars, maxEvalChars)
	body := capText(out, max)
	return tool.Result{Text: body, Details: map[string]any{
		"name": tb.name, "resultChars": len(body), "truncated": len(body) >= max,
	}}, nil
}

// opClose releases tab handles. It never closes the browser's tabs.
func (t *Tool) opClose(in args) tool.Result {
	t.mu.Lock()
	defer t.mu.Unlock()
	if in.All {
		n := len(t.tabs)
		for name, tb := range t.tabs {
			_ = tb.c.Close()
			delete(t.tabs, name)
		}
		return tool.Result{Text: fmt.Sprintf("released %d tab handle(s); the browser tabs themselves are left open", n)}
	}
	name := tabName(in.Name)
	tb, ok := t.tabs[name]
	if !ok {
		return tool.Result{Text: fmt.Sprintf("no tab named %q is attached; nothing to release", name)}
	}
	delete(t.tabs, name)
	_ = tb.c.Close()
	return tool.Result{Text: fmt.Sprintf("released tab %q; the browser tab itself is left open", name)}
}

// tab resolves the named tab, attaching on first use.
func (t *Tool) tab(ctx context.Context, in args) (*tab, error) {
	return t.ensure(ctx, tabName(in.Name))
}

// ensure returns the named tab, attaching to a page when the name is unknown
// or its connection has died.
func (t *Tool) ensure(ctx context.Context, name string) (*tab, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if tb, ok := t.tabs[name]; ok {
		if tb.c.alive() {
			return tb, nil
		}
		delete(t.tabs, name) // dead websocket: re-attach below
	}
	claimed := make(map[string]string, len(t.tabs))
	for n, other := range t.tabs {
		claimed[other.targetID] = n
	}
	endpoint := t.endpoint()
	targets, err := ListTargets(ctx, endpoint, t.autolaunch())
	if err != nil && t.autolaunch() && t.launchErr == nil && ctx.Err() == nil {
		// Nothing is listening: start a browser on the endpoint and retry
		// once. A launch that fails is remembered, so the next call reports
		// the cause instead of launching again.
		if lerr := ensureLaunched(ctx, endpoint, filepath.Join(t.ProfileDir, profileDirName)); lerr != nil {
			t.launchErr = lerr
		} else {
			targets, err = ListTargets(ctx, endpoint, true)
		}
	}
	if err != nil {
		if t.launchErr != nil {
			return nil, t.launchErr
		}
		return nil, err
	}
	tgt, ok := pickPage(targets, claimed)
	if !ok {
		return nil, noPageErr(endpoint, targets)
	}
	c, err := dialCDP(ctx, tgt.WebSocketDebuggerURL)
	if err != nil {
		return nil, err
	}
	tb := &tab{name: name, targetID: tgt.ID, url: tgt.URL, title: tgt.Title, c: c}
	t.tabs[name] = tb
	return tb, nil
}

// endpoint resolves the discovery base for this tool.
func (t *Tool) endpoint() string { return resolveEndpoint(t.Cfg.CDPURL) }

// opTimeout resolves the per-op limit: the call's timeout argument, else the
// configured browser.timeout, else 30s — clamped to 1..300s.
func (t *Tool) opTimeout(seconds float64) time.Duration {
	d := DefaultTimeout
	if t.Cfg.Timeout > 0 {
		d = time.Duration(t.Cfg.Timeout) * time.Second
	}
	if seconds > 0 {
		d = time.Duration(seconds * float64(time.Second))
	}
	if d < MinTimeout {
		return MinTimeout
	}
	if d > MaxTimeout {
		return MaxTimeout
	}
	return d
}

// tabName applies the default handle.
func tabName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return defaultTabName
	}
	return name
}

// clampChars resolves a char-cap argument against its default and hard max.
func clampChars(v, def, max int) int {
	if v <= 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}

// requireSelector rejects an empty selector before any round trip.
func requireSelector(sel string) (string, error) {
	sel = strings.TrimSpace(sel)
	if sel == "" {
		return "", errors.New("browser: selector is required")
	}
	return sel, nil
}

// errResult wraps a failure as a tool result (the loop's contract: tool
// failures are results with IsError, not Go errors).
func errResult(msg string) tool.Result {
	return tool.Result{Text: msg, IsError: true}
}

// domResult turns a page-side DOM action reply into a tool result.
func domResult(verb, selector, out string) (tool.Result, error) {
	var r struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Tag   string `json:"tag"`
		Value string `json:"value"`
		Text  string `json:"text"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		return tool.Result{}, fmt.Errorf("browser: %s %q: unexpected reply %s", verb, selector, capText(out, 200))
	}
	if !r.OK {
		msg := r.Error
		if msg == "" {
			msg = "the action did not run"
		}
		return tool.Result{}, fmt.Errorf("browser: %s %q: %s", verb, selector, msg)
	}
	text := fmt.Sprintf("%s %s (<%s>)", verb, selector, r.Tag)
	if r.Text != "" {
		text += ": " + r.Text
	}
	if r.Value != "" {
		text += " -> " + r.Value
	}
	return tool.Result{Text: text, Details: map[string]any{"selector": selector, "tag": r.Tag}}, nil
}

// navigate loads url and waits (bounded by loadWait) for readyState=complete.
// The last observed state is returned; a page that never finishes loading is
// a warning, not a failure — a slow SPA is still attachable.
func (tb *tab) navigate(ctx context.Context, url string) (string, error) {
	if _, err := tb.c.Call(ctx, "Page.navigate", map[string]any{"url": url}); err != nil {
		return "", err
	}
	wait, cancel := context.WithTimeout(ctx, loadWait)
	defer cancel()
	state := ""
	for {
		info, err := tb.info(wait)
		if err != nil {
			if wait.Err() != nil {
				return state, nil
			}
			return state, err
		}
		state = info.ReadyState
		if state == "complete" {
			return state, nil
		}
		select {
		case <-wait.Done():
			return state, nil
		case <-time.After(loadPoll):
		}
	}
}

// pageInfo is the page identity/load state read in one round trip.
type pageInfo struct {
	URL        string `json:"url"`
	Title      string `json:"title"`
	ReadyState string `json:"readyState"`
}

// pageInfoExpr reads location/title/readyState without touching the DOM body.
const pageInfoExpr = `(() => ({url: location.href, title: document.title || '', readyState: document.readyState}))()`

// snapshotExpr returns the page's visible text (innerText falls back to
// textContent for documents without layout).
const snapshotExpr = `(() => {
  const b = document.body;
  return {url: location.href, title: document.title || '', text: b ? (b.innerText || b.textContent || '') : ''};
})()`

// info reads the page's identity and load state.
func (tb *tab) info(ctx context.Context) (pageInfo, error) {
	out, err := tb.evaluate(ctx, pageInfoExpr)
	if err != nil {
		return pageInfo{}, err
	}
	if strings.TrimSpace(out) == "" {
		return pageInfo{}, errors.New("browser: the page returned no value for its state (it may have navigated mid-call); retry the op")
	}
	var info pageInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return pageInfo{}, fmt.Errorf("browser: page info: %w", err)
	}
	return info, nil
}

// clickExpr builds the page-side click. The selector travels as a JSON
// string literal, so a selector can never become page script.
//
// ponytail: this is a DOM-level click (synthetic .click()), not a real
// input event: it does not open file pickers, does not trigger
// user-activation-gated behavior, and cannot reach into a cross-origin
// iframe or a closed shadow root. Upgrade path: resolve the element's
// bounding box and dispatch Input.dispatchMouseEvent over CDP.
func clickExpr(selector string) string {
	return fmt.Sprintf(`(() => {
  const el = document.querySelector(%s);
  if (!el) return {ok: false, error: 'no element matches this selector'};
  el.scrollIntoView({block: 'center', inline: 'nearest'});
  el.click();
  return {ok: true, tag: (el.tagName || '').toLowerCase(), text: String((el.innerText || el.value || '')).slice(0, 120)};
})()`, jsonString(selector))
}

// typeExpr builds the page-side field write.
//
// ponytail: DOM-level typing — it sets value/textContent and dispatches
// input+change, which satisfies most frameworks (React reads the native
// setter, so a controlled input may need the native value descriptor).
// It does not produce real keystrokes. Upgrade path: focus the element and
// use Input.insertText / Input.dispatchKeyEvent.
func typeExpr(selector, text string) string {
	return fmt.Sprintf(`(() => {
  const el = document.querySelector(%s);
  if (!el) return {ok: false, error: 'no element matches this selector'};
  const v = %s;
  el.focus();
  if (el.isContentEditable) { el.textContent = v; } else { el.value = v; }
  el.dispatchEvent(new Event('input', {bubbles: true}));
  el.dispatchEvent(new Event('change', {bubbles: true}));
  const shown = (el.isContentEditable ? el.textContent : el.value);
  return {ok: true, tag: (el.tagName || '').toLowerCase(), value: String(shown === undefined ? '' : shown).slice(0, 120)};
})()`, jsonString(selector), jsonString(text))
}

// jsonString encodes s as a JavaScript-safe JSON string literal.
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil { // json.Marshal of a string cannot fail; keep the literal honest
		return `""`
	}
	return string(b)
}

// sanitizeText makes page text safe to put in the model's context: CRLF
// becomes LF, control characters the terminal would execute are dropped,
// trailing whitespace is trimmed, runs of blank lines fold to one, and the
// whole result is capped at max bytes on a line/rune boundary.
func sanitizeText(s string, max int) string {
	if max <= 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(min(len(s), max) + 64)
	blank := false
	lines := 0
	truncated := false
	for _, line := range strings.Split(s, "\n") {
		if b.Len() >= max {
			truncated = true
			break
		}
		line = strings.TrimRight(stripControl(line), " \t\r")
		if line == "" {
			if blank || lines == 0 {
				continue // leading/repeated blank line
			}
			blank = true
		} else {
			blank = false
		}
		if len(line)+1 > max-b.Len() {
			line = truncRunes(line, max-b.Len())
			truncated = true
		}
		b.WriteString(line)
		b.WriteByte('\n')
		lines++
		if truncated {
			break
		}
	}
	out := strings.TrimRight(b.String(), "\n")
	if truncated {
		out += fmt.Sprintf("\n… [snapshot truncated at %d chars; %d lines shown]", max, lines)
	}
	return out
}

// capText bounds one output value, noting the cut.
func capText(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return truncRunes(s, max) + fmt.Sprintf("\n… [truncated at %d chars, %d total]", max, len(s))
}

// truncRunes cuts s to at most n bytes without splitting a rune.
func truncRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// stripControl removes C0/DEL control characters (tab and LF excepted): a
// page can otherwise smuggle cursor escapes into the session transcript.
func stripControl(s string) string {
	if !strings.ContainsFunc(s, isControl) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if isControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isControl(r rune) bool { return (r < 0x20 && r != '\t') || r == 0x7f }

// autolaunch reports whether this tool may start a browser itself: an
// explicit browser.autolaunch: false, or no profile dir to launch one with,
// means attach-only and the unreachable-endpoint error names starting Chrome
// by hand.
func (t *Tool) autolaunch() bool {
	return t.Cfg.AutolaunchOn() && t.ProfileDir != ""
}
