package serve

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Minimal RFC6455 codec. The relay needs both roles (server for the paired
// client, client for the upstream CDP endpoint) and nothing else — no
// subprotocol negotiation, no extensions, no deflate — so this is ~200 lines
// of frame handling instead of a dependency. Compressed frames are refused:
// permessage-deflate is negotiated, and nothing here negotiates it.

// WebSocket opcodes (RFC6455 §5.2).
const (
	opContinuation byte = 0x0
	opText         byte = 0x1
	opBinary       byte = 0x2
	opClose        byte = 0x8
	opPing         byte = 0x9
	opPong         byte = 0xa
)

// Close codes this package sends (RFC6455 §7.4.1).
const (
	closeGoingAway     = 1001
	closeProtocolError = 1002
	closeMessageTooBig = 1009
	closeInternalError = 1011
)

// wsGUID is the handshake constant (RFC6455 §1.3).
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

var (
	// errWSProtocol marks a peer violation worth a 1002 close.
	errWSProtocol = errors.New("websocket: protocol error")
	// errWSFrameTooLarge marks a frame over the configured bound (1009).
	errWSFrameTooLarge = errors.New("websocket: frame too large")
)

// wsFrame is one frame as it arrives (payload already unmasked).
type wsFrame struct {
	fin     bool
	opcode  byte
	payload []byte
}

// wsConn is a frame codec over one connection. The role decides masking: a
// server reads masked frames and writes unmasked ones, a client the reverse
// (RFC6455 §5.1). Writes are serialized because control replies and forwarded
// data share the connection from two goroutines.
type wsConn struct {
	conn         net.Conn
	br           *bufio.Reader
	expectMasked bool
	maskWrites   bool
	writeTimeout time.Duration
	writeMu      sync.Mutex
}

func newWSConn(c net.Conn, br *bufio.Reader, maskWrites bool) *wsConn {
	return &wsConn{
		conn:         c,
		br:           br,
		expectMasked: !maskWrites,
		maskWrites:   maskWrites,
		writeTimeout: 15 * time.Second,
	}
}

func (c *wsConn) Close() error { return c.conn.Close() }

// wsAccept computes Sec-WebSocket-Accept (RFC6455 §4.2.2).
func wsAccept(key string) string {
	h := sha1.New()
	io.WriteString(h, key+wsGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// headerHasToken reports whether a comma-separated header lists token.
func headerHasToken(header, token string) bool {
	for _, part := range strings.Split(header, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// isWebSocketUpgrade reports whether r is an RFC6455 handshake.
func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		headerHasToken(r.Header.Get("Connection"), "Upgrade")
}

// upgradeWS completes the server side of the handshake and returns the
// hijacked connection. Validation is strict: a bad version, a malformed key
// or a missing Upgrade is an error the caller reports as a 400, never a
// best-effort upgrade of something that is not a WebSocket client.
func upgradeWS(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if !isWebSocketUpgrade(r) {
		return nil, errors.New("not a websocket handshake")
	}
	if v := r.Header.Get("Sec-WebSocket-Version"); v != "13" {
		return nil, fmt.Errorf("unsupported websocket version %q", v)
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if raw, err := base64.StdEncoding.DecodeString(key); err != nil || len(raw) != 16 {
		return nil, errors.New("bad Sec-WebSocket-Key")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("connection cannot be hijacked")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", wsAccept(key)); err != nil {
		conn.Close()
		return nil, err
	}
	if err := brw.Flush(); err != nil {
		conn.Close()
		return nil, err
	}
	// brw.Reader may already hold bytes the client sent after its handshake.
	return newWSConn(conn, brw.Reader, false), nil
}

// readFrame reads one frame, enforcing max payload bytes. Masking is checked
// against the role and reserved bits are refused, so a peer cannot make this
// endpoint interpret a frame it never sent in that shape.
func (c *wsConn) readFrame(max int) (wsFrame, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		return wsFrame{}, err
	}
	if hdr[0]&0x70 != 0 {
		return wsFrame{}, fmt.Errorf("%w: reserved bits set", errWSProtocol)
	}
	fin := hdr[0]&0x80 != 0
	opcode := hdr[0] & 0x0f
	masked := hdr[1]&0x80 != 0
	length := int64(hdr[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return wsFrame{}, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return wsFrame{}, err
		}
		length = int64(binary.BigEndian.Uint64(ext[:]))
	}
	if !validOpcode(opcode) {
		return wsFrame{}, fmt.Errorf("%w: opcode %#x", errWSProtocol, opcode)
	}
	if opcode >= opClose && (length > 125 || !fin) {
		// Control frames are never fragmented and never carry more than 125
		// bytes (RFC6455 §5.5).
		return wsFrame{}, fmt.Errorf("%w: malformed control frame", errWSProtocol)
	}
	if masked != c.expectMasked {
		return wsFrame{}, fmt.Errorf("%w: unexpected mask bit", errWSProtocol)
	}
	if length > int64(max) {
		return wsFrame{}, fmt.Errorf("%w: %d bytes", errWSFrameTooLarge, length)
	}
	var key [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, key[:]); err != nil {
			return wsFrame{}, err
		}
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return wsFrame{}, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= key[i&3]
		}
	}
	return wsFrame{fin: fin, opcode: opcode, payload: payload}, nil
}

func validOpcode(op byte) bool {
	switch op {
	case opContinuation, opText, opBinary, opClose, opPing, opPong:
		return true
	}
	return false
}

// writeFrame writes one frame. Fragmentation is the caller's: frames are
// forwarded as they arrive, so FIN and opcode come from the peer.
func (c *wsConn) writeFrame(opcode byte, fin bool, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	hdr := make([]byte, 0, 14)
	b0 := opcode
	if fin {
		b0 |= 0x80
	}
	hdr = append(hdr, b0)
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n <= 0xffff:
		hdr = append(hdr, 126, byte(n>>8), byte(n))
	default:
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		hdr = append(hdr, 127)
		hdr = append(hdr, ext[:]...)
	}
	if c.maskWrites {
		hdr[1] |= 0x80
		var key [4]byte
		if _, err := rand.Read(key[:]); err != nil {
			return err
		}
		hdr = append(hdr, key[:]...)
		masked := make([]byte, n)
		for i, b := range payload {
			masked[i] = b ^ key[i&3]
		}
		payload = masked
	}
	if c.writeTimeout > 0 {
		// A peer that stops reading must not wedge this goroutine forever
		// (the relay would leak both connections behind it).
		_ = c.conn.SetWriteDeadline(time.Now().Add(c.writeTimeout))
	}
	if _, err := c.conn.Write(hdr); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := c.conn.Write(payload)
	return err
}

// closeWith sends a close frame with a code and reason (best effort: the
// connection is about to go away either way).
func (c *wsConn) closeWith(code int, reason string) {
	payload := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(payload, uint16(code))
	copy(payload[2:], reason)
	_ = c.writeFrame(opClose, true, payload)
}

// closeCode maps a read error to the close code to report.
func closeCode(err error) int {
	switch {
	case errors.Is(err, errWSFrameTooLarge):
		return closeMessageTooBig
	case errors.Is(err, errWSProtocol):
		return closeProtocolError
	case err == nil:
		return closeGoingAway
	default:
		return closeGoingAway
	}
}

// dialWS performs the client handshake against a ws://, wss://, http:// or
// https:// URL. No Origin header is sent: Chrome's CDP endpoint rejects
// handshakes that carry one, and a relay is not a web origin.
func dialWS(ctx context.Context, rawURL string, timeout time.Duration) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	secure := false
	switch strings.ToLower(u.Scheme) {
	case "ws", "http":
	case "wss", "https":
		secure = true
	default:
		return nil, fmt.Errorf("unsupported websocket scheme %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("websocket URL %q has no host", rawURL)
	}
	host := u.Host
	if u.Port() == "" {
		port := "80"
		if secure {
			port = "443"
		}
		host = net.JoinHostPort(u.Hostname(), port)
	}
	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, err
	}
	if secure {
		tconn := tls.Client(conn, &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12})
		if err := tconn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, err
		}
		conn = tconn
	}
	var keyRaw [16]byte
	if _, err := rand.Read(keyRaw[:]); err != nil {
		conn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyRaw[:])
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	handshake := "GET " + path + " HTTP/1.1\r\nHost: " + u.Host +
		"\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: " + key +
		"\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}
	if _, err := io.WriteString(conn, handshake); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, fmt.Errorf("upstream handshake: %s", resp.Status)
	}
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != wsAccept(key) {
		conn.Close()
		return nil, errors.New("upstream handshake: bad Sec-WebSocket-Accept")
	}
	// The relay is long-lived: the handshake deadline must not become a
	// lifetime, so clear it here.
	_ = conn.SetDeadline(time.Time{})
	return newWSConn(conn, br, true), nil
}
