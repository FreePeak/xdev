package tui

import (
	"fmt"
	"strings"
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
			app, _ := loopApp(w, h)
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
				app, _ := loopApp(w, 24)
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
func loopApp(w, h int) (*App, tcell.SimulationScreen) {
	scr := tcell.NewSimulationScreen("UTF-8")
	_ = scr.Init()
	scr.SetSize(w, h)
	app := New(scr, theme.Load("groknight"), "test/free", "sess1234")
	app.SetHandlers(func(string) {}, func() {}, func() {})
	app.width, app.height = w, h
	return app, scr
}

// The reported freeze, explained: an idle session, a failed command leaves the
// composer empty, and the natural "nothing happened" reflex is Esc — which
// opens the tree selector on an empty composer. While open it swallows EVERY
// key (handleTreeKey returns true unconditionally) and has no Ctrl+C case, so
// the quit chord dies with it. Two routes paint nothing while open: a filter
// or search matching no row, and no room above a tall composer. Invisible panel
// + dead Ctrl+C = "the whole TUI froze, I had to kill it".
func TestTreeSelectorCannotTrapTheKeyboard(t *testing.T) {
	// One entry so the selector is allowed to open, default filter showing it.
	app, scr := loopApp(80, 24)
	app.SetTreeData(func() []TreeEntry {
		return []TreeEntry{{ID: "leaf1aaaaaaaaaa", Type: "message", Role: "user", Summary: "first prompt", Active: true}}
	})
	for _, ch := range "/rename" {
		app.handleKey(tcell.NewEventKey(tcell.KeyRune, ch, tcell.ModNone))
	}
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if app.ed.Text() != "" {
		t.Fatalf("composer not cleared after a command: %q", app.ed.Text())
	}
	app.handleKey(tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone))
	if !app.TreeSelectorOpen() {
		t.Fatal("expected Esc on an empty composer to open the selector")
	}

	// Invariant 1: open ⇒ visible. Type a search that matches nothing; the
	// panel must still be drawn, because a modal the user cannot see reads as
	// a hung terminal.
	for _, ch := range "zzzznomatch" {
		app.handleKey(tcell.NewEventKey(tcell.KeyRune, ch, tcell.ModNone))
	}
	app.draw()
	text := screenText(scr)
	if !strings.Contains(text, "session tree") {
		t.Fatalf("selector open but nothing painted:\n%s", text)
	}
	if !strings.Contains(text, "no rows match") {
		t.Fatalf("empty result set must explain itself and the way out:\n%s", text)
	}

	// Invariant 2: the quit chord is never swallowed by the modal.
	handled := app.handleTreeKey(tcell.NewEventKey(tcell.KeyCtrlC, 0, tcell.ModCtrl))
	if handled {
		t.Fatal("handleTreeKey consumed Ctrl+C — the user has no way out")
	}
	quit := false
	app.SetHandlers(func(string) {}, func() {}, func() { quit = true })
	app.handleKey(tcell.NewEventKey(tcell.KeyCtrlC, 0, tcell.ModCtrl))
	if !quit {
		t.Fatal("Ctrl+C did not reach the quit handler while the selector was open")
	}
}

// Invariant 3: Esc always makes progress toward closing — the first clears a
// search, the second closes. A modal that needs N unknown presses is the same
// trap with extra steps.
func TestTreeSelectorEscClosesAfterClearingSearch(t *testing.T) {
	app, _ := loopApp(80, 24)
	app.SetTreeData(func() []TreeEntry {
		return []TreeEntry{{ID: "leaf1aaaaaaaaaa", Type: "message", Role: "user", Summary: "first prompt", Active: true}}
	})
	app.OpenTreeSelector()
	for _, ch := range "zzz" {
		app.handleKey(tcell.NewEventKey(tcell.KeyRune, ch, tcell.ModNone))
	}
	app.handleKey(tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone))
	if !app.TreeSelectorOpen() {
		t.Fatal("first Esc should clear the search, not close blind")
	}
	app.handleKey(tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone))
	if app.TreeSelectorOpen() {
		t.Fatal("second Esc must close the selector")
	}
	// And with it closed, the composer owns keys again.
	for _, ch := range "hello" {
		app.handleKey(tcell.NewEventKey(tcell.KeyRune, ch, tcell.ModNone))
	}
	if app.ed.Text() != "hello" {
		t.Fatalf("composer rejected input after the modal closed: %q", app.ed.Text())
	}
}

// A terminal too short to fit the panel above the composer must not be left
// with an invisible keyboard-owning modal either.
func TestTreeSelectorClosesWhenItCannotPaint(t *testing.T) {
	app, _ := loopApp(80, 4) // composer + hints consume the screen
	app.SetTreeData(func() []TreeEntry {
		return []TreeEntry{{ID: "leaf1aaaaaaaaaa", Type: "message", Role: "user", Summary: "first prompt", Active: true}}
	})
	app.OpenTreeSelector()
	if !app.TreeSelectorOpen() {
		t.Skip("selector declined to open at this size — already safe")
	}
	app.draw()
	app.mu.Lock()
	open := app.tpick != nil
	app.mu.Unlock()
	if open {
		t.Fatal("selector stayed open while it had no room to paint — keys swallowed, nothing visible")
	}
}
