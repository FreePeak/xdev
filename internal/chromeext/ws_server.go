package chromeext

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

// Minimal server-side RFC6455 (stdlib only), matching internal/browser/ws.go's
// client half. Only text/binary data + ping/pong/close; no extensions.

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

const maxWSMessage = 64 << 20

type wsServerConn struct {
	c  net.Conn
	br *bufio.Reader
	mu sync.Mutex
}

func upgradeWS(w http.ResponseWriter, r *http.Request) (*wsServerConn, error) {
	if !strings.EqualFold(r.Header.Get("Connection"), "Upgrade") &&
		!strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
		return nil, errors.New("missing Connection: Upgrade")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, errors.New("missing Sec-WebSocket-Key")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("response does not support hijack")
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	accept := wsAccept(key)
	_, _ = bufrw.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	_, _ = bufrw.WriteString("Upgrade: websocket\r\n")
	_, _ = bufrw.WriteString("Connection: Upgrade\r\n")
	_, _ = bufrw.WriteString("Sec-WebSocket-Accept: " + accept + "\r\n\r\n")
	if err := bufrw.Flush(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &wsServerConn{c: conn, br: bufrw.Reader}, nil
}

func wsAccept(key string) string {
	h := sha1.New()
	_, _ = io.WriteString(h, key+wsGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func (w *wsServerConn) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.writeFrameLocked(opClose, nil)
	return w.c.Close()
}

func (w *wsServerConn) WriteMessage(payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writeFrameLocked(opText, payload)
}

func (w *wsServerConn) writeFrameLocked(opcode byte, payload []byte) error {
	if len(payload) > maxWSMessage {
		return fmt.Errorf("ws: message too large")
	}
	var hdr []byte
	hdr = append(hdr, 0x80|opcode)
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n < 1<<16:
		hdr = append(hdr, 126, byte(n>>8), byte(n))
	default:
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(n))
		hdr = append(hdr, 127)
		hdr = append(hdr, b[:]...)
	}
	if _, err := w.c.Write(hdr); err != nil {
		return err
	}
	_, err := w.c.Write(payload)
	return err
}

func (w *wsServerConn) ReadMessage() ([]byte, error) {
	var msg []byte
	for {
		opcode, payload, err := w.readFrame()
		if err != nil {
			return nil, err
		}
		switch opcode {
		case opText, opBinary:
			msg = append(msg, payload...)
			return msg, nil
		case opContinuation:
			msg = append(msg, payload...)
			// only return when FIN of a fragmented message; our reader folds
			// single-frame messages above. Fragmented: keep reading until a
			// non-continuation with FIN is awkward; require single frames.
			return msg, nil
		case opPing:
			w.mu.Lock()
			_ = w.writeFrameLocked(opPong, payload)
			w.mu.Unlock()
		case opPong:
			// ignore
		case opClose:
			return nil, io.EOF
		default:
			return nil, fmt.Errorf("ws: unknown opcode %d", opcode)
		}
	}
}

func (w *wsServerConn) readFrame() (opcode byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(w.br, h[:]); err != nil {
		return 0, nil, err
	}
	opcode = h[0] & 0x0f
	masked := h[1]&0x80 != 0
	n := int(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(w.br, b[:]); err != nil {
			return 0, nil, err
		}
		n = int(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(w.br, b[:]); err != nil {
			return 0, nil, err
		}
		u := binary.BigEndian.Uint64(b[:])
		if u > maxWSMessage {
			return 0, nil, fmt.Errorf("ws: frame too large")
		}
		n = int(u)
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(w.br, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(w.br, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return opcode, payload, nil
}
