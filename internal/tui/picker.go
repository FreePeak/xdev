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
// closes. Tab toggles current-folder ↔ all-projects scope (never
// auto-switched); typing filters rows — every whitespace-separated token
// must match id or title, and cmd may rank prompt-text matches in via
// SetPickerSearch; Backspace on an empty query arms a delete that a
// second Backspace confirms; Ctrl+P pins. Bare 'p' stays search text —
// the picker is a filter box, so a printable pin shortcut would make
// queries starting with "p" untypeable. Lifecycle
// status badges stay unrendered: SessionMeta carries no status field, so
// rows show only pin marker + title + mtime + size.
// The data (id/title/mtime/entries/pins) lives in cmd; the TUI owns
// selection and rendering only.

const (
	pickerIdleCap   = 12 // rows shown without an active query
	pickerSearchCap = 50 // rows allowed while a query is active
)

// PickerItem is one row of the session picker. Size is preformatted by
// cmd (SessionMeta exposes bytes, not entry counts, and counting lines per
// row would stat-read up to a dozen large files per keypress).
type PickerItem struct {
	ID     string // short id (8 hex)
	Title  string
	Mtime  string // formatted, e.g. "Jan 02 15:04"
	Size   string // e.g. "128 KB"
	Pinned bool   // session-pins.json sidecar, managed by cmd
	InCwd  bool   // session's folder == the launcher's cwd
}

type sessionPicker struct {
	open    bool
	src     []PickerItem // cmd-provided rows, all projects, pin-sorted
	view    []PickerItem // rows for the current scope + query
	sel     int
	query   string
	allProj bool
	confirm string // short id awaiting delete confirmation ("" = none)
}

func (p *sessionPicker) active() bool { return p != nil && p.open }

func (p *sessionPicker) move(delta int) {
	if p == nil || len(p.view) == 0 {
		return
	}
	p.sel = (p.sel + delta + len(p.view)) % len(p.view)
}

func (p *sessionPicker) selected() (PickerItem, bool) {
	if !p.active() || p.sel >= len(p.view) {
		return PickerItem{}, false
	}
	return p.view[p.sel], true
}

// OpenSessionPicker shows the selector. Pass nil items for a no-op (keeps
// the text-listing fallback honest: nothing to pick means nothing opens).
// Rows default to the current-folder scope; with zero InCwd rows the
// picker renders the Tab hint instead of auto-switching scope.
func (a *App) OpenSessionPicker(items []PickerItem) {
	if len(items) == 0 {
		return
	}
	a.mu.Lock()
	// src keeps cmd's canonical order (newest first); pin-first ordering
	// is applied per view so a pin toggle can re-sort without freezing it.
	a.spick = &sessionPicker{open: true, src: items}
	a.spick.view = pickerLocalView(items, false, "")
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
		a.spick.open = false
		a.spick.confirm = ""
	}
	a.mu.Unlock()
	a.poke()
}

// pickerLocalView computes the visible rows for the local (no search
// hook) path: scope filter, then token filter, pins floated first, and
// the idle cap when not searching.
func pickerLocalView(src []PickerItem, allProj bool, query string) []PickerItem {
	view := scopePickerItems(src, allProj)
	if query != "" {
		view = tokenFilterItems(view, query)
	}
	view = pinSortItems(view)
	limit := pickerSearchCap
	if query == "" {
		limit = pickerIdleCap
	}
	if len(view) > limit {
		view = view[:limit]
	}
	return view
}

// scopePickerItems keeps all rows in all-projects scope, else only the
// launcher's folder.
func scopePickerItems(items []PickerItem, allProj bool) []PickerItem {
	if allProj {
		return items
	}
	out := make([]PickerItem, 0, len(items))
	for _, it := range items {
		if it.InCwd {
			out = append(out, it)
		}
	}
	return out
}

// tokenFilterItems keeps rows where EVERY whitespace-separated token
// appears (case-insensitive) in the id or the title.
func tokenFilterItems(items []PickerItem, query string) []PickerItem {
	tokens := strings.Fields(strings.ToLower(query))
	if len(tokens) == 0 {
		return items
	}
	out := make([]PickerItem, 0, len(items))
	for _, it := range items {
		hay := strings.ToLower(it.ID + " " + it.Title)
		ok := true
		for _, t := range tokens {
			if !strings.Contains(hay, t) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, it)
		}
	}
	return out
}

