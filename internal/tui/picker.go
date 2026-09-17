package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/FreePeak/xdev/internal/theme"
	"github.com/gdamore/tcell/v2"
)

// PickerItem is one selectable row of a picker view.
type PickerItem struct {
	Label   string // left column: "@smol", "onegw/dev"
	Detail  string // dim second column: "→ onegw/free", "Free · 1M ctx"
	Value   string // opaque value handed to the view's OnSelect
	Section string // group header drawn above the first row of the group ("" = none)
	Current bool   // marks the value this session is running on
}

// PickerView is one tab of a picker. The model selector opens a roles view
// plus one view per provider (omp: an all-models view plus one per
// provider), so views are tabs rather than a nested menu.
//
// Action names what Enter does in this view ("use", "set", "resume"); it is
// the footer's verb. OnSelect overrides PickerOptions.OnSelect, which is how
// one picker offers two semantics — the roles tab assigns, a model tab
// switches — without a second key that would fight the type-to-filter.
type PickerView struct {
	Name     string
	Items    []PickerItem
	Action   string
	OnSelect func(value string)
}

// PickerOptions configures one modal list.
type PickerOptions struct {
	Title    string
	Views    []PickerView
	OnSelect func(value string) // Enter, unless the view overrides it
}

// picker is the modal list state: filter-as-you-type, view tabs, section
// headers, and a row window anchored to the selection.
type picker struct {
	opts    PickerOptions
	view    int
	query   string
	match   []int // indices into the active view's Items
	sel     int
	visible int
	// Mouse hit-test, published by the painter every frame — the same table
	// omp's SelectList rebuilds inside render(). hitY0 is the screen row of
	// hitItem[0]; hitItem[i] is the item index that painted row shows, or -1
	// for a section header. tabY is the tab strip's row (-1 when there are no
	// tabs) and tabAt[i] is the column where view i's label starts.
	hitY0   int
	hitItem []int
	tabY    int
	tabAt   []int
}

// pickAt resolves a screen cell to the list row (item index) or tab (view
// index) under it; both are -1 when the point misses the panel's targets.
func (p *picker) pickAt(x, y int) (item, view int) {
	item, view = -1, -1
	if i := y - p.hitY0; p.hitItem != nil && i >= 0 && i < len(p.hitItem) {
		item = p.hitItem[i]
	}
	if p.tabAt != nil && y == p.tabY {
		for j, start := range p.tabAt {
			if x >= start && (j+1 == len(p.tabAt) || x < p.tabAt[j+1]) {
				view = j
				break
			}
		}
	}
	return item, view
}

// selectItem parks the selection on the item the user pointed at (an index into
// the active view's Items, so it survives the filter's re-ranking).
func (p *picker) selectItem(itemIdx int) bool {
	for i, mi := range p.match {
		if mi == itemIdx {
			p.sel = i
			return true
		}
	}
	return false
}

// pickerRowCeiling is the row window's ceiling: half the terminal, the same
// budget the tree selector uses (tree.go). The window scrolls with the
// selection, so a tall terminal shows more rows — the fixed 12 this replaces
// made the list read as "12 long" while the store held hundreds.
func pickerRowCeiling(height int) int { return max(5, height/2) }

// pickerDetailCols is the column the detail ("id · date · status") is worth
// keeping; the name column takes what is left of the row.
const pickerDetailCols = 28

func newPicker(opts PickerOptions) *picker {
	// visible is the last drawn window; 0 until drawPicker measures the
	// terminal, which is why window() clamps a zero row count to 1.
	p := &picker{opts: opts}
	p.refresh()
	p.selectCurrent()
	return p
}

// selectCurrent parks the selection on the active value: opening the
// selector should show the user where they are instead of making them hunt.
func (p *picker) selectCurrent() {
	for i, idx := range p.match {
		if p.item(idx).Current {
			p.sel = i
			return
		}
	}
}

func (p *picker) active() *PickerView {
	if p.view < 0 || p.view >= len(p.opts.Views) {
		return nil
	}
	return &p.opts.Views[p.view]
}

func (p *picker) item(idx int) PickerItem {
	v := p.active()
	if v == nil || idx < 0 || idx >= len(v.Items) {
		return PickerItem{}
	}
	return v.Items[idx]
}

