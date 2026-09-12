package serve

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// newFakeCDP is a stub CDP endpoint: it echoes every text frame back as a
// JSON reply and reports what it received.
func newFakeCDP(t *testing.T) (*httptest.Server, <-chan string) {
	t.Helper()
	msgs := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgradeWS(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer c.Close()
		for {
			f, err := c.readFrame(1 << 20)
			if err != nil {
				return
			}
			switch f.opcode {
			case opText, opBinary:
				msgs <- string(f.payload)
				if err := c.writeFrame(opText, true, []byte(`{"id":1,"result":"ok"}`)); err != nil {
					return
				}
			case opClose:
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, msgs
}

// relayFor builds a relay in front of upstreamURL, served by an httptest
// server; it returns the relay and the ws:// URL of its endpoint.
func relayFor(t *testing.T, upstreamURL string, opts ...func(*RelayOptions)) (*Relay, string) {
	t.Helper()
	o := RelayOptions{
		Options:  Options{DataDir: t.TempDir(), Token: "relay-token"},
		Upstream: "ws" + strings.TrimPrefix(upstreamURL, "http"),
	}
	for _, fn := range opts {
		fn(&o)
	}
	rl, err := NewRelay(o)
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	srv := httptest.NewServer(rl.Handler())
	t.Cleanup(srv.Close)
	return rl, "ws" + strings.TrimPrefix(srv.URL, "http")
}

// rawHandshake sends a handshake over a bare connection and returns the
// response, so 401-vs-101 is observable without a client implementation
// agreeing with this package's server.
func rawHandshake(t *testing.T, wsURL, query, token string) *http.Response {
	t.Helper()
	u, err := url.Parse(wsURL)
	if err != nil {
		t.Fatalf("parsing %s: %v", wsURL, err)
	}
	conn, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", u.Host, err)
	}
	t.Cleanup(func() { conn.Close() })
	path := u.Path
	if path == "" {
		path = "/"
	}
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 16))
	req := "GET " + path + query + " HTTP/1.1\r\nHost: " + u.Host +
		"\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: " + key +
		"\r\nSec-WebSocket-Version: 13\r\n"
	if token != "" {
		req += "Authorization: Bearer " + token + "\r\n"
	}
	req += "\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatalf("writing handshake: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("reading handshake response: %v", err)
	}
	return resp
}

