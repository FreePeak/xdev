package tui

import (
	"strings"
	"testing"
)

// The reasoning level is the one request-side fact the chrome could not show:
// the model name sat on the composer's divider alone, and the sidebar's footer
// named the build alone, so "which thinking mode is this session in" had no
// answer on screen — only /thinking, which a user has to already suspect.
// Both readouts carry the bare rung, never the word "thinking": it repeats the
// command beside its own answer, and it costs width on a 38-column panel.

// thinkWired is the seam /thinking, Shift-Tab and the settings panel share:
// one mutable level, which is what the chrome must read.
func thinkWired(level *string) *ThinkingOps {
	return &ThinkingOps{
		Current: func() string { return *level },
		Set:     func(l string) error { *level = l; return nil },
	}
}

// TestThinkingLevelRidesTheComposerDivider pins the divider's half: the bare
// level sits beside the model it applies to (one request, one line), follows a
// /thinking flip without a rebuild, and stays off the bar entirely when the
// seam is unwired — an invented "auto" would name a budget nobody chose.
func TestThinkingLevelRidesTheComposerDivider(t *testing.T) {
	app, scr := newTestApp(t, 100, 24)
	app.AddSystemBlock("ready")

	// Unwired: the divider carries the model and nothing else.
	app.draw()
	if d := dividerRow(t, scr); strings.Contains(d, "auto") {
		t.Fatalf("an unwired seam must paint no level: %q", d)
	}

	level := "auto"
	app.SetThinkingOps(thinkWired(&level))
	app.AddSystemBlock("level set")
	app.draw()
	div := dividerRow(t, scr)
	if !strings.Contains(div, "test/free · auto") {
		t.Fatalf("divider %q must carry the model and the level together", div)
	}
	// The word is the command, not the label: "model · thinking auto" answered
	// the question by repeating the thing you type to change it.
	if strings.Contains(div, "thinking") {
		t.Fatalf("divider %q must not name the command beside its answer", div)
	}

	// A flip repaints from the live seam: the chrome reads the holder, never a
	// value cached at build time.
	if err := app.ThinkingLevel("high"); err != nil {
		t.Fatal(err)
	}
	app.draw()
	if div = dividerRow(t, scr); !strings.Contains(div, "test/free · high") {
		t.Fatalf("divider %q did not follow the flip", div)
	}
	// …and off is a level, not the absence of one.
	if err := app.ThinkingLevel("off"); err != nil {
		t.Fatal(err)
	}
	app.draw()
	if div = dividerRow(t, scr); !strings.Contains(div, "test/free · off") {
		t.Fatalf("divider %q must show off as a level", div)
	}
}

// TestThinkingLevelRidesTheDockFooter pins the sidebar's half: the request is a
// row in the SESSION section — the one never folded away — so it outlives a
// transcript that fills the band. The model rides the same row, because a bare
// "high" under a path and a branch reads as a name of its own.
func TestThinkingLevelRidesTheDockFooter(t *testing.T) {
	app, _, _ := dockTestApp(t, 200, 40)
	app.SetVersion("0.4.127")
	app.SetLocation("/tmp/somewhere")

	// Unwired: no row, and the version row is still last.
	app.mu.Lock()
	f, _ := app.dockFooter()
	app.mu.Unlock()
	if strings.Contains(dockLines(f.rows), "high") {
		t.Fatalf("an unwired seam must paint no row: %q", dockLines(f.rows))
	}
	if last := f.rows[len(f.rows)-1].text; last != "xdev 0.4.127" {
		t.Fatalf("version is no longer the footer's last row: %q", last)
	}

	level := "medium"
	app.SetThinkingOps(thinkWired(&level))
	app.mu.Lock()
	f, ok := app.dockFooter()
	rows := dockLines(f.rows)
	app.mu.Unlock()
	if !ok || !strings.Contains(rows, "test/free · medium") {
		t.Fatalf("footer rows = %q ok=%v", rows, ok)
	}
	// Under the branch, over the build: what the session is, then which build.
	if f.rows[len(f.rows)-1].text != "xdev 0.4.127" {
		t.Fatalf("the level pushed the build off the end: %+v", f.rows)
	}
}

// A flip reaches the sidebar on the same frame: /thinking writes a system
// block, and the panel's staleness test includes the transcript's shape, so
// the footer is rebuilt without anyone having to remember a DockBump here.
func TestDockFooterSeesAFlipOnTheNextFrame(t *testing.T) {
	app, scr, _ := dockTestApp(t, 200, 40)
	level := "auto"
	app.SetThinkingOps(thinkWired(&level))
	app.draw()
	if !strings.Contains(screenText(scr), "test/free · auto") {
		t.Fatal("the drawn panel must already carry the request")
	}

	if err := app.ThinkingLevel("low"); err != nil {
		t.Fatal(err)
	}
	app.draw()
	if !strings.Contains(screenText(scr), "test/free · low") {
		t.Fatal("the panel did not repaint with the new level")
	}
	if !strings.Contains(dividerRow(t, scr), "test/free · low") {
		t.Fatal("the divider did not repaint with the new level")
	}
}
