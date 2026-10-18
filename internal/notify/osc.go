package notify

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// OSCKind is one rung of the terminal-notification ladder.
type OSCKind string

const (
	// OSC777 is the rxvt-unicode/kitty form, supported by kitty, VTE (GNOME
	// Terminal, Tilix) and rxvt-unicode.
	OSC777 OSCKind = "777"
	// OSC9 is the iTerm2 form, supported by iTerm2, WezTerm, mintty, ConEmu
	// and Windows Terminal.
	OSC9 OSCKind = "9"
	// OSCBell is the universal fallback: a bare BEL, which every terminal
	// rings, including Apple Terminal, which honours no notification escape.
	OSCBell OSCKind = "bell"
)

// DetectOSC picks the strongest notification escape the environment claims to
// understand.
//
// There is no way to ask a terminal what it supports, so the terminal's own
// identifying variables are the only probe available. The ladder is strictly
// ordered 777, then 9, then BEL: BEL is always a valid answer and is always
// last, so an unrecognised terminal degrades to a bell rather than to silence.
//
// Only one rung is chosen, never several: OSC 777 and OSC 9 both raise a
// notification in kitty, so emitting the whole ladder would notify twice.
func DetectOSC(getenv func(string) string) OSCKind {
	if getenv == nil {
		getenv = os.Getenv
	}
	term := strings.ToLower(getenv("TERM"))
	prog := getenv("TERM_PROGRAM")
	switch {
	case getenv("KITTY_WINDOW_ID") != "",
		strings.Contains(term, "kitty"),
		getenv("VTE_VERSION") != "",
		strings.Contains(term, "vte"),
		strings.Contains(term, "gnome"),
		strings.Contains(term, "rxvt"):
		return OSC777
	case prog == "iTerm.app",
		prog == "WezTerm",
		prog == "WarpTerminal",
		prog == "vscode",
		prog == "mintty",
		strings.Contains(term, "mintty"):
		return OSC9
	default:
		return OSCBell
	}
}

// OSCSink raises a desktop notification through the terminal xdev is drawing
// on, which is the only notification channel that follows the user to the
// machine they are actually looking at.
//
// A sink with no writer, or one whose terminal understands nothing, is inert:
// Emit returns nil and writes nothing, so a non-interactive run can hold one
// without emitting a byte (F8).
type OSCSink struct {
	w    io.Writer
	kind OSCKind
	seq  func(Event) string
}

// NewOSCSink builds a sink over w, choosing its rung from the environment.
//
// w is normally the controlling tty. A nil w yields a sink that does nothing.
func NewOSCSink(w io.Writer) *OSCSink {
	return NewOSCSinkKind(w, DetectOSC(os.Getenv))
}

// NewOSCSinkKind builds a sink that always uses kind.
func NewOSCSinkKind(w io.Writer, kind OSCKind) *OSCSink {
	s := &OSCSink{w: w, kind: kind}
	switch kind {
	case OSC777:
		s.seq = seq777
	case OSC9:
		s.seq = seq9
	default:
		s.kind = OSCBell
		s.seq = seqBell
	}
	return s
}

// Enabled reports whether the sink will write anything, so callers can skip the
// work of describing an event at all.
func (s *OSCSink) Enabled() bool {
	return s != nil && s.w != nil && s.seq != nil
}

// Emit writes the notification escape. A write error is returned for the
// dispatcher to log and drop; nothing is retried.
func (s *OSCSink) Emit(ev Event) error {
	if !s.Enabled() {
		return nil
	}
	_, err := io.WriteString(s.w, s.seq(ev.Sanitized()))
	return err
}

// Sequence returns the exact bytes Emit would write, for tests and for wiring
// code that would rather hand the escape to the TUI's own screen writer.
func (s *OSCSink) Sequence(ev Event) string {
	if s == nil || s.seq == nil {
		return ""
	}
	return s.seq(ev.Sanitized())
}

func seq777(ev Event) string {
	return fmt.Sprintf("\x1b]777;notify;%s;%s\a", ev.Title, ev.Body)
}

func seq9(ev Event) string {
	return fmt.Sprintf("\x1b]9;%s\a", oneLine(ev.Title, ev.Body))
}

func seqBell(Event) string { return "\a" }

// oneLine joins the two fields a single-string escape has to carry.
func oneLine(title, body string) string {
	if title == "" {
		return body
	}
	if body == "" {
		return title
	}
	return title + " - " + body
}
