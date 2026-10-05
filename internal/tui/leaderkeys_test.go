package tui

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// The leader prefix is opencode's: Ctrl+X then a letter. Two properties matter
// and both are pinned here — a pair FIRES, and a prefix followed by something
// that is not a pair still resolves on its own (so Ctrl+X does not silently
// swallow Ctrl+N).
func TestLeaderPairResolves(t *testing.T) {
	m := DefaultKeyMap()
	now := time.Now()
	pair := func(follow, want string) {
		t.Helper()
		if got := m.resolve("C-x", now); got != "" {
			t.Fatalf("the leader itself must resolve to nothing, got %q", got)
		}
		if got := m.resolve(follow, now); got != want {
			t.Fatalf("C-x %s = %q, want %q", follow, got, want)
		}
	}
	pair("n", "session.new")
	pair("l", "session.list")
	pair("w", "session.delete")
	pair("d", "session.delete")
	pair("q", "quit")
}

// The follow-up that completes no pair must still do its own job: C-x N is
// "next menu row", and losing that would make Ctrl+X a dead key everywhere.
func TestLeaderFallsThroughWhenPairAbsent(t *testing.T) {
	m := DefaultKeyMap()
	now := time.Now()
	m.resolve("C-x", now)
	if got := m.resolve("z", now); got != "" {
		t.Fatalf("C-x z with no pair = %q, want nothing", got)
	}
}

// An abandoned prefix expires on its own clock, so the next key is not read as
// the second half of a pair nobody finished.
func TestLeaderExpires(t *testing.T) {
	m := DefaultKeyMap()
	now := time.Now()
	m.resolve("C-x", now)
	late := now.Add(leaderTimeout + time.Second)
	if m.ExpirePending(late) != true {
		t.Fatal("the armed prefix must expire on its own clock")
	}
	// Past the timeout the pair is gone: a bare "n" is unbound, so it resolves
	// to nothing where an armed prefix would have fired session.new.
	if got := m.resolve("n", late); got != "" {
		t.Fatalf("after the timeout C-x n = %q, want nothing", got)
	}
}

// A keybindings.yml written in opencode's spelling — one comma-joined string
// with a <leader> token — must reach the table as real chords.
func TestLeaderTokensFromKeybindings(t *testing.T) {
	home := t.TempDir()
	mk(t, home, "keybindings.yml", `
session.delete: ["<leader>w,alt+w", "C-d"]
session.list:   ["<leader>l"]
`)
	t.Setenv("HOME", home)
	t.Setenv("XDEV_AGENT_DIR", filepath.Join(home, ".xdev", "agent"))
	m, err := LoadKeyMap()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	m.resolve("C-x", now)
	if got := m.resolve("w", now); got != "session.delete" {
		t.Fatalf("<leader>w = %q, want session.delete", got)
	}
	if got := m.Resolve(tcell.NewEventKey(tcell.KeyCtrlD, 0, tcell.ModCtrl)); got != "session.delete" {
		t.Fatalf("C-d = %q, want session.delete", got)
	}
	if got := m.Resolve(tcell.NewEventKey(tcell.KeyRune, 'w', tcell.ModAlt)); got != "session.delete" {
		t.Fatalf("A-w = %q, want session.delete", got)
	}
}

// C-d is a close, not an exit, but on ONE session there is nothing to close —
// so it must still quit. The chord cannot become a dead key for the people who
// only ever open one session.
func TestCloseTabChordQuitsOnTheLastTab(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	var quit bool
	app.SetHandlers(func(string) {}, func() {}, func() { quit = true })
	closed := ""
	app.SetTabClose(func(id string) error { closed = id; return nil })
	app.SetTabs([]TabInfo{{ID: "aaa", Title: "only", Current: true}})

	app.handleKey(tcell.NewEventKey(tcell.KeyCtrlD, 0, tcell.ModCtrl))
	if !quit {
		t.Fatal("C-d on the last open session must quit")
	}
	if closed != "" {
		t.Fatalf("nothing was open to close, yet %q was closed", closed)
	}
}

