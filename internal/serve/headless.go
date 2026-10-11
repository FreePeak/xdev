// Package serve, headless service: an HTTP + SSE face on a live agent
// session, so a client (a TUI in another process, a script, a widget) drives
// turns the way OpenCode's `opencode serve` lets its TUI drive a daemon.
//
// Why this exists next to `xdev rpc`: the RPC mode already carries the whole
// contract, but over stdio, which makes the client a CHILD of the daemon. A
// headless server inverts that: the session outlives any one client, and a
// second client can attach to the same conversation. The wire is deliberately
// small — four routes — because the surface a client actually needs is
// prompt/steer/abort/state plus one event stream.
//
// Posture is the one every service in this package shares (see serve.go):
// loopback by default, bearer token on every route but the liveness probe,
// bounded bodies. Two OpenCode mechanisms are adopted because they are what
// makes an HTTP-ified agent usable rather than a toy:
//
//   - a REGISTRATION FILE (<data dir>/serve/headless.json, 0600) holding
//     {id, url, pid, version, token}, so a client connects by reading a file
//     instead of guessing a port (OpenCode's Service.register/Info contract);
//   - `GET /info` stays open, because the client's very first act is a health
//     probe it cannot yet authenticate.
package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// serviceHeadless is the subcommand name and the registration basename.
const serviceHeadless = "headless"

// defaultHeadlessListen is OpenCode's own default shape (a fixed loopback
// port); --port 0 asks the OS for one, which is what a test or a second
// instance wants.
const defaultHeadlessListen = "127.0.0.1:7717"

// Headless is the server: one HTTP surface over whatever drives the session.
type Headless struct {
	token   string
	version string
	id      string
	options Options
	logf    func(string, ...any)
	h       http.Handler
	ln      net.Listener
	srv     *http.Server

	// driver is the bound session (SetHandler); nil means a server that can
	// answer /info but no command, which is a real state while a client is
	// still attaching.
	driver HeadlessHandler

	subMu   sync.Mutex
	subs    map[int64]*headlessSub
	nextSub int64
}

// HeadlessHandler is the session the server drives. cmd/xdev supplies the
// implementation (wrapping the same agent rpcHandler uses), so this package
// stays free of the agent's dependencies — the seam the whole package keeps.
//
// Prompt must return immediately: the turn runs on its own goroutine and its
// events reach subscribers through the server, exactly as the RPC contract
// already specifies. A handler that blocks would make the HTTP request the
// turn's duration, which is what SSE exists to avoid.
type HeadlessHandler interface {
	Prompt(id, text string)
	Steer(text string) error
	Abort() error
	State() map[string]any
}

// HeadlessOptions configures the service.
type HeadlessOptions struct {
	Options
	// MaxBody bounds a command body (default 64 KiB: text, not a file).
	MaxBody int64
}

func (o HeadlessOptions) maxBody() int64 {
	if o.MaxBody > 0 {
		return o.MaxBody
	}
	return 64 << 10
}

// NewHeadless builds the service. It does not listen: Run does.
func NewHeadless(opts HeadlessOptions) (*Headless, error) {
	token, err := resolveToken(serviceHeadless, opts.Options)
	if err != nil {
		return nil, err
	}
	id, err := randHex(16)
	if err != nil {
		return nil, err
	}
	h := &Headless{
		token:   token,
		version: opts.Version,
		id:      id,
		options: opts.Options,
		logf:    opts.Options.logf,
		subs:    map[int64]*headlessSub{},
	}
	// One route stays open, and only one: a client's first act is the health
	// probe it cannot yet authenticate with (OpenCode's /api/info). Everything
	// else — including the event stream — goes through requireToken.
	mux := h.mux(opts.maxBody())
	h.h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/info" && r.URL.Host == "" {
			h.handleInfo(w, r)
			return
		}
		requireToken(token, mux).ServeHTTP(w, r)
	})
	return h, nil
}

// Token is the bearer token this server requires (for a co-located client).
func (h *Headless) Token() string { return h.token }

// Handler serves the API.
func (h *Headless) Handler() http.Handler { return h.h }

// SetHandler binds the session driver. Late binding keeps construction free of
// the agent: a server can start, publish its registration, and take its
// driver once the session exists.
func (h *Headless) SetHandler(d HeadlessHandler) { h.driver = d }

// Addr is the bound address ("" before Run).
func (h *Headless) Addr() string {
	if h.ln == nil {
		return ""
	}
	return h.ln.Addr().String()
}

