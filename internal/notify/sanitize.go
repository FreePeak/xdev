// Package notify delivers a session-failure alert to the human through the
// terminal xdev is already drawing on.
//
// It exists because every portable local-notification helper (osascript,
// terminal-notifier, notify-send) writes to the machine running xdev, which
// over SSH or tmux is the wrong machine by definition. A TUI therefore has to
// notify through the terminal it owns.
//
// Nothing here may ever break a session: transports are dispatched off the
// caller's goroutine, every transport call is recovered and bounded, and a
// notification that cannot be delivered is dropped, not retried.
package notify

import "strings"

const (
	// TitleLimit caps an emitted title, matching the opencode attention API.
	TitleLimit = 80
	// BodyLimit caps an emitted body.
	BodyLimit = 240
)

// Sanitize makes text that a model or a failing child process produced safe to
// hand to a terminal.
//
// A notification body is attacker-adjacent by construction: it carries result
// text the session did not author. An escape sequence in that text is not
// cosmetic damage, it is a terminal command -- an OSC 52 payload rewrites the
// user's clipboard and a CSI sequence can move the cursor out of the TUI's
// control. Every C0/C1 control byte is therefore removed (ESC and BEL among
// them), runs of them collapse into one space, and the result is capped at
// limit runes.
//
// The escape is removed, not escaped: after the ESC byte is gone, the rest of
// a sequence such as "]52;c;..." is inert printable text.
func Sanitize(s string, limit int) string {
	var b strings.Builder
	b.Grow(len(s))
	pending := false // a control byte was seen and a space has not been written yet
	for _, r := range s {
		if isControl(r) {
			if b.Len() > 0 {
				pending = true
			}
			continue
		}
		if pending {
			b.WriteByte(' ')
			pending = false
		}
		b.WriteRune(r)
	}
	out := b.String()
	if limit > 0 {
		if r := []rune(out); len(r) > limit {
			out = string(r[:limit-1]) + "…"
		}
	}
	return out
}

// isControl reports whether r would be interpreted by a terminal rather than
// displayed: C0, DEL, and C1 (which many terminals honour directly).
func isControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}
