package tui

import (
	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// gradientRule paints a run of the frame's horizontal glyph from x0 to x1
// (inclusive) on row y, ramping the foreground from `from` to `to`.
//
// The ramp is computed per cell from the run's OWN length, so the first cell
// always carries `from` and the last always carries `to`, whatever width the
// terminal is. A rule that took its fraction from the pane's width instead
// would end mid-ink on a narrow terminal and be a different colour on a wide
// one.
//
// The style carries the background and any attributes; only the foreground
// is ramped, so a rule on a filled band keeps the band's colour.
func (a *App) gradientRule(y, x0, x1 int, from, to theme.Color, glyph rune, st tcell.Style) {
	n := x1 - x0 + 1
	if n <= 0 {
		return
	}
	for x := x0; x <= x1; x++ {
		a.scr.SetContent(x, y, glyph, nil, st.Foreground(a.cellColor(theme.Ramp(from, to, x-x0, n))))
	}
}
