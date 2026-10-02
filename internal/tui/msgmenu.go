package tui

// The user-message menu: clicking a ❯ row in the transcript offers the three
// things a person wants to do with a message they have already read — look at
// it in the session, undo it, or copy it.
//
// "revert" is the tree selector's user-row rewind, not a private dialect of it
// (user-reported: "click-to-user-message must work like the history tree").
// /tree's Enter on a user row moves the leaf to that message's PARENT and hands
// the prompt back as the composer draft; this menu's revert does the same, so
// the two ways of reaching a point in the conversation agree. That is also why
// the standalone "fork" row is gone: fork WAS that behaviour under a second
// name, and two rows that rewind the same way is one row too many. What a
// revert still cannot do is put files back, and the hint says so.
//
// Two design rules keep this honest:
//
//   - The menu is armed on press but fires on a no-motion RELEASE. A click is
//     a click; a drag is a text selection. Wiring it to the press instead would
//     make it impossible to select a user message's text at all, and the
//     transcript's whole selection model lives on the press/drag/release
//     lifecycle (see selection.go). Firing on release also means the menu never
//     opens under a finger that is on its way somewhere else.
//
//   - Nothing here mutates the session under a.mu. Selecting an action only
//     BUILDS a closure (buildMsgAction); the caller runs it after unlocking,
//     because rewinding a session replays the whole transcript and a
//     summarize can spend up to branchSummaryBudget inside the store.

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// msgMenuAction names one row of the user-message menu.
type msgMenuAction int

const (
	msgActNone msgMenuAction = iota
	msgActJump
	msgActRevert
	msgActCopy
)

// msgMenuRows is the menu, in paint order. The hints spell out the two things a
// person cannot guess: a revert re-lands the prompt in the composer (it is the
// tree selector's rewind, not a discard), and it does NOT put files back.
var msgMenuRows = []struct {
	act   msgMenuAction
	label string
	hint  string
}{
	{msgActJump, "jump", "view this message in the session"},
	{msgActRevert, "revert", "rewind here — the prompt comes back to edit"},
	{msgActCopy, "copy", "copy the message to the clipboard"},
}

// msgMenu is the open menu. x/y are the screen cell it was opened from (the
// anchor the painter positions against); hitY0 and hitRow are the hit table
// the painter publishes every frame, so a click is answered with the geometry
// that was actually on screen rather than geometry recomputed on the spot —
// the same contract picker's hitItem uses.
type msgMenu struct {
	block int // index into a.blocks
	ord   int // ordinal among user rows, for the SessionOps.UserEntryID seam
	sel   int // highlighted row
	ax    int // anchor column of the click that opened it
	ay    int // anchor row of the click that opened it
	// published by drawMsgMenu
	x, y, w, h int
	hitY0      int
	hitRow     []int
}

// msgView is the read-only surface behind "jump": the full untruncated text
// plus the session facts (timestamp, entry id, position on the path). The
// transcript shows a wrapped, scrolled-past row; this shows the message as the
// session actually stores it.
type msgView struct {
	entryID   string
	ord       int
	blocks    int // how many user rows the live path carries
	ts        time.Time
	rows      []string // wrapped body rows
	scrollVp  int
	scrollOff int
}

// --- open / close ---

// openMsgMenu puts the menu up for the user row the click landed on, anchored
// at the click so the painter can place it just below. Caller holds a.mu.
func (a *App) openMsgMenu(ord, block, ax, ay int) {
	a.msgm = &msgMenu{block: block, ord: ord, ax: ax, ay: ay}
	a.poke()
}

// closeMsgMenu dismisses it. Caller holds a.mu.
func (a *App) closeMsgMenuLocked() {
	if a.msgm == nil {
		return
	}
	a.msgm = nil
	a.poke()
}

// closeMsgMenu dismisses it for callers that do not hold the lock.
func (a *App) closeMsgMenu() {
	a.mu.Lock()
	a.closeMsgMenuLocked()
	a.mu.Unlock()
}

// MsgMenuOpen reports whether the user-message menu is up. Callers that only
// need the presence check (the wheel, the scroll actions) use this so they do
// not race a frame that just closed it.
func (a *App) MsgMenuOpen() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.msgm != nil
}

