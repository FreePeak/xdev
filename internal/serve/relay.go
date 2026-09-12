package serve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Relay is the CDP relay daemon (issue #71).
//
// It is an RFC6455 server on loopback whose peer is one upstream CDP
// WebSocket — the paired browser extension's endpoint. Frames cross verbatim,
// so a client (xdev's browser tool, an IDE plugin) drives the tabs the user
// already has open while the browser side stays behind the extension's own
// pairing. The token is what makes "paired" mean something: without it a
// request that reaches this port could drive the user's logged-in session.
//
// Endpoints (bearer token in Authorization/Proxy-Authorization or the `token`
// query parameter; /healthz is open):
//
//	GET /            websocket upgrade, relayed to the upstream CDP endpoint
//	GET /healthz     liveness
type Relay struct {
	token     string
	tokenPath string
	version   string
	options   Options
	upstream  string
	maxFrame  int
	dial      time.Duration
	sem       chan struct{}
	h         http.Handler
	logf      func(string, ...any)
}

// Relay defaults.
const (
	defaultRelayUpstream = "ws://127.0.0.1:9222"
	defaultRelayDial     = 10 * time.Second
	// defaultMaxFrame bounds one relayed frame. A CDP screenshot response is
	// the biggest thing that crosses a relay; 4 MiB is well clear of it and
	// still a bound (an unbounded frame is how a loopback port OOMs the
	// agent).
	defaultMaxFrame = 4 << 20
	// defaultMaxClients bounds concurrent relays: one paired browser, plus
	// room for a second client (an IDE plugin beside the agent) and a
	// reconnect overlap.
	defaultMaxClients = 4
)

// RelayOptions configures the relay.
type RelayOptions struct {
	Options
	// Upstream is the CDP endpoint to relay to (default ws://127.0.0.1:9222).
	Upstream string
	// MaxFrame bounds one relayed frame (default 4 MiB).
	MaxFrame int
	// DialTimeout bounds the upstream handshake (default 10s).
	DialTimeout time.Duration
	// MaxClients bounds concurrent relays (default 4).
	MaxClients int
}

// NewRelay builds the relay service.
func NewRelay(opts RelayOptions) (*Relay, error) {
	token, err := resolveToken(serviceRelay, opts.Options)
	if err != nil {
		return nil, err
	}
	upstream := opts.Upstream
	if upstream == "" {
		upstream = defaultRelayUpstream
	}
	u, err := url.Parse(upstream)
	if err != nil {
		return nil, fmt.Errorf("relay: upstream %q: %w", upstream, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "ws", "wss", "http", "https":
	default:
		return nil, fmt.Errorf("relay: upstream %q: want a ws:// or wss:// endpoint", upstream)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("relay: upstream %q has no host", upstream)
	}
	maxFrame, maxClients := opts.MaxFrame, opts.MaxClients
	if maxFrame <= 0 {
		maxFrame = defaultMaxFrame
	}
	if maxClients <= 0 {
		maxClients = defaultMaxClients
	}
	dial := opts.DialTimeout
	if dial <= 0 {
		dial = defaultRelayDial
	}
	dir, err := serveDir(opts.Options)
	if err != nil {
		return nil, err
	}
	rl := &Relay{
		token:     token,
		tokenPath: tokenPath(dir, serviceRelay),
		version:   opts.Version,
		options:   opts.Options,
		upstream:  upstream,
		maxFrame:  maxFrame,
		dial:      dial,
		sem:       make(chan struct{}, maxClients),
		logf:      opts.Options.logf,
	}
	rl.h = http.HandlerFunc(rl.serve)
	return rl, nil
}

// Token is the token the paired client must present.
func (rl *Relay) Token() string { return rl.token }

// Handler serves the relay.
func (rl *Relay) Handler() http.Handler { return rl.h }

// Run serves until ctx is done.
func (rl *Relay) Run(ctx context.Context) error {
	rl.logf("%s: upstream %s, token %s", serviceRelay, rl.upstream, rl.tokenPath)
	return serveHTTP(ctx, serviceRelay, rl.options.addr(defaultRelayListen), rl.h, rl.logf)
}

func (rl *Relay) serve(w http.ResponseWriter, r *http.Request) {
	if isHealthz(r) {
		writeJSON(w, http.StatusOK, healthPayload{Status: "ok", Service: serviceRelay, Version: rl.version})
		return
	}
	if !tokenEqual(tokenFromRequest(r), rl.token) {
		httpError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	select {
	case rl.sem <- struct{}{}:
		defer func() { <-rl.sem }()
	default:
		httpError(w, http.StatusServiceUnavailable, "relay is at capacity (%d clients)", cap(rl.sem))
		return
	}
	if !isWebSocketUpgrade(r) {
		httpError(w, http.StatusBadRequest, "expected a websocket upgrade")
		return
	}
	client, err := upgradeWS(w, r)
	if err != nil {
		httpError(w, http.StatusBadRequest, "upgrade: %v", err)
		return
	}
	defer client.Close()

	// Background, not r.Context(): the upstream outlives the handshake, and a
	// hijacked request's context is not a lifetime to hang a daemon on.
	ctx, cancel := context.WithTimeout(context.Background(), rl.dial)
	upstream, err := dialWS(ctx, rl.upstream, rl.dial)
	cancel()
	if err != nil {
		rl.logf("upstream %s unavailable: %v", rl.upstream, err)
		client.closeWith(closeInternalError, "upstream unavailable")
		return
	}
	defer upstream.Close()
	rl.logf("relaying %s <-> %s", r.RemoteAddr, rl.upstream)

	errs := make(chan error, 2)
	go func() { errs <- pumpFrames(client, upstream, rl.maxFrame) }()
	go func() { errs <- pumpFrames(upstream, client, rl.maxFrame) }()
	err = <-errs
	// Closing both ends unblocks the surviving pump; wait for it so no
	// goroutine is left holding a socket.
	client.Close()
	upstream.Close()
	<-errs
	if err != nil && !errors.Is(err, errWSFrameTooLarge) && !errors.Is(err, errWSProtocol) {
		rl.logf("relay closed: %v", err)
	}
}

// pumpFrames forwards data frames from src to dst and answers control frames
// locally: a ping must not cross the relay (the peer on the other side
// answers its own), and a close is mirrored so both ends shut down in order.
// Continuations are forwarded as they arrive — the first frame carries the
// opcode, the rest are forwarded with theirs, and FIN is preserved, so a
// fragmented message stays fragmented across the relay.
func pumpFrames(src, dst *wsConn, max int) error {
	for {
		f, err := src.readFrame(max)
		if err != nil {
			if code := closeCode(err); code != closeGoingAway {
				src.closeWith(code, strings.TrimPrefix(err.Error(), "websocket: "))
			}
			return err
		}
		switch f.opcode {
		case opPing:
			if err := src.writeFrame(opPong, true, f.payload); err != nil {
				return err
			}
		case opPong:
			// Unsolicited pong: nothing to match it to.
		case opClose:
			_ = dst.writeFrame(opClose, true, f.payload)
			return nil
		default:
			if err := dst.writeFrame(f.opcode, f.fin, f.payload); err != nil {
				return err
			}
		}
	}
}
