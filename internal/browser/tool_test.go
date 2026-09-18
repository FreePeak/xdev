package browser

// Tool-level tests (M13 #50): every op's CDP request is asserted against the
// in-process fake, plus snapshot sanitization/caps, the missing-browser
// error, the timeout path, and the screenshot blob handoff.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/FreePeak/xdev/internal/session"
)

// toolResult mirrors the tool package's result, keeping the test file
// dependency-free.
type toolResult struct {
	Text    string
	IsError bool
	Details any
}

// newTestTool wires a tool against the fake endpoint with a temp blob store.
func newTestTool(t *testing.T, f *fakeCDP) (*Tool, *session.BlobStore) {
	t.Helper()
	blobs := session.NewBlobStore(t.TempDir())
	tl := NewTool(Settings{CDPURL: f.endpoint()}, blobs)
	t.Cleanup(func() { _ = tl.Close() })
	return tl, blobs
}

// exec runs one op and fails the test on a harness-level error.
func exec(t *testing.T, tl *Tool, in map[string]any) toolResult {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tl.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("Execute(%v): %v", in["op"], err)
	}
	return toolResult{Text: res.Text, IsError: res.IsError, Details: res.Details}
}

// mustJSONString marshals v for embedding in a fake CDP reply.
func mustJSONString(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// params decodes the recorded params of the last call to method.
func params(t *testing.T, f *fakeCDP, method string) map[string]any {
	t.Helper()
	raw, ok := f.lastCall(method)
	if !ok {
		t.Fatalf("no %s call was made", method)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("params of %s: %v", method, err)
	}
	return out
}

// evalReply builds a Runtime.evaluate reply carrying a JSON value. A JSON
// string is typed as V8 types it ("string"), so the client unquotes it.
func evalReply(value string) json.RawMessage {
	typ := "object"
	if strings.HasPrefix(value, `"`) {
		typ = "string"
	}
	return json.RawMessage(fmt.Sprintf(`{"result":{"type":%q,"value":%s}}`, typ, value))
}

// evalHandler answers Runtime.evaluate with one fixed JSON value and
// nothing else. Handlers are built per scenario so a test never mutates a
// variable the serving goroutine reads (the race detector catches that).
func evalHandler(value string) cdpHandler {
	return func(method string, _ json.RawMessage) (json.RawMessage, error) {
		if method != "Runtime.evaluate" {
			return nil, nil
		}
		return evalReply(value), nil
	}
}

// TestOpenAndNavigate asserts open discovers a page through /json/list,
// attaches, and only sends Page.navigate when a url was requested.
func TestOpenAndNavigate(t *testing.T) {
	f := newFakeCDP(t, func(method string, _ json.RawMessage) (json.RawMessage, error) {
		if method == "Runtime.evaluate" {
			return json.RawMessage(`{"result":{"type":"object","value":{"url":"https://example.com/","title":"Example","readyState":"complete"}}}`), nil
		}
		return nil, nil
	})
	tl, _ := newTestTool(t, f)

	res := exec(t, tl, map[string]any{"op": "open"})
	if res.IsError {
		t.Fatalf("open failed: %s", res.Text)
	}
	if !strings.Contains(res.Text, `tab "main" attached: https://example.com/ (Example, readyState=complete)`) {
		t.Errorf("open text = %q", res.Text)
	}
	if _, ok := f.lastCall("Page.navigate"); ok {
		t.Error("open without url must not navigate")
	}
	if f.listPageHits() != 1 {
		t.Errorf("/json/list hits = %d, want 1", f.listPageHits())
	}

	// A second Chrome tab is what makes a second handle possible: this
	// session never reuses a page another handle already drives.
	f.addPage("t2", "about:blank", "Second")
	res = exec(t, tl, map[string]any{"op": "open", "name": "docs", "url": "https://example.com/docs"})
	if res.IsError {
		t.Fatalf("open(url) failed: %s", res.Text)
	}
	if nav := params(t, f, "Page.navigate"); nav["url"] != "https://example.com/docs" {
		t.Errorf("Page.navigate params = %v", nav)
	}
	if !strings.Contains(res.Text, `tab "docs" attached`) {
		t.Errorf("open(url) text = %q", res.Text)
	}
}

// TestOpenWaitsForLoad proves a slow page is waited for, not failed.
func TestOpenWaitsForLoad(t *testing.T) {
	states := []string{"loading", "loading", "complete"}
	var calls int
	f := newFakeCDP(t, func(method string, _ json.RawMessage) (json.RawMessage, error) {
		if method == "Runtime.evaluate" {
			state := states[min(calls, len(states)-1)]
			calls++
			return json.RawMessage(fmt.Sprintf(`{"result":{"type":"object","value":{"url":"https://slow/","title":"Slow","readyState":%q}}}`, state)), nil
		}
		return nil, nil
	})
	tl, _ := newTestTool(t, f)
	res := exec(t, tl, map[string]any{"op": "open", "url": "https://slow/"})
	if res.IsError {
		t.Fatalf("open failed: %s", res.Text)
	}
	if !strings.Contains(res.Text, "readyState=complete") {
		t.Errorf("open did not wait for complete: %q", res.Text)
	}
}

// TestSnapshotSanitizesAndCaps asserts the snapshot request shape and that
// page text arrives control-free, blank-folded, and capped.
func TestSnapshotSanitizesAndCaps(t *testing.T) {
	page := "line one  \r\n\x1b[31mred\x1b[0m\n\n\n\ntail line\n" + strings.Repeat("x", 500)
	f := newFakeCDP(t, func(method string, _ json.RawMessage) (json.RawMessage, error) {
		if method == "Runtime.evaluate" {
			return evalReply(mustJSONString(t, map[string]any{
				"url": "https://example.com/", "title": "T", "text": page,
			})), nil
		}
		return nil, nil
	})
	tl, _ := newTestTool(t, f)

	res := exec(t, tl, map[string]any{"op": "snapshot"})
	if res.IsError {
		t.Fatalf("snapshot failed: %s", res.Text)
	}
	if !strings.Contains(res.Text, "URL: https://example.com/\nTITLE: T\n---\n") {
		t.Errorf("snapshot header = %q", res.Text)
	}
	if strings.Contains(res.Text, "\x1b") {
		t.Error("snapshot leaked an escape character")
	}
	if strings.Contains(res.Text, "\r") {
		t.Error("snapshot leaked a CR")
	}
	if !strings.Contains(res.Text, "red") || !strings.Contains(res.Text, "tail line") {
		t.Error("snapshot dropped sanitized content")
	}
	if strings.Contains(res.Text, "\n\n\n") {
		t.Error("snapshot kept a blank-line run")
	}
	if strings.Contains(res.Text, "line one  \n") {
		t.Error("snapshot kept trailing spaces")
	}

	ev := params(t, f, "Runtime.evaluate")
	if ev["returnByValue"] != true {
		t.Errorf("Runtime.evaluate params = %v, want returnByValue", ev)
	}
	if expr, _ := ev["expression"].(string); !strings.Contains(expr, "document.body") {
		t.Errorf("snapshot expression = %q", expr)
	}

	// maxChars caps the body and says so.
	res = exec(t, tl, map[string]any{"op": "snapshot", "maxChars": 40})
	if !strings.Contains(res.Text, "snapshot truncated at 40 chars") {
		t.Errorf("capped snapshot = %q", res.Text)
	}
	if strings.Contains(res.Text, strings.Repeat("x", 60)) {
		t.Errorf("capped snapshot kept the body tail: %q", res.Text)
	}
}

// TestClick asserts the click op's expression carries the selector and that
// a missing element is an actionable failure.
func TestClick(t *testing.T) {
	ok := evalHandler(`{"ok":true,"tag":"button","text":"Sign in"}`)
	notFound := evalHandler(`{"ok":false,"error":"no element matches this selector"}`)
	f := newFakeCDP(t, ok)
	tl, _ := newTestTool(t, f)

	res := exec(t, tl, map[string]any{"op": "click", "selector": `button[type="submit"]`})
	if res.IsError {
		t.Fatalf("click failed: %s", res.Text)
	}
	if !strings.Contains(res.Text, "clicked") || !strings.Contains(res.Text, "<button>") {
		t.Errorf("click text = %q", res.Text)
	}
	expr, _ := params(t, f, "Runtime.evaluate")["expression"].(string)
	if !strings.Contains(expr, `"button[type=\"submit\"]"`) {
		t.Errorf("click expression does not carry the selector as a string literal: %s", expr)
	}
	if !strings.Contains(expr, "document.querySelector") || !strings.Contains(expr, ".click()") {
		t.Errorf("click expression = %s", expr)
	}

	f.setHandler(notFound)
	res = exec(t, tl, map[string]any{"op": "click", "selector": "button"})
	if !res.IsError || !strings.Contains(res.Text, "no element matches") || !strings.Contains(res.Text, "button") {
		t.Errorf("missing-element click = %+v", res)
	}

	res = exec(t, tl, map[string]any{"op": "click"})
	if !res.IsError || !strings.Contains(res.Text, "selector is required") {
		t.Errorf("click without selector = %+v", res)
	}
}

// TestType asserts the typed text and selector reach the page as literals.
func TestType(t *testing.T) {
	f := newFakeCDP(t, func(method string, _ json.RawMessage) (json.RawMessage, error) {
		if method == "Runtime.evaluate" {
			return evalReply(`{"ok":true,"tag":"input","value":"hello \"world\""}`), nil
		}
		return nil, nil
	})
	tl, _ := newTestTool(t, f)

	res := exec(t, tl, map[string]any{"op": "type", "selector": "#q", "text": `hello "world"`})
	if res.IsError {
		t.Fatalf("type failed: %s", res.Text)
	}
	if !strings.Contains(res.Text, `typed into #q (<input>) -> hello "world"`) {
		t.Errorf("type text = %q", res.Text)
	}
	expr, _ := params(t, f, "Runtime.evaluate")["expression"].(string)
	if !strings.Contains(expr, `"#q"`) || !strings.Contains(expr, `"hello \"world\""`) {
		t.Errorf("type expression = %s", expr)
	}
	if !strings.Contains(expr, "dispatchEvent") {
		t.Errorf("type expression does not fire input/change: %s", expr)
	}
}

// TestEval asserts the eval request shape, value rendering, exception
// surfacing, and output caps.
func TestEval(t *testing.T) {
	long := strings.Repeat("y", 9000)
	f := newFakeCDP(t, evalHandler(`"42"`)) // JSON-encoded string value "42"
	tl, _ := newTestTool(t, f)

	res := exec(t, tl, map[string]any{"op": "eval", "expression": "1 + 41"})
	if res.IsError || res.Text != "42" {
		t.Fatalf("eval = %+v", res)
	}
	ev := params(t, f, "Runtime.evaluate")
	if ev["expression"] != "1 + 41" {
		t.Errorf("eval expression = %v", ev["expression"])
	}
	if ev["returnByValue"] != true || ev["awaitPromise"] != true {
		t.Errorf("eval params = %v, want returnByValue+awaitPromise", ev)
	}

	f.setHandler(evalHandler(`{"a":1}`))
	if res = exec(t, tl, map[string]any{"op": "eval", "expression": "({a:1})"}); res.Text != `{"a":1}` {
		t.Errorf("object eval = %q", res.Text)
	}

	f.setHandler(func(method string, _ json.RawMessage) (json.RawMessage, error) {
		if method == "Runtime.evaluate" {
			return json.RawMessage(`{"result":{"type":"object"},"exceptionDetails":{"text":"Uncaught","exception":{"description":"ReferenceError: nope is not defined"}}}`), nil
		}
		return nil, nil
	})
	res = exec(t, tl, map[string]any{"op": "eval", "expression": "nope()"})
	if !res.IsError || !strings.Contains(res.Text, "ReferenceError: nope is not defined") {
		t.Errorf("exception eval = %+v", res)
	}

	f.setHandler(evalHandler(fmt.Sprintf("%q", long)))
	res = exec(t, tl, map[string]any{"op": "eval", "expression": "x"})
	if !strings.Contains(res.Text, "truncated at 8000 chars") {
		t.Errorf("default eval cap missing (%d chars)", len(res.Text))
	}
	res = exec(t, tl, map[string]any{"op": "eval", "expression": "x", "maxChars": 50})
	if !strings.Contains(res.Text, "truncated at 50 chars") {
		t.Errorf("maxChars eval = %q", res.Text)
	}

	f.setHandler(func(method string, _ json.RawMessage) (json.RawMessage, error) {
		if method == "Runtime.evaluate" {
			return nil, fmt.Errorf("Object couldn't be returned by value")
		}
		return nil, nil
	})
	res = exec(t, tl, map[string]any{"op": "eval", "expression": "document"})
	if !res.IsError || !strings.Contains(res.Text, "JSON.stringify") {
		t.Errorf("by-value rejection = %+v", res)
	}

	res = exec(t, tl, map[string]any{"op": "eval"})
	if !res.IsError || !strings.Contains(res.Text, "expression") {
		t.Errorf("eval without expression = %+v", res)
	}
}

// TestScreenshotStoresBlob asserts the capture request, the PNG validation,
// and that the returned path really holds the image.
func TestScreenshotStoresBlob(t *testing.T) {
	png := append(append([]byte(nil), pngMagic...), []byte("fake-png-body")...)
	f := newFakeCDP(t, func(method string, _ json.RawMessage) (json.RawMessage, error) {
		if method == "Page.captureScreenshot" {
			return json.RawMessage(fmt.Sprintf(`{"data":%q}`, base64.StdEncoding.EncodeToString(png))), nil
		}
		return nil, nil
	})
	tl, blobs := newTestTool(t, f)

	res := exec(t, tl, map[string]any{"op": "screenshot"})
	if res.IsError {
		t.Fatalf("screenshot failed: %s", res.Text)
	}
	if p := params(t, f, "Page.captureScreenshot"); p["format"] != "png" {
		t.Errorf("captureScreenshot params = %v", p)
	}
	details, ok := res.Details.(map[string]any)
	if !ok {
		t.Fatalf("screenshot details = %#v", res.Details)
	}
	path, _ := details["path"].(string)
	if path == "" || !strings.HasPrefix(path, blobs.Root()) {
		t.Fatalf("screenshot path = %q (root %s)", path, blobs.Root())
	}
	if !strings.Contains(res.Text, path) {
		t.Errorf("screenshot text does not name the path: %q", res.Text)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the stored screenshot: %v", err)
	}
	if string(got) != string(png) {
		t.Errorf("stored %d bytes, want the captured %d", len(got), len(png))
	}
	if ref, _ := details["blob"].(string); !strings.HasPrefix(ref, session.BlobRefPrefix) {
		t.Errorf("screenshot blob ref = %q", ref)
	}
}

// TestScreenshotRejectsNonPNGAndOversize covers the two screenshot guards.
func TestScreenshotRejectsNonPNGAndOversize(t *testing.T) {
	shotHandler := func(data string) cdpHandler {
		return func(method string, _ json.RawMessage) (json.RawMessage, error) {
			if method == "Page.captureScreenshot" {
				return json.RawMessage(fmt.Sprintf(`{"data":%q}`, data)), nil
			}
			return nil, nil
		}
	}
	f := newFakeCDP(t, shotHandler(base64.StdEncoding.EncodeToString([]byte("not a png"))))
	tl, _ := newTestTool(t, f)
	res := exec(t, tl, map[string]any{"op": "screenshot"})
	if !res.IsError || !strings.Contains(res.Text, "not a PNG") {
		t.Errorf("non-PNG screenshot = %+v", res)
	}

	// The cap is checked before decoding, so this string is all the fake needs.
	f.setHandler(shotHandler(strings.Repeat("A", base64.StdEncoding.EncodedLen(maxScreenshotBytes)+4)))
	res = exec(t, tl, map[string]any{"op": "screenshot"})
	if !res.IsError || !strings.Contains(res.Text, "exceeds the") {
		t.Errorf("oversize screenshot = %+v", res)
	}
}

// TestClose covers handle release: by name, by all, and for an unknown name.
func TestClose(t *testing.T) {
	f := newFakeCDP(t, func(method string, _ json.RawMessage) (json.RawMessage, error) {
		if method == "Runtime.evaluate" {
			return json.RawMessage(`{"result":{"type":"string","value":"ok"}}`), nil
		}
		return nil, nil
	})
	tl, _ := newTestTool(t, f)

	if res := exec(t, tl, map[string]any{"op": "close", "name": "ghost"}); res.IsError || !strings.Contains(res.Text, `no tab named "ghost"`) {
		t.Errorf("close(ghost) = %+v", res)
	}

	if res := exec(t, tl, map[string]any{"op": "eval", "expression": "1"}); res.IsError {
		t.Fatalf("eval failed: %s", res.Text)
	}
	if res := exec(t, tl, map[string]any{"op": "close"}); res.IsError || !strings.Contains(res.Text, `released tab "main"`) {
		t.Errorf("close = %+v", res)
	}
	// Releasing the handle means the next op attaches again.
	if res := exec(t, tl, map[string]any{"op": "close"}); !strings.Contains(res.Text, "no tab named") {
		t.Errorf("second close = %+v", res)
	}
	if hits := f.listPageHits(); hits != 1 {
		t.Errorf("/json/list hits = %d, want 1 before re-attach", hits)
	}
	if res := exec(t, tl, map[string]any{"op": "eval", "expression": "1"}); res.IsError {
		t.Fatalf("eval after close failed: %s", res.Text)
	}
	if hits := f.listPageHits(); hits != 2 {
		t.Errorf("/json/list hits = %d, want 2 after re-attach", hits)
	}

	if res := exec(t, tl, map[string]any{"op": "close", "all": true}); !strings.Contains(res.Text, "released 1 tab handle(s)") {
		t.Errorf("close all = %+v", res)
	}
}

// TestMissingBrowser asserts the actionable error when nothing is listening.
func TestMissingBrowser(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	srv.Close() // the port is now free: nothing to attach to

	tl := NewTool(Settings{CDPURL: addr}, session.NewBlobStore(t.TempDir()))
	defer tl.Close()
	res := exec(t, tl, map[string]any{"op": "open"})
	if !res.IsError {
		t.Fatalf("open against a dead endpoint = %+v", res)
	}
	for _, want := range []string{"no Chrome DevTools endpoint", addr, "--remote-debugging-port=9222", "browser.cdpUrl"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("missing-browser error lacks %q: %s", want, res.Text)
		}
	}
}

