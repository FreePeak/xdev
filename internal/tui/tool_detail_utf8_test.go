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
// bytes and then at the first newline with a " …", which meant the one thing a
// user opens the transcript to read was the one thing it hid.
func TestToolDetailReturnsTheWholeCommand(t *testing.T) {
	cmd := strings.Repeat("a", 5000)
	raw := `{"command":"` + cmd + `"}`
	if got := toolDetail(raw); got != cmd {
		t.Fatalf("detail = %d bytes (%q…), want all %d", len(got), got[:min(40, len(got))], len(cmd))
	}
}

// Every line of it, and the lines stay lines: a heredoc's body is where a
// command says what it is doing, and flattening it to one row lost that. The
// row WRAPS this (blockLines, case KindTool), so a second line costs a row, not
// its text — and the row budget belongs to the transcript, not to the tool.
func TestToolDetailKeepsEveryLine(t *testing.T) {
	raw, _ := json.Marshal(map[string]string{"command": "cat <<'EOF'\nline one\nline two\nEOF"})
	got := toolDetail(string(raw))
	if got != "cat <<'EOF'\nline one\nline two\nEOF" {
		t.Fatalf("detail = %q, want the command whole, lines and all", got)
	}
	if strings.Contains(got, "…") {
		t.Fatalf("a command that fits the row must not be cut: %q", got)
	}

	// And the row it paints carries every line, not just the first.
	app, _ := newTestApp(t, 200, 40)
	app.AddToolBlock("", "bash", string(raw))
	app.mu.Lock()
	painted := joinedLines(app.blockLines(0, app.blocks[0], 160))
	app.mu.Unlock()
	for _, want := range []string{"cat <<'EOF'", "line one", "line two", "EOF"} {
		if !strings.Contains(painted, want) {
			t.Fatalf("the call row lost %q:\n%s", want, painted)
		}
	}
}
