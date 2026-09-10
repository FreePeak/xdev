package tui

import (
	"sort"
	"strings"
)

// suggestion is one dropdown row (mirrors grok's SuggestionRow).
type suggestion struct {
	Name        string // with leading '/' for display
	Description string
	Tag         string // "markdown" for discovered commands, "" for built-ins
}

// slashMenu is the autocomplete dropdown model for slash commands
// (grok slash_dropdown.rs + matcher.rs, minus nucleo: simple subsequence
// fuzzy match with consecutive-character scoring — enough for a ≤50-item
// command list).
type slashMenu struct {
	items   []suggestion
	match   []int // indices into items, ranked
	sel     int   // selection within match
	visible int   // max visible rows (grok: MAX_VISIBLE_SUGGESTIONS = 8)
}

const maxVisibleSuggestions = 8

func newSlashMenu() *slashMenu {
	return &slashMenu{visible: maxVisibleSuggestions}
}

// open (re)builds the item list from built-ins + discovered markdown
// commands and runs the query. An empty query shows everything.
func (m *slashMenu) open(query string, cwd string) {
	m.items = nil
	for _, c := range builtinCommands() {
		m.items = append(m.items, suggestion{
			Name: "/" + c.Name, Description: c.Description,
		})
	}
	for _, mc := range DiscoverCommands(cwd) {
		m.items = append(m.items, suggestion{
			Name: "/" + mc.Name, Description: mc.Description, Tag: "markdown",
		})
	}
	m.query(query)
}

// query re-ranks items against the typed text (the word after "/",
// without the slash). Selection resets to the first row.
func (m *slashMenu) query(q string) {
	q = strings.ToLower(strings.TrimSpace(q))
	type scored struct {
		idx, score int
	}
	var hits []scored
	for i, it := range m.items {
		s := fuzzyScore(strings.TrimPrefix(it.Name, "/"), q)
		if s >= 0 {
			hits = append(hits, scored{i, s})
		}
	}
	// Score desc, then name asc — stable and deterministic.
	sort.SliceStable(hits, func(a, b int) bool {
		if hits[a].score != hits[b].score {
			return hits[a].score > hits[b].score
		}
		return m.items[hits[a].idx].Name < m.items[hits[b].idx].Name
	})
	m.match = m.match[:0]
	for _, h := range hits {
		m.match = append(m.match, h.idx)
	}
	m.sel = 0
}

// fuzzyScore returns -1 when q is not a subsequence of s; otherwise a
// score where exact-prefix > word-start > consecutive subsequence > sparse.
// Case-insensitive on ASCII.
func fuzzyScore(s, q string) int {
	if q == "" {
		return 0
	}
	s, q = strings.ToLower(s), strings.ToLower(q)
	if strings.HasPrefix(s, q) {
		return 1000 - len(s) // shorter exact-prefix wins
	}
	score := 0
	si := strings.IndexByte(s, q[0])
	if si < 0 {
		return -1
	}
	if si == 0 || s[si-1] == '-' || s[si-1] == ' ' {
		score += 50 // word-start match
	}
	for qi := 1; qi < len(q); qi++ {
		next := strings.IndexByte(s[si+1:], q[qi])
		if next < 0 {
			return -1
		}
		si++
		if next == 0 {
			score++ // consecutive
		} else {
			score-- // sparse gap
		}
		si += next
	}
	return score
}

// active reports whether a dropdown is showing with matches.
func (m *slashMenu) active() bool { return len(m.match) > 0 }

// selected returns the current selection, if any.
func (m *slashMenu) selected() (suggestion, bool) {
	if !m.active() || m.sel >= len(m.match) {
		return suggestion{}, false
	}
	return m.items[m.match[m.sel]], true
}

// move changes the selection by delta (up = -1), wrapping within the
// visible window (grok wraps the full list).
func (m *slashMenu) move(delta int) {
	n := len(m.match)
	if n == 0 {
		return
	}
	m.sel = ((m.sel+delta)%n + n) % n
}

// rows renders up to visible rows starting at a window anchored to keep
// the selection in view (grok: MAX_DROPDOWN_ROWS = 8).
func (m *slashMenu) rows() []suggestion {
	if !m.active() {
		return nil
	}
	start := 0
	if m.sel >= m.visible {
		start = m.sel - m.visible + 1
	}
	if start > len(m.match)-m.visible {
		start = max(0, len(m.match)-m.visible)
	}
	end := min(start+m.visible, len(m.match))
	out := make([]suggestion, 0, end-start)
	for _, idx := range m.match[start:end] {
		out = append(out, m.items[idx])
	}
	return out
}
