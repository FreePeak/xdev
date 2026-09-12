package collab

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// GuestConfig configures a guest connection. The callbacks all run on the
// guest's read goroutine (Run), in frame order.
type GuestConfig struct {
	// Link is the parsed room to join (a full link can prompt; a view-only
	// link reads only).
	Link Link
	// Name is the display name sent to the host.
	Name string
	// ReplicaDir overrides where the transcript replica is written
	// (default ~/.xdev/collab).
	ReplicaDir string
	// OnWelcome fires on the handshake (and again when the host confirms
	// write permission).
	OnWelcome func(Frame)
	// OnSnapshot fires once with the reassembled back-transcript (session
	// JSONL, as written by the host).
	OnSnapshot func(data []byte)
	// OnEntry fires for each durable entry appended on the host.
	OnEntry func(line []byte)
	// OnEvent fires for live host events / notices.
	OnEvent func(raw json.RawMessage)
	// OnState fires for footer state snapshots.
	OnState func(raw json.RawMessage)
	// OnBus fires for mirrored subagent bus traffic.
	OnBus func(raw json.RawMessage)
	// OnAgents fires for agent-registry snapshots.
	OnAgents func(raw json.RawMessage)
	// OnUIRequest fires for host select/editor prompts (writable guests).
	OnUIRequest func(id string, req json.RawMessage)
	// OnClose reports the read loop ending (nil error = host closed).
	OnClose func(err error)
}

// Guest is a client replica of a host session: the back-transcript plus every
// entry the host appends afterwards. It renders nothing itself — the caller's
// callbacks decide (the TUI replays into its own transcript, the CLI mirrors
// to stdout).
type Guest struct {
	cfg      GuestConfig
	conn     *WSConn
	recon    *Reassembler
	replica  string
	mu       sync.Mutex
	writable bool
	closed   bool
	entries  [][]byte // replica contents, in arrival order
}

// Join connects to the room's relay, proves the write token (full links),
// and returns the live guest. The caller then runs Run.
func Join(ctx context.Context, cfg GuestConfig) (*Guest, error) {
	if cfg.Link.RoomID == "" || len(cfg.Link.Key) != KeySize {
		return nil, errors.New("collab: incomplete link (no room or key)")
	}
	if cfg.Link.Relay == "" {
		return nil, errors.New("collab: this link has no relay address — use the full link printed by /collab")
	}
	wsURL := cfg.Link.Relay + "/r/" + cfg.Link.Room() + "?role=guest"
	if cfg.Name != "" {
		wsURL += "&name=" + url.QueryEscape(cfg.Name)
	}
	if cfg.ReplicaDir == "" {
		cfg.ReplicaDir = ReplicaDir()
	}
	conn, err := DialWS(ctx, wsURL)
	if err != nil {
		return nil, err
	}
	g := &Guest{
		cfg:      cfg,
		conn:     conn,
		recon:    &Reassembler{},
		replica:  filepath.Join(cfg.ReplicaDir, cfg.Link.RoomID+".jsonl"),
		writable: cfg.Link.Full(),
	}
	// hello proves the write token; a view-only link sends none and the host
	// answers with a notice instead of write permission.
	f := Frame{Type: FrameHello, Name: cfg.Name}
	if cfg.Link.Full() {
		f.Token = base64.RawURLEncoding.EncodeToString(cfg.Link.Write)
	}
	blob, err := SealFrame(cfg.Link.Key, cfg.Link.RoomID, f)
	if err == nil {
		err = conn.WriteBinary(blob)
	}
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("collab: hello: %w", err)
	}
	return g, nil
}

// Run reads frames until the host closes the room or ctx is canceled. It
// returns nil for a clean host-side close. Call it once.
func (g *Guest) Run(ctx context.Context) error {
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			g.Close()
		case <-done:
		}
	}()
	var err error
	for {
		blob, rerr := g.conn.ReadBinary()
		if rerr != nil {
			// A clean host close (close frame / EOF) or a local Close is
			// not an error; anything else is reported to the caller.
			if !errors.Is(rerr, io.EOF) && ctx.Err() == nil && !g.isClosed() {
				err = rerr
			}
			break
		}
		f, oerr := OpenFrame(g.cfg.Link.Key, g.cfg.Link.RoomID, blob)
		if oerr != nil {
			// A frame that fails authentication is dropped, never
			// rendered: tamper detection is the point of the E2E layer.
			continue
		}
		g.dispatch(f)
	}
	if g.cfg.OnClose != nil {
		g.cfg.OnClose(err)
	}
	return err
}

