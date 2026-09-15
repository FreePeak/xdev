package tui

import (
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/theme"

	"github.com/gdamore/tcell/v2"
)

// hangScreen blocks inside Show until released: the terminal hiccup of
// #283 (a suspended tty whose full write buffer held one flush for
// 12m9s). Everything else delegates to a real simulation screen.
type hangScreen struct {
	tcell.SimulationScreen
	inShow  chan struct{}
	release chan struct{}
}

func (h *hangScreen) Show() {
	select {
	case <-h.inShow:
	default:
		close(h.inShow)
	}
	<-h.release
}

// TestFrozenTtyCannotStallTheAgentGoroutine pins the #283 decoupling:
// while the tty flush is wedged, provider deltas — which reach the app
// synchronously on the agent goroutine (tuiHooks.OnEvent →
// AppendAssistant) — must keep flowing. Pre-fix, draw() held a.mu
// across Show(), so a frozen terminal froze the agent mid-stream and
// the idle watchdog condemned a healthy request.
func TestFrozenTtyCannotStallTheAgentGoroutine(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	_ = sim.Init()
	scr := &hangScreen{SimulationScreen: sim, inShow: make(chan struct{}), release: make(chan struct{})}
	app := New(scr, theme.Load("groknight"), "test/free", "sess1234")
	app.width, app.height = 80, 24
	app.AddUserBlock("hi") // non-empty transcript: the main paint path

	go app.draw()
	<-scr.inShow // the flush is now stuck in the tty write

	done := make(chan struct{})
	go func() {
		app.AppendAssistant("delta")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("provider delta blocked behind a frozen tty flush: a.mu is held across Show()")
	}
	close(scr.release)
}