// mux is the route table. Four routes plus the open probe: anything more is a
// surface a client has to learn, and the RPC contract already covers the rest.
func (h *Headless) mux(maxBody int64) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /event", h.handleEvent)
	mux.HandleFunc("GET /state", h.handleState)
	mux.HandleFunc("POST /prompt", h.handlePrompt(maxBody))
	mux.HandleFunc("POST /steer", h.handleSimpleCommand(maxBody, "text", func(text string) error {
		return h.handler().Steer(text)
	}))
	mux.HandleFunc("POST /abort", h.handleSimpleCommand(maxBody, "", func(string) error {
		return h.handler().Abort()
	}))
	return mux
}

// handleInfo answers the identity probe. It is registered under /info and, by
// requireToken's healthz exemption, is the ONE route a client may call before
// it holds the token.
func (h *Headless) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"service": serviceHeadless,
		"version": h.version,
		"pid":     os.Getpid(),
		"id":      h.id,
		// urls keeps OpenCode's shape: a bound 0.0.0.0 is reachable on every
		// interface, and a client that prints "connect here" should say so
		// rather than name the wildcard.
		"urls": h.urls(r),
	})
}

// urls lists the addresses a client can reach this server on.
func (h *Headless) urls(r *http.Request) []string {
	if h.ln == nil {
		return nil
	}
	addr := h.ln.Addr().String()
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host != "0.0.0.0" && host != "::" {
		return []string{"http://" + addr}
	}
	out := []string{}
	if r != nil && r.Host != "" {
		out = append(out, "http://"+r.Host)
	}
	for _, iface := range mustInterfaces() {
		out = append(out, fmt.Sprintf("http://%s:%s", iface, port))
	}
	return out
}

// mustInterfaces lists non-loopback IPv4 interface addresses (a wildcard bind
// is only useful to a peer).
func mustInterfaces() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.To4() == nil {
			continue
		}
		out = append(out, ipnet.IP.String())
	}
	return out
}

// handler returns the bound driver, or a refusing one when none was set: a
// server with no session must answer an actionable error, never panic.
func (h *Headless) handler() HeadlessHandler {
	if h.driver == nil {
		return unboundHandler{}
	}
	return h.driver
}

type unboundHandler struct{}

func (unboundHandler) Prompt(string, string) {}
func (unboundHandler) Steer(string) error    { return errors.New("no session bound to this server") }
func (unboundHandler) Abort() error          { return errors.New("no session bound to this server") }
func (unboundHandler) State() map[string]any { return map[string]any{} }

// handleState reports the session snapshot (what /state carries in RPC).
func (h *Headless) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.handler().State())
}

// handlePrompt accepts {id, text} and returns at once: the turn streams to
// /event subscribers. A missing id is minted here, so a client that does not
// correlate still gets a correlatable response.
func (h *Headless) handlePrompt(maxBody int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		}
		if err := readJSON(w, r, maxBody, &in); err != nil {
			httpError(w, http.StatusBadRequest, "bad request: %v", err)
			return
		}
		if strings.TrimSpace(in.Text) == "" {
			httpError(w, http.StatusBadRequest, "prompt: text required")
			return
		}
		if in.ID == "" {
			id, err := randHex(8)
			if err != nil {
				httpError(w, http.StatusInternalServerError, "mint id: %v", err)
				return
			}
			in.ID = id
		}
		h.handler().Prompt(in.ID, in.Text)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": in.ID})
	}
}

