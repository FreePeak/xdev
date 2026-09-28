package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// clickAt is the full press → release lifecycle of a no-motion click. The
// caller holds a.mu, matching every other gesture helper here.
func clickAt(app *App, x, y int) {
	press(app, x, y)
	release(app, x, y)
}

// userRow returns the screen row of the transcript's i-th user prompt. It
// matches on what the row actually renders — a user block paints with the "❯ "
// prefix — so these tests name the row by content instead of by a layout
// constant and survive the transcript moving.
func userRow(t *testing.T, app *App, i int) (y int) {
	t.Helper()
	app.mu.Lock()
	seen, text := 0, ""
	for _, b := range app.blocks {
		if b.Kind != KindUser {
			continue
		}
		if seen == i {
			text = "❯ " + strings.SplitN(b.Text, "\n", 2)[0]
			break
		}
		seen++
	}
	app.mu.Unlock()
	if text == "" {
		t.Fatalf("no user block at ordinal %d", i)
	}
	return contentRow(t, app, text)
}

// threeTurns is a transcript with a user prompt above and below an assistant
// reply, so a test can prove the menu armed the prompt and not its neighbour.
func threeTurns(app *App) {
	app.AddUserBlock("first prompt")
	app.AddAssistantBlock("first answer")
	app.AddUserBlock("second prompt")
}

// TestClickUserRowOpensMenu pins the headline behaviour: a click on a ❯ row
// opens the four-action menu.
func TestClickUserRowOpensMenu(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	defer scr.Fini()
	threeTurns(app)
	app.draw()

	y := userRow(t, app, 0)
	app.mu.Lock()
	clickAt(app, 6, y)
	open := app.msgm != nil
	app.mu.Unlock()

	if !open {
		t.Fatal("clicking a user row did not open the message menu")
	}
}

// TestClickNonUserRowLeavesMenuClosed is the other half of the hit test: the
// menu must not open on an assistant row, a system row, or chrome.
func TestClickNonUserRowLeavesMenuClosed(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	defer scr.Fini()
	app.AddUserBlock("first prompt")
	app.AddAssistantBlock("first answer")
	app.draw()

	y := contentRow(t, app, "first answer")
	app.mu.Lock()
	clickAt(app, 6, y)
	open := app.msgm != nil
	app.mu.Unlock()

	if open {
		t.Fatal("clicking an assistant row opened the message menu")
	}
}

// TestDragFromUserRowStillSelectsText pins the reason the menu arms on press
// but opens on a no-motion release: a drag that starts on a prompt is a text
// selection, and must still reach the clipboard.
func TestDragFromUserRowStillSelectsText(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	defer scr.Fini()
	threeTurns(app)
	app.draw()

	y := userRow(t, app, 0)
	app.mu.Lock()
	drag(app, 3, y, 11, y)
	menu := app.msgm != nil
	app.mu.Unlock()

	if menu {
		t.Fatal("a drag opened the menu; only a click should")
	}
	// The row paints as "❯ first prompt", so cells 3..11 are the body text
	// "irst prom" — the drag is measured in screen cells, prefix included.
	if got := string(scr.GetClipboardData()); got != "irst prom" {
		t.Fatalf("clipboard = %q, want %q", got, "irst prom")
	}
}

// TestMenuEscCloses pins the modal contract: Esc dismisses the menu, and it is
// consumed so the double-Esc rewind ladder underneath never sees it.
func TestMenuEscCloses(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	defer scr.Fini()
	threeTurns(app)
	app.draw()

	y := userRow(t, app, 0)
	app.mu.Lock()
	clickAt(app, 6, y)
	app.mu.Unlock()
	if !app.MsgMenuOpen() {
		t.Fatal("menu did not open")
	}

	app.handleKey(tcell.NewEventKey(tcell.KeyEsc, 0, tcell.ModNone))

	if app.MsgMenuOpen() {
		t.Fatal("Esc did not close the message menu")
	}
}

