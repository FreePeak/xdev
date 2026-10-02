package tui

import (
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// The × on the tab strip deadlocked the whole UI loop (stall dump
// tui-stall-20261002-151321, pid 80950: goroutine 1 [sync.Mutex.Lock] in
// SetTabs <- closeTabByID <- closeTab <- handleTabStripMouse). The mouse
// handler held a.mu across the host callback, and the host's close path
// republishes the tabset through SetTabs, which takes the same
// non-reentrant mutex. The loop never came back; the watchdog killed the
// process 90s later. These go through handleKey — the entry the Run loop
// uses — because that is where the wedge happened.

// A close callback that does what cmd's does (republish the tabset) must
// still return, or the UI thread is gone.
func TestTabStripCloseClickDoesNotDeadlock(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	app.SetTabs([]TabInfo{
		{ID: "aaaa1111", Title: "first"},
		{ID: "bbbb2222", Title: "second", Current: true},
	})
	closed := ""
	// The host's real close path: close the tab, then SetTabs + a notice,
	// both of which take a.mu.
	app.SetTabClose(func(id string) error {
		closed = id
		app.SetTabs([]TabInfo{{ID: "aaaa1111", Title: "first", Current: true}})
		app.AddSystemBlock("· closed a session · now first")
		return nil
	})
	app.draw()

	closeX := app.closeHitX(t, "bbbb2222")

	done := make(chan struct{})
	go func() {
		defer close(done)
		app.handleKey(tcell.NewEventMouse(closeX, tabStripRow, tcell.Button1, tcell.ModNone))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the tab strip × deadlocked: the host callback ran with a.mu held")
	}
	if closed != "bbbb2222" {
		t.Fatalf("clicked the × and closed %q, want the clicked tab", closed)
	}
}

// The same lock discipline on the focus path: clicking a label hands the id
// to the host, which rebuilds the view (Reset + replay) and republishes the
// tabset. Only the hit lookup may hold the lock.
func TestTabStripLabelClickDoesNotDeadlock(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	app.SetTabs([]TabInfo{
		{ID: "aaaa1111", Title: "first", Current: true},
		{ID: "bbbb2222", Title: "second"},
	})
	fired := make(chan string, 1)
	app.SetTabPick(func(id string) error {
		app.SetTabs([]TabInfo{{ID: id, Current: true}})
		fired <- id
		return nil
	})
	app.draw()

	hit := app.labelHitX(t, "bbbb2222")
	if hit < 0 {
		t.Skip("no label rectangle published; the strip did not fit the screen")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		app.handleKey(tcell.NewEventMouse(hit, tabStripRow, tcell.Button1, tcell.ModNone))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the tab strip label click deadlocked: the host callback ran with a.mu held")
	}
	select {
	case got := <-fired:
		if got != "bbbb2222" {
			t.Fatalf("clicked the label and focused %q, want the clicked tab", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the label click never reached the host's focus callback")
	}
}

// closeHitX is the column of the named tab's × as the painter published it,
// so the test follows the layout instead of pinning a coordinate.
func (a *App) closeHitX(t *testing.T, id string) int {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, h := range a.tabHits {
		if h.isClose && h.id == id {
			return h.rect.x
		}
	}
	t.Fatalf("the painter published no close rectangle for %q", id)
	return 0
}

// labelHitX is the column of the named tab's label rectangle — the cell the
// eye reads, not the × that sits inside it.
func (a *App) labelHitX(t *testing.T, id string) int {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, h := range a.tabHits {
		if !h.isClose && h.id == id {
			return h.rect.x
		}
	}
	t.Fatalf("the painter published no label rectangle for %q", id)
	return 0
}
