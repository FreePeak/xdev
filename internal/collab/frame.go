package collab

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
)

// Frame type discriminators (issue #59 §2). The host→guest half carries the
// session; the guest→host half carries requests the host applies.
const (
	// Host → guest.
	FrameWelcome   = "welcome"        // handshake: room, display name, write permission
	FrameSnapshot  = "snapshot-chunk" // one chunk of the back-transcript
	FrameEntry     = "entry"          // one durable session entry (JSONL line)
	FrameEvent     = "event"          // live host event / notice
	FrameState     = "state"          // footer state snapshot
	FrameBus       = "bus"            // mirrored subagent bus traffic
	FrameAgents    = "agents"         // agent-registry snapshot
	FrameUIRequest = "ui-request"     // host select/editor prompt for guests

	// Guest → host.
	FrameHello      = "hello"       // display name + write-token proof
	FramePrompt     = "prompt"      // prompt the host agent
	FrameAbort      = "abort"       // interrupt the host's in-flight turn
	FrameUIResponse = "ui-response" // settle a host ui-request
)

// EventKindNotice marks an event frame that carries a human-readable notice
// (guest joined/left, refused write from a view-only link, ...).
const EventKindNotice = "notice"

// Frame is one protocol message. Session payloads ride as raw JSON so the
// durable entry bytes reach the guest verbatim (ids and parent chain intact).
type Frame struct {
	Type   string `json:"type"`
	RoomID string `json:"roomId,omitempty"`
	Seq    int64  `json:"seq,omitempty"`

	// snapshot-chunk.
	Index int    `json:"index,omitempty"`
	Total int    `json:"total,omitempty"`
	Chunk []byte `json:"chunk,omitempty"`

	// payloads.
	Entry  json.RawMessage `json:"entry,omitempty"`
	Event  json.RawMessage `json:"event,omitempty"`
	State  json.RawMessage `json:"state,omitempty"`
	Bus    json.RawMessage `json:"bus,omitempty"`
	Agents json.RawMessage `json:"agents,omitempty"`
	Req    json.RawMessage `json:"request,omitempty"`

	// handshake / requests.
	Token    string `json:"token,omitempty"`
	Name     string `json:"name,omitempty"`
	Writable bool   `json:"writable,omitempty"`
	Text     string `json:"text,omitempty"`
	ID       string `json:"id,omitempty"`
	Value    string `json:"value,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Notice is the payload of an event frame whose kind is EventKindNotice.
type Notice struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// NoticeFrame builds an event frame carrying a human-readable notice.
func NoticeFrame(text string) Frame {
	raw, err := json.Marshal(Notice{Kind: EventKindNotice, Text: text})
	if err != nil {
		raw = []byte(`{"kind":"notice","text":"collab notice"}`)
	}
	return Frame{Type: FrameEvent, Event: raw}
}

// frameAAD binds a sealed frame to this protocol and this room, so a frame
// replayed into another room (or another protocol version) fails to open.
const frameAAD = "xdev-collab/v1"

// SealFrame encrypts one frame with the room key (AES-256-GCM). The wire
// blob is nonce||ciphertext: the relay sees only opaque bytes.
func SealFrame(key []byte, roomID string, f Frame) ([]byte, error) {
	plain, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("collab: encode frame: %w", err)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("collab: nonce: %w", err)
	}
	// ponytail: one random 96-bit nonce per frame. Safe far beyond any
	// realistic session (birthday bound ~2^32 frames per key); a counter
	// nonce would be the upgrade path if a room ever ran that long.
	return gcm.Seal(nonce, nonce, plain, aad(roomID)), nil
}

// OpenFrame decrypts and authenticates one frame. A modified ciphertext (or
// the wrong key/room) is a hard error, never partial plaintext.
func OpenFrame(key []byte, roomID string, blob []byte) (Frame, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return Frame{}, err
	}
	if len(blob) < gcm.NonceSize() {
		return Frame{}, errors.New("collab: frame shorter than its nonce")
	}
	nonce, ct := blob[:gcm.NonceSize()], blob[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, aad(roomID))
	if err != nil {
		return Frame{}, fmt.Errorf("collab: frame authentication failed: %w", err)
	}
	var f Frame
	if err := json.Unmarshal(plain, &f); err != nil {
		return Frame{}, fmt.Errorf("collab: decode frame: %w", err)
	}
	return f, nil
}

func aad(roomID string) []byte { return []byte(frameAAD + ":" + roomID) }

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("collab: room key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("collab: cipher: %w", err)
	}
	return cipher.NewGCM(block)
}
