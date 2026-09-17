package tui

import (
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/theme"
)

// testBox is the shipped rounded outline, spelled out so the test does not
// depend on which symbol preset the active theme resolves to.
var testBox = theme.BoxChars{
	TopLeft: "\u256d", TopRight: "\u256e",
	BottomLeft: "\u2570", BottomRight: "\u256f",
	Horizontal: "\u2500", Vertical: "\u2502",
}

// TestBoxSelectableStripsTheFrame pins the copy geometry of a boxed surface: a
// border and the pad cells inside it are dropped, the column the copy starts at
// moves with them, a rule row keeps only the label set into it, and a bare rule
// contributes nothing. x0 is the half a content-only assertion misses — the
// highlight and the copy both measure from it.
func TestBoxSelectableStripsTheFrame(t *testing.T) {
	for _, tc := range []struct {
		name, in, text string
		x0             int
	}{
		{"body row", "\u2502 hello   \u2502", "hello", 2},
		{"ruled top", "\u256d\u2500 bash \u2500\u2500\u2500\u256e", "bash", 3},
		{"bare rule", "\u2570\u2500\u2500\u2500\u2500\u2500\u2500\u256f", "", 0},
		{"grid margin", " \u2502 hello \u2502", "hello", 3},
		{"no box at all", "plain row", "plain row", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, x0 := boxSelectable(testBox, tc.in, 0)
			if text != tc.text {
				t.Fatalf("boxSelectable(%q) text = %q, want %q", tc.in, text, tc.text)
			}
			if text != "" && x0 != tc.x0 {
				t.Fatalf("boxSelectable(%q) x0 = %d, want %d", tc.in, x0, tc.x0)
			}
		})
	}
}

// TestToolBoxCopyDropsTheFrame is the end-to-end half: a drag over a finished
// tool result copies its label and its output, and none of the glyphs framing
// them. The label rides the top rule, so a drag that took the rule's glyphs or
// dropped its label fails here.
func TestToolBoxCopyDropsTheFrame(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.AddToolBlock("bash", `{"command":"echo hi"}`)
	app.FinishTool("bash", false, "hello\nworld\n", ToolOutcome{Dur: "5ms"})
	app.draw()

	app.mu.Lock()
	top, _ := app.selViewport()
	y0 := app.transcriptTop() + int(app.rowIdx.start[0]) - top
	y1 := app.transcriptTop() + int(app.rowIdx.start[len(app.blocks)]) - 1 - top
	y0, y1 = max(y0, 0), min(y1, app.height-1)
	if y0 > y1 {
		app.mu.Unlock()
		t.Fatalf("the tool box is off screen (rows %d..%d)", y0, y1)
	}
	drag(app, 0, y0, 79, y1)
	app.mu.Unlock()
	app.draw()

	got := string(scr.GetClipboardData())
	for _, want := range []string{"hello", "world", "bash"} {
		if !strings.Contains(got, want) {
			t.Fatalf("copy %q: the box's %q is missing", got, want)
		}
	}
	for _, frame := range []string{"\u256d", "\u256e", "\u2570", "\u256f", "\u2502", "\u2500"} {
		if strings.Contains(got, frame) {
			t.Fatalf("copy %q still carries the frame glyph %q", got, frame)
		}
	}
}
