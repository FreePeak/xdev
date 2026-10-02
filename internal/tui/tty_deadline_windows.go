//go:build windows

package tui

import (
	"sync/atomic"

	"github.com/gdamore/tcell/v2"
)

// Windows has no pty to fill: a console handle writes into a screen buffer the
// kernel owns, so the wedge this bounds cannot happen there, and os.File
// deadlines mean nothing on a console. NewDeadlineScreen is therefore the
// stock screen with no flag — cmd/xdev/tui.go calls the same two functions on
// every platform, and a nil flag is what draw() already treats as "nothing was
// dropped".
func NewDeadlineScreen() (tcell.Screen, *atomic.Bool, error) {
	scr, err := tcell.NewScreen()
	return scr, nil, err
}
