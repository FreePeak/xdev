package tui

import (
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// Session picker (M10 residual, omp /resume parity): /resume with no
// argument opens an interactive selector above the composer instead of a
// text listing. Up/Down move, Enter resumes the selected session, Esc
// closes. The data (id/title/mtime/entries) lives in cmd; the TUI owns
// selection and rendering only.

// PickerItem is one row of the session picker. Size is preformatted by
// cmd (SessionMeta exposes bytes, not entry counts, and counting lines per
// row would stat-read up to a dozen large files per keypress).
type PickerItem struct {
	ID    string // short id (8 hex)
	Title string
	Mtime string // formatted, e.g. "Jan 02 15:04"
	Size  string // e.g. "128 KB"
}

type sessionPicker struct {
	items []PickerItem
	sel   int
}

func (p *sessionPicker) active() bool { return p != nil && len(p.items) > 0 }

func (p *sessionPicker) move(delta int) {
	if p == nil || len(p.items) == 0 {
		return
	}
	p.sel = (p.sel + delta + len(p.items)) % len(p.items)
}

func (p *sessionPicker) selected() (PickerItem, bool) {
	if !p.active() {
		return PickerItem{}, false
	}
	return p.items[p.sel], true
}

func (p *sessionPicker) close() { p.items = nil }

// OpenSessionPicker shows the selector. Pass nil items for a no-op (keeps
// the text-listing fallback honest: nothing to pick means nothing opens).
func (a *App) OpenSessionPicker(items []PickerItem) {
	if len(items) == 0 {
		return
	}
	a.mu.Lock()
	a.spick = &sessionPicker{items: items}
	a.mu.Unlock()
	a.poke()
}

// SessionPickerOpen reports whether the picker is on screen.
func (a *App) SessionPickerOpen() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.spick.active()
}

// SessionPickerSelection returns the currently highlighted item.
func (a *App) SessionPickerSelection() (PickerItem, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.spick.selected()
}

// CloseSessionPicker dismisses the picker without resuming.
func (a *App) CloseSessionPicker() {
	a.mu.Lock()
	if a.spick != nil {
		a.spick.close()
	}
	a.mu.Unlock()
	a.poke()
}

// pickerRows renders one row per session: id — title — mtime — entries.
func pickerRowText(it PickerItem) string {
	title := it.Title
	if title == "" {
		title = "(untitled)"
	}
	return fmt.Sprintf("%s  %s  %s  %s", it.ID, title, it.Mtime, it.Size)
}

// drawSessionPicker renders the selector above the composer (same chrome
// as the slash dropdown).
func (a *App) drawSessionPicker(yComposerTop int) {
	if !a.SessionPickerOpen() {
		return
	}
	a.mu.Lock()
	items := append([]PickerItem(nil), a.spick.items...)
	sel := a.spick.sel
	a.mu.Unlock()

	rows := items
	if len(rows) > 8 {
		// Window the visible slice around the selection (slash-menu shape).
		start := sel - 4
		if start < 0 {
			start = 0
		}
		if start+8 > len(rows) {
			start = len(rows) - 8
		}
		rows = rows[start : start+8]
		sel -= start
	}
	w := a.width
	s := a.scr
	selSt := tcell.StyleDefault.Background(a.cellColor(a.th.Get(theme.BgHighlight)))
	rowSt := tcell.StyleDefault.Background(a.cellColor(a.th.Get(theme.BgBase)))

	nameW := 0
	for _, it := range rows {
		nameW = max(nameW, width(it.ID)+width(it.Title)+width(it.Mtime)+width(it.Size)+8)
	}
	nameW = min(nameW+2, w-6)

	y := yComposerTop - len(rows) - 2
	drawText(s, 2, y, "╭"+strings.Repeat("─", min(w-4, nameW+10))+"╮",
		tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.PromptBorderActive))))
	y++
	for i, it := range rows {
		st := rowSt
		if i == sel {
			st = selSt
		}
		for x := 2; x < w-2; x++ {
			s.SetContent(x, y, ' ', nil, st)
		}
		mark := "  "
		if i == sel {
			mark = "❯ "
		}
		drawText(s, 2, y, mark+pickerRowText(it),
			st.Foreground(a.cellColor(a.th.Get(theme.TextPrimary))))
		y++
	}
	drawText(s, 2, y, "╰"+strings.Repeat("─", min(w-4, nameW+10))+"╯",
		tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.PromptBorderActive))))
}

// handlePickerKey routes keys while the session picker is open. Returns
// handled=true when the key belonged to the picker.
func (a *App) handlePickerKey(key *tcell.EventKey) (handled bool) {
	if !a.SessionPickerOpen() {
		return false
	}
	switch key.Key() {
	case tcell.KeyUp:
		a.mu.Lock()
		a.spick.move(-1)
		a.mu.Unlock()
		a.poke()
		return true
	case tcell.KeyDown:
		a.mu.Lock()
		a.spick.move(1)
		a.mu.Unlock()
		a.poke()
		return true
	case tcell.KeyEnter:
		it, ok := a.SessionPickerSelection()
		a.CloseSessionPicker()
		if !ok {
			return true
		}
		if a.onPickerResume != nil {
			a.onPickerResume(it.ID)
		}
		return true
	case tcell.KeyEsc:
		a.CloseSessionPicker()
		return true
	}
	return false
}