// refresh re-ranks the active view against the typed filter. An empty query
// keeps the configured order (providers in models.yml order): the picker is
// a menu, not a search result page.
func (p *picker) refresh() {
	v := p.active()
	p.match = p.match[:0]
	if v == nil {
		p.sel = 0
		return
	}
	q := strings.ToLower(strings.TrimSpace(p.query))
	for i, it := range v.Items {
		if q == "" {
			p.match = append(p.match, i)
			continue
		}
		hay := strings.ToLower(it.Label + " " + it.Detail + " " + it.Section)
		if fuzzyScore(it.Label, q) >= 0 || strings.Contains(hay, q) {
			p.match = append(p.match, i)
		}
	}
	if p.sel >= len(p.match) {
		p.sel = max(0, len(p.match)-1)
	}
	if p.sel < 0 {
		p.sel = 0
	}
}

// move changes the selection by delta, clamped to the list. It does not
// wrap: the row window is anchored to the selection, so wrapping would jump
// the whole panel.
func (p *picker) move(delta int) {
	n := len(p.match)
	if n == 0 {
		return
	}
	p.sel = min(max(0, p.sel+delta), n-1)
}

// switchView changes the tab, wrapping around, and resets the filter (a
// filter typed for one view usually matches nothing in the next).
func (p *picker) switchView(delta int) {
	n := len(p.opts.Views)
	if n < 2 {
		return
	}
	p.view = ((p.view+delta)%n + n) % n
	p.query = ""
	p.sel = 0
	p.refresh()
	p.selectCurrent()
}

func (p *picker) selected() (PickerItem, bool) {
	if p.sel < 0 || p.sel >= len(p.match) {
		return PickerItem{}, false
	}
	return p.item(p.match[p.sel]), true
}

// typeFilter appends a rune to the filter and re-ranks.
func (p *picker) typeFilter(r rune) {
	p.query += string(r)
	p.sel = 0
	p.refresh()
}

// backspace drops the last filter rune.
func (p *picker) backspace() {
	if p.query == "" {
		return
	}
	r := []rune(p.query)
	p.query = string(r[:len(r)-1])
	p.sel = 0
	p.refresh()
}

// pickerLine is one rendered line: either a section header or a row.
type pickerLine struct {
	header  bool
	text    string
	item    PickerItem
	itemIdx int // index into the view's Items (-1 for a header)
}

// lines expands the filtered matches into display lines. Section headers
// appear only without a filter: while searching the user wants rows.
func (p *picker) lines() []pickerLine {
	v := p.active()
	if v == nil {
		return nil
	}
	out := make([]pickerLine, 0, len(p.match))
	lastSection := ""
	for _, idx := range p.match {
		it := v.Items[idx]
		if p.query == "" && it.Section != "" && it.Section != lastSection {
			out = append(out, pickerLine{header: true, text: it.Section, itemIdx: -1})
			lastSection = it.Section
		}
		out = append(out, pickerLine{item: it, itemIdx: idx})
	}
	return out
}

// window returns the display lines to draw, the index of the first drawn
// line, and the display index of the selection (-1 when nothing matches).
func (p *picker) window(rows int) (lines []pickerLine, start, selLine int) {
	all := p.lines()
	selLine = -1
	for i, ln := range all {
		if !ln.header && p.sel < len(p.match) && ln.itemIdx == p.match[p.sel] {
			selLine = i
			break
		}
	}
	if selLine < 0 {
		return nil, 0, -1
	}
	if rows < 1 {
		rows = 1
	}
	start = 0
	if selLine >= rows {
		start = selLine - rows + 1
	}
	if start > len(all)-rows {
		start = max(0, len(all)-rows)
	}
	end := min(start+rows, len(all))
	return all[start:end], start, selLine
}

// footer renders the status/hint line: the filter (or the item count) on
// the left, the key hints on the right.
func (p *picker) footer() (left, right string) {
	if len(p.match) == 0 {
		if p.query != "" {
			return "/" + p.query + " · no rows match", "Backspace edits search · esc cancel"
		}
		return "no rows match", "esc cancel"
	}
	pos := strconv.Itoa(p.sel+1) + "/" + strconv.Itoa(len(p.match))
	if p.query != "" {
		left = "/" + p.query + "  " + pos
	} else {
		left = pos + " items"
	}
	hints := []string{"↑↓ move"}
	if p.viewCount() > 1 {
		// Tab advances, so the hint must name the view it moves TO. Naming
		// the active one read as "⇥ Roles" while Roles was already showing —
		// the key looked broken. switchView wraps, so mirror its arithmetic.
		n := p.viewCount()
		if v := p.opts.Views[((p.view+1)%n+n)%n]; v.Name != "" {
			hints = append(hints, "⇥ "+v.Name)
		}
	}
	hints = append(hints, "⏎ "+p.action())
	hints = append(hints, "esc cancel")
	return left, strings.Join(hints, " · ")
}

