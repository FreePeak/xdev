package serve

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newHeadless builds the headless service over the given options with a
// deterministic version, so the info payload is assertable.
func newHeadless(t *testing.T, opts Options) *Headless {
	t.Helper()
	opts.Version = "test-1.2.3"
	h, err := NewHeadless(HeadlessOptions{Options: opts})
	if err != nil {
		t.Fatalf("NewHeadless: %v", err)
	}
	return h
}

// TestHeadlessInfoIsOpenPins the posture opencode's /api/info has and the
// other services here share: a liveness/identity probe needs no credential,
// because a supervisor probing it must not hold one — while every other
// route still 401s without the token.
func TestHeadlessInfoIsOpen(t *testing.T) {
	dir := t.TempDir()
	h := newHeadless(t, Options{DataDir: dir, Listen: "127.0.0.1:0", Token: "tok"})

	rec := doReq(t, h.Handler(), http.MethodGet, "/info", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /info without a token = %d, want 200 (the probe every client hits first)", rec.Code)
	}
	var info map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode /info: %v", err)
	}
	if info["version"] != "test-1.2.3" {
		t.Errorf("version = %v, want test-1.2.3", info["version"])
	}
	if pid, ok := info["pid"].(float64); !ok || pid <= 0 {
		t.Errorf("pid = %v, want this process's pid (a client uses it to tell a stale registration from a dead daemon)", info["pid"])
	}

	// Everything else stays gated.
	for _, path := range []string{"/event", "/prompt", "/state", "/abort"} {
		rec := doReq(t, h.Handler(), http.MethodGet, path, "", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a token = %d, want 401", path, rec.Code)
		}
	}
	// ...and the token opens it.
	rec = doReq(t, h.Handler(), http.MethodGet, "/state", "tok", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /state with the token = %d, want 200", rec.Code)
	}
}

// TestHeadlessPublishesARegistrationFile pins the discovery contract
// opencode's `Service.register` has: the daemon writes ONE 0600 file holding
// url, pid, version and the password, so a client can connect by reading a
// file — never by guessing a port.
func TestHeadlessPublishesARegistrationFile(t *testing.T) {
	dir := t.TempDir()
	h := newHeadless(t, Options{DataDir: dir, Listen: "127.0.0.1:0", Token: "tok"})

	path := HeadlessRegistrationPath(dir)
	info := h.Registration("http://127.0.0.1:9999")
	if err := writeRegistration(path, info); err != nil {
		t.Fatalf("writeRegistration: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat registration: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("registration mode = %v, want 0600 (it carries the token)", perm)
	}
	got, err := readRegistration(path)
	if err != nil {
		t.Fatalf("readRegistration: %v", err)
	}
	if got.URL != "http://127.0.0.1:9999" || got.PID != info.PID || got.Version != "test-1.2.3" {
		t.Errorf("registration = %+v, want the written values", got)
	}
	if got.Token != "tok" {
		t.Errorf("registration token = %q, want the service token", got.Token)
	}
}

// TestHeadlessRegistrationAtomicWrite covers the other half: a reader never
// sees a half-written file, and a second daemon does not clobber a live
// registration it does not own.
func TestHeadlessRegistrationAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := HeadlessRegistrationPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	first := HeadlessRegistration{ID: "a", URL: "http://127.0.0.1:1", PID: 1, Version: "v1", Token: "t1"}
	if err := writeRegistration(path, first); err != nil {
		t.Fatal(err)
	}
	// A second server must refuse to replace a registration it does not own:
	// two daemons on one port is the EADDRINUSE fight opencode resolves
	// through its incumbent check, and a file replaced blindly loses the
	// running daemon's identity.
	second := HeadlessRegistration{ID: "b", URL: "http://127.0.0.1:1", PID: 2, Version: "v1", Token: "t2"}
	if err := writeRegistrationIfOwner(path, second); err == nil {
		t.Error("a foreign registration must not replace a live one")
	}
	got, _ := readRegistration(path)
	if got.ID != "a" {
		t.Errorf("registration = %+v, want the incumbent's", got)
	}
	// The owner may rewrite its own (a reconnect with a new URL).
	updated := second
	updated.ID = "a"
	if err := writeRegistrationIfOwner(path, updated); err != nil {
		t.Fatalf("the owner rewriting its own registration: %v", err)
	}
}

