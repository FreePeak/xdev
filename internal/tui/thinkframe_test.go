package tui

import (
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// The reasoning box's frame is xdev's OWN vocabulary, not a palette slot: it
// wears Rosé Pine's foam (#9ccfd8) on a dark canvas and the Dawn twin of its
// pine (#286983) on a light one, in EVERY theme. A slot would follow the
// palette, and the report is that the frame did not look like one colour at
// all — it followed whatever the loaded theme's accent happened to be.
//
// Two polarities, because one hex cannot be both: foam reads 5.4–10.8:1 on
// every dark canvas xdev ships, dawn pine 4.9–5.3:1 on the two light ones, and
// foam on grokday's #eeeeee measures 1.5:1 — no frame at all, the defect
// PR #587 spent itself closing.
//
// The label above the frame stays bold whatever the focus state (boxTop), and
// the focus mark moves from the tween to Bold on the rule itself, so a click
// still says which box has the wheel.

// TestReasoningFrameIsRosePineOnEveryTheme drives the render path per theme,
// the way a user reaches it: paint a box, read the frame's ink off the painted
// screen, compare it to the one ink that theme should wear.
func TestReasoningFrameIsRosePineOnEveryTheme(t *testing.T) {
	for _, name := range everyPalette {
		t.Run(name, func(t *testing.T) {
			app, scr := newTestApp(t, 80, 24)
			app.SetTheme(theme.Load(name))
			app.SetHandlers(func(string) {}, func() {}, func() {})
			app.BeginThinking()
			app.AppendThinking("a thought worth reading")
			app.EndThinking()

			y := firstThinkBoxRow(t, app, scr)
			if y < 0 {
				t.Fatalf("no reasoning box painted under %s", name)
			}
			got, _, _ := cellStyle(scr, 3, y).Decompose() // x=3: the box's corner
			want := app.cellColor(theme.ThinkFrame(app.th))
			if got != want {
				t.Errorf("%s: frame painted %v, want the Rosé Pine %+v", name, got, theme.ThinkFrame(app.th))
			}
		})
	}
}

// TestReasoningFrameHexesArePolar is the pinned-literal half. Reading the
// expectation back out of theme.ThinkFrame would pass for ANY value that
// function returns, which is the trap the first version of the Rosé Pine
// migration walked into: a test that asks the theme what colour it should
// have painted.
func TestReasoningFrameHexesArePolar(t *testing.T) {
	if foam := (theme.Color{R: 0x9c, G: 0xcf, B: 0xd8}); theme.ThinkFrameDark != foam {
		t.Errorf("dark frame = %+v, want foam #9ccfd8 %+v", theme.ThinkFrameDark, foam)
	}
	if dawn := (theme.Color{R: 0x28, G: 0x69, B: 0x83}); theme.ThinkFrameLight != dawn {
		t.Errorf("light frame = %+v, want Dawn pine #286983 %+v", theme.ThinkFrameLight, dawn)
	}
	// Polarity is what selects between them: a dark theme wearing the light
	// ink loses its frame, and a light one wearing foam never had one.
	for _, tc := range []struct {
		name string
		want theme.Color
	}{
		{"groknight", theme.ThinkFrameDark},
		{"rose-pine", theme.ThinkFrameDark},
		{"grokday", theme.ThinkFrameLight},
		{"one-light", theme.ThinkFrameLight},
	} {
		if got := theme.ThinkFrame(theme.Load(tc.name)); got != tc.want {
			t.Errorf("ThinkFrame(%q) = %+v, want %+v", tc.name, got, tc.want)
		}
	}
	if got := theme.ThinkFrame(nil); got != theme.ThinkFrameDark {
		t.Errorf("ThinkFrame(nil) = %+v, want the dark twin", got)
	}
}

// TestReasoningFrameIsOneFixedInk is the "static colour" claim. The frame used
// to ease gray_dim → accent_thinking over ~200ms and turn bold on focus; the
// request is ONE fixed ink. Bold stays — with the tween gone it is the only
// mark that says which box has the wheel — and nothing else may move.
func TestReasoningFrameIsOneFixedInk(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.SetHandlers(func(string) {}, func() {}, func() {})
	app.BeginThinking()
	app.AppendThinking(thinkLines(40))
	app.EndThinking()

	// Focus moves the mark, not the ink: the frame is the same cell either
	// way, and bold is the whole difference between them.
	resting, _, restingAttr := borderAttrs(app, 0)
	focusBox(app, 0)
	focused, _, focusedAttr := borderAttrs(app, 0)
	focusBox(app, -1)
	if resting != focused {
		t.Errorf("frame ink differs by focus: resting %v, focused %v", resting, focused)
	}
	if focusedAttr != restingAttr|tcell.AttrBold {
		t.Errorf("focused frame attrs = %v, want the resting %v plus bold and nothing else",
			focusedAttr, restingAttr)
	}
}

// borderAttrs is the frame run's foreground, background and attributes on
// block i, read off the styles blockLines produced.
func borderAttrs(app *App, i int) (tcell.Color, tcell.Color, tcell.AttrMask) {
	app.mu.Lock()
	defer app.mu.Unlock()
	lines := app.blockLines(i, app.blocks[i], 80)
	if len(lines) == 0 || len(lines[0].runs) == 0 {
		return tcell.ColorDefault, tcell.ColorDefault, 0
	}
	fg, bg, attr := lines[0].runs[0].style.Decompose()
	return fg, bg, attr
}
