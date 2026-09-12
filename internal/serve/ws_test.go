package serve

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// pipeConns returns a connected client/server pair: the first masks its
// writes (client role), the second requires masked frames (server role).
func pipeConns(t *testing.T) (client, server *wsConn) {
	t.Helper()
	c1, c2 := net.Pipe()
	t.Cleanup(func() {
		c1.Close()
		c2.Close()
	})
	return newWSConn(c1, bufio.NewReader(c1), true), newWSConn(c2, bufio.NewReader(c2), false)
}

// readOne runs readFrame on c with a deadline, so a codec bug fails the test
// instead of hanging it.
func readOne(t *testing.T, c *wsConn, max int) (wsFrame, error) {
	t.Helper()
	type result struct {
		f   wsFrame
		err error
	}
	ch := make(chan result, 1)
	go func() {
		f, err := c.readFrame(max)
		ch <- result{f, err}
	}()
	select {
	case r := <-ch:
		return r.f, r.err
	case <-time.After(5 * time.Second):
		t.Fatal("readFrame did not return")
		return wsFrame{}, nil
	}
}

// readRaw feeds raw bytes to a fresh connection and decodes one frame from it.
// maskWrites selects the reading role: a client-role reader (maskWrites true)
// requires unmasked frames, a server-role reader requires masked ones.
func readRaw(t *testing.T, raw []byte, maskWrites bool, max int) (wsFrame, error) {
	t.Helper()
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	go func() { _, _ = c1.Write(raw) }()
	return newWSConn(c2, bufio.NewReader(c2), maskWrites).readFrame(max)
}

func TestWSAcceptMatchesRFCExample(t *testing.T) {
	// RFC6455 §1.3.
	if got := wsAccept("dGhlIHNhbXBsZSBub25jZQ=="); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("wsAccept = %q, want the RFC example value", got)
	}
}

func TestReadFrameDecodesMaskedClientFrame(t *testing.T) {
	// RFC6455 §5.7: a masked "Hello" frame.
	raw := []byte{0x81, 0x85, 0x37, 0xfa, 0x21, 0x3d, 0x7f, 0x9f, 0x4d, 0x51, 0x58}
	f, err := readRaw(t, raw, false, 1024)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if f.opcode != opText || !f.fin || string(f.payload) != "Hello" {
		t.Fatalf("frame = %+v payload %q, want a final text frame carrying Hello", f, f.payload)
	}
}

func TestReadFrameRejectsMalformedFrames(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		role bool // reading role: true = client (frames must be unmasked)
		want error
	}{
		{"reserved bits", []byte{0xc1, 0x80, 0, 0, 0, 0}, false, errWSProtocol},
		{"unknown opcode", []byte{0x83, 0x80, 0, 0, 0, 0}, false, errWSProtocol},
		{"fragmented control frame", []byte{0x09, 0x80, 0, 0, 0, 0}, false, errWSProtocol},
		{"oversized control frame", []byte{0x89, 0xfe, 0x00, 0x7e, 0, 0, 0, 0}, false, errWSProtocol},
		{"unmasked frame to server", []byte{0x81, 0x05, 'H', 'e', 'l', 'l', 'o'}, false, errWSProtocol},
		{"masked frame to client", []byte{0x81, 0x85, 1, 2, 3, 4, 0, 0, 0, 0, 0}, true, errWSProtocol},
		{"oversized data frame", []byte{0x81, 0xfe, 0x10, 0x00, 0, 0, 0, 0}, false, errWSFrameTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := readRaw(t, tc.raw, tc.role, 64); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestWriteFrameRoundTrip(t *testing.T) {
	client, server := pipeConns(t)
	payload := []byte(`{"id":1,"method":"Target.getTargets"}`)
	go func() {
		if err := client.writeFrame(opText, true, payload); err != nil {
			t.Errorf("client writeFrame: %v", err)
		}
	}()
	f, err := readOne(t, server, 4096)
	if err != nil {
		t.Fatalf("server readFrame: %v", err)
	}
	if string(f.payload) != string(payload) || f.opcode != opText || !f.fin {
		t.Fatalf("frame = %+v %q", f, f.payload)
	}
}

func TestWriteFrameMasksForClientRole(t *testing.T) {
	// A round trip through this package's own reader would pass even if the
	// client forgot to mask, so check the wire bits directly.
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	go func() { _ = newWSConn(c1, bufio.NewReader(c1), true).writeFrame(opText, true, []byte("abc")) }()
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c2, hdr); err != nil {
		t.Fatalf("reading frame header: %v", err)
	}
	if hdr[1]&0x80 == 0 {
		t.Fatal("client frame was written unmasked")
	}
}

func TestWriteFramePayloadLengths(t *testing.T) {
	client, server := pipeConns(t)
	for _, n := range []int{0, 125, 126, 65535, 65536} {
		payload := bytes.Repeat([]byte{'x'}, n)
		go func() {
			if err := client.writeFrame(opBinary, true, payload); err != nil {
				t.Errorf("write %d bytes: %v", n, err)
			}
		}()
		f, err := readOne(t, server, 1<<20)
		if err != nil {
			t.Fatalf("read %d bytes: %v", n, err)
		}
		if len(f.payload) != n {
			t.Fatalf("payload length = %d, want %d", len(f.payload), n)
		}
	}
}

func TestUpgradeAndDialHandshake(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgradeWS(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer c.Close()
		f, err := c.readFrame(1 << 20)
		if err != nil {
			return
		}
		_ = c.writeFrame(f.opcode, true, append([]byte("echo:"), f.payload...))
	}))
	defer srv.Close()

	c, err := dialWS(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), 5*time.Second)
	if err != nil {
		t.Fatalf("dialWS: %v", err)
	}
	defer c.Close()
	if err := c.writeFrame(opText, true, []byte("hello")); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	f, err := c.readFrame(1 << 20)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if string(f.payload) != "echo:hello" {
		t.Fatalf("payload = %q, want the upstream echo", f.payload)
	}
}

func TestUpgradeWSRejectsBadHandshakes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgradeWS(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c.Close()
	}))
	defer srv.Close()

	status := func(version, key string, upgrade bool) int {
		req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		if upgrade {
			req.Header.Set("Upgrade", "websocket")
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Sec-WebSocket-Key", key)
			req.Header.Set("Sec-WebSocket-Version", version)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("handshake: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if got := status("8", "dGhlIHNhbXBsZSBub25jZQ==", true); got != http.StatusBadRequest {
		t.Fatalf("version 8: status = %d, want 400", got)
	}
	if got := status("13", "not-base64-16-bytes", true); got != http.StatusBadRequest {
		t.Fatalf("bad key: status = %d, want 400", got)
	}
	if got := status("", "", false); got != http.StatusBadRequest {
		t.Fatalf("plain GET: status = %d, want 400", got)
	}
	if got := status("13", "dGhlIHNhbXBsZSBub25jZQ==", true); got != http.StatusSwitchingProtocols {
		t.Fatalf("valid handshake: status = %d, want 101", got)
	}
}

func TestDialWSRejectsBadUpstreams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()
	if _, err := dialWS(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), 5*time.Second); err == nil {
		t.Fatal("dialWS accepted a 401 response as an upgrade")
	}
	if _, err := dialWS(context.Background(), "ftp://127.0.0.1:1/x", time.Second); err == nil {
		t.Fatal("dialWS accepted an unsupported scheme")
	}
}
