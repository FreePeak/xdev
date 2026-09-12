package theme

import (
	"errors"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
)

// errBadHex marks a reply component that is not hex.
var errBadHex = errors.New("bad hex")

// OSC 11 background detection (research F4): ask the terminal for its
// background color and pick the day/night polarity from the luminance of the
// answer, falling back to COLORFGBG when the terminal does not answer.
//
// The query is skipped unless stdout is a terminal, so tests and piped runs
// never wait on a reply nobody will send.

// backgroundQueryTimeout bounds the OSC 11 wait: a terminal answers in
// microseconds, and a terminal that does not understand the query never
// answers at all.
const backgroundQueryTimeout = 150 * time.Millisecond

// DetectBackground reports whether the terminal background is light. ok is
// false when no verdict was reached (not a terminal, unsupported, timeout),
// so the caller falls back to COLORFGBG.
func DetectBackground(timeout time.Duration) (light, ok bool) {
	if !interactiveTerminal() {
		return false, false
	}
	// The query talks to the controlling terminal rather than stdout: xdev
	// may be launched with stdout redirected while the TUI still owns the
	// tty (print mode is the common case).
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false, false
	}
	defer tty.Close()
	state, err := term.MakeRaw(int(tty.Fd()))
	if err != nil {
		return false, false
	}
	defer func() { _ = term.Restore(int(tty.Fd()), state) }()

	if err := tty.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return false, false // no deadline support: skip rather than block
	}
	if _, err := tty.WriteString("\x1b]11;?\x1b\\"); err != nil {
		return false, false
	}
	reply, err := readOSCReply(tty)
	if err != nil {
		return false, false
	}
	c, ok := parseOSC11(reply)
	if !ok {
		return false, false
	}
	return lightBackground(c), true
}

// interactiveTerminal reports whether stdout is a character device (a real
// terminal), not a pipe or a file.
func interactiveTerminal() bool {
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// readOSCReply reads one terminal reply: everything up to BEL or ST.
func readOSCReply(tty *os.File) (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := tty.Read(buf)
		if err != nil {
			return b.String(), err
		}
		if n == 0 {
			continue
		}
		if buf[0] == 0x07 { // BEL terminator
			return b.String(), nil
		}
		if buf[0] == 0x1b { // ESC: ST ("ESC \") or a fresh sequence
			b.WriteByte(buf[0])
			continue
		}
		if buf[0] == '\\' && strings.HasSuffix(b.String(), "\x1b") {
			return strings.TrimSuffix(b.String(), "\x1b"), nil
		}
		b.WriteByte(buf[0])
		if b.Len() > 256 { // a stray stream without a terminator
			return b.String(), nil
		}
	}
}

// parseOSC11 extracts the background color from an OSC 11 reply such as
// "rgb:1e1e/1e1e/1e1e" (1-4 hex digits per component) or "#1e1e1e".
func parseOSC11(reply string) (Color, bool) {
	body := reply
	if i := strings.Index(body, "11;"); i >= 0 {
		body = body[i+3:]
	}
	body = strings.TrimSpace(strings.TrimPrefix(body, "\x1b]"))
	// Drop the terminator (BEL or ST) the reply carried.
	if i := strings.IndexAny(body, "\x1b\x07"); i >= 0 {
		body = body[:i]
	}
	if strings.HasPrefix(body, "rgb:") {
		parts := strings.Split(strings.TrimPrefix(body, "rgb:"), "/")
		if len(parts) != 3 {
			return Color{}, false
		}
		var c Color
		for i, p := range parts {
			p = strings.TrimSpace(p)
			if len(p) == 0 || len(p) > 4 {
				return Color{}, false
			}
			v, err := hexToUint(p)
			if err != nil {
				return Color{}, false
			}
			// Scale 1-4 hex digits to a byte (ffff → 255, ff → 255).
			if len(p) < 2 {
				v = v*16 + v
			} else if len(p) > 2 {
				v >>= 4 * (len(p) - 2)
			}
			switch i {
			case 0:
				c.R = uint8(v)
			case 1:
				c.G = uint8(v)
			case 2:
				c.B = uint8(v)
			}
		}
		return c, true
	}
	if strings.HasPrefix(body, "#") && len(body) == 7 {
		v, err := hexToUint(body[1:])
		if err != nil {
			return Color{}, false
		}
		return Color{uint8(v >> 16), uint8(v >> 8), uint8(v)}, true
	}
	return Color{}, false
}

func hexToUint(s string) (uint32, error) {
	var v uint32
	for _, ch := range s {
		var d uint32
		switch {
		case ch >= '0' && ch <= '9':
			d = uint32(ch - '0')
		case ch >= 'a' && ch <= 'f':
			d = uint32(ch-'a') + 10
		case ch >= 'A' && ch <= 'F':
			d = uint32(ch-'A') + 10
		default:
			return 0, errBadHex
		}
		v = v<<4 | d
	}
	return v, nil
}

// lightBackground reports whether a background color reads as light (Rec.601
// luminance above half). Approximation is fine: this only selects between
// two palettes.
func lightBackground(c Color) bool {
	lum := 0.2126*float64(c.R) + 0.7152*float64(c.G) + 0.0722*float64(c.B)
	return lum > 127.5
}
