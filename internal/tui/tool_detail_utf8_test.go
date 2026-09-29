package tui

import (
	"encoding/json"
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
// exactly this shape. The 400-byte window this row used to carry was the
// second source — it cut wherever byte 400 happened to fall, which is inside a
// rune for any non-ASCII command. The window is gone; the row wraps instead,
// and the wrap is rune-safe, so the only tear left to stop is the raw bytes.
func TestToolDetailNeverReturnsTornUTF8(t *testing.T) {
	cases := []struct{ name, raw string }{
		// The bytes-only tear: after json decoding.
		{"torn in command", `{"command":"` + strings.Repeat("a", 10) + "\xe1" + strings.Repeat("b", 10) + `"}`},
		// A rune straddling whatever a byte-indexed cut would have used.
		{"2-byte past 399", `{"command":"` + strings.Repeat("a", 399) + "ệ" + `"}`},
		{"3-byte past 399", `{"command":"` + strings.Repeat("a", 399) + "ệ" + `"}`},
		{"4-byte past 399", `{"command":"` + strings.Repeat("a", 399) + "\U0001F600" + `"}`},
		{"wide runes", `{"command":"` + strings.Repeat("中", 300) + `"}`},
		// The wrap boundary, which is where a naive cut would tear one.
		{"wide runes at wrap width", `{"command":"` + strings.Repeat("中", 40) + `"}`},
		// No JSON at all: the flattened fallback path.
		{"unparseable torn", strings.Repeat("ế", 500) + "\xe1"},
	}
	for _, c := range cases {
		if got := toolDetail(c.raw); !utf8.ValidString(got) {
			t.Errorf("%s: toolDetail returned invalid UTF-8: %q", c.name, got)
		}
		// And through the row that renders it, at every width a terminal can
		// be: the wrap is the one place a row is cut now, so every segment it
		// produces must be whole runes.
		b := &Block{Kind: KindTool, ToolName: "bash", Text: c.raw}
		for _, w := range []int{200, 60, 24, 12} {
			_, detail := toolSummary(b)
			for _, seg := range wrap(detail, max(10, w-width("◈ bash")-3)) {
				if !utf8.ValidString(seg) {
					t.Errorf("%s at width %d: row segment invalid: %q", c.name, w, seg)
				}
			}
		}
	}
}

// The headline: a long command is no longer shortened. It used to stop at 400
// bytes and then at the row width, both with an ellipsis, which meant the one
// thing a user opens the transcript to read was the one thing it hid.
func TestToolDetailReturnsTheWholeFirstLine(t *testing.T) {
	cmd := strings.Repeat("a", 5000)
	raw := `{"command":"` + cmd + `"}`
	if got := toolDetail(raw); got != cmd {
		t.Fatalf("detail = %d bytes (%q…), want all %d", len(got), got[:min(40, len(got))], len(cmd))
	}
}

// First line only, still: a `write` body or a heredoc does not become the row.
func TestToolDetailKeepsOnlyTheFirstLine(t *testing.T) {
	raw, _ := json.Marshal(map[string]string{"command": "cat <<'EOF'\nline one\nline two\nEOF"})
	got := toolDetail(string(raw))
	if !strings.HasPrefix(got, "cat <<'EOF' …") {
		t.Fatalf("detail = %q, want the first line and an ellipsis", got)
	}
	if strings.Contains(got, "line two") {
		t.Fatalf("a multi-line argument leaked past its first line: %q", got)
	}
}
