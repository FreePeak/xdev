package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// dividerRow returns the composer's info divider (the ╰─ model ─╯ row) from a
// rendered screen: the bottom rows are shortcuts, divider, composer input, box
// top — so the divider is the third line counting back from the end.
func dividerRow(t *testing.T, scr tcell.SimulationScreen) string {
	t.Helper()
	rows := strings.Split(strings.TrimRight(screenText(scr), "\n"), "\n")
	if len(rows) < 3 {
		t.Fatalf("screen too short to have a composer: %q", strings.Join(rows, "|"))
	}
	return rows[len(rows)-3]
}

// TestScrollIndicatorNeverPaintsTranscriptRow pins the fix for "the last prompt
// sits static in the first line": the ▲n▼n hint was painted at y=0 over the
// scrolling transcript, so the top row showed the hint glued onto whatever
// content reached it — a long thinking line, or the last prompt — and ate the
// right-aligned timestamp there too. The hint belongs on the composer's
// divider (chrome); row 0 belongs to the content.
func TestScrollIndicatorNeverPaintsTranscriptRow(t *testing.T) {
	app, scr := newTestApp(t, 100, 24)
	app.AddSystemBlock(strings.Repeat("line\n", 40)) // guarantees rows hidden above
	app.draw()

	rows := strings.Split(strings.TrimRight(screenText(scr), "\n"), "\n")
	if len(rows) == 0 {
		t.Fatal("nothing drawn")
	}
	if strings.Contains(rows[0], "▲") || strings.Contains(rows[0], "▼") {
		t.Fatalf("scroll hint overwrote the first transcript row: %q", rows[0])
	}
	if !strings.Contains(rows[0], "line") {
		t.Fatalf("the first row lost its content: %q", rows[0])
	}
	divider := dividerRow(t, scr)
	if !strings.Contains(divider, "╰") {
		t.Fatalf("expected the info divider, got %q", divider)
	}
	if !strings.Contains(divider, "▲") || !strings.Contains(divider, "▼") {
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
