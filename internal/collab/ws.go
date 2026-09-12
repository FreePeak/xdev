package collab

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

// RFC 6455 framing. The relay speaks binary messages only; text frames are
// accepted (and returned as-is) for hand-debugging with generic clients.
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Caps keep a hostile peer from forcing unbounded allocation: one message
// (one encrypted frame) at a time is at most maxMessageSize.
const (
	maxMessageSize = 4 << 20
	wsHandshakeTO  = 10 * time.Second
)

// WSConn is one RFC 6455 connection (stdlib net/http + net only; no
// dependency). Writes are serialized; one reader at a time.
type WSConn struct {
	conn   net.Conn
	br     *bufio.Reader
	client bool // client connections must mask outgoing frames
	wmu    sync.Mutex
	closed bool
}

// WriteBinary sends one binary message.
func (c *WSConn) WriteBinary(data []byte) error { return c.write(opBinary, data) }

// ReadBinary returns the next complete binary (or text) message, transparently
// answering pings and finishing fragmented messages.
func (c *WSConn) ReadBinary() ([]byte, error) {
	var buf []byte
	for {
		op, fin, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case opPing:
			if err := c.write(opPong, payload); err != nil {
				return nil, err
			}
		case opPong:
		case opClose:
			// Echo the close and report a clean end of stream.
			_ = c.write(opClose, payload)
			return nil, io.EOF
		case opBinary, opText:
			if buf != nil {
				return nil, errors.New("collab: new message before the previous one finished")
			}
			if fin {
				return payload, nil
			}
			buf = payload
		case opContinuation:
			if buf == nil {
				return nil, errors.New("collab: continuation frame without a start")
			}
			buf = append(buf, payload...)
			if len(buf) > maxMessageSize {
				return nil, errors.New("collab: message exceeds the size limit")
			}
			if fin {
				return buf, nil
			}
		default:
			return nil, fmt.Errorf("collab: unsupported websocket opcode %#x", op)
		}
	}
}

// write sends one frame; client connections are masked per RFC 6455 §5.3.
func (c *WSConn) write(op int, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	return c.writeLocked(op, payload)
}

// writeLocked frames and sends one message; the caller holds wmu.
func (c *WSConn) writeLocked(op int, payload []byte) error {
	header := make([]byte, 0, 14)
	header = append(header, byte(0x80|op))
	maskBit := byte(0)
	if c.client {
		maskBit = 0x80
	}
	n := len(payload)
	switch {
	case n < 126:
		header = append(header, maskBit|byte(n))
	case n <= 0xFFFF:
		header = append(header, maskBit|126, byte(n>>8), byte(n))
	default:
		header = append(header, maskBit|127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		header = append(header, ext[:]...)
	}
	body := payload
	if c.client {
		var mask [4]byte
		if _, err := rand.Read(mask[:]); err != nil {
			return err
		}
		header = append(header, mask[:]...)
		body = make([]byte, len(payload))
		for i, b := range payload {
			body[i] = b ^ mask[i%4]
		}
	}
	if _, err := c.conn.Write(header); err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	_, err := c.conn.Write(body)
	return err
}

// readFrame reads one frame and unmasks its payload.
func (c *WSConn) readFrame() (op int, fin bool, payload []byte, err error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		return 0, false, nil, err
	}
	fin = hdr[0]&0x80 != 0
	if hdr[0]&0x70 != 0 {
		return 0, false, nil, errors.New("collab: reserved websocket bits set (no extensions negotiated)")
	}
	op = int(hdr[0] & 0x0F)
	masked := hdr[1]&0x80 != 0
	// Client → server frames must be masked; server → client frames must not.
	if masked == c.client {
		return 0, false, nil, errors.New("collab: websocket masking rule violated")
	}
	n := int64(hdr[1] & 0x7F)
	switch n {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return 0, false, nil, err
		}
		n = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return 0, false, nil, err
		}
		u := binary.BigEndian.Uint64(ext[:])
		if u > maxMessageSize {
			return 0, false, nil, errors.New("collab: frame exceeds the size limit")
		}
		n = int64(u)
	}
	if n > maxMessageSize {
		return 0, false, nil, errors.New("collab: frame exceeds the size limit")
	}
	if op >= opClose && (!fin || n > 125) {
		return 0, false, nil, errors.New("collab: invalid control frame")
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, mask[:]); err != nil {
			return 0, false, nil, err
		}
	}
	payload = make([]byte, n)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return 0, false, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return op, fin, payload, nil
}

