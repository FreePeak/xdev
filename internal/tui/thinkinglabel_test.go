package tui

import (
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/theme"
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
	if !strings.Contains(div, "Free · auto") {
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
	if div = dividerRow(t, scr); !strings.Contains(div, "Free · high") {
		t.Fatalf("divider %q did not follow the flip", div)
	}
	// …and off is a level, not the absence of one.
	if err := app.ThinkingLevel("off"); err != nil {
		t.Fatal(err)
	}
	app.draw()
	if div = dividerRow(t, scr); !strings.Contains(div, "Free · off") {
		t.Fatalf("divider %q must show off as a level", div)
	}
}

// TestNarrowDividerKeepsTheModelName pins the divider's degradation: the model
// is the one part that shortens instead of leaving, because the name beside a
// level is what the row is for. Every width reads the same name — the
// provider prefixes are routing the /model menu shows in full — so a narrow
// row cuts the NAME, never the prefixes off a raw id.
func TestNarrowDividerKeepsTheModelName(t *testing.T) {
	app, scr := newTestApp(t, 40, 24)
	app.AddSystemBlock("ready")
	level := "high"
	app.SetThinkingOps(thinkWired(&level))
	app.SetStatusModel("onegw/opencode/space-bunny-free")
	app.draw()
	div := dividerRow(t, scr)
	if strings.Contains(div, "onegw") {
		t.Fatalf("the provider prefix is not part of the model's name: %q", div)
	}
	if !strings.Contains(div, "Space Bunny Free · high") {
		t.Fatalf("the name must buy the level its cells: %q", div)
	}

	// A name too long for the row is ellipsised, not dropped: the level keeps
	// its cells and the row still says which model.
	long, scrLong := newTestApp(t, 40, 24)
	long.SetStatusModel("onegw/" + strings.Repeat("very-long-", 6) + "model")
	long.draw()
	if d := dividerRow(t, scrLong); !strings.Contains(d, "…") || !strings.Contains(d, "Very Long") {
		t.Fatalf("an overlong name must be ellipsised, got %q", d)
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
	if !ok || !strings.Contains(rows, "Free · medium") {
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
	if !strings.Contains(screenText(scr), "Free · auto") {
		t.Fatal("the drawn panel must already carry the request")
	}

	if err := app.ThinkingLevel("low"); err != nil {
		t.Fatal(err)
	}
	app.draw()
	if !strings.Contains(screenText(scr), "Free · low") {
		t.Fatal("the panel did not repaint with the new level")
	}
	if !strings.Contains(dividerRow(t, scr), "Free · low") {
		t.Fatal("the divider did not repaint with the new level")
	}
}

// TestThinkingLevelWearsItsRail pins the reasoning half of the divider ink:
// the level is an amount of effort, so it wears the theme's rail colour for
// that rung rather than the model grey, and a level nobody named falls back
// to the model ink instead of borrowing a rung's colour.
func TestThinkingLevelWearsItsRail(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)
	lvl := "high"
	app.SetThinkingOps(&ThinkingOps{Current: func() string { return lvl }, Set: func(l string) error { lvl = l; return nil }})
	if got := app.thinkingToken(); got != theme.ThinkingHigh {
		t.Fatalf("high token = %q, want %q", got, theme.ThinkingHigh)
	}
	if got := app.thinkingToken(); got != app.dividerParts()[1].token {
		t.Fatalf("the divider must paint the level in its rail token")
	}
	lvl = "off"
	if got := app.thinkingToken(); got != theme.ThinkingOff {
		t.Fatalf("off token = %q, want %q", got, theme.ThinkingOff)
	}
	lvl = "whatever"
	if got := app.thinkingToken(); got != theme.StatusLineModel {
		t.Fatalf("an unknown rung must fall back to the model ink, got %q", got)
	}
}