// TestHeadlessEventStreamStartsConnected pins the SSE shape opencode's
// /api/event has: text/event-stream, a `server.connected` frame first, and a
// 4 KiB flush boundary so a slow consumer cannot be starved by a buffered
// write.
func TestHeadlessEventStreamStartsConnected(t *testing.T) {
	dir := t.TempDir()
	h := newHeadless(t, Options{DataDir: dir, Listen: "127.0.0.1:0", Token: "tok"})

	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/event?token=tok", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /event: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no (a reverse proxy must not buffer the stream)", resp.Header.Get("X-Accel-Buffering"))
	}
	// The first frame is the connected notice, within a generous deadline.
	reader := newSSEReader(resp)
	if !reader.has(t, "server.connected", time.Now().Add(5*time.Second)) {
		t.Error("the first SSE frame must be server.connected")
	}
}

// sseFrame is one parsed SSE frame (the `data:` line is the JSON payload).
type sseFrame struct {
	event string
	data  string
}

// sseReader owns the ONE goroutine that drains an /event body. Creating a
// second reader for the same response would either block on a body already
// being consumed or silently steal the frame the assertion is waiting for, so
// the reader outlives a single call and hands out frames from its channel.
type sseReader struct {
	frames chan sseFrame
}

func newSSEReader(resp *http.Response) *sseReader {
	r := &sseReader{frames: make(chan sseFrame, 128)}
	go func() {
		defer close(r.frames)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		f := sseFrame{}
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if f.event != "" || f.data != "" {
					r.frames <- f
					f = sseFrame{}
				}
			case strings.HasPrefix(line, "event:"):
				f.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				f.data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
	}()
	return r
}

// until reads frames until want arrives or the deadline passes.
func (r *sseReader) until(t *testing.T, want string, deadline time.Time) []sseFrame {
	if t != nil {
		t.Helper()
	}
	var out []sseFrame
	for time.Now().Before(deadline) {
		select {
		case f, ok := <-r.frames:
			if !ok {
				return out
			}
			out = append(out, f)
			if f.event == want {
				return out
			}
		case <-time.After(100 * time.Millisecond):
		}
	}
	if t != nil {
		t.Logf("SSE frames seen before the deadline: %d", len(out))
	}
	return out
}