// pinSortItems stably floats pinned rows first (mtime order within each
// group, or search ranking when cmd supplied one).
func pinSortItems(items []PickerItem) []PickerItem {
	out := make([]PickerItem, 0, len(items))
	for _, it := range items {
		if it.Pinned {
			out = append(out, it)
		}
	}
	for _, it := range items {
		if !it.Pinned {
			out = append(out, it)
		}
	}
	return out
}

// pickerRefresh recomputes the visible rows after a query or scope
// change. It runs WITHOUT a.mu held (the search hook is cmd code) and is
// a no-op once the picker closed.
func (a *App) pickerRefresh() {
	a.mu.Lock()
	p := a.spick
	if p == nil || !p.open {
		a.mu.Unlock()
		return
	}
	q, allProj, src := p.query, p.allProj, p.src
	a.mu.Unlock()

	var view []PickerItem
	switch {
	case q == "":
		view = pickerLocalView(src, allProj, "")
	case a.onPickerSearch != nil:
		// cmd ranks prompt-text matches; re-apply scope + pin-first.
		view = pinSortItems(scopePickerItems(a.onPickerSearch(q), allProj))
	default:
		view = pickerLocalView(src, allProj, q)
	}

	a.mu.Lock()
	if a.spick == p {
		p.view = view
		if p.sel >= len(view) {
			p.sel = 0
		}
	}
	a.mu.Unlock()
	a.poke()
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
	case tcell.KeyTab:
		a.mu.Lock()
		a.spick.allProj = !a.spick.allProj
		a.spick.sel = 0
		a.mu.Unlock()
		a.pickerRefresh()
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
		a.mu.Lock()
		if a.spick.confirm != "" {
			// First Esc cancels the armed delete, not the picker.
			a.spick.confirm = ""
			a.mu.Unlock()
			a.poke()
			return true
		}
		a.mu.Unlock()
		a.CloseSessionPicker()
		return true
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		a.pickerBackspace()
		return true
	case tcell.KeyCtrlP:
		a.pickerTogglePin()
		return true
	case tcell.KeyRune:
		r := key.Rune()
		a.mu.Lock()
		if a.spick.confirm != "" {
			a.spick.confirm = "" // typing abandons the armed delete
		}
		a.spick.query += string(r)
		a.spick.sel = 0
		a.mu.Unlock()
		a.pickerRefresh()
		return true
	}
	return false
}

// pickerBackspace: delete-last-char while searching; on an empty query it
// arms a confirmed delete of the highlighted row (second Backspace
// confirms, Esc cancels).
func (a *App) pickerBackspace() {
	a.mu.Lock()
	p := a.spick
	if p == nil || !p.active() {
		a.mu.Unlock()
		return
	}
	switch {
	case p.confirm != "":
		id, fn := p.confirm, a.onPickerDelete
		p.confirm = ""
		a.mu.Unlock()
		a.pickerConfirmDelete(id, fn)
	case p.query != "":
		r := []rune(p.query)
		p.query = string(r[:len(r)-1])
		p.sel = 0
		a.mu.Unlock()
		a.pickerRefresh()
	default:
		if it, ok := p.selected(); ok && a.onPickerDelete != nil {
			p.confirm = it.ID
		}
		a.mu.Unlock()
		a.poke()
	}
}

// pickerConfirmDelete runs the cmd-side delete and drops the row from the
// source list so the picker reflects reality without a re-open.
func (a *App) pickerConfirmDelete(id string, fn func(id string) error) {
	if fn == nil {
		return
	}
	if err := fn(id); err != nil {
		a.AddSystemBlock("picker: delete " + id + ": " + err.Error())
		a.poke()
		return
	}
	a.mu.Lock()
	if p := a.spick; p != nil {
		kept := make([]PickerItem, 0, len(p.src))
		for _, it := range p.src {
			if it.ID != id {
				kept = append(kept, it)
			}
		}
		p.src = kept
		if p.sel >= len(p.view) {
			p.sel = 0
		}
	}
	a.mu.Unlock()
	a.pickerRefresh()
}

