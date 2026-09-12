package collab

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Backend is what the relay needs from the host process. Snapshot and the
// apply hooks are the whole host surface: the relay never touches the
// session store, the agent, or the TUI itself.
type Backend struct {
	// Snapshot returns the host transcript as session JSONL bytes (title
	// slot + header + entries). Guests replay it through the session
	// store's context builder, so compaction and branches behave natively.
	Snapshot func() []byte
	// Prompt applies a guest prompt through the host's normal submit path
	// (rendered locally with the guest's display name).
	Prompt func(name, text string)
	// Interrupt cancels the host's in-flight turn.
	Interrupt func()
	// UIResponse settles a host ui-request (optional; nil drops responses).
	UIResponse func(id, value string)
}

// HostConfig configures a host relay.
type HostConfig struct {
	// Addr is the listen address. Empty → 127.0.0.1:0 (loopback, random
	// port). A non-loopback address requires AllowRemote.
	Addr string
	// AllowRemote opts into binding a non-loopback address. The relay is
	// content-blind and E2E-encrypted, but a public bind still exposes the
	// room endpoint and connection metadata.
	AllowRemote bool
	// Name is the display name guests see for the host.
	Name string
	// Backend wires the host session into the relay.
	Backend Backend
	// Entries returns the current entry lines (session JSONL, in order).
	// The host polls it and broadcasts the ones it has not sent yet — the
	// session store has no append observer, so polling is the seam.
	Entries func() [][]byte
	// PollEvery is the Entries poll interval (default 250ms).
	PollEvery time.Duration
	// Logf receives relay diagnostics (optional).
	Logf func(format string, args ...any)
}

// Host is the in-process relay: it owns the room, serves the WebSocket
// endpoint, keeps the participant list, and fans encrypted frames out to
// guests. The host process itself is not a socket peer.
type Host struct {
	cfg  HostConfig
	room Link
	seq  atomic.Int64

	mu       sync.Mutex
	guests   map[*wsGuest]struct{}
	seen     int // entries already broadcast
	stopped  bool
	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
	srv      *http.Server
	ln       net.Listener
	base     string
}

// wsGuest is one connected guest and its outbound queue.
type wsGuest struct {
	conn     *WSConn
	name     string
	addr     string
	writable bool
	out      chan Frame
	done     chan struct{}
	once     sync.Once
}

// NewHost mints the room and returns an unstarted host.
func NewHost(cfg HostConfig) (*Host, error) {
	room, err := NewRoom()
	if err != nil {
		return nil, err
	}
	if cfg.PollEvery <= 0 {
		cfg.PollEvery = 250 * time.Millisecond
	}
	return &Host{cfg: cfg, room: room, guests: map[*wsGuest]struct{}{}, stop: make(chan struct{})}, nil
}

// RoomID reports the room id (safe to show; without the key it is useless).
func (h *Host) RoomID() string { return h.room.RoomID }

// Tail renders "<roomId>.<secret>" for this room (full=false → view-only).
func (h *Host) Tail(full bool) string {
	l := h.room
	if !full {
		l = l.ViewOnly()
	}
	return l.Room()
}

// URL renders the shareable link on the host's own relay (full=false →
// view-only). It is empty before the host listens.
func (h *Host) URL(full bool) string {
	h.mu.Lock()
	base := h.base
	h.mu.Unlock()
	if base == "" {
		return ""
	}
	l := h.room
	if !full {
		l = l.ViewOnly()
	}
	return base + "/r/" + l.Room()
}

// Participants lists the connected guests: name, address, and whether the
// link verified a write token.
func (h *Host) Participants() []Participant {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Participant, 0, len(h.guests))
	for g := range h.guests {
		out = append(out, Participant{Name: g.name, Addr: g.addr, Writable: g.writable})
	}
	return out
}

// Participant is one guest in the host's roster.
type Participant struct {
	Name     string
	Addr     string
	Writable bool
}

