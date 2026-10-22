package theme

import "testing"

func TestAgentStatusGlyphSet(t *testing.T) {
	want := map[string]string{
		"running": "⟳",
		"idle":    "●",
		"parked":  "○",
		"aborted": "⏹",
	}
	for status, glyph := range want {
		if got := StatusGlyph(status); got != glyph {
			t.Errorf("StatusGlyph(%q) = %q, want %q", status, got, glyph)
		}
	}
	for status, glyph := range want {
		if got := StatusGlyphs[status]; got != glyph {
			t.Errorf("StatusGlyphs[%q] = %q, want %q", status, got, glyph)
		}
	}
	if got := StatusGlyph("nope"); got != "" {
		t.Errorf("unknown key = %q, want empty", got)
	}
}
