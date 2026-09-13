package tui

import (
	"fmt"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/theme"
	"github.com/gdamore/tcell/v2"
)

// A user reported a TOTAL lockup: an idle session, one `/rename` with no
// argument (so the only visible effect is a system block reading
// "error: usage: /rename <new title>"), and from then on no key did anything
// and the process had to be killed.
//
// handleKey and draw both run on the UI loop's goroutine, so anything that
// fails to return there produces exactly that symptom — dead keys, dead quit
// chord, dead output, process alive. This drives the reported sequence
// through the real key path and requires each loop iteration to finish, at
// every plausible terminal size, because wrapping and the status-line squeeze
// are width-dependent (which is why it can survive one terminal and not
// another).
func TestRenameUsageErrorCannotHangTheLoop(t *testing.T) {
	sizes := []struct{ w, h int }{
		{1, 1}, {1, 24}, {2, 24}, {3, 24}, {4, 24}, {5, 24}, {8, 24}, {10, 24},
		{12, 24}, {16, 24}, {20, 24}, {24, 24}, {28, 24}, {32, 24}, {33, 24},
		{40, 24}, {48, 24}, {60, 24}, {79, 24}, {80, 24}, {81, 24}, {100, 30},
		{120, 40}, {200, 50}, {40, 4}, {40, 3}, {40, 2}, {40, 1},
	}
	for _, sz := range sizes {
		w, h := sz.w, sz.h
		errc := make(chan error, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					errc <- fmt.Errorf("panic: %v", r)
				}
			}()
			app := loopApp(w, h)
			// The reported keystrokes, through the same entry point the UI
			// loop uses: type "/rename", press Enter, then keep typing.
			for _, ch := range "/rename" {
				app.handleKey(tcell.NewEventKey(tcell.KeyRune, ch, tcell.ModNone))
			}
			app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
			// After the error lands, the user found nothing responded:
			// reproduce the keys they would have tried.
			for _, ch := range "esc?" {
				app.handleKey(tcell.NewEventKey(tcell.KeyRune, ch, tcell.ModNone))
			}
			app.handleKey(tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone))
			app.handleKey(tcell.NewEventKey(tcell.KeyCtrlC, 0, tcell.ModCtrl))
			errc <- nil
		}()
		select {
		case err := <-errc:
			if err != nil {
				t.Fatalf("w=%d h=%d: %v", w, h, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("w=%d h=%d: the UI loop did not return — this is the reported freeze", w, h)
		}
	}
}

// The shapes the renderer must survive inside one system block: an
// angle-bracket token (markup-aware path), an unclosed marker, an
// unbreakable word longer than the terminal, and box-drawing pipes.
func TestDrawTerminatesOnMarkupShapedContent(t *testing.T) {
	contents := []string{
		"error: usage: /rename <new title>",
		"error: usage: /goal <objective> (view | create <objective> | resume <objective> | evidence <text>)",
		"<system-reminder>unclosed block continues for quite a while to force the marker path",
		"</system-reminder> leading closer with trailing text",
		"supercalifragilisticexpialidocious" + "supercalifragilisticexpialidocious",
		"<a><b><c><d><e><f><g><h><i><j><k><l><m><n><o><p><q><r><s><t>",
		"┃ box │ chars │ and │ <markup> │ mixed │ together │",
		"<system-reminder></system-reminder>",
		"<" + "",
	}
	for _, content := range contents {
		for _, w := range []int{1, 2, 3, 4, 5, 9, 17, 33, 40, 80, 200} {
			content, w := content, w
			errc := make(chan error, 1)
			go func() {
				defer func() {
					if r := recover(); r != nil {
						errc <- fmt.Errorf("panic: %v", r)
					}
				}()
				app := loopApp(w, 24)
				app.AddSystemBlock(content)
				app.draw()
				// A resize rebuilds the line cache from scratch: a cached
				// layout that assumed another width shows up only here.
				app.width = w/2 + 1
				app.draw()
				errc <- nil
			}()
			select {
			case err := <-errc:
				if err != nil {
					t.Fatalf("w=%d content=%.48q: %v", w, content, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("w=%d content=%.48q: draw did not return", w, content)
			}
		}
	}
}

// loopApp is newTestApp with a live status line and handlers installed, so
// handleKey behaves exactly as it does inside Run.
func loopApp(w, h int) *App {
	scr := tcell.NewSimulationScreen("UTF-8")
	_ = scr.Init()
	scr.SetSize(w, h)
	app := New(scr, theme.Load("groknight"), "test/free", "sess1234")
	app.SetHandlers(func(string) {}, func() {}, func() {})
	app.width, app.height = w, h
	return app
}