// Handler returns the relay's HTTP handler: GET /r/<roomId>.<secret> upgrades
// to a WebSocket. Exported so tests (and alternative listeners) can mount it.
func (h *Host) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/r/", h.handleRoom)
	return mux
}

// ListenAndServe binds cfg.Addr, serves the relay, starts the entry poller,
// and returns the full-control link.
func (h *Host) ListenAndServe() (string, error) {
	addr := h.cfg.Addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	if !IsLoopback(addr) && !h.cfg.AllowRemote {
		return "", fmt.Errorf("collab: refusing to bind non-loopback %q without AllowRemote (use -collab-remote to share beyond this machine)", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("collab: listen on %s: %w", addr, err)
	}
	if err := h.Serve(ln); err != nil {
		return "", err
	}
	return h.URL(true), nil
}

// Serve starts serving on an existing listener (tests mount their own).
func (h *Host) Serve(ln net.Listener) error {
	h.mu.Lock()
	h.ln = ln
	h.base = "ws://" + ln.Addr().String()
	h.srv = &http.Server{Handler: h.Handler(), ReadHeaderTimeout: 10 * time.Second}
	srv := h.srv
	h.mu.Unlock()

	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			h.logf("relay serve: %v", err)
		}
	}()
	if h.cfg.Entries != nil {
		h.wg.Add(1)
		go h.pollEntries()
	}
	return nil
}

// Stop closes every guest, stops the poller, and shuts the relay down.
func (h *Host) Stop() error {
	var err error
	h.stopOnce.Do(func() {
		close(h.stop)
		h.mu.Lock()
		h.stopped = true
		srv := h.srv
		guests := make([]*wsGuest, 0, len(h.guests))
		for g := range h.guests {
			guests = append(guests, g)
		}
		h.mu.Unlock()
		for _, g := range guests {
			g.close()
		}
		if srv != nil {
			err = srv.Close()
		}
		h.wg.Wait()
	})
	return err
}

// --- broadcasting ---

// Post fans one frame out to every guest (seq is assigned by the host).
func (h *Host) Post(f Frame) {
	f.RoomID = h.room.RoomID
	f.Seq = h.seq.Add(1)
	h.mu.Lock()
	for g := range h.guests {
		g.send(f)
	}
	h.mu.Unlock()
}

// BroadcastEntry replicates one durable session entry (a JSONL line).
func (h *Host) BroadcastEntry(line []byte) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return
	}
	h.Post(Frame{Type: FrameEntry, Entry: json.RawMessage(append([]byte(nil), trimmed...))})
}

// BroadcastState replicates footer state (streaming flag, model, context).
func (h *Host) BroadcastState(state json.RawMessage) {
	h.Post(Frame{Type: FrameState, State: state})
}

// BroadcastEvent replicates a live host event / notice.
func (h *Host) BroadcastEvent(event json.RawMessage) { h.Post(Frame{Type: FrameEvent, Event: event}) }

// BroadcastBus replicates subagent bus traffic.
func (h *Host) BroadcastBus(bus json.RawMessage) { h.Post(Frame{Type: FrameBus, Bus: bus}) }

// BroadcastAgents replicates an agent-registry snapshot.
func (h *Host) BroadcastAgents(agents json.RawMessage) {
	h.Post(Frame{Type: FrameAgents, Agents: agents})
}

// RequestUI asks writable guests to answer a host select/editor prompt.
func (h *Host) RequestUI(id string, req json.RawMessage) {
	f := Frame{Type: FrameUIRequest, ID: id, Req: req}
	f.RoomID = h.room.RoomID
	f.Seq = h.seq.Add(1)
	h.mu.Lock()
	for g := range h.guests {
		if g.writable {
			g.send(f)
		}
	}
	h.mu.Unlock()
}

// --- relay internals ---