// MsgViewOpen reports whether the read-only message surface is up.
func (a *App) MsgViewOpen() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.msgv != nil
}

// userRowAt maps a screen row to the user block it shows, and to that block's
// ordinal among the transcript's user rows. It is the same screen-row →
// document-row → block-index walk thinkBoxAt does, because the row index is the
// only thing that knows where a block landed on screen — with one exception: a
// row under the sticky header shows the prompt the header pinned, whatever
// document row that row's index names, so a click on the header opens the same
// menu a click on the prompt inline does. Caller holds a.mu.
func (a *App) userRowAt(y int) (ord, block int) {
	top, vp := a.selViewport()
	hdr := a.transcriptTop()
	if vp <= 0 || y < hdr || y >= hdr+vp {
		return -1, -1
	}
	bi := -1
	switch dy := y - hdr; {
	case dy < a.stickyVis && a.stickyBlock >= 0:
		bi = a.stickyBlock // a row of the pinned prompt
	case dy < a.stickyHdr:
		bi = -1 // the blank gap under it: nothing to hit
	default:
		bi = a.rowIdx.blockAt(int32(top + dy))
	}
	if bi < 0 || bi >= len(a.blocks) || a.blocks[bi].Kind != KindUser {
		return -1, -1
	}
	n := 0
	for i := 0; i < bi; i++ {
		if a.blocks[i].Kind == KindUser {
			n++
		}
	}
	return n, bi
}

// --- action dispatch ---

// buildMsgAction snapshots everything the chosen action needs and returns a
// closure that performs it. The split is the whole point: the store rewind
// replays the transcript and must not run under a.mu, so the menu is free to
// be opened and closed from handleMouse, which is called with the lock held.
//
// Caller holds a.mu.
func (a *App) buildMsgAction(act msgMenuAction) func() {
	m := a.msgm
	if m == nil || act == msgActNone {
		return nil
	}
	if m.block < 0 || m.block >= len(a.blocks) {
		return nil
	}
	blk := a.blocks[m.block]
	ord, block := m.ord, m.block
	// Resolving the entry id here rather than in the closure keeps the store
	// read on the locked side; the seam is a pure lookup, and a session that
	// rewound under us must fail at menu time, not half-way through a rewind.
	entryID := a.userEntryID(ord)
	return func() { a.runMsgAction(act, ord, block, entryID, blk.Text, blk.Ts) }
}

// userEntryID asks the session seam which store entry backs the i-th user
// prompt row. An unwired seam (and an unresolvable row) both read as "" — the
// actions that need an id then say so instead of guessing one.
func (a *App) userEntryID(ord int) string {
	if a.ops == nil || a.ops.UserEntryID == nil {
		return ""
	}
	return a.ops.UserEntryID(ord)
}

// runMsgAction performs one menu choice. It runs UNLOCKED.
func (a *App) runMsgAction(act msgMenuAction, ord, block int, entryID, text string, ts time.Time) {
	switch act {
	case msgActJump:
		a.openMsgView(ord, block, entryID, text, ts)
	case msgActCopy:
		a.copyMessage(text)
	case msgActRevert:
		a.revertMessage(entryID, text)
	}
}

// runPendingMsgAction takes the action a menu row armed and runs it, then
// clears the slot. The mouse and key paths both call it right after leaving
// the lock: buildMsgAction can only read the store under a.mu, but the rewind
// it schedules replays the whole transcript and must not run locked.
//
// The clear happens BEFORE the call so a panicking or re-entrant action cannot
// leave a stale closure to fire on the next click.
func (a *App) runPendingMsgAction() {
	a.mu.Lock()
	fire := a.msgFire
	a.msgFire = nil
	a.mu.Unlock()
	if fire != nil {
		fire()
	}
}

// copyMessage puts the message on the clipboard and confirms it with the same
// toast a drag-copy uses, so the two copy paths look alike. A failure is a
// transcript block, not a toast: it says what went wrong, and the block is
// where a message the user may want to act on belongs.
func (a *App) copyMessage(text string) {
	if strings.TrimSpace(text) == "" {
		a.AddSystemBlock("message is empty — nothing to copy")
		return
	}
	if err := a.copyToClipboard(text); err != nil {
		a.AddSystemBlock("copy failed: " + err.Error())
		return
	}
	a.mu.Lock()
	a.setNotice("Copied " + strconv.Itoa(utf8.RuneCountInString(text)) + " chars")
	a.mu.Unlock()
	a.poke()
}

