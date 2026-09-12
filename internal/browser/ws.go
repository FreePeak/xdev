package browser

// Minimal RFC6455 websocket client (M13 #50).
//
// The browser tool speaks CDP straight to the DevTools websocket, so the
// only thing needed from the protocol is: HTTP upgrade handshake, masked
// client frames, unmasked server frames, fragmentation assembly, and the
// ping/pong/close control frame handling. That is ~200 lines of stdlib
// net/textproto work versus chromedp's dependency tree, and it keeps xdev
// CGO-free and its module graph small (PRD §1: stdlib first).
//
// Deliberately absent: extensions (permessage-deflate), subprotocol
// negotiation, and client-side frame fragmentation (every CDP message is
// written as one frame, which is what Chrome itself does).

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
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"time"
)

// wsGUID is the RFC6455 handshake constant (RFC 6455 §1.3).
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// maxWSMessage caps one assembled message (a CDP response). A screenshot
// response is base64 in JSON, so this must be comfortably above the
// screenshot cap; anything larger is a protocol error, not a payload to
// allocate.
const maxWSMessage = 64 << 20

// Frame opcodes (RFC 6455 §5.2).
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// wsConn is a client-side websocket connection. One reader (readMessage)
// and any number of concurrent writers are allowed; write is serialized.
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	wmu  sync.Mutex
}

// dialWS performs the HTTP upgrade handshake against rawURL ("ws://host:port/path",
// "wss://...", or the http(s) equivalent) and returns the framed connection.
func dialWS(ctx context.Context, rawURL string) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("browser: invalid websocket url %q: %w", rawURL, err)
	}
	var port string
	var secure bool
	switch u.Scheme {
	case "ws", "http":
		port = "80"
	case "wss", "https":
		port, secure = "443", true
	default:
		return nil, fmt.Errorf("browser: unsupported websocket scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("browser: websocket url %q has no host", rawURL)
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), port)
	}
	dialer := &net.Dialer{}
	var conn net.Conn
	if secure {
		td := &tls.Dialer{NetDialer: dialer, Config: &tls.Config{ServerName: u.Hostname()}}
		conn, err = td.DialContext(ctx, "tcp", host)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", host)
	}
	if err != nil {
		return nil, fmt.Errorf("browser: dial %s: %w", host, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	ws, err := handshake(conn, u, host)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{}) // the op context bounds each Call, not the socket
	return ws, nil
}

// handshake writes the upgrade request and validates the 101 response. The
// response head is parsed by hand (textproto) so the bufio.Reader keeps the
// frame bytes that may already sit in its buffer.
func handshake(conn net.Conn, u *url.URL, host string) (*wsConn, error) {
	var key [16]byte
	if _, err := rand.Read(key[:]); err != nil {
		return nil, fmt.Errorf("browser: websocket key: %w", err)
	}
	keyB64 := base64.StdEncoding.EncodeToString(key[:])
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + keyB64 + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		return nil, fmt.Errorf("browser: websocket handshake write: %w", err)
	}
	br := bufio.NewReader(conn)
	tp := textproto.NewReader(br)
	status, err := tp.ReadLine()
	if err != nil {
		return nil, fmt.Errorf("browser: websocket handshake read: %w", err)
	}
	if !strings.Contains(status, " 101") {
		return nil, fmt.Errorf("browser: websocket handshake rejected: %s", strings.TrimSpace(status))
	}
	hdr, err := tp.ReadMIMEHeader()
	if err != nil {
		return nil, fmt.Errorf("browser: websocket handshake headers: %w", err)
	}
	if !strings.EqualFold(hdr.Get("Upgrade"), "websocket") {
		return nil, fmt.Errorf("browser: websocket handshake: missing Upgrade: websocket")
	}
	want := wsAccept(keyB64)
	if got := hdr.Get("Sec-WebSocket-Accept"); got != want {
		return nil, fmt.Errorf("browser: websocket handshake: bad Sec-WebSocket-Accept %q", got)
	}
	return &wsConn{conn: conn, br: br}, nil
}

