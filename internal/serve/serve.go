// Package serve implements the M15 loopback services behind one dispatch:
// `xdev serve <auth-broker|auth-gateway|browser-relay>` (issue #71).
//
// The three services are one trust domain — the developer's own machine — and
// share a posture rather than a framework:
//
//   - auth-broker: a credential vault (AES-256-GCM at rest) plus observed
//     usage reporting; the gateway's credential source and the place a
//     client's provider tokens live once they leave a shell profile.
//   - auth-gateway: a forward proxy that attaches broker-held credentials for
//     allowlisted upstream hosts, so a client (a container, a widget, a
//     script) makes provider calls without ever seeing the secret.
//   - browser-relay: an RFC6455 relay to a live CDP endpoint (the paired
//     browser extension), token-gated so only the paired client drives the
//     user's real tabs.
//
// Uniform rules, enforced in one place each:
//
//	loopback only     the default listen is 127.0.0.1; anything else must be
//	                  asked for explicitly with --listen AND come with a token,
//	                  because the per-install token file is readable by every
//	                  local process.
//	bearer token      every endpoint but /healthz compares a constant-time
//	                  bearer token.
//	bounded work      a request body / relayed frame limit and a per-request
//	                  deadline on every service (a loopback port is not an
//	                  excuse for unbounded memory).
package serve

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/FreePeak/xdev/internal/config"
)

// Service names, also the subcommands and the token file basenames.
const (
	serviceBroker  = "auth-broker"
	serviceGateway = "auth-gateway"
	serviceRelay   = "browser-relay"
)

// Default (loopback-only) listen addresses.
const (
	defaultBrokerListen  = "127.0.0.1:8765"
	defaultGatewayListen = "127.0.0.1:4000"
	defaultRelayListen   = "127.0.0.1:9223"
)

// tokenEnv supplies a token for any service, taking precedence over the
// generated per-install file.
const tokenEnv = "XDEV_SERVE_TOKEN"

const serveUsage = `xdev serve <service> — loopback services

  xdev serve auth-broker   [--listen 127.0.0.1:8765] [--token T]
  xdev serve auth-gateway  [--listen 127.0.0.1:4000] [--broker URL]
                           [--upstream '[scheme://]host=provider[:header]']...
  xdev serve browser-relay [--listen 127.0.0.1:9223] [--upstream ws://127.0.0.1:9222] [--token T]

Every service binds loopback and requires a bearer token; the token is
--token, ` + tokenEnv + `, or <data dir>/serve/<service>.token (generated 0600 on
first start). Exposing the port beyond loopback additionally requires an
explicit token: --listen 0.0.0.0:4000 without one refuses to start.
`

// Dispatch runs `xdev serve <service> [flags]` and returns a process exit code.
// IsService reports whether a bare word names one of the three services, so
// the top-level spellings omp uses (`xdev auth-broker`) can dispatch here
// instead of reaching the model as a prompt (#104).
func IsService(name string) bool {
	switch name {
	case serviceBroker, serviceGateway, serviceRelay:
		return true
	}
	return false
}

func Dispatch(args []string, version string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, serveUsage)
		return 2
	}
	switch args[0] {
	case serviceBroker:
		return runBroker(args[1:], version)
	case serviceGateway:
		return runGateway(args[1:], version)
	case serviceRelay:
		return runRelay(args[1:], version)
	case "help", "-h", "--help":
		fmt.Fprint(os.Stderr, serveUsage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "xdev serve: unknown service %q\n\n%s", args[0], serveUsage)
		return 2
	}
}