// revertMessage undoes the message, and it is the tree selector's rewind rather
// than a private dialect of it: the leaf moves to the message's PARENT, the
// transcript is replayed from there, and the prompt comes back as the composer
// draft, so the next send continues from that point as a NEW message. That is
// exactly what /tree's Enter does on a user row, and the two must not disagree —
// a click that looked like the tree and rewound somewhere else is worse than
// either alone. (The menu's old "fork" row was this same rewind under a second
// name, so it folded in here; "revert" is the name a person would try first.)
//
// It does NOT put files back. There is no file-revert machinery in xdev — no
// working-tree snapshot, no recorded baseline commit — so anything the turn
// wrote to disk stays written. The notice says so in the same breath as the
// rewind, because a "revert" that silently leaves half the turn's effects in
// place is worse than no rewind at all.
//
// summarize is false on purpose: a summary costs a model round trip (up to
// branchSummaryBudget) and would freeze the UI thread this runs on. /tree's
// Shift+Enter is still the way to get one.
//
// text is the row's own text, the fallback for a seam that resolved the entry
// id but handed back no draft (a harness-attributed row, a store that cannot
// rebuild the message). Caller: runMsgAction, unlocked.
func (a *App) revertMessage(entryID, text string) {
	if entryID == "" {
		a.AddSystemBlock("revert: this message is not in the session store")
		return
	}
	draft, ok := a.navigateDraft(entryID)
	if !ok {
		return
	}
	if draft == "" {
		draft = text
	}
	// The draft is primed only over an empty composer: a parked or typed draft
	// is somebody's unfinished thought and must not be overwritten. Same rule
	// treeSelect applies.
	a.mu.Lock()
	empty := strings.TrimSpace(a.ed.Text()) == ""
	a.mu.Unlock()
	if empty {
		a.escDraft, a.escUsed = nil, false
		a.ed.SetBuffer(draft)
		a.AddSystemBlock("· reverted — the session rewound and the message is back in the composer, edit and send")
	} else {
		a.AddSystemBlock("· reverted — the session rewound; the composer was not empty, so the prompt was not restored")
	}
	a.AddSystemBlock("· files were NOT restored: xdev keeps no snapshot of the working tree, so anything that turn wrote to disk is still there")
	a.poke()
}

// navigateDraft moves the session leaf to entryID and reports the prompt the
// rewind hands back. The rewind is the one operation this menu performs, and the
// tree selector performs the same one through the same seam, so both surfaces
// land in the same place (summarize is deliberately off: a branch summary costs
// a model round trip and this runs on the UI thread).
func (a *App) navigateDraft(entryID string) (string, bool) {
	if a.ops == nil || a.ops.NavigateTree == nil {
		a.AddSystemBlock("revert: session branch not wired")
		return "", false
	}
	draft, err := a.ops.NavigateTree(entryID, false)
	if err != nil {
		a.AddSystemBlock("revert: " + err.Error())
		return "", false
	}
	return draft, true
}

// --- the read-only message surface (behind "jump") ---

// openMsgView shows the message in full, with the session facts the wrapped
// transcript row drops. It takes the lock itself: the closure that calls it
// runs unlocked (the rewind path must), so the block count it reports has to be
// read here rather than snapshotted by the locked caller.
func (a *App) openMsgView(ord, block int, entryID, text string, ts time.Time) {
	w := a.contentWidth()
	v := &msgView{entryID: entryID, ord: ord, ts: ts, rows: wrap(strings.TrimRight(text, "\n"), max(10, w-6))}
	v.scrollVp = max(1, a.height-a.composerRows()-6)
	a.mu.Lock()
	v.blocks = a.countUserRows()
	a.msgv = v
	a.mu.Unlock()
	a.poke()
}

// closeMsgView dismisses the read-only surface.
func (a *App) closeMsgView() {
	a.mu.Lock()
	a.msgv = nil
	a.mu.Unlock()
	a.poke()
}

