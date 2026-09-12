package collab

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- frame codec ---

func TestFrameRoundTripAndTamper(t *testing.T) {
	room, err := NewRoom()
	if err != nil {
		t.Fatalf("NewRoom: %v", err)
	}
	sent := Frame{
		Type:  FrameEntry,
		Entry: json.RawMessage(`{"type":"message","id":"a1"}`),
		Name:  "guest1",
		Seq:   7,
	}
	blob, err := SealFrame(room.Key, room.RoomID, sent)
	if err != nil {
		t.Fatalf("SealFrame: %v", err)
	}
	got, err := OpenFrame(room.Key, room.RoomID, blob)
	if err != nil {
		t.Fatalf("OpenFrame: %v", err)
	}
	if got.Type != sent.Type || got.Name != sent.Name || got.Seq != sent.Seq || string(got.Entry) != string(sent.Entry) {
		t.Fatalf("round trip mismatch: %+v", got)
	}

	// Any modified ciphertext byte must fail authentication, never decrypt.
	for _, i := range []int{0, len(blob) / 2, len(blob) - 1} {
		tampered := append([]byte(nil), blob...)
		tampered[i] ^= 0x01
		if _, err := OpenFrame(room.Key, room.RoomID, tampered); err == nil {
			t.Fatalf("tampered blob (byte %d) opened without error", i)
		}
	}
	// Wrong room (AAD) and wrong key fail too.
	if _, err := OpenFrame(room.Key, "other-room", blob); err == nil {
		t.Fatal("frame opened under a different room id")
	}
	other, err := NewRoom()
	if err != nil {
		t.Fatalf("NewRoom: %v", err)
	}
	if _, err := OpenFrame(other.Key, room.RoomID, blob); err == nil {
		t.Fatal("frame opened under a different key")
	}
	if _, err := OpenFrame(room.Key, room.RoomID, blob[:4]); err == nil {
		t.Fatal("truncated blob opened")
	}
}

// --- snapshot chunking ---

func TestSnapshotChunkReassembly(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789"), 100) // 1000 bytes
	chunks := ChunkBytes(payload, 256)
	if len(chunks) != 4 {
		t.Fatalf("chunk count = %d, want 4", len(chunks))
	}
	if len(chunks[0]) != 256 || len(chunks[3]) != 232 {
		t.Fatalf("chunk sizes = %d/%d, want 256/232", len(chunks[0]), len(chunks[3]))
	}
	if got := ChunkBytes(nil, 0); got != nil {
		t.Fatalf("empty payload chunked into %d chunks", len(got))
	}

	// In order.
	r := &Reassembler{}
	var data []byte
	for i, c := range chunks {
		out, done, err := r.Add(i, len(chunks), c)
		if err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
		if done {
			data = out
		}
	}
	if !bytes.Equal(data, payload) {
		t.Fatalf("reassembled %d bytes, want %d", len(data), len(payload))
	}

	// Out of order, with one duplicate.
	r = &Reassembler{}
	order := []int{3, 1, 1, 0, 2}
	var done bool
	for _, i := range order {
		out, d, err := r.Add(i, len(chunks), chunks[i])
		if err != nil {
			t.Fatalf("out-of-order Add(%d): %v", i, err)
		}
		done, data = d, out
	}
	if !done || !bytes.Equal(data, payload) {
		t.Fatalf("out-of-order reassembly failed (done=%v, %d bytes)", done, len(data))
	}

	// Errors: bad index, changed total, conflicting duplicate, oversized total.
	if _, _, err := r.Add(0, 4, chunks[0]); err != nil {
		t.Fatalf("clean re-add after completion: %v", err)
	}
	if _, _, err := r.Add(9, 4, chunks[0]); err == nil {
		t.Fatal("out-of-range index accepted")
	}
	if _, _, err := r.Add(1, 5, chunks[1]); err == nil {
		t.Fatal("changed chunk total accepted")
	}
	r = &Reassembler{}
	if _, _, err := r.Add(1, 4, chunks[1]); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	if _, _, err := r.Add(1, 4, append([]byte(nil), chunks[1]...)); err != nil {
		t.Fatalf("identical duplicate rejected: %v", err)
	}
	if _, _, err := r.Add(1, 4, []byte("different")); err == nil {
		t.Fatal("conflicting duplicate accepted")
	}
	r = &Reassembler{}
	if _, _, err := r.Add(0, MaxSnapshot/DefaultChunkSize+1, chunks[0]); err == nil {
		t.Fatal("oversized snapshot total accepted")
	}
	// Byte cap: chunks larger than DefaultChunkSize must still be bounded.
	r = &Reassembler{}
	big := make([]byte, 1<<20)
	var err error
	for i := 0; i < 33; i++ {
		if _, _, err = r.Add(i, 40, big); err != nil {
			break
		}
	}
	if err == nil {
		t.Fatal("snapshot byte cap not enforced")
	}
}