// TestMenuClickOutsideCloses pins the dismissal affordance the footer and the
// diff overlay both promise, and that the dismissing click is swallowed: it
// must not also anchor a selection on the row behind the panel.
func TestMenuClickOutsideCloses(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	defer scr.Fini()
	threeTurns(app)
	app.draw()

	y := userRow(t, app, 0)
	app.mu.Lock()
	clickAt(app, 6, y)
	app.mu.Unlock()
	if !app.MsgMenuOpen() {
		t.Fatal("menu did not open")
	}

	// Routed through handleKey, not handleMouse: dismissing the menu is the
	// modal ladder's job (handleMsgMenuMouse), the same way the diff overlay
	// is dismissed from the real event path.
	app.handleKey(tcell.NewEventMouse(90, 2, tcell.Button1, tcell.ModNone))
	app.mu.Lock()
	stillOpen := app.msgm != nil
	selDown := app.selDown
	app.mu.Unlock()

	if stillOpen {
		t.Fatal("a click outside the menu did not dismiss it")
	}
	if selDown {
		t.Fatal("the dismissing click leaked into a transcript selection")
	}
}

// TestMenuClickPicksRow drives the menu the way a person does: open it, then
// click one of its rows using the geometry the painter published.
func TestMenuClickPicksRow(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	defer scr.Fini()
	threeTurns(app)
	app.draw()

	y := userRow(t, app, 1)
	app.mu.Lock()
	clickAt(app, 6, y)
	app.mu.Unlock()
	app.draw() // paints the menu and publishes its hit table

	app.mu.Lock()
	m := app.msgm
	var mx, ry int
	if m != nil && len(m.hitRow) > 2 {
		mx, ry = m.x+2, m.hitRow[2] // the third row is "copy"
	}
	app.mu.Unlock()
	if ry == 0 {
		t.Fatal("menu did not publish a hit table")
	}

	// Routed through handleKey so the modal ladder owns the click, exactly as
	// it does for a real event — which also means the ladder runs the armed
	// action itself. The test therefore asserts the effect, not a closure.
	app.handleKey(tcell.NewEventMouse(mx, ry, tcell.Button1, tcell.ModNone))

	if app.MsgMenuOpen() {
		t.Fatal("picking a row did not close the menu")
	}
	if got := string(scr.GetClipboardData()); got != "second prompt" {
		t.Fatalf("clipboard = %q, want %q", got, "second prompt")
	}
}

// TestMenuClickPicksJumpRow pins that the first row opens the read-only view,
// the one action whose effect is a surface rather than a clipboard or a rewind.
func TestMenuClickPicksJumpRow(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	defer scr.Fini()
	threeTurns(app)
	app.draw()

	y := userRow(t, app, 0)
	app.mu.Lock()
	clickAt(app, 6, y)
	app.mu.Unlock()
	app.draw()

	app.mu.Lock()
	m := app.msgm
	var mx, ry int
	if m != nil && len(m.hitRow) > 0 {
		mx, ry = m.x+2, m.hitRow[0]
	}
	app.mu.Unlock()
	if ry == 0 {
		t.Fatal("menu did not publish a hit table")
	}

	app.handleKey(tcell.NewEventMouse(mx, ry, tcell.Button1, tcell.ModNone))

	if app.MsgMenuOpen() {
		t.Fatal("picking a row did not close the menu")
	}
	if !app.MsgViewOpen() {
		t.Fatal("picking \"jump\" did not open the read-only message view")
	}
}

// TestMessageViewEscCloses pins that the surface behind "jump" is dismissible
// by keyboard — without its own Esc case the key would fall through to the
// double-Esc rewind and the footer's promise would be a lie.
func TestMessageViewEscCloses(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	defer scr.Fini()
	threeTurns(app)
	app.draw()

	y := userRow(t, app, 0)
	app.mu.Lock()
	clickAt(app, 6, y)
	fire := pickRow(app, msgActJump)
	app.mu.Unlock()
	if fire == nil {
		t.Fatal("no action armed for the jump row")
	}
	fire()
	if !app.MsgViewOpen() {
		t.Fatal("jump did not open the message view")
	}

	app.draw()
	if rowContaining(scr, "message 1 of 2") == "" {
		t.Fatal("the message view did not paint its header")
	}

	app.handleKey(tcell.NewEventKey(tcell.KeyEsc, 0, tcell.ModNone))
	if app.MsgViewOpen() {
		t.Fatal("Esc did not close the message view")
	}
}

// pickRow builds the action for a menu row and closes the menu, which is
// exactly what both real paths do — the mouse path and the Enter/digit path
// arm the action and drop the menu before the action runs. Caller holds a.mu.
func pickRow(app *App, act msgMenuAction) func() {
	fire := app.buildMsgAction(act)
	app.msgm = nil
	return fire
}

