package tui

import (
	"sort"
	"strings"
)

// suggestion is one dropdown row (mirrors grok's SuggestionRow).
// suggestion kinds: the dropdown serves slash commands and @-file paths.
const (
	kindCommand = iota
	kindPath
)

type suggestion struct {
	Name        string // command names carry a leading '/' for display
	Description string
	Tag         string // "markdown"/"path"; "" for built-ins
	kind        int
}

// slashMenu is the autocomplete dropdown model for slash commands
// (grok slash_dropdown.rs + matcher.rs, minus nucleo: simple subsequence
// fuzzy match with consecutive-character scoring — enough for a ≤50-item
// command list).
type slashMenu struct {
	menuState
	items   []suggestion
	match   []int // indices into items, ranked
	sel     int   // selection within match
	visible int   // max visible rows (grok: MAX_VISIBLE_SUGGESTIONS = 8)
}

const maxVisibleSuggestions = 8

func newSlashMenu() *slashMenu {
	return &slashMenu{visible: maxVisibleSuggestions}
}

// pathPrefix is the composer text before the `@` token, so accepting a path
// replaces just that token instead of clobbering the sentence.
type menuState struct {
	pathPrefix string
}

// openPaths (re)builds the dropdown from a candidate list for @-completion.
func (m *slashMenu) openPaths(prefix, query string, items []suggestion) {
	m.menuState.pathPrefix = prefix
	m.items = items
	m.query(query)
}

// open (re)builds the item list from built-ins + discovered markdown
// commands and runs the query. An empty query shows everything. Command
// mode resets the path-token state so the two menus never mix.
func (m *slashMenu) open(query string, cwd string, ext ...map[string]string) {
	m.menuState.pathPrefix = ""
	m.items = nil
	var extCommands map[string]string
	if len(ext) > 0 {
		extCommands = ext[0]
	}
	for _, c := range builtinCommands() {
		m.items = append(m.items, suggestion{
			Name: "/" + c.Name, Description: c.Description,
		})
	}
	// Discovered SKILL.md packs ride the dropdown as "/skill:<name>" rows.
	m.items = append(m.items, skillSuggestions(cwd)...)
	for _, mc := range DiscoverCommands(cwd) {
		m.items = append(m.items, suggestion{
			Name: "/" + mc.Name, Description: mc.Description, Tag: "markdown",
		})
	}
	for name, desc := range extCommands {
		m.items = append(m.items, suggestion{
			Name: "/" + name, Description: desc, Tag: "extension",
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
