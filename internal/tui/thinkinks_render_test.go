package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// The palette half of the reasoning box's ink contract is
// internal/theme/thinkinks_test.go (contrast ratios, the alias table). This is
// the render-path half: the box must PAINT the two slots that contract names,
// on every palette, so a swap that moved the numbers but not the paint fails
// here.
//
// Both defects it exists for were invisible from the launch theme and both
// survived the whole chrome suite: `accent_thinking` aliased `thinkingText`
// (every ported palette fills that token with its own base colour — a frame
// painted in the canvas), and the body wore `gray_dim`, chrome ink at
// 1.3–2.6:1 on the same canvas across the dracula / one-dark / gruvbox / nord
// family. Neither is a question the render-cache tests ask.

// everyPalette is every theme reachable from /theme. A render check that
// looked only at groknight would pass against both defects.
var everyPalette = []string{
	"groknight", "grokday",
	"catppuccin", "dracula", "gruvbox", "nord",
	"one-dark", "one-light", "rose-pine", "tokyo-night",
}

// cellInk is the foreground a rendered cell would paint.
func cellInk(c cell) tcell.Color {
	fg, _, _ := c.style.Decompose()
	return fg
}

// boxInks returns the frame ink and the body ink of the reasoning box at block
// i, read off the styles blockLines produced — thinkbox_test.go's renderBox
// keeps the text and throws the styles away.
//
// A box row runs border / body / pad+border, so runs[1] is the reasoning text
// and runs[0] of the top row is the frame. The text comes back with the inks
// because a test that reads the ink off a pad cell nobody sees proves nothing.
func boxInks(app *App, i int) (frame, body tcell.Color, text string) {
	app.mu.Lock()
	defer app.mu.Unlock()
	lines := app.blockLines(i, app.blocks[i], 80)
	if len(lines) < 2 {
		return tcell.ColorDefault, tcell.ColorDefault, ""
	}
	return cellInk(lines[0].runs[0]), cellInk(lines[1].runs[1]), lineText(lines[1])
}

// assertBoxInks checks one rendered box against the theme in force. Both inks
// are named in the failure, because "the colours are wrong" is not actionable
// and a user can only ever see this as "the thinking box looks wrong".
func assertBoxInks(t *testing.T, app *App, label string, frame, body tcell.Color, text string) {
	t.Helper()
	if want := app.cellColor(app.th.Get(theme.AccentThinking)); frame != want {
		t.Errorf("%s: border painted %v, want accent_thinking %v", label, frame, want)
	}
	if want := app.cellColor(app.th.Get(theme.TextPrimary)); body != want {
		t.Errorf("%s: body painted %v, want text_primary %v", label, body, want)
	}
	// Two inks that resolve alike leave a box with no frame — the shape both
	// defects took on a palette whose ink was the canvas.
	if frame == body {
		t.Errorf("%s: border and body are the same ink %v: there is no frame", label, frame)
	}
	if !strings.Contains(text, "worth reading") {
		t.Errorf("%s: body row = %q, want the reasoning text", label, text)
	}
}

func TestReasoningBoxPaintsBodyAndBorderFromTheTheme(t *testing.T) {
	for _, name := range everyPalette {
		t.Run(name, func(t *testing.T) {
			app, _ := newTestApp(t, 80, 24)
			app.SetTheme(theme.Load(name))
			app.BeginThinking()
			app.AppendThinking("a thought worth reading")
			app.EndThinking()

			frame, body, text := boxInks(app, 0)
			assertBoxInks(t, app, name, frame, body, text)
		})
	}
}

// TestReasoningBoxSurvivesAThemeSwitch is the reported symptom — the thinking
// text and its border lose their colour when the theme changes — driven the way
// a user drives it: render under one palette, swap, render again.
//
// blockLines memoizes per block, so this also pins that SetTheme drops that
// cache (clearRenderCache). A switch that left the old render in place would
// keep the old inks on screen however the slots resolved — a failure no slot
// assertion above would catch.
func TestReasoningBoxSurvivesAThemeSwitch(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.BeginThinking()
	app.AppendThinking("a thought worth reading")
	app.EndThinking()

	firstFrame, firstBody, _ := boxInks(app, 0)

	for _, name := range everyPalette {
		app.SetTheme(theme.Load(name))
		frame, body, text := boxInks(app, 0)
		assertBoxInks(t, app, "/theme "+name, frame, body, text)
	}

	// Back to where it started: a theme switch is not a one-way trip.
	app.SetTheme(theme.Load("groknight"))
	frame, body, _ := boxInks(app, 0)
	if frame != firstFrame || body != firstBody {
		t.Errorf("returning to the first theme painted %v/%v, want %v/%v", frame, body, firstFrame, firstBody)
	}
}