// TestNoPageTarget asserts the error when the endpoint is up but has no page.
func TestNoPageTarget(t *testing.T) {
	f := newFakeCDP(t, nil)
	f.mu.Lock()
	f.pages = nil
	f.mu.Unlock()
	tl, _ := newTestTool(t, f)
	res := exec(t, tl, map[string]any{"op": "open"})
	if !res.IsError || !strings.Contains(res.Text, "no page target") {
		t.Errorf("open without pages = %+v", res)
	}
}

// TestAllPagesClaimed asserts the second handle cannot take a page another
// handle already drives (attach-only mode cannot create one).
func TestAllPagesClaimed(t *testing.T) {
	f := newFakeCDP(t, func(method string, _ json.RawMessage) (json.RawMessage, error) {
		if method == "Runtime.evaluate" {
			return json.RawMessage(`{"result":{"type":"object","value":{"url":"about:blank","title":"Fake","readyState":"complete"}}}`), nil
		}
		return nil, nil
	})
	tl, _ := newTestTool(t, f)
	if res := exec(t, tl, map[string]any{"op": "open"}); res.IsError {
		t.Fatalf("first open failed: %s", res.Text)
	}
	res := exec(t, tl, map[string]any{"op": "open", "name": "second"})
	if !res.IsError || !strings.Contains(res.Text, "already driven by this session") {
		t.Errorf("second open = %+v", res)
	}
}