// countUserRows is how many user rows the live transcript carries, so the
// surface can say "3 of 7" instead of a bare ordinal. Caller holds a.mu.
func (a *App) countUserRows() int {
	n := 0
	for _, b := range a.blocks {
		if b.Kind == KindUser {
			n++
		}
	}
	return n
}

// msgViewScroll moves the read-only surface's viewport. Self-locking: it is
// reached from the wheel and the scroll actions, which do not hold a.mu.
func (a *App) msgViewScroll(n int, down bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	v := a.msgv
	if v == nil {
		return
	}
	maxOff := max(0, len(v.rows)-v.scrollVp)
	if down {
		v.scrollOff += n
	} else {
		v.scrollOff -= n
	}
	v.scrollOff = max(0, min(v.scrollOff, maxOff))
	a.poke()
}

// --- mouse ---

// handleMsgMenuMouse gives the menu and the read-only surface the mouse before
// the transcript sees it, so a click cannot leak a selection into the rows
// behind a panel. It answers true when the event belonged to one of them.
func (a *App) handleMsgMenuMouse(m *tcell.EventMouse, press bool) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	x, y := m.Position()
	switch {
	case a.msgv != nil:
		// Same shape as the diff overlay: a click outside dismisses, a click
		// inside is consumed so it cannot start a selection behind the panel.
		if !a.msgViewBounds().contains(x, y) {
			a.msgv = nil
			a.poke()
		}
		return true
	case a.msgm != nil:
		if i := a.msgMenuHit(x, y); i >= 0 {
			if btn := m.Buttons(); btn&tcell.Button1 != 0 && press {
				a.msgm.sel = i
				a.msgFire = a.buildMsgAction(a.msgMenuActionAt(i))
				a.msgm = nil
			}
			a.poke()
			return true
		}
		// Outside: dismiss, and swallow the click. Swallowing matters — the
		// press that dismissed the menu must not also anchor a selection on
		// the transcript row underneath.
		a.msgm = nil
		a.poke()
		return true
	}
	return false
}

// msgMenuHit returns the menu row under (x, y), or -1. Caller holds a.mu.
func (a *App) msgMenuHit(x, y int) int {
	m := a.msgm
	if m == nil || m.hitRow == nil {
		return -1
	}
	for i, ry := range m.hitRow {
		if y == ry && x >= m.x && x < m.x+m.w {
			return i
		}
	}
	return -1
}

// --- keys ---

// handleMsgMenuKey owns the keys while a menu or the read-only surface is up.
// It is modal: every key is swallowed, because both surfaces are anchored to a
// specific message and a key that fell through would land on the composer.
// Ctrl+C is handed back so the app can still quit (see handleTreeKey).
func (a *App) handleMsgMenuKey(key *tcell.EventKey) bool {
	if key.Key() == tcell.KeyCtrlC && !a.MsgMenuOpen() && !a.MsgViewOpen() {
		return false
	}
	a.mu.Lock()
	v, m := a.msgv, a.msgm
	a.mu.Unlock()
	if v == nil && m == nil {
		return false
	}
	if key.Key() == tcell.KeyCtrlC {
		return false
	}
	if v != nil {
		return a.handleMsgViewKey(key)
	}
	return a.handleMsgMenuKeys(key)
}

// handleMsgViewKey routes keys while the read-only surface is open. The Esc
// case is load-bearing for the same reason the diff overlay's is: without it
// Esc falls through to the double-Esc rewind ladder in handleKey and the
// footer's "Esc close" is a lie.
func (a *App) handleMsgViewKey(key *tcell.EventKey) bool {
	switch key.Key() {
	case tcell.KeyEsc:
		a.closeMsgView()
	case tcell.KeyUp:
		a.msgViewScroll(1, false)
	case tcell.KeyDown:
		a.msgViewScroll(1, true)
	case tcell.KeyPgUp:
		a.msgViewScroll(msgViewPage(a), false)
	case tcell.KeyPgDn:
		a.msgViewScroll(msgViewPage(a), true)
	case tcell.KeyHome:
		a.msgViewScroll(1<<30, false)
	case tcell.KeyEnd:
		a.msgViewScroll(1<<30, true)
	default:
		return false
	}
	return true
}