// Close sends a close frame (best effort) and closes the socket.
func (c *WSConn) Close() error {
	c.wmu.Lock()
	if !c.closed {
		c.closed = true
		_ = c.writeLocked(opClose, nil)
	}
	c.wmu.Unlock()
	return c.conn.Close()
}

// RemoteAddr reports the peer address.
func (c *WSConn) RemoteAddr() net.Addr { return c.conn.RemoteAddr() }

// acceptKey computes Sec-WebSocket-Accept for the client's key.
func acceptKey(key string) string {
	h := sha1.New()
	io.WriteString(h, key+wsGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// upgrade performs the RFC 6455 server handshake and takes over the
// connection. The http.Server must support hijacking (it does for HTTP/1.1,
// which is all this relay serves).
func upgrade(w http.ResponseWriter, r *http.Request) (*WSConn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || !headerHasToken(r.Header, "Connection", "upgrade") {
		http.Error(w, "collab: websocket upgrade required", http.StatusBadRequest)
		return nil, errors.New("collab: not a websocket upgrade")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" || r.Header.Get("Sec-WebSocket-Version") != "13" {
		http.Error(w, "collab: unsupported websocket version", http.StatusBadRequest)
		return nil, errors.New("collab: bad websocket handshake")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "collab: connection cannot be hijacked", http.StatusInternalServerError)
		return nil, errors.New("collab: hijack unsupported")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, fmt.Errorf("collab: hijack: %w", err)
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " +
		acceptKey(key) + "\r\n\r\n"
	if _, err := conn.Write([]byte(resp)); err != nil {
		conn.Close()
		return nil, err
	}
	return &WSConn{conn: conn, br: brw.Reader}, nil
}

// headerHasToken reports whether a comma-separated header contains token
// (case-insensitive) — Connection: Upgrade / keep-alive, Upgrade.
func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// DialWS opens a client WebSocket connection to a ws:// or wss:// URL.
func DialWS(ctx context.Context, rawURL string) (*WSConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("collab: bad relay URL %q: %w", rawURL, err)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return nil, fmt.Errorf("collab: relay URL %q must be ws:// or wss://", rawURL)
	}
	if u.Scheme == "ws" && !IsLoopback(u.Host) {
		return nil, fmt.Errorf("collab: plain ws to %q is loopback-only; use wss://", u.Host)
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "wss" {
			host = net.JoinHostPort(u.Hostname(), "443")
		} else {
			host = net.JoinHostPort(u.Hostname(), "80")
		}
	}
	d := &net.Dialer{Timeout: wsHandshakeTO}
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("collab: dial relay: %w", err)
	}
	if u.Scheme == "wss" {
		tc := tls.Client(conn, &tls.Config{ServerName: u.Hostname()})
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, fmt.Errorf("collab: relay TLS: %w", err)
		}
		conn = tc
	}
	var keyBytes [16]byte
	if _, err := rand.Read(keyBytes[:]); err != nil {
		conn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes[:])
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	req := "GET " + path + " HTTP/1.1\r\nHost: " + u.Host +
		"\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: " + key +
		"\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("collab: relay handshake: %w", err)
	}
	br := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(wsHandshakeTO))
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("collab: relay handshake: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, fmt.Errorf("collab: relay refused the upgrade: %s", resp.Status)
	}
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != acceptKey(key) {
		conn.Close()
		return nil, errors.New("collab: relay returned a bad Sec-WebSocket-Accept")
	}
	_ = conn.SetReadDeadline(time.Time{})
	return &WSConn{conn: conn, br: br, client: true}, nil
}
