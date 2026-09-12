package browser

// RFC6455 framing tests: the client codec is checked against an independent
// frame reader/writer written here from the RFC, not against the client's
// own encoder.

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// testFrame is one decoded websocket frame (test-side view).
type testFrame struct {
	fin     bool
	opcode  byte
	masked  bool
	lenCode byte // the 7-bit length field as it appeared on the wire
	payload []byte
}

// pipeWS returns a client wsConn wired to an in-memory peer.
func pipeWS(t *testing.T) (*wsConn, net.Conn, *bufio.Reader) {
	t.Helper()
	cli, peer := net.Pipe()
	t.Cleanup(func() { _ = cli.Close(); _ = peer.Close() })
	return &wsConn{conn: cli, br: bufio.NewReader(cli)}, peer, bufio.NewReader(peer)
}

// readTestFrame decodes one frame exactly as RFC 6455 §5.2 lays it out.
func readTestFrame(r *bufio.Reader) (testFrame, error) {
	var f testFrame
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return f, err
	}
	f.fin = h[0]&0x80 != 0
	f.opcode = h[0] & 0x0f
	f.masked = h[1]&0x80 != 0
	f.lenCode = h[1] & 0x7f
	n := int64(f.lenCode)
	switch f.lenCode {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return f, err
		}
		n = int64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return f, err
		}
		n = int64(binary.BigEndian.Uint64(b[:]))
	}
	var mask [4]byte
	if f.masked {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return f, err
		}
	}
	f.payload = make([]byte, n)
	if _, err := io.ReadFull(r, f.payload); err != nil {
		return f, err
	}
	for i := range f.payload {
		if f.masked {
			f.payload[i] ^= mask[i&3]
		}
	}
	return f, nil
}

// writeTestFrame encodes one frame with the caller's masking choice.
func writeTestFrame(w io.Writer, fin bool, opcode byte, payload []byte, mask *[4]byte) error {
	var hdr [14]byte
	hdr[0] = opcode
	if fin {
		hdr[0] |= 0x80
	}
	n := len(payload)
	off := 2
	switch {
	case n < 126:
		hdr[1] = byte(n)
	case n <= 0xffff:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:], uint16(n))
		off = 4
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:], uint64(n))
		off = 10
	}
	body := payload
	if mask != nil {
		hdr[1] |= 0x80
		copy(hdr[off:], mask[:])
		off += 4
		body = make([]byte, n)
		for i, b := range payload {
			body[i] = b ^ mask[i&3]
		}
	}
	if _, err := w.Write(hdr[:off]); err != nil {
		return err
	}
	// net.Pipe has no buffer: a zero-length write still blocks until a
	// reader arrives, so an empty body must not be written at all.
	if len(body) == 0 {
		return nil
	}
	_, err := w.Write(body)
	return err
}

// TestClientFrameEncoding asserts the exact wire shape a client frame must
// have: FIN set, mask bit set (RFC 6455 §5.1), 7/16/64-bit length selection,
// and a correctly masked payload.
func TestClientFrameEncoding(t *testing.T) {
	cases := []struct {
		size     int
		wantCode byte // the 7-bit length field on the wire
	}{
		{size: 5, wantCode: 5},       // 7-bit length
		{size: 200, wantCode: 126},   // 16-bit length
		{size: 70000, wantCode: 127}, // 64-bit length
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d-bytes", tc.size), func(t *testing.T) {
			ws, peer, pr := pipeWS(t)
			payload := make([]byte, tc.size)
			for i := range payload {
				payload[i] = byte(i) // a repeating pattern hides mask errors
			}
			done := make(chan error, 1)
			go func() { done <- ws.write(opText, payload) }()

			f, err := readTestFrame(pr)
			if err != nil {
				t.Fatalf("decode client frame: %v", err)
			}
			if !f.fin {
				t.Error("FIN not set (client frames are never fragmented)")
			}
			if f.opcode != opText {
				t.Errorf("opcode = %#x, want text %#x", f.opcode, opText)
			}
			if !f.masked {
				t.Error("mask bit not set: client frames MUST be masked (RFC 6455 §5.1)")
			}
			if f.lenCode != tc.wantCode {
				t.Errorf("length code = %d, want %d", f.lenCode, tc.wantCode)
			}
			if len(f.payload) != tc.size {
				t.Fatalf("payload length = %d, want %d", len(f.payload), tc.size)
			}
			for i := range payload {
				if f.payload[i] != payload[i] {
					t.Fatalf("payload[%d] = %q, want %q", i, f.payload[i], payload[i])
				}
			}
			if err := <-done; err != nil {
				t.Fatalf("write: %v", err)
			}
			_ = peer
		})
	}
}