// msgViewPage is one page of the read-only surface.
func msgViewPage(a *App) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.msgv == nil {
		return 1
	}
	return max(1, a.msgv.scrollVp-1)
}

// handleMsgMenuKeys routes keys while the menu is open. Enter runs the
// highlighted row, ↑↓ move, Esc closes, and the number keys 1-4 pick a row
// directly (a four-item menu is small enough that digits beat arrows).
func (a *App) handleMsgMenuKeys(key *tcell.EventKey) bool {
	run := func(i int) {
		a.mu.Lock()
		if a.msgm != nil && i < len(msgMenuRows) {
			a.msgm.sel = i
			fire := a.buildMsgAction(msgMenuRows[i].act)
			a.msgm = nil
			a.mu.Unlock()
			if fire != nil {
				fire()
			}
			return
		}
		a.mu.Unlock()
	}
	switch key.Key() {
	case tcell.KeyEsc:
		a.closeMsgMenu()
	case tcell.KeyUp:
		a.moveMsgMenuSel(-1)
	case tcell.KeyDown:
		a.moveMsgMenuSel(1)
	case tcell.KeyEnter:
		a.mu.Lock()
		sel := -1
		if a.msgm != nil {
			sel = a.msgm.sel
		}
		a.mu.Unlock()
		if sel >= 0 {
			run(sel)
		}
	case tcell.KeyRune:
		if r := key.Rune(); r >= '1' && int(r-'1') < len(msgMenuRows) {
			run(int(r - '1'))
		} else {
			return false
		}
	default:
		return false
	}
	return true
}

// moveMsgMenuSel moves the highlight, wrapping at both ends.
func (a *App) moveMsgMenuSel(d int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.msgm == nil {
		return
	}
	a.msgm.sel = (a.msgm.sel + d + len(msgMenuRows)) % len(msgMenuRows)
	a.poke()
}

// msgMenuActionAt is the action of menu row i. Caller holds a.mu.
func (a *App) msgMenuActionAt(i int) msgMenuAction {
	if i < 0 || i >= len(msgMenuRows) {
		return msgActNone
	}
	return msgMenuRows[i].act
}

// --- painting ---

// panelRect is a screen rectangle. msgMenuHit and the click-outside test both
// need "is this point in the panel", and a bounds struct says that once.
type panelRect struct{ x, y, w, h int }

func (r panelRect) contains(x, y int) bool {
	return x >= r.x && x < r.x+r.w && y >= r.y && y < r.y+r.h
}

// msgViewBounds is the rectangle the read-only surface occupies. Caller holds
// a.mu.
func (a *App) msgViewBounds() panelRect {
	w := a.width
	x := 2
	y0 := 1
	panelH := a.height - 1 - a.composerRows() - y0 - 1
	if panelH < 4 {
		panelH = 4
	}
	return panelRect{x: x, y: y0, w: w - x, h: panelH}
}