// TestOpTimeout asserts a hung CDP command fails within the op budget and
// names the command that hung.
func TestOpTimeout(t *testing.T) {
	f := newFakeCDP(t, nil)
	f.hang("Runtime.evaluate")
	tl, _ := newTestTool(t, f)

	start := time.Now()
	res := exec(t, tl, map[string]any{"op": "snapshot", "timeout": 1})
	elapsed := time.Since(start)
	if !res.IsError || !strings.Contains(res.Text, "deadline exceeded") {
		t.Fatalf("hung snapshot = %+v", res)
	}
	if !strings.Contains(res.Text, "Runtime.evaluate") {
		t.Errorf("timeout error does not name the command: %s", res.Text)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("snapshot took %s, want it bounded by the op timeout", elapsed)
	}
}

// TestUnknownOpAndArgs asserts the dispatch surface.
func TestUnknownOpAndArgs(t *testing.T) {
	tl, _ := newTestTool(t, newFakeCDP(t, nil))
	if res := exec(t, tl, map[string]any{"op": "fly"}); !res.IsError || !strings.Contains(res.Text, "unknown op") {
		t.Errorf("unknown op = %+v", res)
	}
	if res := exec(t, tl, map[string]any{}); !res.IsError || !strings.Contains(res.Text, "op is required") {
		t.Errorf("missing op = %+v", res)
	}
	res, err := tl.Execute(context.Background(), json.RawMessage(`"nope"`))
	if err != nil || !res.IsError || !strings.Contains(res.Text, "invalid arguments") {
		t.Errorf("malformed args = %+v (err %v)", res, err)
	}
}