// handleRoom upgrades one guest connection and pumps its two directions.
func (h *Host) handleRoom(w http.ResponseWriter, r *http.Request) {
	tail := strings.TrimPrefix(r.URL.Path, "/r/")
	roomID, secret, err := splitTail(tail)
	if err != nil || roomID != h.room.RoomID {
		http.Error(w, "collab: unknown room", http.StatusNotFound)
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil {
		http.Error(w, "collab: bad room secret", http.StatusBadRequest)
		return
	}
	// The key is the join credential: a guest holding the write token in the
	// link is also holding the same key, so the key check gates decryption.
	if len(raw) < KeySize || subtle.ConstantTimeCompare(raw[:KeySize], h.room.Key) != 1 {
		http.Error(w, "collab: room key mismatch", http.StatusForbidden)
		return
	}
	conn, err := upgrade(w, r)
	if err != nil {
		return
	}
	h.serveGuest(conn, r)
}

func (h *Host) serveGuest(conn *WSConn, r *http.Request) {
	g := &wsGuest{
		conn: conn,
		name: "guest",
		addr: conn.RemoteAddr().String(),
		out:  make(chan Frame, 1024),
		done: make(chan struct{}),
	}
	if n := r.URL.Query().Get("name"); n != "" {
		g.name = sanitizeName(n)
	}
	// Queue the snapshot before registering, so no live frame can slip
	// between the transcript read and the guest's place in the fan-out.
	// An empty transcript still sends one (empty) chunk so the guest can
	// tell "snapshot complete" from "no snapshot started".
	chunks := ChunkBytes(h.snapshot(), DefaultChunkSize)
	if len(chunks) == 0 {
		chunks = [][]byte{{}}
	}
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		conn.Close()
		return
	}
	g.send(Frame{Type: FrameWelcome, RoomID: h.room.RoomID, Name: h.cfg.Name, Writable: false})
	for i, chunk := range chunks {
		g.send(Frame{Type: FrameSnapshot, Index: i, Total: len(chunks), Chunk: chunk})
	}
	h.guests[g] = struct{}{}
	// Add under h.mu: Stop flips stopped under the same lock before Wait,
	// so a handler can never Add after the host began waiting.
	h.wg.Add(1)
	h.mu.Unlock()

	go func() { defer h.wg.Done(); h.writePump(g) }()
	h.notice(g.name + " joined (" + g.addr + ")")

	defer func() {
		h.mu.Lock()
		delete(h.guests, g)
		h.mu.Unlock()
		g.close()
		h.notice(g.name + " left")
	}()

	for {
		blob, err := conn.ReadBinary()
		if err != nil {
			return
		}
		f, err := OpenFrame(h.room.Key, h.room.RoomID, blob)
		if err != nil {
			h.logf("guest %s: %v", g.addr, err)
			continue
		}
		h.handleGuestFrame(g, f)
	}
}

// handleGuestFrame applies one decrypted guest request. Every write-gated
// frame is refused with a notice when the guest proved no write token.
func (h *Host) handleGuestFrame(g *wsGuest, f Frame) {
	switch f.Type {
	case FrameHello:
		if tok, err := base64.RawURLEncoding.DecodeString(f.Token); err == nil &&
			len(tok) == TokenSize && subtle.ConstantTimeCompare(tok, h.room.Write) == 1 {
			h.mu.Lock()
			g.writable = true
			if f.Name != "" {
				g.name = sanitizeName(f.Name)
			}
			h.mu.Unlock()
			g.send(Frame{Type: FrameWelcome, RoomID: h.room.RoomID, Name: h.cfg.Name, Writable: true})
			h.notice(g.name + " has full control")
			return
		}
		if f.Name != "" {
			h.mu.Lock()
			g.name = sanitizeName(f.Name)
			h.mu.Unlock()
		}
		g.send(NoticeFrame("view-only link: this guest can read but not prompt, interrupt, or answer host prompts — share the full link to grant control"))
	case FramePrompt:
		if !h.writable(g) {
			h.refuse(g, "prompt")
			return
		}
		text := strings.TrimSpace(f.Text)
		if text == "" {
			return
		}
		if h.cfg.Backend.Prompt != nil {
			go h.cfg.Backend.Prompt(g.name, text)
		}
	case FrameAbort:
		if !h.writable(g) {
			h.refuse(g, "interrupt")
			return
		}
		if h.cfg.Backend.Interrupt != nil {
			go h.cfg.Backend.Interrupt()
		}
	case FrameUIResponse:
		if !h.writable(g) {
			h.refuse(g, "answer host prompts")
			return
		}
		if h.cfg.Backend.UIResponse != nil {
			go h.cfg.Backend.UIResponse(f.ID, f.Value)
		}
	default:
		h.logf("guest %s: unsupported frame %q ignored", g.addr, f.Type)
	}
}