func TestRelayRequiresToken(t *testing.T) {
	upstream, _ := newFakeCDP(t)
	_, wsURL := relayFor(t, upstream.URL)

	resp := rawHandshake(t, wsURL, "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	resp = rawHandshake(t, wsURL, "?token=wrong", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong query token: %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	resp = rawHandshake(t, wsURL, "?token=relay-token", "")
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("paired client with a query token: %d, want 101", resp.StatusCode)
	}
	resp.Body.Close()

	resp = rawHandshake(t, wsURL, "", "relay-token")
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("paired client with a header token: %d, want 101", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestRelayForwardsFramesToUpstreamCDP(t *testing.T) {
	upstream, msgs := newFakeCDP(t)
	_, wsURL := relayFor(t, upstream.URL)

	c, err := dialWS(context.Background(), wsURL+"?token=relay-token", 5*time.Second)
	if err != nil {
		t.Fatalf("dialWS: %v", err)
	}
	defer c.Close()

	const call = `{"id":1,"method":"Target.getTargets"}`
	if err := c.writeFrame(opText, true, []byte(call)); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	select {
	case got := <-msgs:
		if got != call {
			t.Fatalf("upstream received %q, want %q", got, call)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream CDP endpoint never saw the frame")
	}

	f, err := c.readFrame(1 << 20)
	if err != nil {
		t.Fatalf("reading the upstream reply: %v", err)
	}
	if !strings.Contains(string(f.payload), `"result":"ok"`) {
		t.Fatalf("relayed reply = %q", f.payload)
	}

	// Only the transport is relayed: a ping is answered locally, so the
	// upstream never sees it and the client still gets its pong.
	if err := c.writeFrame(opPing, true, []byte("hb")); err != nil {
		t.Fatalf("ping: %v", err)
	}
	f, err = c.readFrame(1 << 20)
	if err != nil {
		t.Fatalf("reading pong: %v", err)
	}
	if f.opcode != opPong || string(f.payload) != "hb" {
		t.Fatalf("frame = %+v %q, want a pong", f, f.payload)
	}

	// A clean close is mirrored to the upstream rather than dropped.
	if err := c.writeFrame(opClose, true, nil); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestRelayClosesClientWhenUpstreamIsDown(t *testing.T) {
	// Reserve a port and release it: the relay's dial is refused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()

	_, wsURL := relayFor(t, "http://"+dead)
	c, err := dialWS(context.Background(), wsURL+"?token=relay-token", 5*time.Second)
	if err != nil {
		t.Fatalf("the relay must accept the paired client before dialing upstream: %v", err)
	}
	defer c.Close()
	f, err := c.readFrame(1024)
	if err != nil {
		t.Fatalf("reading the failure close: %v", err)
	}
	if f.opcode != opClose {
		t.Fatalf("frame = %+v, want a close frame", f)
	}
	if len(f.payload) < 2 {
		t.Fatalf("close frame carries no code: %+v", f)
	}
	if code := int(f.payload[0])<<8 | int(f.payload[1]); code != closeInternalError {
		t.Fatalf("close code = %d, want %d", code, closeInternalError)
	}
}

func TestRelayRefusesOversizedFrame(t *testing.T) {
	upstream, _ := newFakeCDP(t)
	_, wsURL := relayFor(t, upstream.URL, func(o *RelayOptions) { o.MaxFrame = 64 })
	c, err := dialWS(context.Background(), wsURL+"?token=relay-token", 5*time.Second)
	if err != nil {
		t.Fatalf("dialWS: %v", err)
	}
	defer c.Close()
	if err := c.writeFrame(opText, true, bytes.Repeat([]byte{'x'}, 1024)); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	f, err := c.readFrame(1 << 20)
	if err != nil {
		t.Fatalf("reading the protocol close: %v", err)
	}
	if f.opcode != opClose {
		t.Fatalf("frame = %+v, want a close frame", f)
	}
	if code := int(f.payload[0])<<8 | int(f.payload[1]); code != closeMessageTooBig {
		t.Fatalf("close code = %d, want %d", code, closeMessageTooBig)
	}
}

func TestRelayCapacityBound(t *testing.T) {
	upstream, _ := newFakeCDP(t)
	_, wsURL := relayFor(t, upstream.URL, func(o *RelayOptions) { o.MaxClients = 1 })

	// Hold the single slot open.
	held, err := dialWS(context.Background(), wsURL+"?token=relay-token", 5*time.Second)
	if err != nil {
		t.Fatalf("first client: %v", err)
	}
	defer held.Close()
	if err := held.writeFrame(opText, true, []byte(`{"id":1}`)); err != nil {
		t.Fatalf("first client write: %v", err)
	}
	if _, err := held.readFrame(1 << 20); err != nil {
		t.Fatalf("first client read: %v", err)
	}

	resp := rawHandshake(t, wsURL, "?token=relay-token", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second client: %d, want 503", resp.StatusCode)
	}
}

func TestRelayHealthzAndConfig(t *testing.T) {
	rl, _ := relayFor(t, "http://127.0.0.1:1")
	rec := doReq(t, rl.Handler(), http.MethodGet, "/healthz", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz: %d", rec.Code)
	}
	// A non-websocket request with a valid token is refused, not upgraded.
	rec = doReq(t, rl.Handler(), http.MethodGet, "/", "relay-token", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("plain request: %d, want 400", rec.Code)
	}
	for _, bad := range []RelayOptions{
		{Options: Options{Token: "t"}, Upstream: "ftp://127.0.0.1:1"},
		{Options: Options{Token: "t"}, Upstream: "ws:///"},
	} {
		if _, err := NewRelay(bad); err == nil {
			t.Fatalf("NewRelay(%q) accepted a bad upstream", bad.Upstream)
		}
	}
}
