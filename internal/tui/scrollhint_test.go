package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// dividerRow returns the composer's info divider (the ─ model ─ rule) from a
// rendered screen: the bottom rows are the status row, the divider, and the
// prompt above it — so the divider is the second line counting back from the
// end.
func dividerRow(t *testing.T, scr tcell.SimulationScreen) string {
	t.Helper()
	rows := strings.Split(strings.TrimRight(screenText(scr), "\n"), "\n")
	if len(rows) < 3 {
		t.Fatalf("screen too short to have a composer: %q", strings.Join(rows, "|"))
	}
	return rows[len(rows)-2]
}

// TestScrollIndicatorNeverPaintsTranscriptRow pins the fix for "the last prompt
// sits static in the first line": the ▲n▼n hint was painted at y=0 over the
// scrolling transcript, so the top row showed the hint glued onto whatever
// content reached it — a long thinking line, or the last prompt — and ate the
// right-aligned timestamp there too. The hint belongs on the composer's
// divider (chrome); content gets content rows — never a chrome row.
func TestScrollIndicatorNeverPaintsTranscriptRow(t *testing.T) {
	app, scr := newTestApp(t, 100, 24)
	app.AddSystemBlock(strings.Repeat("line\n", 40)) // guarantees rows hidden above
	app.draw()

	rows := strings.Split(strings.TrimRight(screenText(scr), "\n"), "\n")
	if len(rows) == 0 {
		t.Fatal("nothing drawn")
	}
	// Row 0 is the top bar's; the first transcript row sits below it.
	if strings.Contains(rows[0], "▲") || strings.Contains(rows[0], "▼") {
		t.Fatalf("scroll hint overwrote the top bar: %q", rows[0])
	}
	if !strings.Contains(rows[1], "line") {
		t.Fatalf("the first transcript row lost its content: %q", rows[1])
	}
	divider := dividerRow(t, scr)
	if !strings.Contains(divider, "─") || !strings.Contains(divider, "test/free") {
		t.Fatalf("expected the info divider, got %q", divider)
		t.Fatalf("the hint did not move to the divider: %q", divider)
	}
}

// TestScrollIndicatorYieldsToTheModelName: the divider is shared with the model
// ref and the model name wins — a hint that cannot fit is dropped rather than
// eating the text that says which model you are talking to.
func TestScrollIndicatorYieldsToTheModelName(t *testing.T) {
	app, scr := newTestApp(t, 14, 24)
	app.AddSystemBlock(strings.Repeat("line\n", 40))
	app.draw()

	divider := dividerRow(t, scr)
	if strings.Contains(divider, "▲") {
		t.Fatalf("a hint too narrow to fit was painted over the model name: %q", divider)
	}
	if !strings.Contains(divider, "test/free") {
		t.Fatalf("model name lost on a narrow divider: %q", divider)
	}
}

// TestScrollIndicatorGoneWithTheScrollback: the hint is recomputed every frame.
// A frame whose transcript fits (or is gone — /clear, the welcome screen) must
// not keep painting last frame's hint on the divider.
func TestScrollIndicatorGoneWithTheScrollback(t *testing.T) {
	app, scr := newTestApp(t, 100, 24)
	app.AddSystemBlock(strings.Repeat("line\n", 40))
	app.draw()
	app.mu.Lock()
	hinted := app.scrollHint
	app.mu.Unlock()
	if hinted == "" {
		t.Fatal("the fixture must produce a hint before it can clear one")
	}
	if !strings.Contains(dividerRow(t, scr), "▲") {
		t.Fatal("fixture did not reach the divider")
	}

	app.mu.Lock()
	app.blocks = nil // /clear: the welcome frame never sets a viewport hint
	app.mu.Unlock()
	app.draw()
	app.mu.Lock()
	got := app.scrollHint
	app.mu.Unlock()
	if got != "" {
		t.Fatalf("scrollHint still %q with no transcript", got)
	}
	if strings.Contains(dividerRow(t, scr), "▲") {
		t.Fatalf("stale hint painted on the divider: %q", dividerRow(t, scr))
	}
}