// TestReadMessageFraming covers the shapes a server may send: one frame, a
// fragmented message, a masked frame, and control frames.
func TestReadMessageFraming(t *testing.T) {
	t.Run("single", func(t *testing.T) {
		ws, peer, _ := pipeWS(t)
		go func() { _ = writeTestFrame(peer, true, opText, []byte(`{"hello":1}`), nil) }()
		got, err := ws.readMessage()
		if err != nil {
			t.Fatalf("readMessage: %v", err)
		}
		if string(got) != `{"hello":1}` {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("fragmented", func(t *testing.T) {
		ws, peer, _ := pipeWS(t)
		go func() {
			_ = writeTestFrame(peer, false, opText, []byte("frag"), nil)
			_ = writeTestFrame(peer, true, opContinuation, []byte("mented"), nil)
		}()
		got, err := ws.readMessage()
		if err != nil {
			t.Fatalf("readMessage: %v", err)
		}
		if string(got) != "fragmented" {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("masked-from-peer", func(t *testing.T) {
		ws, peer, _ := pipeWS(t)
		mask := [4]byte{1, 2, 3, 4}
		go func() { _ = writeTestFrame(peer, true, opText, []byte("secret"), &mask) }()
		got, err := ws.readMessage()
		if err != nil {
			t.Fatalf("readMessage: %v", err)
		}
		if string(got) != "secret" {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("ping-answered-with-pong", func(t *testing.T) {
		ws, peer, pr := pipeWS(t)
		go func() {
			_ = writeTestFrame(peer, true, opPing, []byte("beat"), nil)
			// the pong must arrive before the next data frame
			f, err := readTestFrame(pr)
			if err != nil || f.opcode != opPong || !f.fin || string(f.payload) != "beat" {
				return
			}
			_ = writeTestFrame(peer, true, opText, []byte("after-ping"), nil)
		}()
		got, err := ws.readMessage()
		if err != nil {
			t.Fatalf("readMessage: %v", err)
		}
		if string(got) != "after-ping" {
			t.Fatalf("message = %q (the ping was not answered first)", got)
		}
	})

	t.Run("pong-ignored", func(t *testing.T) {
		ws, peer, _ := pipeWS(t)
		go func() {
			_ = writeTestFrame(peer, true, opPong, []byte("late"), nil)
			_ = writeTestFrame(peer, true, opText, []byte("payload"), nil)
		}()
		got, err := ws.readMessage()
		if err != nil {
			t.Fatalf("readMessage: %v", err)
		}
		if string(got) != "payload" {
			t.Fatalf("message = %q", got)
		}
	})

	t.Run("close-is-eof", func(t *testing.T) {
		ws, peer, _ := pipeWS(t)
		go func() {
			_ = writeTestFrame(peer, true, opClose, nil, nil)
			// drain the close echo the client sends back
			_, _ = readTestFrame(bufio.NewReader(peer))
		}()
		if _, err := ws.readMessage(); err != io.EOF {
			t.Fatalf("err = %v, want io.EOF", err)
		}
	})

	t.Run("oversized-frame-rejected", func(t *testing.T) {
		ws, peer, _ := pipeWS(t)
		go func() {
			var hdr [10]byte
			hdr[0] = 0x80 | opText
			hdr[1] = 127
			binary.BigEndian.PutUint64(hdr[2:], uint64(maxWSMessage)+1)
			_, _ = peer.Write(hdr[:])
		}()
		if _, err := ws.readMessage(); err == nil || !strings.Contains(err.Error(), "exceeds cap") {
			t.Fatalf("err = %v, want an exceeds-cap error", err)
		}
	})

	t.Run("oversized-control-frame-rejected", func(t *testing.T) {
		ws, peer, _ := pipeWS(t)
		go func() { _ = writeTestFrame(peer, true, opPing, make([]byte, 126), nil) }()
		if _, err := ws.readMessage(); err == nil || !strings.Contains(err.Error(), "invalid control frame") {
			t.Fatalf("err = %v, want an invalid control frame error", err)
		}
	})
}

// TestDialWSHandshake covers the upgrade path against real HTTP servers.
func TestDialWSHandshake(t *testing.T) {
	t.Run("rejected", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no", http.StatusForbidden)
		}))
		defer srv.Close()
		if _, err := dialWS(context.Background(), wsScheme(srv.URL)); err == nil || !strings.Contains(err.Error(), "rejected") {
			t.Fatalf("err = %v, want a rejected handshake", err)
		}
	})

	t.Run("bad-accept", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Upgrade", "websocket")
			w.Header().Set("Connection", "Upgrade")
			w.Header().Set("Sec-WebSocket-Accept", "wrong")
			w.WriteHeader(http.StatusSwitchingProtocols)
		}))
		defer srv.Close()
		if _, err := dialWS(context.Background(), wsScheme(srv.URL)); err == nil || !strings.Contains(err.Error(), "Sec-WebSocket-Accept") {
			t.Fatalf("err = %v, want a bad-accept error", err)
		}
	})

	t.Run("accepted", func(t *testing.T) {
		f := newFakeCDP(t, nil)
		c, err := dialCDP(context.Background(), f.wsURL("t1"))
		if err != nil {
			t.Fatalf("dialCDP: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
	})

	t.Run("bad-scheme", func(t *testing.T) {
		if _, err := dialWS(context.Background(), "ftp://example.com/x"); err == nil || !strings.Contains(err.Error(), "unsupported websocket scheme") {
			t.Fatalf("err = %v, want an unsupported-scheme error", err)
		}
	})
}

// wsScheme rewrites an http test-server URL to ws.
func wsScheme(raw string) string { return "ws" + strings.TrimPrefix(raw, "http") }

// TestCDPRoundTrip drives real request/response correlation: concurrent
// calls, an interleaved event frame with no id, and an error reply.
func TestCDPRoundTrip(t *testing.T) {
	f := newFakeCDP(t, func(method string, params json.RawMessage) (json.RawMessage, error) {
		if method == "Boom" {
			return nil, fmt.Errorf("boom")
		}
		return json.RawMessage(fmt.Sprintf(`{"method":%q,"params":%s}`, method, params)), nil
	})
	f.preEvent("Echo", json.RawMessage(`{"method":"Page.loadEventFired","params":{"timestamp":1}}`))
	c, err := dialCDP(context.Background(), f.wsURL("t1"))
	if err != nil {
		t.Fatalf("dialCDP: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	results := make([]string, 4)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			raw, err := c.Call(ctx, "Echo", map[string]any{"n": i})
			if err != nil {
				t.Errorf("call %d: %v", i, err)
				return
			}
			results[i] = string(raw)
		}(i)
	}
	wg.Wait()
	for i, got := range results {
		want := fmt.Sprintf(`"params":{"n":%d}`, i)
		if !strings.Contains(got, want) {
			t.Errorf("reply %d = %s, want it to contain %s (responses crossed wires)", i, got, want)
		}
	}

	if _, err := c.Call(ctx, "Boom", nil); err == nil || !strings.Contains(err.Error(), "CDP error -32000: boom") {
		t.Fatalf("err = %v, want the CDP error surfaced", err)
	}
}

// TestCDPCallTimeout proves a hung command fails on the context, not forever.
func TestCDPCallTimeout(t *testing.T) {
	f := newFakeCDP(t, nil)
	f.hang("Never")
	c, err := dialCDP(context.Background(), f.wsURL("t1"))
	if err != nil {
		t.Fatalf("dialCDP: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Call(ctx, "Never", nil); err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("err = %v, want a deadline error", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("call took %s, want it bounded by the context", elapsed)
	}
}
