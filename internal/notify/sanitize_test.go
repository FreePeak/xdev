package notify

import (
	"strings"
	"testing"
)

// TestSanitizeDefeatsEscapeInjection is the F6 evidence: result text carrying a
// terminal command must emit as inert single-line text.
func TestSanitizeDefeatsEscapeInjection(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"osc52 clipboard write", "\x1b]52;c;cGF3bg==\x07", "]52;c;cGF3bg=="},
		{"embedded newlines", "line one\nline two\r\nline three", "line one line two line three"},
		{"tabs and nul", "a\tb\x00c", "a b c"},
		// Each control byte leaves one space behind, so removing two escapes
		// that bracket printable text cannot glue the text together.
		{"csi erase", "\x1b[2J\x1b[H", "[2J [H"},
		{"c1 control", "x\u0085y", "x y"},
		{"leading control", "\x1b]9;hi", "]9;hi"},
		{"plain text untouched", "subagent failed: exit 1", "subagent failed: exit 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Sanitize(tc.in, 0)
			if got != tc.want {
				t.Fatalf("Sanitize(%q) = %q, want %q", tc.in, got, tc.want)
			}
			for i := 0; i < len(got); i++ {
				if b := got[i]; b < 0x20 || b == 0x7f {
					t.Fatalf("Sanitize(%q) left control byte %#x in %q", tc.in, b, got)
				}
			}
		})
	}
}

func TestSanitizeCaps(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := Sanitize(long, TitleLimit)
	if n := len([]rune(got)); n != TitleLimit {
		t.Fatalf("capped title has %d runes, want %d", n, TitleLimit)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated title %q should end with an ellipsis marker", got)
	}
	if got := Sanitize(long, 0); len(got) != 500 {
		t.Fatalf("limit 0 should not truncate, got %d bytes", len(got))
	}
}

func TestEventSanitizedAppliesBothLimits(t *testing.T) {
	ev := Event{
		Title: strings.Repeat("t", 200),
		Body:  "\x1b]52;c;x\x07" + strings.Repeat("b", 400),
		JobID: "job-1",
	}.Sanitized()
	if len([]rune(ev.Title)) != TitleLimit {
		t.Fatalf("title not capped: %d runes", len([]rune(ev.Title)))
	}
	if len([]rune(ev.Body)) != BodyLimit {
		t.Fatalf("body not capped: %d runes", len([]rune(ev.Body)))
	}
	if strings.ContainsAny(ev.Body, "\x1b\x07") {
		t.Fatalf("body still carries control bytes: %q", ev.Body)
	}
	if ev.JobID != "job-1" {
		t.Fatalf("identifiers must survive sanitizing, got %q", ev.JobID)
	}
}