func (h *Host) writable(g *wsGuest) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return g.writable
}

// refuse tells a guest that a write-gated request was rejected.
func (h *Host) refuse(g *wsGuest, what string) {
	g.send(NoticeFrame("view-only link: " + what + " is disabled — ask the host for the full link"))
}

// notice broadcasts a host notice to every guest.
func (h *Host) notice(text string) {
	h.BroadcastEvent(mustJSON(Notice{Kind: EventKindNotice, Text: text}))
}

// writePump seals and writes queued frames for one guest until it closes.
func (h *Host) writePump(g *wsGuest) {
	for {
		select {
		case <-g.done:
			return
		case f := <-g.out:
			blob, err := SealFrame(h.room.Key, h.room.RoomID, f)
			if err != nil {
				h.logf("seal frame: %v", err)
				g.close()
				return
			}
			if err := g.conn.WriteBinary(blob); err != nil {
				g.close()
				return
			}
		}
	}
}

// pollEntries diffs the host's entry lines and broadcasts new ones.
func (h *Host) pollEntries() {
	defer h.wg.Done()
	t := time.NewTicker(h.cfg.PollEvery)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
			h.syncEntries()
		}
	}
}

// syncEntries broadcasts entries the relay has not sent yet. The store is
// append-only (a rewind appends a branch marker), so an index diff is exact.
// ponytail: full re-marshal per tick is O(entries) every 250ms; a store
// append observer would replace both the poll and the diff.
func (h *Host) syncEntries() {
	lines := h.cfg.Entries()
	h.mu.Lock()
	if len(lines) < h.seen {
		h.seen = 0 // store swapped (new/fork/resume): restart the diff
	}
	pending := lines[h.seen:]
	h.seen = len(lines)
	h.mu.Unlock()
	for _, line := range pending {
		h.BroadcastEntry(line)
	}
}

func (h *Host) snapshot() []byte {
	if h.cfg.Backend.Snapshot == nil {
		return nil
	}
	return h.cfg.Backend.Snapshot()
}

func (h *Host) logf(format string, args ...any) {
	if h.cfg.Logf != nil {
		h.cfg.Logf(format, args...)
	}
}

// send queues a frame; a guest that cannot keep up is dropped rather than
// stalling the relay or corrupting its frame order.
func (g *wsGuest) send(f Frame) {
	select {
	case <-g.done:
		return
	default:
	}
	select {
	case g.out <- f:
	case <-g.done:
	default:
		// ponytail: 1024 queued frames (~64MB worst case of chunked
		// snapshot) is the ceiling for a slow guest; the upgrade path is
		// a byte-budgeted queue that pauses the snapshot instead of
		// dropping the guest.
		g.close()
	}
}

func (g *wsGuest) close() { g.once.Do(func() { close(g.done); _ = g.conn.Close() }) }

// sanitizeName keeps display names short and single-line.
func sanitizeName(n string) string {
	n = strings.Join(strings.Fields(n), " ")
	if len(n) > 32 {
		n = n[:32]
	}
	if n == "" {
		return "guest"
	}
	return n
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return raw
}
