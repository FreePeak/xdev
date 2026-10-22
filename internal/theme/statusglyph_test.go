package theme

import "testing"

// The four agent-status glyphs are omp's agent-hub set: a running agent is
// a spinner arrow (U+27F3, NOT the double-width U+23F3 clock), a settled
// one is a filled dot, a parked one an open dot, and a canceled one a stop
// square. The glyph is the information — a reader scans the shape first.
func TestAgentStatusGlyphSet(t *testing.T) {
	want := map[string]string{
		AgentStatusRunning: "⟳",
		AgentStatusIdle:    "●",
		AgentStatusParked:  "○",
		AgentStatusAborted: "⏹",
	}
	for status, glyph := range want {
		if got := StatusGlyph(status); got != glyph {
			t.Errorf("StatusGlyph(%q) = %q, want %q", status, got, glyph)
		}
	}
	// An unknown status draws nothing rather than a wrong shape.
	if got := StatusGlyph("nope"); got != "" {
		t.Fatalf("StatusGlyph of an unknown status = %q, want empty", got)
	}
}