// TestTimeoutClamp pins the per-op timeout resolution.
func TestTimeoutClamp(t *testing.T) {
	tl := NewTool(Settings{}, session.NewBlobStore(t.TempDir()))
	cases := []struct {
		cfg  int
		arg  float64
		want time.Duration
	}{
		{cfg: 0, arg: 0, want: DefaultTimeout},
		{cfg: 0, arg: 10, want: 10 * time.Second},
		{cfg: 45, arg: 0, want: 45 * time.Second},
		{cfg: 45, arg: 5, want: 5 * time.Second},
		{cfg: 0, arg: 0.001, want: MinTimeout},
		{cfg: 0, arg: 9999, want: MaxTimeout},
	}
	for _, tc := range cases {
		tl.Cfg.Timeout = tc.cfg
		if got := tl.opTimeout(tc.arg); got != tc.want {
			t.Errorf("opTimeout(cfg=%d, arg=%v) = %s, want %s", tc.cfg, tc.arg, got, tc.want)
		}
	}
}

// TestSanitizeText pins the sanitizer's rules.
func TestSanitizeText(t *testing.T) {
	in := "  \r\nfirst \t\n\x00\x07second\n\n\n\nthird\n" + strings.Repeat("x", 50)
	want := "first\nsecond\n\nthird\n" + strings.Repeat("x", 50)
	if got := sanitizeText(in, 1000); got != want {
		t.Errorf("sanitizeText = %q, want %q", got, want)
	}

	// The cap cuts on a rune boundary and announces itself.
	got := sanitizeText("héllo wörld and more", 8)
	if !strings.HasSuffix(got, "[snapshot truncated at 8 chars; 1 lines shown]") {
		t.Errorf("capped = %q", got)
	}
	if !strings.HasPrefix(got, "héllo w") {
		t.Errorf("capped prefix = %q", got)
	}
	got = sanitizeText(strings.Repeat("é", 10), 5)
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Errorf("sanitizeText split a rune: %q", got)
	}
	if sanitizeText("anything", 0) != "" {
		t.Error("max<=0 must produce no text")
	}
	if got := sanitizeText("plain", 100); got != "plain" {
		t.Errorf("sanitizeText(plain) = %q", got)
	}
}

