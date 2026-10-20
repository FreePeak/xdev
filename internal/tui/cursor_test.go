package tui

import (
	"testing"

	"github.com/gdamore/tcell/v2"
)

// The composer cursor is a block, not the terminal's default bar: an input
// that reads as a shell prompt rather than a plain text field.
//
// The cursor keeps the TERMINAL's colour on purpose — tcell paints a cursor
// colour as an OSC 12 sequence, which a pane-style renderer reads as
// printable text. The shape is a CSI sequence every renderer understands.
func TestComposerCursorIsABlock(t *testing.T) {
	app, _ := drawnApp(t, 60, 14)
	app.draw()

	if app.cursorStyle != tcell.CursorStyleBlinkingBlock {
		t.Errorf("cursor style = %v, want a blinking block", app.cursorStyle)
	}
}
