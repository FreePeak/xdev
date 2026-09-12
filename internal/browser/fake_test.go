package browser

// In-process fake CDP endpoint for the browser tests: an HTTP server that
// serves /json/list and upgrades /devtools/page/<id> to a websocket, then
// answers whatever the handler says. The server side of the protocol is
// implemented independently of the client (its own frame reader/writer), so
// a framing bug on the client shows up as a test failure rather than as a
// mirror-image bug in the fake.

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

// cdpHandler answers one CDP command; a nil result becomes {}.
type cdpHandler func(method string, params json.RawMessage) (json.RawMessage, error)

type fakeCDP struct {
	t       *testing.T
	srv     *httptest.Server
	handler cdpHandler

	mu       sync.Mutex
	pages    []Target
	recorded []fakeCall
	hangs    map[string]bool
	// preEvents are event frames written before the reply to that method.
	preEvents map[string]json.RawMessage
	// listCount counts /json/list hits (attach/re-attach assertions).
	listCount int
}

type fakeCall struct {
	method string
	params json.RawMessage
}

func newFakeCDP(t *testing.T, handler cdpHandler) *fakeCDP {
	t.Helper()
	f := &fakeCDP{t: t, handler: handler, hangs: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/json/list", f.serveList)
	mux.HandleFunc("/devtools/page/", f.serveWS)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	f.addPage("t1", "about:blank", "Fake Page")
	return f
}

func (f *fakeCDP) endpoint() string { return f.srv.URL }

// wsURL is the page websocket URL derived from the live test server.
func (f *fakeCDP) wsURL(id string) string {
	u, err := url.Parse(f.srv.URL)
	if err != nil {
		f.t.Fatal(err)
	}
	u.Scheme = "ws"
	u.Path = "/devtools/page/" + id
	return u.String()
}

func (f *fakeCDP) addPage(id, pageURL, title string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pages = append(f.pages, Target{
		ID: id, Type: "page", Title: title, URL: pageURL, WebSocketDebuggerURL: f.wsURL(id),
	})
}

// hang makes method never answer, exercising the client's timeout path.
func (f *fakeCDP) hang(method string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hangs[method] = true
}

// preEvent makes the fake emit an unsolicited event frame (no id) right
// before it answers method — the interleaving a real CDP peer produces.
func (f *fakeCDP) preEvent(method string, event json.RawMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.preEvents == nil {
		f.preEvents = map[string]json.RawMessage{}
	}
	f.preEvents[method] = event
}

// setHandler swaps the responder between ops (the serving goroutine reads it
// under the same mutex, so a test may change behavior mid-scenario).
func (f *fakeCDP) setHandler(handler cdpHandler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handler = handler
}

func (f *fakeCDP) listHits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCount
}

func (f *fakeCDP) calls(method string) []json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []json.RawMessage
	for _, c := range f.recorded {
		if c.method == method {
			out = append(out, c.params)
		}
	}
	return out
}

func (f *fakeCDP) lastCall(method string) (json.RawMessage, bool) {
	mine := f.calls(method)
	if len(mine) == 0 {
		return nil, false
	}
	return mine[len(mine)-1], true
}

func (f *fakeCDP) serveList(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.listCount++
	pages := append([]Target(nil), f.pages...)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(pages); err != nil {
		f.t.Errorf("fake cdp: list encode: %v", err)
	}
}

func (f *fakeCDP) serveWS(w http.ResponseWriter, r *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		f.t.Error("fake cdp: response writer cannot hijack")
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		f.t.Errorf("fake cdp: hijack: %v", err)
		return
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
		wsAccept(r.Header.Get("Sec-WebSocket-Key"))); err != nil {
		return
	}
	ws := &fakeWS{t: f.t, conn: conn, br: brw.Reader}
	for {
		msg, err := ws.readMessage()
		if err != nil {
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(msg, &req); err != nil {
			f.t.Errorf("fake cdp: bad request frame: %v", err)
			return
		}
		f.mu.Lock()
		f.recorded = append(f.recorded, fakeCall{method: req.Method, params: req.Params})
		hangs := f.hangs[req.Method]
		event := f.preEvents[req.Method]
		handler := f.handler
		f.mu.Unlock()
		if hangs {
			continue // deliberately no reply
		}
		if len(event) > 0 {
			if err := ws.writeText(event); err != nil {
				return
			}
		}
		result, herr := []byte("{}"), error(nil)
		if handler != nil {
			result, herr = handler(req.Method, req.Params)
		}
		if herr != nil {
			reply := map[string]any{"id": json.RawMessage(req.ID), "error": map[string]any{"code": -32000, "message": herr.Error()}}
			if err := ws.writeText(mustJSON(f.t, reply)); err != nil {
				return
			}
			continue
		}
		if len(result) == 0 {
			result = []byte("{}")
		}
		reply := map[string]any{"id": json.RawMessage(req.ID), "result": json.RawMessage(result)}
		if err := ws.writeText(mustJSON(f.t, reply)); err != nil {
			return
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fakeWS is the server half of RFC6455: it requires masked client frames
// (RFC 6455 §5.1) and writes unmasked server frames.
type fakeWS struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
	wmu  sync.Mutex
}

func (c *fakeWS) readMessage() ([]byte, error) {
	var msg []byte
	for {
		var h [2]byte
		if _, err := io.ReadFull(c.br, h[:]); err != nil {
			return nil, err
		}
		fin := h[0]&0x80 != 0
		opcode := h[0] & 0x0f
		if h[1]&0x80 == 0 {
			c.t.Error("fake cdp: client frame is not masked (RFC 6455 §5.1)")
			return nil, fmt.Errorf("unmasked client frame")
		}
		length := int64(h[1] & 0x7f)
		switch length {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(c.br, b[:]); err != nil {
				return nil, err
			}
			length = int64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(c.br, b[:]); err != nil {
				return nil, err
			}
			length = int64(binary.BigEndian.Uint64(b[:]))
		}
		var mask [4]byte
		if _, err := io.ReadFull(c.br, mask[:]); err != nil {
			return nil, err
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(c.br, payload); err != nil {
			return nil, err
		}
		for i := range payload {
			payload[i] ^= mask[i&3]
		}
		switch opcode {
		case opClose:
			return nil, io.EOF
		case opPing, opPong:
			continue
		}
		msg = append(msg, payload...)
		if fin {
			return msg, nil
		}
	}
}

func (c *fakeWS) writeText(payload []byte) error {
	return c.writeFrame(opText, payload)
}

// writeFrame writes one unmasked server frame.
func (c *fakeWS) writeFrame(opcode byte, payload []byte) error {
	n := len(payload)
	var hdr [10]byte
	hdr[0] = 0x80 | opcode
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
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.conn.Write(hdr[:off]); err != nil {
		return err
	}
	_, err := c.conn.Write(payload)
	return err
}
