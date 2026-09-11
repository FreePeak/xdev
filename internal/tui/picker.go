package tui

import (
	"strconv"
	"strings"
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
}

// pickerMaxRows caps the visible row window; the panel never eats the
// transcript — the list scrolls instead.
const pickerMaxRows = 12

func newPicker(opts PickerOptions) *picker {
	p := &picker{opts: opts, visible: pickerMaxRows}
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
	pos := strconv.Itoa(p.sel+1) + "/" + strconv.Itoa(len(p.match))
	if p.query != "" {
		left = "/" + p.query + "  " + pos
	} else {
		left = pos + " items"
	}
	hints := []string{"↑↓ move"}
	if p.viewCount() > 1 {
		if v := p.active(); v != nil {
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