// wsAccept computes the RFC6455 §4.2.2 accept token.
func wsAccept(key string) string {
	sum := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// Close sends a close frame (best effort) and closes the socket.
func (c *wsConn) Close() error {
	_ = c.write(opClose, nil)
	return c.conn.Close()
}

// write sends one complete frame. Client frames MUST be masked (RFC 6455
// §5.3); the mask is fresh per frame.
func (c *wsConn) write(opcode byte, payload []byte) error {
	n := len(payload)
	var hdr [14]byte
	hdr[0] = 0x80 | opcode // FIN set: CDP messages are never fragmented
	off := 2
	switch {
	case n < 126:
		hdr[1] = 0x80 | byte(n)
	case n <= 0xffff:
		hdr[1] = 0x80 | 126
		binary.BigEndian.PutUint16(hdr[2:], uint16(n))
		off = 4
	default:
		hdr[1] = 0x80 | 127
		binary.BigEndian.PutUint64(hdr[2:], uint64(n))
		off = 10
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return fmt.Errorf("browser: websocket mask: %w", err)
	}
	copy(hdr[off:], mask[:])
	off += 4
	masked := make([]byte, n)
	for i, b := range payload {
		masked[i] = b ^ mask[i&3]
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	// A zero-length payload (close frames) must not reach the socket as its
	// own write: it is a no-op on TCP but blocks forever on an unbuffered
	// pipe (net.Buffers does not skip empty slices).
	bufs := net.Buffers{hdr[:off]}
	if n > 0 {
		bufs = append(bufs, masked)
	}
	if _, err := bufs.WriteTo(c.conn); err != nil {
		return fmt.Errorf("browser: websocket write: %w", err)
	}
	return nil
}

// readMessage reads frames until one message is complete, answering pings
// and surfacing a peer close as io.EOF. Control frames are handled inline so
// the caller only ever sees text/binary messages.
func (c *wsConn) readMessage() ([]byte, error) {
	var msg []byte
	for {
		fin, opcode, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch opcode {
		case opPing:
			if err := c.write(opPong, payload); err != nil {
				return nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			_ = c.write(opClose, nil)
			return nil, io.EOF
		case opText, opBinary, opContinuation:
		default:
			return nil, fmt.Errorf("browser: websocket: unexpected opcode 0x%x", opcode)
		}
		if len(msg)+len(payload) > maxWSMessage {
			return nil, fmt.Errorf("browser: websocket message exceeds %d bytes", maxWSMessage)
		}
		msg = append(msg, payload...)
		if fin {
			return msg, nil
		}
	}
}

// readFrame reads one frame and unmasks its payload.
func (c *wsConn) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(c.br, h[:]); err != nil {
		return false, 0, nil, err
	}
	fin = h[0]&0x80 != 0
	opcode = h[0] & 0x0f
	masked := h[1]&0x80 != 0
	n := int64(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(c.br, b[:]); err != nil {
			return false, 0, nil, err
		}
		n = int64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(c.br, b[:]); err != nil {
			return false, 0, nil, err
		}
		u := binary.BigEndian.Uint64(b[:])
		if u > maxWSMessage {
			return false, 0, nil, fmt.Errorf("browser: websocket frame length %d exceeds cap", u)
		}
		n = int64(u)
	}
	if opcode >= opClose && (n > 125 || !fin) {
		return false, 0, nil, errors.New("browser: websocket: invalid control frame")
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(c.br, mask[:]); err != nil {
			return false, 0, nil, err
		}
	}
	if n > 0 {
		payload = make([]byte, n)
		if _, err = io.ReadFull(c.br, payload); err != nil {
			return false, 0, nil, err
		}
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i&3]
		}
	}
	return fin, opcode, payload, nil
}