// TestEndpointResolution pins the config → env → default precedence.
func TestEndpointResolution(t *testing.T) {
	t.Setenv(envEndpoint, "")
	if got := resolveEndpoint(""); got != defaultEndpoint {
		t.Errorf("default endpoint = %q", got)
	}
	if got := resolveEndpoint("127.0.0.1:9333"); got != "http://127.0.0.1:9333" {
		t.Errorf("scheme-less endpoint = %q", got)
	}
	if got := resolveEndpoint("http://host:9222/"); got != "http://host:9222" {
		t.Errorf("trailing slash = %q", got)
	}
	t.Setenv(envEndpoint, "http://env:9222")
	if got := resolveEndpoint(""); got != "http://env:9222" {
		t.Errorf("env endpoint = %q", got)
	}
	if got := resolveEndpoint("http://cfg:9222"); got != "http://cfg:9222" {
		t.Errorf("configured endpoint must win: %q", got)
	}
	if got := discoveryURL("http://h:9222"); got != "http://h:9222/json/list" {
		t.Errorf("discovery url = %q", got)
	}
	if got := discoveryURL("http://h:9222/json"); got != "http://h:9222/json" {
		t.Errorf("explicit /json must be kept: %q", got)
	}
}

// TestPickPageSkipsClaimedAndNonPages pins target selection.
func TestPickPageSkipsClaimedAndNonPages(t *testing.T) {
	targets := []Target{
		{ID: "sw", Type: "service_worker", WebSocketDebuggerURL: "ws://x/sw"},
		{ID: "no-ws", Type: "page"},
		{ID: "t1", Type: "page", WebSocketDebuggerURL: "ws://x/t1"},
		{ID: "t2", Type: "page", WebSocketDebuggerURL: "ws://x/t2"},
	}
	got, ok := pickPage(targets, map[string]string{"t1": "main"})
	if !ok || got.ID != "t2" {
		t.Fatalf("pickPage = %+v (ok=%v), want t2", got, ok)
	}
	if _, ok := pickPage(targets, map[string]string{"t1": "main", "t2": "docs"}); ok {
		t.Error("pickPage returned a target when every page was claimed")
	}
}