// pickerTogglePin flips the highlighted row's pin via the cmd sidecar and
// re-orders locally (pinned first).
func (a *App) pickerTogglePin() {
	a.mu.Lock()
	p := a.spick
	if p == nil || !p.active() {
		a.mu.Unlock()
		return
	}
	it, ok := p.selected()
	fn := a.onPickerPinToggle
	a.mu.Unlock()
	if !ok {
		return
	}
	if fn == nil {
		return // not wired (nil ops no-op): never show a pin that cannot persist
	}
	fn(it.ID) // cmd persists session-pins.json; errors surface there
	a.mu.Lock()
	for i := range p.src {
		if p.src[i].ID == it.ID {
			p.src[i].Pinned = !p.src[i].Pinned
		}
	}
	a.mu.Unlock()
	a.pickerRefresh() // rebuild from canonical src: pins float, order stays stable
}

// pickerRowText renders one row: id — title — mtime — size.
func pickerRowText(it PickerItem) string {
	title := it.Title
	if title == "" {
		title = "(untitled)"
	}
	return fmt.Sprintf("%s  %s  %s  %s", it.ID, title, it.Mtime, it.Size)
}

// drawSessionPicker renders the selector above the composer (same chrome
// as the slash dropdown): a scope-labelled border, the pinned-first rows,
// and a footer line for the query, the delete confirmation, or the
// empty-folder hint.
func (a *App) drawSessionPicker(yComposerTop int) {
	// Callers hold a.mu (draw does), so this must not re-lock — use the
	// internal accessors directly (SessionPickerOpen would deadlock).
	p := a.spick
	if p == nil || !p.active() {
		return
	}
	rows, lo := p.view, 0
	if len(p.view) > 8 {
		// Window the visible slice around the selection (slash-menu shape).
		lo = p.sel - 4
		if lo < 0 {
			lo = 0
		}
		if lo+8 > len(p.view) {
			lo = len(p.view) - 8
		}
		rows = p.view[lo : lo+8]
	}
	selIdx := p.sel - lo

	label := " resume — current folder · Tab all projects · Ctrl+P pin "
	if p.allProj {
		label = " resume — all projects · Tab current folder · Ctrl+P pin "
	}
	var foot []string
	if p.query != "" {
		foot = append(foot, "/"+p.query+" — Enter resume · Esc close")
	}
	switch {
	case p.confirm != "":
		foot = append(foot, "delete "+p.confirm+"? Backspace again confirms · Esc cancels")
	case len(p.view) == 0 && p.allProj:
		foot = append(foot, "No matching sessions.")
	case len(p.view) == 0:
		foot = append(foot, "No sessions in current folder. Press Tab to view all.")
	}

	w := a.width
	s := a.scr
	selSt := tcell.StyleDefault.Background(a.cellColor(a.th.Get(theme.BgHighlight)))
	rowSt := tcell.StyleDefault.Background(a.cellColor(a.th.Get(theme.BgBase)))
	dimSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.Gray)))
	borderSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.PromptBorderActive)))

	rowText := func(i int) string {
		it := rows[i]
		mark := "  "
		if i == selIdx {
			mark = "❯ "
		}
		pin := " "
		if it.Pinned {
			pin = "★"
		}
		return mark + pin + " " + pickerRowText(it)
	}
	inner := width(label)
	for i := range rows {
		inner = max(inner, width(rowText(i)))
	}
	for _, f := range foot {
		inner = max(inner, width(f))
	}
	inner = min(inner+2, w-6)

	y := yComposerTop - len(rows) - len(foot) - 2
	if y < 1 {
		return
	}
	box := a.th.Box()
	drawText(s, 2, y, box.TopLeft+label+strings.Repeat(box.Horizontal, max(0, inner-width(label)))+box.TopRight, borderSt)
	y++
	for i := range rows {
		st := rowSt
		if i == selIdx {
			st = selSt
		}
		for x := 2; x < w-2; x++ {
			s.SetContent(x, y, ' ', nil, st)
		}
		drawText(s, 2, y, rowText(i), st.Foreground(a.cellColor(a.th.Get(theme.TextPrimary))))
		y++
	}
	for _, f := range foot {
		for x := 2; x < w-2; x++ {
			s.SetContent(x, y, ' ', nil, rowSt)
		}
		drawText(s, 2, y, f, dimSt)
		y++
	}
	drawText(s, 2, y, box.BottomLeft+strings.Repeat(box.Horizontal, inner)+box.BottomRight, borderSt)
}