// With two sessions open, C-d closes the current one and never quits.
func TestCloseTabChordClosesCurrentTab(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	var quit bool
	app.SetHandlers(func(string) {}, func() {}, func() { quit = true })
	closed := ""
	app.SetTabClose(func(id string) error { closed = id; return nil })
	app.SetTabs([]TabInfo{
		{ID: "aaa", Title: "first"},
		{ID: "bbb", Title: "second", Current: true},
	})

	app.handleKey(tcell.NewEventKey(tcell.KeyCtrlD, 0, tcell.ModCtrl))
	if quit {
		t.Fatal("C-d closed a tab; it must not exit the app while tabs remain")
	}
	if closed != "bbb" {
		t.Fatalf("closed %q, want the current tab bbb", closed)
	}
}

// The strip's × and its label are two different answers to one click: close vs
// focus. If the label rectangle swallowed the ×, closing a tab by mouse would
// switch to it instead.
func TestTabStripHitSeparatesLabelFromClose(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.SetTabPolicy(true, false) // the strip is opt-in; this tests its hit table
	app.SetTabs([]TabInfo{
		{ID: "aaa", Title: "first", Current: true},
		{ID: "bbb", Title: "second"},
	})
	app.mu.Lock()
	app.drawTabStrip(scr, 80)
	app.mu.Unlock()

	if len(app.tabHits) == 0 {
		t.Fatal("a two-session frame published no hit table")
	}
	var closeHit, labelHit bool
	for _, h := range app.tabHits {
		if h.id != "bbb" {
			continue
		}
		if h.isClose {
			closeHit = true
			if _, ok := app.tabHitAt(h.rect.x, h.rect.y); !ok {
				t.Fatal("the × cell must hit something")
			}
			continue
		}
		labelHit = true
		// A point on the label that is NOT inside the × rect must be the
		// focus answer, never the close one.
		pick := h.rect
		if r := closeRectOf(app.tabHits, "bbb"); r.contains(pick.x, pick.y) {
			pick.x = h.rect.x // the leftmost cell is label-only
		}
		if got, _ := app.tabHitAt(pick.x, pick.y); got.isClose {
			t.Fatal("a click on the label resolved to the ×")
		}
	}
	if !closeHit || !labelHit {
		t.Fatalf("strip published label=%v close=%v for bbb, want both", labelHit, closeHit)
	}
}

// closeRectOf returns the × rectangle published for a session, so the label
// test can prove the two are told apart.
func closeRectOf(hits []tabHit, id string) panelRect {
	for _, h := range hits {
		if h.id == id && h.isClose {
			return h.rect
		}
	}
	return panelRect{}
}

// A single open session gets no strip: the row would carry one word and cost
// the transcript a line.
func TestTabStripHiddenForOneSession(t *testing.T) {
	app, scr := newTestApp(t, 80, 24)
	app.SetTabs([]TabInfo{{ID: "aaa", Current: true}})
	app.mu.Lock()
	app.drawTabStrip(scr, 80)
	app.mu.Unlock()
	if len(app.tabHits) != 0 {
		t.Fatalf("one session published %d hit rects, want none", len(app.tabHits))
	}
}

// There is no top bar, so a single session's transcript starts at row 0; a
// visible strip owns row 0 and pushes it down — otherwise the strip paints
// over the first line of output.
func TestTranscriptTopAccountsForStrip(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.SetTabPolicy(true, false) // the strip is opt-in; this tests its row
	app.AddUserBlock("hi")
	app.SetTabs([]TabInfo{{ID: "aaa", Current: true}})
	if got := app.transcriptTop(); got != 0 {
		t.Fatalf("transcriptTop with one tab = %d, want 0", got)
	}
	app.SetTabs([]TabInfo{{ID: "aaa"}, {ID: "bbb", Current: true}})
	if got := app.transcriptTop(); got != 1 {
		t.Fatalf("transcriptTop with a strip = %d, want 1", got)
	}
}

func TestKeybindingsAbsentIsDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", filepath.Join(home, "agent"))
	if _, err := os.Stat(filepath.Join(home, "agent", "keybindings.yml")); !os.IsNotExist(err) {
		t.Fatal("fixture should have no keybindings.yml")
	}
	m, err := LoadKeyMap()
	if err != nil {
		t.Fatalf("a missing keybindings.yml is not an error: %v", err)
	}
	if m.Resolve(tcell.NewEventKey(tcell.KeyCtrlC, 0, tcell.ModCtrl)) != "quit" {
		t.Fatal("defaults did not survive a missing keybindings.yml")
	}
}