// handleSimpleCommand is the shape steer/abort share: decode an optional body,
// call the driver, answer synchronously (both are one-way requests into a
// running turn, not turns of their own).
func (h *Headless) handleSimpleCommand(maxBody int64, textField string, call func(string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		text := ""
		if textField != "" && r.Body != nil && r.ContentLength != 0 {
			var in map[string]any
			if err := readJSON(w, r, maxBody, &in); err != nil {
				httpError(w, http.StatusBadRequest, "bad request: %v", err)
				return
			}
			if v, ok := in[textField].(string); ok {
				text = v
			}
		}
		if textField != "" && strings.TrimSpace(text) == "" {
			httpError(w, http.StatusBadRequest, "%s: %s required", strings.TrimPrefix(r.URL.Path, "/"), textField)
			return
		}
		if err := call(text); err != nil {
			httpError(w, http.StatusConflict, "%v", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// Run binds, publishes the registration, and serves until ctx is done.
func (h *Headless) Run(ctx context.Context) error {
	addr := h.options.addr(defaultHeadlessListen)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	h.ln = ln
	h.srv = &http.Server{
		Handler: h.h,
		// Deliberately no WriteTimeout: /event is a long-lived stream, and a
		// global write deadline would cut it at the first lull.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       0,
		MaxHeaderBytes:    64 << 10,
	}
	url := "http://" + ln.Addr().String()
	regPath := HeadlessRegistrationPath(h.options.dataDir())
	if err := writeRegistration(regPath, h.Registration(url)); err != nil {
		h.logf("%s: registration: %v", serviceHeadless, err)
	}
	h.logf("%s listening on %s (registration %s)", serviceHeadless, url, regPath)

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		h.closeSubscribers()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.srv.Shutdown(shutCtx)
	}()
	// The registration goes away with the process that owns it, so a client
	// reading a stale file fails on connect rather than on a wrong port. This
	// runs even when the process is killed mid-serve (the deferred form),
	// because a leftover file pointing at a dead pid is what makes a client
	// hang on a port nothing answers.
	defer func() { _, _ = removeRegistrationIfOwner(regPath, h.id) }()
	if err := h.srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-shutdownDone
	return nil
}

// HeadlessRegistration is what a client reads: everything needed to connect.
// Mirrors OpenCode's Service.Info (url/pid/version/password) with the pid
// check it uses to tell a stale file from a live daemon.
type HeadlessRegistration struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	PID     int    `json:"pid"`
	Version string `json:"version"`
	Token   string `json:"token"`
}

// Registration is the record this process publishes for url.
func (h *Headless) Registration(url string) HeadlessRegistration {
	return HeadlessRegistration{ID: h.id, URL: url, PID: os.Getpid(), Version: h.version, Token: h.token}
}

// HeadlessRegistrationPath is <data dir>/serve/headless.json — the same
// directory the service tokens live in (0700).
func HeadlessRegistrationPath(dataDir string) string {
	return filepath.Join(dataDir, "serve", serviceHeadless+".json")
}

// writeRegistration publishes the record 0600 from creation: it carries the
// token, so an open-then-chmod window would briefly expose a credential. The
// write is atomic (temp + rename), so a client never reads half a file.
func writeRegistration(path string, info HeadlessRegistration) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(info)
	if err != nil {
		return err
	}
	if err := createPrivate(path+".tmp", b); err != nil {
		// A leftover temp file from a crashed write must not block startup.
		_ = os.Remove(path + ".tmp")
		if err := createPrivate(path+".tmp", b); err != nil {
			return err
		}
	}
	return os.Rename(path+".tmp", path)
}

// writeRegistrationIfOwner rewrites the file only when the incumbent is this
// process (same id): a second daemon publishing over a live one would hand
// clients a URL whose owner is still serving the old port.
func writeRegistrationIfOwner(path string, info HeadlessRegistration) error {
	cur, err := readRegistration(path)
	if err == nil && cur.ID != "" && cur.ID != info.ID {
		return fmt.Errorf("registration %s belongs to %s (pid %d)", path, cur.ID, cur.PID)
	}
	return writeRegistration(path, info)
}

// removeRegistrationIfOwner deletes the file only if this process wrote it.
func removeRegistrationIfOwner(path, id string) (bool, error) {
	cur, err := readRegistration(path)
	if err != nil {
		return false, nil
	}
	if cur.ID != id {
		return false, nil
	}
	return true, os.Remove(path)
}

// readRegistration loads the published record.
func readRegistration(path string) (HeadlessRegistration, error) {
	var out HeadlessRegistration
	b, err := os.ReadFile(path)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return out, err
	}
	return out, nil
}

// DiscoverHeadless finds a running server by registration file and verifies it
// is alive. A stale file (dead pid, or /info answering a different pid) is
// reported as not found rather than as an error: a client that wants to start
// its own server should not care that a previous one crashed.
func DiscoverHeadless(dataDir string) (HeadlessRegistration, bool) {
	reg, err := readRegistration(HeadlessRegistrationPath(dataDir))
	if err != nil || reg.URL == "" {
		return HeadlessRegistration{}, false
	}
	if !processAlive(reg.PID) {
		return HeadlessRegistration{}, false
	}
	// The pid check alone is not enough: pids are recycled, and a stale file
	// pointing at an unrelated listener would send the client to a stranger.
	info, err := probeHeadless(reg.URL, 3*time.Second)
	if err != nil || info.PID != reg.PID {
		return HeadlessRegistration{}, false
	}
	return reg, true
}

// headlessInfo is the /info payload a client decodes.
type headlessInfo struct {
	Service string   `json:"service"`
	Version string   `json:"version"`
	PID     int      `json:"pid"`
	ID      string   `json:"id"`
	URLs    []string `json:"urls"`
}

// probeHeadless is the health check a client runs before trusting a URL. It is
// the one call that needs no token, by design.
func probeHeadless(url string, timeout time.Duration) (headlessInfo, error) {
	var out headlessInfo
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(strings.TrimRight(url, "/") + "/info")
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("/info: %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, err
	}
	if out.Service != serviceHeadless {
		return out, fmt.Errorf("/info: not a %s service", serviceHeadless)
	}
	return out, nil
}