func runBroker(args []string, version string) int {
	fs := flag.NewFlagSet("xdev serve "+serviceBroker, flag.ContinueOnError)
	listen := fs.String("listen", "", "bind address (default "+defaultBrokerListen+")")
	token := fs.String("token", "", "bearer token (default: per-install token file)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	b, err := NewBroker(BrokerOptions{Options: Options{Listen: *listen, Token: *token, Version: version}})
	if err != nil {
		return fail(err)
	}
	return serveUntilSignal(b.Run)
}

func runGateway(args []string, version string) int {
	fs := flag.NewFlagSet("xdev serve "+serviceGateway, flag.ContinueOnError)
	listen := fs.String("listen", "", "bind address (default "+defaultGatewayListen+")")
	token := fs.String("token", "", "bearer token (default: per-install token file)")
	brokerURL := fs.String("broker", "", "auth-broker base URL (default "+defaultBrokerURL+")")
	brokerToken := fs.String("broker-token", "", "token for the broker (default: the broker's stored token)")
	timeout := fs.Duration("timeout", 0, "per-request upstream deadline (default "+defaultGatewayTimeout.String()+")")
	upstreams := stringList{}
	fs.Var(&upstreams, "upstream", "allowlisted upstream as [scheme://]host=provider[:header] (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	allow := make([]Upstream, 0, len(upstreams))
	for _, spec := range upstreams {
		u, err := parseUpstream(spec)
		if err != nil {
			return fail(err)
		}
		allow = append(allow, u)
	}
	g, err := NewGateway(GatewayOptions{
		Options:     Options{Listen: *listen, Token: *token, Version: version},
		BrokerURL:   *brokerURL,
		BrokerToken: *brokerToken,
		Allow:       allow,
		Timeout:     *timeout,
	})
	if err != nil {
		return fail(err)
	}
	return serveUntilSignal(g.Run)
}

func runRelay(args []string, version string) int {
	fs := flag.NewFlagSet("xdev serve "+serviceRelay, flag.ContinueOnError)
	listen := fs.String("listen", "", "bind address (default "+defaultRelayListen+")")
	token := fs.String("token", "", "bearer token the paired client must present")
	upstream := fs.String("upstream", "", "upstream CDP websocket (default "+defaultRelayUpstream+")")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rl, err := NewRelay(RelayOptions{
		Options:  Options{Listen: *listen, Token: *token, Version: version},
		Upstream: *upstream,
	})
	if err != nil {
		return fail(err)
	}
	return serveUntilSignal(rl.Run)
}

// serveUntilSignal runs a service until SIGINT/SIGTERM, which is how a
// supervised daemon is expected to stop.
func serveUntilSignal(run func(context.Context) error) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		return fail(err)
	}
	return 0
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "xdev serve:", err)
	return 1
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// Options is the configuration every service shares.
type Options struct {
	// Listen is host:port; empty selects the service's loopback default.
	Listen string
	// Token gates every request. Empty asks for the per-install token file
	// (see resolveToken for what that does and does not authorize).
	Token string
	// DataDir overrides the agent data dir (tests); empty means
	// config.DataDir().
	DataDir string
	// Version is what /healthz reports.
	Version string
	// Logf receives lifecycle lines (default: stderr).
	Logf func(format string, args ...any)
}

func (o Options) dataDir() string {
	if o.DataDir != "" {
		return o.DataDir
	}
	return config.DataDir()
}

func (o Options) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
		return
	}
	fmt.Fprintf(os.Stderr, "xdev serve: "+format+"\n", args...)
}

// addr resolves a listen address against the service default.
func (o Options) addr(def string) string {
	if o.Listen != "" {
		return o.Listen
	}
	return def
}

// defaultListen is the loopback default for a service.
func defaultListen(service string) string {
	switch service {
	case serviceBroker:
		return defaultBrokerListen
	case serviceGateway:
		return defaultGatewayListen
	default:
		return defaultRelayListen
	}
}

// resolveToken decides the bearer token, refusing an exposed bind that has no
// operator-supplied one. The per-install token file is readable by any local
// process, which is exactly the process boundary a loopback bind already
// trusts; a port reachable from the network needs a token the operator chose
// (--token or tokenEnv), not one this process minted into a shared directory.
func resolveToken(service string, o Options) (string, error) {
	if t := strings.TrimSpace(o.Token); t != "" {
		return t, nil
	}
	if t := strings.TrimSpace(os.Getenv(tokenEnv)); t != "" {
		return t, nil
	}
	if addr := o.addr(defaultListen(service)); !loopbackAddr(addr) {
		return "", fmt.Errorf("%s: refusing to bind %s without an explicit token (pass --token or set %s)",
			service, addr, tokenEnv)
	}
	return ensureToken(service, o)
}