// TestMenuCopyPutsMessageOnClipboard pins the one action with no session
// semantics: copy must put the message text, and only the message text, on the
// clipboard.
func TestMenuCopyPutsMessageOnClipboard(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	defer scr.Fini()
	threeTurns(app)
	app.draw()

	y := userRow(t, app, 1)
	app.mu.Lock()
	clickAt(app, 6, y)
	fire := pickRow(app, msgActCopy)
	app.mu.Unlock()
	if fire == nil {
		t.Fatal("no action armed for the copy row")
	}
	fire()

	if got := string(scr.GetClipboardData()); got != "second prompt" {
		t.Fatalf("clipboard = %q, want %q", got, "second prompt")
	}
}

// TestMenuRevertWarnsFilesAreNotRestored pins the honesty rule: xdev has no
// working-tree snapshot, so a revert that silently left half the turn on disk
// would be worse than no revert. The rewind happens AND the warning is shown.
func TestMenuRevertWarnsFilesAreNotRestored(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	defer scr.Fini()
	threeTurns(app)

	var rewound string
	var summarize bool
	app.SetSessionOps(&SessionOps{
		NavigateTree: func(entryID string, s bool) (string, error) {
			rewound, summarize = entryID, s
			return "", nil
		},
		UserEntryID: func(i int) string { return "entry" + string(rune('A'+i)) },
	})
	app.draw()

	y := userRow(t, app, 1)
	app.mu.Lock()
	clickAt(app, 6, y)
	fire := pickRow(app, msgActRevert)
	app.mu.Unlock()
	if fire == nil {
		t.Fatal("no action armed for the revert row")
	}
	fire()

	if rewound != "entryB" {
		t.Fatalf("reverted entry = %q, want %q", rewound, "entryB")
	}
	if summarize {
		t.Fatal("revert asked for a branch summary; that costs a model round trip on the UI thread")
	}

	app.mu.Lock()
	var notices []string
	for _, b := range app.blocks {
		if b.Kind == KindSystem {
			notices = append(notices, b.Text)
		}
	}
	draft := app.ed.Text()
	app.mu.Unlock()

	joined := strings.Join(notices, "\n")
	if !strings.Contains(joined, "files were NOT restored") {
		t.Fatalf("revert did not warn that files are not restored; notices were:\n%s", joined)
	}
	if draft != "" {
		t.Fatalf("revert primed the composer with %q; revert must leave the composer alone", draft)
	}
}

// TestMenuForkPrimesComposer pins the difference between revert and fork: fork
// rewinds to the same point but hands the prompt back to be edited and resent.
func TestMenuForkPrimesComposer(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	defer scr.Fini()
	threeTurns(app)

	var rewound string
	app.SetSessionOps(&SessionOps{
		NavigateTree: func(entryID string, _ bool) (string, error) {
			rewound = entryID
			return "edited prompt", nil
		},
		UserEntryID: func(i int) string { return "entry" + string(rune('A'+i)) },
	})
	app.draw()

	y := userRow(t, app, 0)
	app.mu.Lock()
	clickAt(app, 6, y)
	fire := pickRow(app, msgActFork)
	app.mu.Unlock()
	if fire == nil {
		t.Fatal("no action armed for the fork row")
	}
	fire()

	if rewound != "entryA" {
		t.Fatalf("forked entry = %q, want %q", rewound, "entryA")
	}
	app.mu.Lock()
	got := app.ed.Text()
	app.mu.Unlock()
	if got != "edited prompt" {
		t.Fatalf("composer = %q, want %q", got, "edited prompt")
	}
}

// TestMenuActionsDegradeWithoutSessionOps pins that an unwired session seam
// produces a notice, never a panic and never a rewind somewhere random.
func TestMenuActionsDegradeWithoutSessionOps(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	defer scr.Fini()
	threeTurns(app)
	app.draw()

	y := userRow(t, app, 0)
	app.mu.Lock()
	clickAt(app, 6, y)
	fire := pickRow(app, msgActRevert)
	app.mu.Unlock()

	if fire == nil {
		t.Fatal("revert armed no action")
	}
	fire() // must not panic

	app.mu.Lock()
	last := ""
	if n := len(app.blocks); n > 0 {
		last = app.blocks[n-1].Text
	}
	app.mu.Unlock()
	if !strings.Contains(last, "not in the session store") {
		t.Fatalf("last notice = %q, want one about the missing store entry", last)
	}
}

