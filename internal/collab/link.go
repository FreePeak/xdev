// Package collab implements live session sharing (M14 #59): the host serves
// its session over an in-process WebSocket relay with E2E-encrypted frames,
// and guests build a native replica of the host transcript in their own TUI.
//
// Trust model: the room secret is base64url(key || optional write token),
// where key is the 32-byte AES-256-GCM room key and the token is the 16-byte
// proof that grants prompting. A 48-byte secret (full link) reads and steers;
// a 32-byte secret (view-only link) reads only. Every frame is sealed with the
// room key before it reaches the socket, so the relay moves opaque bytes.
package collab

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Sizes of the room secret: the AES-256-GCM key, plus the optional write
// token that upgrades a view-only link to full control.
const (
	KeySize   = 32
	TokenSize = 16
)

// Link is a parsed collab link: the relay to reach, the room, the room key,
// and (full links only) the write token.
type Link struct {
	Relay  string // ws://host:port or wss://host:port; "" when the link is bare
	RoomID string
	Key    []byte
	Write  []byte // nil for a view-only link
}

// Full reports whether the link carries the write token (full control).
func (l Link) Full() bool { return len(l.Write) == TokenSize }

// Secret is the base64url room secret: 48 bytes (key||token) for a full
// link, 32 bytes (key) for a view-only link.
func (l Link) Secret() string {
	raw := make([]byte, 0, KeySize+TokenSize)
	raw = append(raw, l.Key...)
	if l.Full() {
		raw = append(raw, l.Write...)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// Room is "<roomId>.<secret>": the portable link tail.
func (l Link) Room() string { return l.RoomID + "." + l.Secret() }

// ViewOnly strips the write token (same room, read access only).
func (l Link) ViewOnly() Link { return Link{Relay: l.Relay, RoomID: l.RoomID, Key: l.Key} }

// String renders the shareable link: a full relay URL when the relay is
// known, otherwise the bare room tail. A view-only link prints the same way
// with a 32-byte secret.
func (l Link) String() string {
	if l.Relay == "" {
		return l.Room()
	}
	return strings.TrimSuffix(l.Relay, "/") + "/r/" + l.Room()
}

// NewRoom mints a room id, key, and write token (the relay is filled in by
// the host once it listens).
func NewRoom() (Link, error) {
	id := make([]byte, 16)
	key := make([]byte, KeySize)
	tok := make([]byte, TokenSize)
	for _, b := range [][]byte{id, key, tok} {
		if _, err := rand.Read(b); err != nil {
			return Link{}, fmt.Errorf("collab: generate room: %w", err)
		}
	}
	return Link{RoomID: base64.RawURLEncoding.EncodeToString(id), Key: key, Write: tok}, nil
}

// ParseLink accepts the documented link forms:
//
//	<roomId>.<secret>                  bare tail (no relay; in-process only)
//	<roomId>#<secret>                  legacy bare tail
//	host[:port]/r/<roomId>.<secret>    custom relay, wss:// inferred
//	https://host[:port]/r/<roomId>.<secret>
//	wss://host[:port]/r/<roomId>.<secret>
//	ws://localhost:7475/r/<roomId>.<secret>   plain ws: loopback only
//	https://host[:port]/#<roomId>.<secret>    browser deep link
//	https://web/#<relay-host>/r/<roomId>.<secret>
//
// The fragment wins over the HTTP host, so a web-UI deep link joins the relay
// it names instead of the web host.
func ParseLink(raw string) (Link, error) {
	s := strings.Trim(strings.TrimSpace(raw), `"'`)
	if s == "" {
		return Link{}, errors.New("collab: empty link")
	}
	// Legacy deep links arrive %23-mangled; normalize before splitting.
	s = strings.ReplaceAll(s, "%23", "#")

	if i := strings.Index(s, "://"); i >= 0 {
		u, err := url.Parse(s)
		if err != nil {
			return Link{}, fmt.Errorf("collab: bad link %q: %w", raw, err)
		}
		if frag := u.Fragment; frag != "" {
			// A complete relay link in the fragment wins (web UI and relay
			// on different hosts); otherwise the fragment is a bare tail
			// whose relay is the URL's own host.
			if strings.Contains(frag, "/r/") {
				return ParseLink(frag)
			}
			tailRoom, tailSecret, err := splitTail(frag)
			if err != nil {
				return Link{}, err
			}
			relay, err := relayFor(u.Scheme, u.Host)
			if err != nil {
				return Link{}, err
			}
			return newLink(relay, tailRoom, tailSecret)
		}
		relay, err := relayFor(u.Scheme, u.Host)
		if err != nil {
			return Link{}, err
		}
		room, secret, err := splitRelayPath(u.Path)
		if err != nil {
			return Link{}, err
		}
		return newLink(relay, room, secret)
	}

	// host[:port]/r/<tail> — wss:// is inferred (plain ws is loopback-only,
	// so the host must be explicit about it with a ws:// URL).
	if i := strings.Index(s, "/"); i >= 0 {
		host := s[:i]
		if host == "" {
			return Link{}, fmt.Errorf("collab: bad link %q", raw)
		}
		relay, err := relayFor("", host)
		if err != nil {
			return Link{}, err
		}
		room, secret, err := splitRelayPath(s[i:])
		if err != nil {
			return Link{}, err
		}
		return newLink(relay, room, secret)
	}

	// Bare tail: key is the rightmost "." (base64url cannot contain one).
	room, secret, err := splitTail(s)
	if err != nil {
		return Link{}, err
	}
	return newLink("", room, secret)
}

// splitRelayPath extracts "<roomId>.<secret>" from a "/r/<tail>" path.
func splitRelayPath(path string) (roomID, secret string, err error) {
	tail, ok := strings.CutPrefix(strings.Trim(path, "/"), "r/")
	if !ok || tail == "" {
		return "", "", fmt.Errorf("collab: relay path %q is not /r/<roomId>.<secret>", path)
	}
	return splitTail(tail)
}

// splitTail splits the room tail into its room id and base64url secret.
func splitTail(tail string) (roomID, secret string, err error) {
	i := strings.IndexAny(tail, ".#")
	if i <= 0 || i == len(tail)-1 {
		return "", "", fmt.Errorf("collab: %q is not <roomId>.<secret>", tail)
	}
	roomID = tail[:i]
	if !validRoomID(roomID) {
		return "", "", fmt.Errorf("collab: bad room id %q", roomID)
	}
	return roomID, tail[i+1:], nil
}

// validRoomID accepts the base64url charset the host mints.
func validRoomID(id string) bool {
	if len(id) < 8 || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// newLink decodes the secret and enforces the two legal strengths.
func newLink(relay, roomID, secret string) (Link, error) {
	raw, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil {
		// Tolerate padded input (`=` is legal base64url but not emitted).
		raw, err = base64.URLEncoding.DecodeString(secret)
		if err != nil {
			return Link{}, fmt.Errorf("collab: bad room secret: %w", err)
		}
	}
	switch len(raw) {
	case KeySize:
		return Link{Relay: relay, RoomID: roomID, Key: raw}, nil
	case KeySize + TokenSize:
		return Link{Relay: relay, RoomID: roomID, Key: raw[:KeySize], Write: raw[KeySize:]}, nil
	default:
		return Link{}, fmt.Errorf("collab: room secret must be %d (view-only) or %d (full) bytes, got %d",
			KeySize, KeySize+TokenSize, len(raw))
	}
}

// relayFor normalizes a scheme+host into a websocket base URL. Plain ws is
// refused for non-loopback hosts: an unencrypted relay leaks frame sizes and
// metadata that the E2E layer cannot hide.
func relayFor(scheme, host string) (string, error) {
	if host == "" {
		return "", errors.New("collab: link has no relay host")
	}
	switch scheme {
	case "wss", "https":
		return "wss://" + host, nil
	case "http":
		if !IsLoopback(host) {
			return "", fmt.Errorf("collab: plain http relay %q must be loopback; use https://", host)
		}
		return "ws://" + host, nil
	case "ws":
		if !IsLoopback(host) {
			return "", fmt.Errorf("collab: plain ws relay %q must be loopback; use wss://", host)
		}
		return "ws://" + host, nil
	case "":
		if IsLoopback(host) {
			return "ws://" + host, nil
		}
		return "wss://" + host, nil
	default:
		return "", fmt.Errorf("collab: unsupported relay scheme %q", scheme)
	}
}

// IsLoopback reports whether host (with optional port) resolves to a loopback
// address by name. The relay binds loopback by default; a non-loopback bind
// is an explicit opt-in.
func IsLoopback(host string) bool {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.Trim(h, "[]")
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
