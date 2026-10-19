package notify

import (
	"bytes"
	"strings"
	"testing"
)

// TestDetectOSCLadder pins the detection order: the strongest escape the
// environment claims, and never silence.
func TestDetectOSCLadder(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want OSCKind
	}{
		{"kitty by window id", map[string]string{"KITTY_WINDOW_ID": "1"}, OSC777},
		{"kitty by TERM", map[string]string{"TERM": "xterm-kitty"}, OSC777},
		{"vte", map[string]string{"TERM": "xterm-256color", "VTE_VERSION": "7000"}, OSC777},
		{"gnome", map[string]string{"TERM": "gnome-256color"}, OSC777},
		{"rxvt", map[string]string{"TERM": "rxvt-unicode-256color"}, OSC777},
		{"iterm2", map[string]string{"TERM_PROGRAM": "iTerm.app"}, OSC9},
		{"wezterm", map[string]string{"TERM_PROGRAM": "WezTerm"}, OSC9},
		{"mintty", map[string]string{"TERM": "xterm", "TERM_PROGRAM": "mintty"}, OSC9},
		{"apple terminal falls back", map[string]string{"TERM_PROGRAM": "Apple_Terminal"}, OSCBell},
		{"dumb terminal falls back", map[string]string{"TERM": "dumb"}, OSCBell},
		{"nothing set", map[string]string{}, OSCBell},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			get := func(k string) string { return tc.env[k] }
			if got := DetectOSC(get); got != tc.want {
				t.Fatalf("DetectOSC(%v) = %q, want %q", tc.env, got, tc.want)
			}
		})
	}
}

// TestOSCSequencePrefixes pins the wire format of each rung.
func TestOSCSequencePrefixes(t *testing.T) {
	cases := []struct {
		kind   OSCKind
		prefix string
		suffix string
	}{
		{OSC777, "\x1b]777;notify;", "\a"},
		{OSC9, "\x1b]9;", "\a"},
		{OSCBell, "\a", ""},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			var buf bytes.Buffer
			s := NewOSCSinkKind(&buf, tc.kind)
			if !s.Enabled() {
				t.Fatal("sink with a writer should be enabled")
			}
			if err := s.Emit(Event{Title: "job failed", Body: "exit 1"}); err != nil {
				t.Fatalf("Emit: %v", err)
			}
			got := buf.String()
			if !strings.HasPrefix(got, tc.prefix) {
				t.Fatalf("%s sequence %q does not start with %q", tc.kind, got, tc.prefix)
			}
			if !strings.HasSuffix(got, tc.suffix) {
				t.Fatalf("%s sequence %q does not end with %q", tc.kind, got, tc.suffix)
			}
		})
	}
}

// TestOSCSinkEmitsSanitizedText: the escape is the last thing between result
// text and a terminal, so it is where injection has to die.
func TestOSCSinkEmitsSanitizedText(t *testing.T) {
	var buf bytes.Buffer
	s := NewOSCSinkKind(&buf, OSC777)
	if err := s.Emit(Event{
		Title: "failure",
		Body:  "\x1b]52;c;cGF3bg==\x07\nsecond line",
	}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	got := buf.String()
	// Exactly two ESC bytes may survive: the one starting our own escape and
	// none from the payload.
	if n := strings.Count(got, "\x1b"); n != 1 {
		t.Fatalf("emitted %d ESC bytes, want 1 own: %q", n, got)
	}
	if n := strings.Count(got, "\a"); n != 1 {
		t.Fatalf("emitted %d BEL bytes, want 1 own: %q", n, got)
	}
	if strings.Contains(got, "52;c;") {
		// The digits may appear, but never with a live ESC in front of them.
		if strings.Contains(got, "\x1b]52") {
			t.Fatalf("clipboard-write escape survived: %q", got)
		}
	}
}

// TestOSCSinkWithoutWriterIsInert is the F8 promise: a non-interactive run holds
// a sink that writes nothing.
func TestOSCSinkWithoutWriterIsInert(t *testing.T) {
	s := NewOSCSink(nil)
	if s.Enabled() {
		t.Fatal("sink without a writer must report disabled")
	}
	if err := s.Emit(Event{Title: "x"}); err != nil {
		t.Fatalf("inert sink returned %v, want nil", err)
	}
	if got := s.Sequence(Event{Title: "x"}); got == "" {
		t.Fatal("Sequence should still describe what would be written")
	}
}