// TestMenuDigitPicksRow pins the keyboard path: a four-item menu is small
// enough that "3" beats three arrow presses, and the digit must run the same
// action the click on that row would.
func TestMenuDigitPicksRow(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	defer scr.Fini()
	threeTurns(app)
	app.draw()

	y := userRow(t, app, 0)
	app.mu.Lock()
	clickAt(app, 6, y)
	app.mu.Unlock()
	if !app.MsgMenuOpen() {
		t.Fatal("menu did not open")
	}

	// Row 3 is "copy" (1-indexed digit "3").
	app.handleKey(tcell.NewEventKey(tcell.KeyRune, '3', tcell.ModNone))

	if app.MsgMenuOpen() {
		t.Fatal("a digit did not close the menu")
	}
	if got := string(scr.GetClipboardData()); got != "first prompt" {
		t.Fatalf("clipboard = %q, want %q", got, "first prompt")
	}
}

// TestMenuArrowsAndEnterRunHighlightedRow pins that the highlight moves and
// Enter acts on it — the keyboard equivalent of aiming at a row and clicking.
func TestMenuArrowsAndEnterRunHighlightedRow(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	defer scr.Fini()
	threeTurns(app)
	app.draw()

	y := userRow(t, app, 0)
	app.mu.Lock()
	clickAt(app, 6, y)
	app.mu.Unlock()
	if !app.MsgMenuOpen() {
		t.Fatal("menu did not open")
	}

	// Starts on row 0 ("jump"); two downs land on row 2 ("copy").
	app.handleKey(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	app.handleKey(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	app.mu.Lock()
	sel := app.msgm.sel
	app.mu.Unlock()
	if sel != 2 {
		t.Fatalf("highlight at row %d after two Down presses, want 2", sel)
	}

	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))

	if app.MsgMenuOpen() {
		t.Fatal("Enter did not close the menu")
	}
	if got := string(scr.GetClipboardData()); got != "first prompt" {
		t.Fatalf("clipboard = %q, want %q", got, "first prompt")
	}
}

// TestMenuHighlightWraps pins that ↑ from the first row reaches the last, so a
// keyboard user is never trapped at the top of the list.
func TestMenuHighlightWraps(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	defer scr.Fini()
	threeTurns(app)
	app.draw()

	y := userRow(t, app, 0)
	app.mu.Lock()
	clickAt(app, 6, y)
	app.mu.Unlock()

	// handleKey reaches the menu handler, which takes a.mu itself — so the
	// lock must be released before the call, exactly as app.go does.
	app.handleKey(tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone))

	app.mu.Lock()
	sel := -1
	if app.msgm != nil {
		sel = app.msgm.sel
	}
	app.mu.Unlock()

	if want := len(msgMenuRows) - 1; sel != want {
		t.Fatalf("Up from row 0 landed on %d, want %d", sel, want)
	}
}

// TestUserRowAtOrdinalsMessagesInOrder pins the mapping the whole feature rests
// on: the i-th user row's ordinal is its position among user rows, so the
// session seam can resolve it back to the right store entry.
func TestUserRowAtOrdinalsMessagesInOrder(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	defer scr.Fini()
	threeTurns(app)
	app.draw()

	want := []string{"first prompt", "second prompt"}
	seen := 0
	for _, b := range app.blocks {
		if b.Kind != KindUser {
			continue
		}
		if b.Text != want[seen] {
			t.Fatalf("user row %d = %q, want %q", seen, b.Text, want[seen])
		}
		seen++
	}
	if seen != 2 {
		t.Fatalf("found %d user rows, want 2", seen)
	}
}

// TestResetClosesMessageSurfaces pins that replaying a session away cannot
// leave a menu pointing at blocks that no longer exist.
func TestResetClosesMessageSurfaces(t *testing.T) {
	app, scr := newTestApp(t, 100, 30)
	defer scr.Fini()
	threeTurns(app)
	app.draw()

	y := userRow(t, app, 0)
	app.mu.Lock()
	clickAt(app, 6, y)
	app.msgArmed = true
	app.mu.Unlock()
	if !app.MsgMenuOpen() {
		t.Fatal("menu did not open")
	}

	app.Reset()

	app.mu.Lock()
	menu, view, arm := app.msgm, app.msgv, app.msgArmed
	app.mu.Unlock()
	if menu != nil || view != nil {
		t.Fatal("Reset left a message surface open over replayed-away blocks")
	}
	if arm {
		t.Fatal("Reset left the message menu armed")
	}
}