// --- links ---

func TestLinkRoundTripAndParse(t *testing.T) {
	room, err := NewRoom()
	if err != nil {
		t.Fatalf("NewRoom: %v", err)
	}
	full := Link{Relay: "wss://relay.example", RoomID: room.RoomID, Key: room.Key, Write: room.Write}
	if !full.Full() {
		t.Fatal("full link reports view-only")
	}
	if !room.ViewOnly().Full() == false {
		t.Fatal("view-only link reports full control")
	}
	for _, raw := range []string{full.String(), full.Room()} {
		got, err := ParseLink(raw)
		if err != nil {
			t.Fatalf("ParseLink(%q): %v", raw, err)
		}
		if got.RoomID != full.RoomID || !bytes.Equal(got.Key, full.Key) || !bytes.Equal(got.Write, full.Write) {
			t.Fatalf("ParseLink(%q) mismatch: %+v", raw, got)
		}
	}

	view := room.ViewOnly()
	view.Relay = "ws://127.0.0.1:7575"
	got, err := ParseLink(view.String())
	if err != nil {
		t.Fatalf("ParseLink(view): %v", err)
	}
	if got.Full() || got.Write != nil {
		t.Fatalf("view-only link parsed as writable: %+v", got)
	}
	if !bytes.Equal(got.Key, room.Key) {
		t.Fatal("view-only link lost the room key")
	}
	// The secret really is 32 vs 48 bytes.
	if n := len(mustDecodeSecret(t, view.Secret())); n != KeySize {
		t.Fatalf("view-only secret = %d bytes, want %d", n, KeySize)
	}
	if n := len(mustDecodeSecret(t, full.Secret())); n != KeySize+TokenSize {
		t.Fatalf("full secret = %d bytes, want %d", n, KeySize+TokenSize)
	}
}

func TestParseLinkForms(t *testing.T) {
	room, err := NewRoom()
	if err != nil {
		t.Fatalf("NewRoom: %v", err)
	}
	tail := room.Room()
	legacy := room.RoomID + "#" + room.Secret()

	cases := []struct {
		in      string
		relay   string
		wantErr string
	}{
		{in: tail, relay: ""},
		{in: " " + tail + " ", relay: ""},
		{in: legacy, relay: ""},
		{in: strings.Replace(tail, ".", "%23", 1), relay: ""},
		{in: "relay.example/r/" + tail, relay: "wss://relay.example"},
		{in: "localhost:7575/r/" + tail, relay: "ws://localhost:7575"},
		{in: "ws://localhost:7575/r/" + tail, relay: "ws://localhost:7575"},
		{in: "ws://127.0.0.1:7575/r/" + tail, relay: "ws://127.0.0.1:7575"},
		{in: "wss://relay.example/r/" + tail, relay: "wss://relay.example"},
		{in: "https://relay.example/r/" + tail, relay: "wss://relay.example"},
		{in: "http://localhost:7575/r/" + tail, relay: "ws://localhost:7575"},
		{in: "https://127.0.0.1:7575/#" + tail, relay: "wss://127.0.0.1:7575"},
		{in: "https://web.example/collab/#relay.example/r/" + tail, relay: "wss://relay.example"},
		// Refusals.
		{in: "ws://relay.example/r/" + tail, wantErr: "loopback"},
		{in: "http://relay.example/r/" + tail, wantErr: "loopback"},
		{in: "", wantErr: "empty link"},
		{in: "just-a-word", wantErr: "not <roomId>.<secret>"},
		{in: "roomidroomid." + base64.RawURLEncoding.EncodeToString([]byte("too short")), wantErr: "room secret must be"},
		{in: "wss://relay.example/other/" + tail, wantErr: "not /r/"},
		{in: "ftp://relay.example/r/" + tail, wantErr: "unsupported relay scheme"},
	}
	for _, tc := range cases {
		got, err := ParseLink(tc.in)
		if tc.wantErr != "" {
			if err == nil {
				t.Errorf("ParseLink(%q) succeeded, want error containing %q", tc.in, tc.wantErr)
			} else if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("ParseLink(%q) error = %v, want %q", tc.in, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseLink(%q): %v", tc.in, err)
			continue
		}
		if got.Relay != tc.relay {
			t.Errorf("ParseLink(%q).Relay = %q, want %q", tc.in, got.Relay, tc.relay)
		}
		if got.RoomID != room.RoomID || !got.Full() {
			t.Errorf("ParseLink(%q) lost the room or token: %+v", tc.in, got)
		}
	}
}

func mustDecodeSecret(t *testing.T, s string) []byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decode secret: %v", err)
	}
	return raw
}

