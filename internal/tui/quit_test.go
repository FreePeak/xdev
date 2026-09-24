package tui

import (
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// A repeated quit chord is a valid terminal event burst: the first Ctrl+C
// closes quitCh, then drainKeys applies the second before Run gets another
// turn. Quit must be idempotent, or that harmless repeat panics the TUI with
// "close of closed channel" and exits the process.
func TestRepeatedQuitChordDoesNotPanic(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.SetHandlers(func(string) {}, func() {}, func() { app.Quit() })

	done := make(chan struct{})
	go func() { app.Run(); close(done) }()
	scr.InjectKey(tcell.KeyCtrlC, 0, tcell.ModCtrl)
	scr.InjectKey(tcell.KeyCtrlC, 0, tcell.ModCtrl)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit after repeated quit chords")
	}
}
