package tui

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A call row is the one render path with no sanitizer behind it: whatever
// toolDetail returns goes straight into SetContent. It must therefore be valid
// UTF-8 itself — a lone continuation byte is not a rune, and every consumer
// downstream (width math, wrap, the terminal) assumes runes.
//
// The tear is not hypothetical: JSON gives no guarantee that a model-written
// command is well-formed, and a command the model built by slicing bytes holds
// exactly this shape. The 400-byte window is the second source — it used to
// cut wherever byte 400 happened to fall, which is inside a rune for any
// non-ASCII command.
func TestToolDetailNeverReturnsTornUTF8(t *testing.T) {
	cases := []struct{ name, raw string }{
		// The bytes-only tear: before the window, after json decoding.
		{"torn in command", `{"command":"` + strings.Repeat("a", 10) + "\xe1" + strings.Repeat("b", 10) + `"}`},
		// The window itself, landing inside a 2/3/4-byte rune.
		{"window splits 2-byte", `{"command":"` + strings.Repeat("a", 399) + "ệ" + `"}`},
		{"window splits 3-byte", `{"command":"` + strings.Repeat("a", 398) + "ệ" + `"}`},
		{"window splits 4-byte", `{"command":"` + strings.Repeat("a", 398) + "\U0001F600" + `"}`},
		{"wide runes", `{"command":"` + strings.Repeat("中", 300) + `"}`},
		// No JSON at all: the flattened fallback path.
		{"unparseable torn", strings.Repeat("ế", 500) + "\xe1"},
	}
	for _, c := range cases {
		if got := toolDetail(c.raw); !utf8.ValidString(got) {
			t.Errorf("%s: toolDetail returned invalid UTF-8: %q", c.name, got)
		}
		// And through the row's own split, which truncates a second time.
		b := &Block{Kind: KindTool, ToolName: "bash", Text: c.raw}
		for _, w := range []int{200, 60, 24, 12} {
			if _, detail := toolSummary(b, w); !utf8.ValidString(detail) {
				t.Errorf("%s at width %d: row detail invalid: %q", c.name, w, detail)
			}
		}
	}
}

// The cap still caps: sanitizing must not hand the row an unbounded phrase.
func TestToolDetailStillBoundsThePhrase(t *testing.T) {
	raw := `{"command":"` + strings.Repeat("a", 5000) + `"}`
	if got := toolDetail(raw); len(got) > 400 {
		t.Fatalf("detail = %d bytes, want the 400-byte window", len(got))
	}
}