// --- host + guest integration over a real HTTP test server ---

// testHost starts a host relay on an httptest server and returns it with the
// guest link for the room.
func testHost(t *testing.T, backend Backend, entries func() [][]byte) (*Host, string) {
	t.Helper()
	h, err := NewHost(HostConfig{Name: "hostbox", Backend: backend, Entries: entries, PollEvery: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	// Hand the relay to httptest: the same http.Handler the real listener
	// serves, over a real TCP socket, so the guest exercises a real
	// WebSocket handshake and masked client frames.
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(func() {
		srv.Close()
		_ = h.Stop()
	})
	relay := "ws" + strings.TrimPrefix(srv.URL, "http")
	link, err := ParseLink(relay + "/r/" + h.Tail(true))
	if err != nil {
		t.Fatalf("ParseLink(host link): %v", err)
	}
	return h, link.String()
}

func TestHostGuestExchange(t *testing.T) {
	snapshot := []byte(`{"type":"message","id":"a1","message":{"role":"user","content":[{"type":"text","text":"hello from host"}]}}` + "\n")
	var (
		mu      sync.Mutex
		prompts []string
		aborts  int
	)
	h, linkRaw := testHost(t, Backend{
		Snapshot:  func() []byte { return snapshot },
		Prompt:    func(name, text string) { mu.Lock(); prompts = append(prompts, name+": "+text); mu.Unlock() },
		Interrupt: func() { mu.Lock(); aborts++; mu.Unlock() },
	}, nil)

	link, err := ParseLink(linkRaw)
	if err != nil {
		t.Fatalf("ParseLink: %v", err)
	}
	snapCh := make(chan []byte, 1)
	entryCh := make(chan []byte, 4)
	stateCh := make(chan json.RawMessage, 1)
	busCh := make(chan json.RawMessage, 1)
	g, err := Join(context.Background(), GuestConfig{
		Link:       link,
		Name:       "guest1",
		ReplicaDir: t.TempDir(),
		OnSnapshot: func(data []byte) { snapCh <- data },
		OnEntry:    func(line []byte) { entryCh <- line },
		OnState:    func(raw json.RawMessage) { stateCh <- raw },
		OnBus:      func(raw json.RawMessage) { busCh <- raw },
	})
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- g.Run(ctx) }()

	// 1. The back-transcript arrives (chunked, reassembled).
	select {
	case data := <-snapCh:
		if string(data) != string(snapshot) {
			t.Fatalf("snapshot = %q, want %q", data, snapshot)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the snapshot")
	}
	waitFor(t, "the host to confirm write permission", func() bool { return g.Writable() })
	waitFor(t, "host sees the participant", func() bool {
		for _, p := range h.Participants() {
			if p.Name == "guest1" && p.Writable {
				return true
			}
		}
		return false
	})

	// 2. A guest prompt reaches the host backend under the guest's name.
	if err := g.Prompt("please run the tests"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if err := g.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	waitFor(t, "host applied the prompt", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(prompts) == 1 && prompts[0] == "guest1: please run the tests" && aborts == 1
	})

	// 3. A durable host entry reaches the guest live and lands in the replica.
	entry := []byte(`{"type":"message","id":"a2","parentId":"a1","message":{"role":"assistant","content":[{"type":"text","text":"tests are green"}]}}`)
	h.BroadcastEntry(append(append([]byte(nil), entry...), '\n'))
	select {
	case got := <-entryCh:
		if !bytes.Equal(bytes.TrimSpace(got), entry) {
			t.Fatalf("entry = %s, want %s", got, entry)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the entry frame")
	}
	replica := g.ReplicaPath()
	waitFor(t, "replica holds the snapshot and the entry", func() bool {
		data, err := os.ReadFile(replica)
		return err == nil && bytes.Contains(data, []byte("hello from host")) && bytes.Contains(data, []byte("tests are green"))
	})
	if msgs := Messages(append(append([]byte(nil), snapshot...), append(entry, '\n')...)); len(msgs) != 2 {
		t.Fatalf("Messages() replayed %d messages, want 2", len(msgs))
	}
	if lines := MirrorLines(Messages(snapshot)); len(lines) != 1 || !strings.Contains(lines[0], "hello from host") {
		t.Fatalf("MirrorLines(snapshot) = %v", lines)
	}

	// 4. Non-entry frames pass through too (state, bus) — the guest replica
	//    reads the host's footer and subagent traffic natively.
	h.BroadcastState(json.RawMessage(`{"streaming":true}`))
	h.BroadcastBus(json.RawMessage(`{"kind":"subagent"}`))
	select {
	case raw := <-stateCh:
		if string(raw) != `{"streaming":true}` {
			t.Fatalf("state = %s", raw)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the state frame")
	}
	select {
	case raw := <-busCh:
		if string(raw) != `{"kind":"subagent"}` {
			t.Fatalf("bus = %s", raw)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the bus frame")
	}

	// 5. Entry frames keep ids intact (the replica is resumable by id).
	if !bytes.Contains(readFile(t, replica), []byte(`"id":"a2"`)) {
		t.Fatal("replica lost an entry id")
	}

	// 6. Closing the room ends the guest cleanly.
	cancel()
	if err := <-runErr; err != nil {
		t.Fatalf("Run after cancel: %v", err)
	}
}

func TestViewOnlyGuestIsRefused(t *testing.T) {
	var (
		mu      sync.Mutex
		prompts []string
	)
	h, linkRaw := testHost(t, Backend{
		Snapshot: func() []byte { return nil },
		Prompt:   func(name, text string) { mu.Lock(); prompts = append(prompts, text); mu.Unlock() },
	}, nil)
	full, err := ParseLink(linkRaw)
	if err != nil {
		t.Fatalf("ParseLink: %v", err)
	}
	notices := make(chan string, 8)
	g, err := Join(context.Background(), GuestConfig{
		Link:       full.ViewOnly(),
		Name:       "looker",
		ReplicaDir: t.TempDir(),
		OnEvent: func(raw json.RawMessage) {
			if txt := NoticeText(raw); txt != "" {
				notices <- txt
			}
		},
	})
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- g.Run(ctx) }()

	// The host handshake must say the guest is read-only, even though the
	// process holds a writable link's key. Notices also carry the join
	// announcement, so drain until the refusal shows up.
	waitFor(t, "view-only notice", func() bool {
		select {
		case txt := <-notices:
			return strings.Contains(txt, "view-only")
		default:
			return false
		}
	})
	if g.Writable() {
		t.Fatal("view-only link reports write permission")
	}
	for _, p := range h.Participants() {
		if p.Writable {
			t.Fatalf("host granted write permission to %+v", p)
		}
	}

	// The local guard refuses first ...
	if err := g.Prompt("nope"); err == nil || !strings.Contains(err.Error(), "view-only") {
		t.Fatalf("Prompt on a view-only link = %v, want a view-only refusal", err)
	}
	if err := g.Abort(); err == nil || !strings.Contains(err.Error(), "view-only") {
		t.Fatalf("Abort on a view-only link = %v, want a view-only refusal", err)
	}
	// ... and the host refuses a hand-crafted prompt frame that skips it.
	blob, err := SealFrame(full.Key, full.RoomID, Frame{Type: FramePrompt, Text: "sneaky"})
	if err != nil {
		t.Fatalf("SealFrame: %v", err)
	}
	if err := g.conn.WriteBinary(blob); err != nil {
		t.Fatalf("raw prompt frame: %v", err)
	}
	waitFor(t, "host refusal notice", func() bool {
		select {
		case txt := <-notices:
			return strings.Contains(txt, "prompt")
		default:
			return false
		}
	})
	time.Sleep(100 * time.Millisecond) // give a wrong host time to apply it
	mu.Lock()
	defer mu.Unlock()
	if len(prompts) != 0 {
		t.Fatalf("host applied a prompt from a view-only guest: %v", prompts)
	}
	cancel()
	<-runErr
}

func TestJoinRejectsWrongKey(t *testing.T) {
	_, linkRaw := testHost(t, Backend{Snapshot: func() []byte { return nil }}, nil)
	link, err := ParseLink(linkRaw)
	if err != nil {
		t.Fatalf("ParseLink: %v", err)
	}
	other, err := NewRoom()
	if err != nil {
		t.Fatalf("NewRoom: %v", err)
	}
	link.Key = other.Key // right room id, wrong key
	if _, err := Join(context.Background(), GuestConfig{Link: link, ReplicaDir: t.TempDir()}); err == nil {
		t.Fatal("joined a room with the wrong key")
	}
}

func TestListenRefusesRemoteBindWithoutOptIn(t *testing.T) {
	h, err := NewHost(HostConfig{Addr: "0.0.0.0:0", Backend: Backend{}})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	if _, err := h.ListenAndServe(); err == nil || !strings.Contains(err.Error(), "non-loopback") {
		t.Fatalf("ListenAndServe(0.0.0.0) = %v, want a non-loopback refusal", err)
	}
	if err := h.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestListenLoopbackAndEntryPolling(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"message","id":"a1","message":{"role":"user","content":[{"type":"text","text":"one"}]}}`),
	}
	var mu sync.Mutex
	h, err := NewHost(HostConfig{
		Backend:   Backend{Snapshot: func() []byte { return nil }},
		Entries:   func() [][]byte { mu.Lock(); defer mu.Unlock(); return lines },
		PollEvery: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	linkRaw, err := h.ListenAndServe()
	if err != nil {
		t.Fatalf("ListenAndServe: %v", err)
	}
	defer h.Stop()
	link, err := ParseLink(linkRaw)
	if err != nil {
		t.Fatalf("ParseLink(%q): %v", linkRaw, err)
	}
	// The default bind is loopback, and the printed link names it.
	if !strings.HasPrefix(linkRaw, "ws://127.0.0.1:") && !strings.HasPrefix(linkRaw, "ws://[::1]:") {
		t.Fatalf("relay %q is not loopback", linkRaw)
	}
	entryCh := make(chan []byte, 4)
	g, err := Join(context.Background(), GuestConfig{
		Link:       link,
		ReplicaDir: t.TempDir(),
		OnEntry:    func(line []byte) { entryCh <- line },
	})
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = g.Run(ctx) }()

	// The poller must not re-send an entry it already delivered.
	mu.Lock()
	lines = append(lines, []byte(`{"type":"message","id":"a2","message":{"role":"assistant","content":[{"type":"text","text":"two"}]}}`))
	mu.Unlock()
	var got [][]byte
	for len(got) < 2 {
		select {
		case line := <-entryCh:
			got = append(got, line)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out after %d entries", len(got))
		}
	}
	if !bytes.Contains(got[0], []byte(`"a1"`)) || !bytes.Contains(got[1], []byte(`"a2"`)) {
		t.Fatalf("polled entries = %s / %s", got[0], got[1])
	}
	select {
	case dup := <-entryCh:
		t.Fatalf("entry re-sent: %s", dup)
	case <-time.After(150 * time.Millisecond):
	}
}

// --- helpers ---

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