// action is the active view's Enter verb ("select" when it has none).
func (p *picker) action() string {
	if v := p.active(); v != nil && v.Action != "" {
		return v.Action
	}
	return "select"
}

// choose runs the Enter action for the selected row: the view's own
// callback when it set one, else the picker's default. It reports whether a
// selection was made.
func (p *picker) choose() (func(value string), bool) {
	if _, ok := p.selected(); !ok {
		return nil, false
	}
	if v := p.active(); v != nil && v.OnSelect != nil {
		return v.OnSelect, true
	}
	return p.opts.OnSelect, p.opts.OnSelect != nil
}

func (p *picker) viewCount() int { return len(p.opts.Views) }

const (
	pickerIdleCap   = 12 // rows shown without an active query
	pickerSearchCap = 50 // rows allowed while a query is active
)

// SessionPickerItem is one row of the session picker. Size is preformatted by
// cmd (SessionMeta exposes bytes, not entry counts, and counting lines per
// row would stat-read up to a dozen large files per keypress).
type SessionPickerItem struct {
	ID     string // short id (8 hex)
	Title  string
	Mtime  string // formatted, e.g. "Jan 02 15:04"
	Size   string // e.g. "128 KB"
	Pinned bool   // session-pins.json sidecar, managed by cmd
	InCwd  bool   // session's folder == the launcher's cwd
	// Status is the lifecycle badge, read from the session's tail by cmd
	// (#107): "done" when the last turn finished, "interrupted" otherwise.
	// Empty hides it (a caller that did not classify).
	Status string
}

type sessionPicker struct {
	open    bool
	src     []SessionPickerItem // cmd-provided rows, all projects, pin-sorted
	view    []SessionPickerItem // rows for the current scope + query
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

func (p *sessionPicker) selected() (SessionPickerItem, bool) {
	if !p.active() || p.sel >= len(p.view) {
		return SessionPickerItem{}, false
	}
	return p.view[p.sel], true
}

// OpenSessionPicker shows the selector. Pass nil items for a no-op (keeps
// the text-listing fallback honest: nothing to pick means nothing opens).
// Rows default to the current-folder scope; with zero InCwd rows the
// picker renders the Tab hint instead of auto-switching scope.
func (a *App) OpenSessionPicker(items []SessionPickerItem) {
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
func (a *App) SessionPickerSelection() (SessionPickerItem, bool) {
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
func pickerLocalView(src []SessionPickerItem, allProj bool, query string) []SessionPickerItem {
	view := scopeSessionPickerItems(src, allProj)
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

// scopeSessionPickerItems keeps all rows in all-projects scope, else only the
// launcher's folder.
func scopeSessionPickerItems(items []SessionPickerItem, allProj bool) []SessionPickerItem {
	if allProj {
		return items
	}
	out := make([]SessionPickerItem, 0, len(items))
	for _, it := range items {
		if it.InCwd {
			out = append(out, it)
		}
	}
	return out
}

// tokenFilterItems keeps rows where EVERY whitespace-separated token
// appears (case-insensitive) in the id, the title, or the lifecycle
// status — "interrupted" has to be a real filter, not just a badge (#107).
func tokenFilterItems(items []SessionPickerItem, query string) []SessionPickerItem {
	tokens := strings.Fields(strings.ToLower(query))
	if len(tokens) == 0 {
		return items
	}
	out := make([]SessionPickerItem, 0, len(items))
	for _, it := range items {
		hay := strings.ToLower(it.ID + " " + it.Title + " " + it.Status)
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
func pinSortItems(items []SessionPickerItem) []SessionPickerItem {
	out := make([]SessionPickerItem, 0, len(items))
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

	var view []SessionPickerItem
	switch {
	case q == "":
		view = pickerLocalView(src, allProj, "")
	case a.onPickerSearch != nil:
		// cmd ranks prompt-text matches; re-apply scope + pin-first.
		view = pinSortItems(scopeSessionPickerItems(a.onPickerSearch(q), allProj))
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
func (a *App) handleSessionPickerKey(key *tcell.EventKey) (handled bool) {
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
		kept := make([]SessionPickerItem, 0, len(p.src))
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

// sessionPickerRowText renders one row: id — title — mtime — size — status.
// The badge closes the row so a killed session is legible before Enter.
func sessionPickerRowText(it SessionPickerItem) string {
	title := it.Title
	if title == "" {
		title = "(untitled)"
	}
	row := fmt.Sprintf("%s  %s  %s  %s", it.ID, title, it.Mtime, it.Size)
	if it.Status != "" {
		row += "  " + it.Status
	}
	return row
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
		return mark + pin + " " + sessionPickerRowText(it)
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