// loopbackAddr reports whether addr can only be reached from this host. An
// empty host (":4000"), 0.0.0.0 and :: are every-interface binds, not
// loopback.
func loopbackAddr(addr string) bool {
	if addr == "" {
		return true
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// serveDir is <data dir>/serve, created 0700: tokens, keys and vaults live
// here and must not be world-readable.
func serveDir(o Options) (string, error) {
	dir := filepath.Join(o.dataDir(), "serve")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// ensureToken returns a service's token, minting one on first use. The
// exclusive create means two daemons racing at startup agree on one token
// instead of each believing its own.
func ensureToken(service string, o Options) (string, error) {
	dir, err := serveDir(o)
	if err != nil {
		return "", err
	}
	path := tokenPath(dir, service)
	if t := readToken(path); t != "" {
		return t, nil
	}
	tok, err := randHex(32)
	if err != nil {
		return "", err
	}
	if err := createPrivate(path, []byte(tok+"\n")); err != nil {
		if t := readToken(path); t != "" { // lost the race: adopt the winner
			return t, nil
		}
		return "", err
	}
	return tok, nil
}

// loadToken reads a persisted token without minting one: how the gateway
// finds the broker's token on the same machine.
func loadToken(service string, o Options) string {
	return readToken(tokenPath(filepath.Join(o.dataDir(), "serve"), service))
}

func tokenPath(dir, service string) string { return filepath.Join(dir, service+".token") }

func readToken(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// randHex returns n random bytes as hex.
func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// createPrivate writes a new file with 0600 from creation (an
// open-then-chmod window briefly exposes a secret) and fails if it exists.
func createPrivate(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// writePrivate replaces a file atomically with 0600 content: the temporary
// file is private from creation and the rename means a reader never observes
// a half-written vault.
func writePrivate(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// healthPayload is the unauthenticated liveness answer every service shares.
type healthPayload struct {
	Status  string `json:"status"`
	Service string `json:"service"`
	Version string `json:"version,omitempty"`
}

// isHealthz reports a liveness probe for this service itself. An absolute-form
// request is never a probe, even when its path is /healthz: on a forward proxy
// that path belongs to the upstream, and letting it skip the token would hand
// an unauthenticated caller a path through the proxy.
func isHealthz(r *http.Request) bool { return r.URL.Path == "/healthz" && r.URL.Host == "" }

// requireToken gates a handler behind its bearer token. /healthz stays open:
// a supervisor probing liveness should not need a credential.
func requireToken(token string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isHealthz(r) {
			h.ServeHTTP(w, r)
			return
		}
		if !tokenEqual(tokenFromRequest(r), token) {
			httpError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		h.ServeHTTP(w, r)
	})
}

// tokenFromRequest reads the caller's bearer token from Proxy-Authorization,
// Authorization, or the `token` query parameter. Proxy-Authorization wins when
// both are present: on a forward proxy that header is this hop's credential
// while Authorization belongs to the client's own upstream request (and is
// stripped before forwarding), so preferring Authorization would 401 a
// correctly authenticated proxy call. The query parameter is what lets a
// browser extension connect at all — it cannot set headers on a WebSocket
// handshake.
func tokenFromRequest(r *http.Request) string {
	if v := bearerValue(r.Header.Get("Proxy-Authorization")); v != "" {
		return v
	}
	if v := bearerValue(r.Header.Get("Authorization")); v != "" {
		return v
	}
	return r.URL.Query().Get("token")
}

// bearerValue extracts the token of a "Bearer x" header value.
func bearerValue(v string) string {
	const prefix = "bearer "
	if len(v) < len(prefix) || !strings.EqualFold(v[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(v[len(prefix):])
}

// tokenEqual compares tokens in constant time, so the 401 path cannot be
// timed to recover one.
func tokenEqual(got, want string) bool {
	if want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// serveHTTP runs h on addr until ctx is done, then drains in-flight requests.
// The bounds are deliberately lopsided: reads and headers are capped, writes
// are not, because the gateway streams SSE bodies and the relay hijacks the
// connection — a global write deadline would cut long responses short.
func serveHTTP(ctx context.Context, service, addr string, h http.Handler, logf func(string, ...any)) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	logf("%s listening on http://%s", service, ln.Addr())
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-shutdownDone
	return nil
}

// writeJSON writes one JSON value with the service's content type.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// httpError writes the {"error": "..."} shape every service uses.
func httpError(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}

// readJSON decodes one JSON value with a hard size bound: an unbounded body
// on a loopback port is still a memory bug.
func readJSON(w http.ResponseWriter, r *http.Request, limit int64, v any) error {
	if r.Body == nil {
		return errors.New("empty body")
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		// Trailing bytes mean the sender and this server disagree about the
		// payload; silently using the first value would hide that.
		return errors.New("unexpected trailing JSON")
	}
	return nil
}