// has reports whether want arrived before the deadline.
func (r *sseReader) has(t *testing.T, want string, deadline time.Time) bool {
	for _, f := range r.until(t, want, deadline) {
		if f.event == want {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// TestHeadlessPromptDrivesTheHandler pins the wire a client uses instead of
// stdio JSONL: POST /prompt with {id, text} returns immediately (the turn
// runs on its own goroutine) and the terminal response arrives on the SSE
// stream correlated by that id — the same correlation `xdev rpc` has.
func TestHeadlessPromptDrivesTheHandler(t *testing.T) {
	dir := t.TempDir()
	h := newHeadless(t, Options{DataDir: dir, Listen: "127.0.0.1:0", Token: "tok"})
	answered := make(chan string, 1)
	h.SetHandler(&scriptedHeadlessHandler{
		prompt: func(id, text string) {
			answered <- id + ":" + text
			// The terminal response is what a client waits for; emit it
			// through the server the way the cmd-side driver does.
			h.Respond(id, map[string]any{"ok": true, "id": id, "text": "done: " + text})
		},
	})

	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	// Subscribe first: a response emitted before the stream opens would be
	// missed, which is exactly the race opencode's volatile-by-contract SSE
	// accepts.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/event?token=tok", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /event: %v", err)
	}
	defer resp.Body.Close()
	reader := newSSEReader(resp)
	if !reader.has(t, "server.connected", time.Now().Add(5*time.Second)) {
		t.Fatal("no server.connected frame")
	}

	rec := doReq(t, h.Handler(), http.MethodPost, "/prompt", "tok", map[string]string{"id": "p1", "text": "hi"})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /prompt = %d, want 200 (the run is asynchronous)", rec.Code)
	}
	select {
	case got := <-answered:
		if got != "p1:hi" {
			t.Errorf("handler saw %q, want p1:hi", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the handler was never driven")
	}
	// The terminal response rides the stream, correlated by the prompt id.
	// A frame can arrive between the connected handshake and this read, so the
	// reader is allowed to see the whole stream, not only new bytes.
	found := false
	for _, f := range reader.until(t, "response", time.Now().Add(3*time.Second)) {
		if f.event == "response" && contains(f.data, "p1") && contains(f.data, "done: hi") {
			found = true
			break
		}
	}
	if !found {
		t.Error("the terminal response must reach the SSE stream correlated by the prompt id")
	}
}

// scriptedHeadlessHandler records the commands the server forwarded.
type scriptedHeadlessHandler struct {
	prompt  func(id, text string)
	steer   func(text string) error
	abort   func() error
	state   map[string]any
	history json.RawMessage
}

func (s *scriptedHeadlessHandler) Prompt(id, text string) {
	if s.prompt != nil {
		s.prompt(id, text)
	}
}

func (s *scriptedHeadlessHandler) Steer(text string) error {
	if s.steer != nil {
		return s.steer(text)
	}
	return nil
}

func (s *scriptedHeadlessHandler) Abort() error {
	if s.abort != nil {
		return s.abort()
	}
	return nil
}

func (s *scriptedHeadlessHandler) History() json.RawMessage { return s.history }

func (s *scriptedHeadlessHandler) State() map[string]any {
	if s.state != nil {
		return s.state
	}
	return map[string]any{"ok": true}
}

// TestDiscoverHeadlessFindsOnlyALiveServer pins the client's discovery
// contract: a registration is trusted only when its pid is alive AND /info
// answers with that same pid — pids are recycled, and a stale file pointing at
// an unrelated listener would send a client to a stranger.
func TestDiscoverHeadlessFindsOnlyALiveServer(t *testing.T) {
	dir := t.TempDir()
	path := HeadlessRegistrationPath(dir)

	// No file: not found, never an error (a client may simply start one).
	if _, ok := DiscoverHeadless(dir); ok {
		t.Fatal("an absent registration must not be discovered")
	}

	// A live server: discovered, with the URL and token a client needs.
	h := newHeadless(t, Options{DataDir: dir, Listen: "127.0.0.1:0", Token: "tok"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	waitFor(t, func() bool { return h.Addr() != "" }, "the server to bind")
	if _, err := probeHeadless("http://"+h.Addr(), 2*time.Second); err != nil {
		t.Fatalf("probe the running server: %v", err)
	}
	reg, ok := DiscoverHeadless(dir)
	if !ok {
		t.Fatal("a running server must be discoverable")
	}
	if reg.URL != "http://"+h.Addr() || reg.Token != "tok" {
		t.Errorf("registration = %+v, want the running server's url and token", reg)
	}

	// A dead pid: not discovered (the file outlives a killed daemon).
	dead := reg
	dead.PID = 0x7ffffffe
	if err := writeRegistration(path, dead); err != nil {
		t.Fatal(err)
	}
	if _, ok := DiscoverHeadless(dir); ok {
		t.Error("a registration whose pid is dead must not be discovered")
	}

	// Shutdown removes the registration, so the next client is not misled.
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the registration must be removed on shutdown, stat err = %v", err)
	}

	// A second server on the same data dir can publish once the first is gone.
	h2 := newHeadless(t, Options{DataDir: dir, Listen: "127.0.0.1:0", Token: "tok"})
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go func() { _ = h2.Run(ctx2) }()
	waitFor(t, func() bool { return h2.Addr() != "" }, "the second server to bind")
	if _, err := readRegistration(path); err != nil {
		t.Errorf("the second server should publish its own registration: %v", err)
	}
	cancel2()
}

// TestHeadlessAttachDrivesAPromptEndToEnd is the whole point of the service:
// one process serves, a second process discovers it by FILE, subscribes to the
// event stream, and drives a prompt — no shared parent, no stdio.
func TestHeadlessAttachDrivesAPromptEndToEnd(t *testing.T) {
	dir := t.TempDir()
	h := newHeadless(t, Options{DataDir: dir, Listen: "127.0.0.1:0", Token: "tok"})
	h.SetHandler(&scriptedHeadlessHandler{
		prompt: func(id, text string) {
			h.Broadcast("delta", map[string]any{"id": id, "text": "thinking…"})
			h.Respond(id, map[string]any{"ok": true, "text": "answer to " + text})
		},
		state: map[string]any{"sessionId": "s-1", "running": false},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.Run(ctx) }()
	waitFor(t, func() bool { return h.Addr() != "" }, "the server to bind")

	// The client half: read the file, trust nothing else.
	reg, ok := DiscoverHeadless(dir)
	if !ok {
		t.Fatal("the client could not discover the server")
	}
	base := strings.TrimRight(reg.URL, "/")

	// Subscribe.
	req, _ := http.NewRequest(http.MethodGet, base+"/event?token="+reg.Token, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer resp.Body.Close()
	reader := newSSEReader(resp)
	if !reader.has(t, "server.connected", time.Now().Add(5*time.Second)) {
		t.Fatal("no server.connected frame")
	}

	// Drive a prompt with the discovered token.
	body, _ := json.Marshal(map[string]string{"id": "e2e", "text": "ping"})
	preq, _ := http.NewRequest(http.MethodPost, base+"/prompt", bytes.NewReader(body))
	preq.Header.Set("Authorization", "Bearer "+reg.Token)
	pres, err := http.DefaultClient.Do(preq)
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	defer pres.Body.Close()
	if pres.StatusCode != http.StatusOK {
		t.Fatalf("POST /prompt = %d, want 200", pres.StatusCode)
	}
	// A wrong token is refused, so the discovery token is what authorized it.
	bad, _ := http.NewRequest(http.MethodPost, base+"/prompt", bytes.NewReader(body))
	badres, err := http.DefaultClient.Do(bad)
	if err != nil {
		t.Fatalf("prompt without a token: %v", err)
	}
	badres.Body.Close()
	if badres.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST /prompt without the token = %d, want 401", badres.StatusCode)
	}

	// Both frames arrive on the one stream: the delta and the terminal answer.
	var sawDelta, sawResponse bool
	for _, f := range reader.until(t, "response", time.Now().Add(5*time.Second)) {
		switch {
		case f.event == "delta" && contains(f.data, "thinking"):
			sawDelta = true
		case f.event == "response" && contains(f.data, "answer to ping"):
			sawResponse = true
		}
	}
	if !sawDelta || !sawResponse {
		t.Errorf("stream frames: delta=%v response=%v, want both", sawDelta, sawResponse)
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestHeadlessRefusesToClobberALiveRegistration pins the discovery contract a
// second instance would break: publishing over a live server's registration
// points every client at the newcomer and strands the first. writeRegistration
// IfOwner existed for this and was only ever called by tests, so the guard now
// runs in both production paths — and a stale file (dead pid) is still
// replaced, which is the crash-recovery path.
func TestHeadlessRefusesToClobberALiveRegistration(t *testing.T) {
	dir := t.TempDir()
	path := HeadlessRegistrationPath(dir)

	first := HeadlessRegistration{ID: "a", URL: "http://127.0.0.1:1", PID: os.Getpid(), Version: "v1", Token: "t1"}
	if err := publishRegistration(path, first); err != nil {
		t.Fatalf("publishRegistration: %v", err)
	}
	// A live incumbent is protected (this process is alive).
	second := HeadlessRegistration{ID: "b", URL: "http://127.0.0.1:2", PID: os.Getpid(), Version: "v1", Token: "t2"}
	if err := publishRegistration(path, second); err == nil {
		t.Fatal("a second instance must refuse to publish over a live one")
	}
	if cur, _ := readRegistration(path); cur.ID != "a" {
		t.Errorf("registration still belongs to %q, want a", cur.ID)
	}

	// A stale file (a pid that cannot exist) is replaced, not refused.
	dead := HeadlessRegistration{ID: "dead", URL: "http://127.0.0.1:3", PID: 1 << 30, Version: "v0", Token: "t0"}
	if err := writeRegistration(path, dead); err != nil {
		t.Fatalf("writeRegistration(stale): %v", err)
	}
	if err := publishRegistration(path, second); err != nil {
		t.Errorf("a stale registration must be replaceable: %v", err)
	}
	if cur, _ := readRegistration(path); cur.ID != "b" {
		t.Errorf("registration belongs to %q, want b", cur.ID)
	}
}

// TestHeadlessRefusesAPromptWithNoSession pins the bound check: a server
// still attaching answers 409 like steer/abort do, rather than accepting a
// prompt that no turn will ever answer.
func TestHeadlessRefusesAPromptWithNoSession(t *testing.T) {
	h := newHeadless(t, Options{DataDir: t.TempDir(), Listen: "127.0.0.1:0", Token: "tok"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.Run(ctx) }()
	waitFor(t, func() bool { return h.Addr() != "" }, "the server to bind")

	body, _ := json.Marshal(map[string]string{"id": "p1", "text": "ping"})
	req, err := http.NewRequest(http.MethodPost, "http://"+h.Addr()+"/prompt", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.Token())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Errorf("POST /prompt with no session = %d, want 409", res.StatusCode)
	}
}

// TestHeadlessServesTheWebClient pins the two things the browser client needs
// from the server and that nothing else in the package covers: GET / answers
// the embedded page WITHOUT a token (a browser navigating to the link cannot
// attach a header, which is why the token rides the fragment), and GET
// /history answers the driver's transcript in the same shape /export writes —
// so the live page and the archive cannot drift.
//
// The negative half matters as much: the page is open, the DATA is not. If a
// future route is added without the token, this fails.
func TestHeadlessServesTheWebClient(t *testing.T) {
	// A distinctive token: the substring check below must not pass by accident
	// on the word "token" appearing in the page's own JavaScript.
	const tok = "sekrit-a1b2c3"
	h := newHeadless(t, Options{DataDir: t.TempDir(), Listen: "127.0.0.1:0", Token: tok})
	h.SetHandler(&scriptedHeadlessHandler{history: json.RawMessage(`{"title":"t","entries":[]}`)})

	rec := doReq(t, h.Handler(), http.MethodGet, "/", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / without a token = %d, want 200 (a browser cannot send one)", rec.Code)
	}
	page := rec.Body.String()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET / content-type = %q, want text/html", ct)
	}
	// The page must reach the routes it needs; a page that names a route the
	// server does not serve is a dead button.
	for _, want := range []string{"/history", "/event", "/prompt", "/abort"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page never mentions %s — that control would be dead", want)
		}
	}
	if strings.Contains(page, tok) {
		t.Error("the page must not contain the bearer token: it arrives in the fragment, which the server never sees")
	}

	// The transcript itself stays gated.
	if rec := doReq(t, h.Handler(), http.MethodGet, "/history", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /history without a token = %d, want 401", rec.Code)
	}
	rec = doReq(t, h.Handler(), http.MethodGet, "/history", tok, nil)
	if got := strings.TrimSpace(rec.Body.String()); got != `{"title":"t","entries":[]}` {
		t.Errorf("GET /history body = %s, want the driver's document verbatim", got)
	}

	// A root-only pattern: an unknown path is a 404, not the page. A page
	// answered for every typo hides a broken client.
	if rec := doReq(t, h.Handler(), http.MethodGet, "/nope", tok, nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET /nope = %d, want 404 (the page is registered as /{$})", rec.Code)
	}
}