// dispatch routes one decrypted frame to the caller's callbacks.
func (g *Guest) dispatch(f Frame) {
	switch f.Type {
	case FrameWelcome:
		g.mu.Lock()
		// Write permission is the link's write token confirmed by the
		// host: neither side alone can upgrade a view-only guest.
		g.writable = f.Writable && g.cfg.Link.Full()
		g.mu.Unlock()
		if g.cfg.OnWelcome != nil {
			g.cfg.OnWelcome(f)
		}
	case FrameSnapshot:
		data, complete, err := g.recon.Add(f.Index, f.Total, f.Chunk)
		if err != nil {
			return
		}
		if !complete {
			return
		}
		g.writeReplica(data)
		if g.cfg.OnSnapshot != nil {
			g.cfg.OnSnapshot(data)
		}
	case FrameEntry:
		line := []byte(f.Entry)
		if len(line) == 0 {
			return
		}
		g.appendReplica(line)
		if g.cfg.OnEntry != nil {
			g.cfg.OnEntry(line)
		}
	case FrameEvent:
		if g.cfg.OnEvent != nil {
			g.cfg.OnEvent(f.Event)
		}
	case FrameState:
		if g.cfg.OnState != nil {
			g.cfg.OnState(f.State)
		}
	case FrameBus:
		if g.cfg.OnBus != nil {
			g.cfg.OnBus(f.Bus)
		}
	case FrameAgents:
		if g.cfg.OnAgents != nil {
			g.cfg.OnAgents(f.Agents)
		}
	case FrameUIRequest:
		if g.cfg.OnUIRequest != nil {
			g.cfg.OnUIRequest(f.ID, f.Req)
		}
	}
}

// Prompt asks the host to run text as a prompt. A view-only link refuses
// locally (and the host refuses again server-side).
func (g *Guest) Prompt(text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if !g.Writable() {
		return errors.New("collab: this is a view-only link — prompting is disabled; ask the host for the full link")
	}
	return g.send(Frame{Type: FramePrompt, Text: text})
}

// Abort interrupts the host's in-flight turn (full links only).
func (g *Guest) Abort() error {
	if !g.Writable() {
		return errors.New("collab: this is a view-only link — interrupting is disabled")
	}
	return g.send(Frame{Type: FrameAbort})
}

// UIResponse settles a host ui-request (full links only).
func (g *Guest) UIResponse(id, value string) error {
	if !g.Writable() {
		return errors.New("collab: this is a view-only link — answering host prompts is disabled")
	}
	return g.send(Frame{Type: FrameUIResponse, ID: id, Value: value})
}

// Writable reports whether the host granted write permission (full link).
func (g *Guest) Writable() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.writable
}

// RoomID reports the joined room.
func (g *Guest) RoomID() string { return g.cfg.Link.RoomID }

// ReplicaPath is the on-disk transcript replica (~/.xdev/collab/<room>.jsonl).
func (g *Guest) ReplicaPath() string { return g.replica }

// EntryLines returns the entries received so far, in arrival order.
func (g *Guest) EntryLines() [][]byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([][]byte, len(g.entries))
	copy(out, g.entries)
	return out
}

// Close ends the connection.
func (g *Guest) Close() error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil
	}
	g.closed = true
	g.mu.Unlock()
	return g.conn.Close()
}

func (g *Guest) isClosed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closed
}

// send seals and writes one guest frame.
func (g *Guest) send(f Frame) error {
	blob, err := SealFrame(g.cfg.Link.Key, g.cfg.Link.RoomID, f)
	if err != nil {
		return err
	}
	return g.conn.WriteBinary(blob)
}

// writeReplica replaces the replica file with the host snapshot (0600 in a
// 0700 directory: the replica holds the whole transcript).
func (g *Guest) writeReplica(data []byte) {
	if g.replica == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(g.replica), 0o700); err != nil {
		return
	}
	if err := os.WriteFile(g.replica, data, 0o600); err != nil {
		return
	}
	g.mu.Lock()
	g.entries = g.entries[:0]
	for _, line := range splitLines(data) {
		g.entries = append(g.entries, line)
	}
	g.mu.Unlock()
}

// appendReplica streams one entry into the replica file.
func (g *Guest) appendReplica(line []byte) {
	g.mu.Lock()
	g.entries = append(g.entries, append([]byte(nil), line...))
	path := g.replica
	g.mu.Unlock()
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(append([]byte(nil), line...), '\n'))
}