// drawMsgMenu paints the menu next to the click that opened it and publishes
// its geometry for the next click to hit-test against.
func (a *App) drawMsgMenu() {
	m := a.msgm
	if m == nil {
		return
	}
	s := a.scr
	labelW, hintW := 0, 0
	for _, r := range msgMenuRows {
		labelW = max(labelW, width(r.label))
		hintW = max(hintW, width(r.hint))
	}
	// Width: the label column, a gap, then the widest hint, plus the borders
	// and the one-cell inset. Sizing to the widest hint (rather than to
	// max(label,hint)) is what keeps "files are NOT put back" from being cut
	// to "files are NOT pu…" on a terminal with room for it — the truncation
	// below is then only for terminals genuinely too narrow.
	w := min(a.width-4, labelW+3+hintW+4)
	h := len(msgMenuRows) + 2 // rows + top/bottom border
	// Anchor below the click, then clamp: a click near the bottom edge would
	// otherwise push the panel off screen and leave an unclickable menu.
	x := min(max(2, m.ax-1), max(2, a.width-w-2))
	y := min(max(1, m.ay+1), max(1, a.height-a.composerRows()-h-1))

	brdSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentUser)))
	fgSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.TextPrimary)))
	selFg := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.TextPrimary)))
	dimSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim)))
	bg := tcell.StyleDefault.Background(a.cellColor(a.th.Get(theme.BgBase)))
	selBg := tcell.StyleDefault.Background(a.cellColor(a.th.Get(theme.BgHighlight)))
	box := a.th.Box()

	fillPanelRows(s, y, y+h-1, x, w, bg)
	cx := x
	for _, r := range boxTop(box, brdSt, "", w).runs {
		drawText(s, cx, y, r.text, r.style)
		cx += width(r.text)
	}
	cx = x
	for _, r := range boxBottom(box, brdSt, w).runs {
		drawText(s, cx, y+h-1, r.text, r.style)
		cx += width(r.text)
	}
	vr := boxRune(box.Vertical)
	for i := y + 1; i < y+h-1; i++ {
		s.SetContent(x, i, vr, nil, brdSt)
		s.SetContent(x+w-1, i, vr, nil, brdSt)
	}

	// Publish before drawing the rows so a click that arrives on the very next
	// event — before the next paint — still hit-tests against real geometry.
	m.x, m.y, m.w, m.h = x, y, w, h
	m.hitY0 = y + 1
	m.hitRow = make([]int, len(msgMenuRows))
	for i, r := range msgMenuRows {
		ry := y + 1 + i
		m.hitRow[i] = ry
		st, rowBg := fgSt, bg
		if i == m.sel {
			st, rowBg = selFg.Bold(true), selBg
		}
		for cx := x + 1; cx < x+w-1; cx++ {
			s.SetContent(cx, ry, ' ', nil, rowBg)
		}
		drawText(s, x+2, ry, r.label, st)
		drawText(s, x+3+labelW, ry, truncateCells(r.hint, max(0, w-5-labelW), "…"), dimSt)
	}
}

// drawMsgView paints the read-only message surface above the composer.
func (a *App) drawMsgView(yComposerTop int) {
	v := a.msgv
	if v == nil {
		return
	}
	s := a.scr
	w := a.width
	x := 2
	y0 := 1
	panelH := yComposerTop - y0 - 1
	if panelH < 4 {
		return
	}
	brdSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentUser)))
	fgSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.TextPrimary)))
	dimSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim)))
	bg := tcell.StyleDefault.Background(a.cellColor(a.th.Get(theme.BgBase)))
	box := a.th.Box()
	fillPanelRows(s, y0, y0+panelH-1, x, w-x, bg)
	cx := x
	for _, r := range boxTop(box, brdSt, "", w-2*x).runs {
		drawText(s, cx, y0, r.text, r.style)
		cx += width(r.text)
	}
	cx = x
	for _, r := range boxBottom(box, brdSt, w-2*x).runs {
		drawText(s, cx, y0+panelH-1, r.text, r.style)
		cx += width(r.text)
	}
	vr := boxRune(box.Vertical)
	for y := y0 + 1; y < y0+panelH-1; y++ {
		s.SetContent(x, y, vr, nil, brdSt)
		s.SetContent(w-x-1, y, vr, nil, brdSt)
	}

	drawText(s, x+2, y0+1, "message "+strconv.Itoa(v.ord+1)+" of "+strconv.Itoa(v.blocks), fgSt.Bold(true))
	meta := "no session entry"
	if v.entryID != "" {
		meta = "entry " + v.entryID
		if len(v.entryID) > 12 {
			meta = "entry " + v.entryID[:12] + "…"
		}
	}
	if !v.ts.IsZero() {
		meta += " · " + v.ts.Format("2006-01-02 15:04:05")
	}
	drawText(s, x+2, y0+2, truncateCells(meta, w-2*x-4, "…"), dimSt)

	// Title rows at y0+1 and y0+2, footer at y0+panelH-2, bottom border at
	// y0+panelH-1 → panelH-4 interior lines the viewport can show.
	v.scrollVp = max(1, panelH-4)
	maxOff := max(0, len(v.rows)-v.scrollVp)
	if v.scrollOff > maxOff {
		v.scrollOff = maxOff
	}
	start := v.scrollOff
	end := min(start+v.scrollVp, len(v.rows))
	for i := start; i < end; i++ {
		drawText(s, x+2, y0+3+(i-start), truncateCells(v.rows[i], w-2*x-4, "…"), fgSt)
	}
	drawText(s, x+2, y0+panelH-2, "Esc close · ↑↓ scroll", dimSt)
}
