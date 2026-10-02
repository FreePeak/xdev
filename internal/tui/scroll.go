package tui

// scrollModel tracks the transcript viewport: offset counts lines scrolled
// up from the live tail (0 = tail). follow is true while the viewport
// tracks new output; scrolling up clears it, returning to the bottom
// restores it. last remembers the total row count at the previous update so
// NewContent can keep the user's position while streaming appends.
type scrollModel struct {
	offset int
	follow bool
	last   int
}

func newScrollModel() scrollModel { return scrollModel{follow: true} }

// normVP keeps a degenerate viewport (<=0) at one row so every calculation
// stays well-defined.
func normVP(vp int) int {
	if vp < 1 {
		return 1
	}
	return vp
}

// maxOffset is the largest meaningful offset for the current content size.
func maxOffset(total, vp int) int {
	if m := total - normVP(vp); m > 0 {
		return m
	}
	return 0
}

// clamp forces the offset into [0, maxOffset] and re-derives follow from the
// final position: offset 0 (tail, or content that fits) means following.
func (m *scrollModel) clamp(total, vp int) {
	if max := maxOffset(total, vp); m.offset > max {
		m.offset = max
	}
	if m.offset < 0 {
		m.offset = 0
	}
	m.follow = m.offset == 0
}

// ScrollUp moves the viewport toward older rows by n lines (n <= 0 no-op).
func (m *scrollModel) ScrollUp(n, total, vp int) {
	if n <= 0 {
		return
	}
	m.offset += n
	m.clamp(total, vp)
}

// ScrollDown moves the viewport toward newer rows by n lines (n <= 0
// no-op). Reaching the tail resumes following.
func (m *scrollModel) ScrollDown(n, total, vp int) {
	if n <= 0 {
		return
	}
	m.offset -= n
	m.clamp(total, vp)
}

// Top jumps to the oldest row (follow clears unless everything fits).
func (m *scrollModel) Top(total, vp int) {
	m.offset = maxOffset(total, vp)
	m.clamp(total, vp)
}

// Bottom jumps back to the live tail and resumes following.
func (m *scrollModel) Bottom() {
	m.offset = 0
	m.follow = true
}

// Start is the index of the first visible row; renderers slice
// rows[start : start+vp]. The viewport is tail-anchored: offset counts rows
// above the tail, so Start = total - vp - offset.
func (m *scrollModel) Start(total, vp int) int {
	if total < 0 {
		return 0
	}
	if s := total - normVP(vp) - m.offset; s > 0 {
		return s
	}
	return 0
}

// Following reports whether the viewport is pinned to the live tail.
func (m *scrollModel) Following() bool { return m.follow }

// NewContent is called whenever the rendered row count changes. While
// following it stays at the tail; otherwise the same visible content stays
// in place (the offset absorbs the row delta) so streaming output never
// drags the user's position. Clamps to the new bounds.
func (m *scrollModel) NewContent(total, vp int) {
	if !m.follow && m.last > 0 {
		m.offset += total - m.last // preserve the pinned content
	}
	m.last = total
	m.clamp(total, vp)
}

// Indicator returns the rows hidden above and below the viewport (0,0 when
// everything fits or the viewport is at the tail).
func (m *scrollModel) Indicator(total, vp int) (up, down int) {
	if total <= 0 {
		return 0, 0
	}
	start := m.Start(total, vp)
	end := start + normVP(vp)
	if end > total {
		end = total
	}
	return start, total - end
}

// Scrollbar computes the thumb geometry for a right-edge scrollbar in HALF
// ROWS — the unit opencode's slider works in (two virtual cells per terminal
// row, so a thumb that lands between two rows paints the upper or lower half
// block instead of rounding the movement away). It returns the half-row span
// [start,end) the thumb covers, measured from the first visible row; ok is
// false when the whole transcript fits, so no bar is drawn at all.
//
// The thumb length is proportional to viewport/total, floored, never shorter
// than one half row and never longer than the track (omp's ScrollView:
// floor(vp*vp/total)); its position tracks the current offset, thumb at the
// bottom means following the tail and at the top means the oldest row.
func (m *scrollModel) Scrollbar(total, vp int) (start, end int, ok bool) {
	vp = normVP(vp)
	if total <= vp {
		return 0, 0, false
	}
	// Half rows throughout: a terminal row is two of them, so the track is
	// 2*vp tall and a thumb only one row long still has a row of travel to
	// use — the resolution that makes a long transcript drag smoothly instead
	// of jumping a whole row per move.
	track := 2 * vp
	thumb := 2 * vp * vp / total
	if thumb < 1 {
		thumb = 1
	}
	if thumb > track {
		thumb = track
	}
	// offset counts rows above the tail: 0 = tail, maxOff = oldest row.
	maxOff := total - vp
	pos := track - thumb // default: thumb pinned to the bottom (at the tail)
	if maxOff > 0 {
		pos = (maxOff - m.offset) * (track - thumb) / maxOff
	}
	if pos < 0 {
		pos = 0
	}
	if pos > track-thumb {
		pos = track - thumb
	}
	return pos, pos + thumb, true
}

// sbGlyph names the cell that paints one track row of a scrollbar whose thumb
// covers the half-row span [from,to) of a 2*vp-tall track: a full block where
// both halves are covered, the upper or lower half block where only one is, and
// a space for the bare groove. opencode's slider draws the same three states
// (█ ▀ ▄) over a filled track, which is what makes its bar move by halves
// instead of rows.
func sbGlyph(row, from, to int) rune {
	if to <= from {
		return ' ' // an empty span is the bare groove, never a half block
	}
	hi, lo := 2*row, 2*row+1 // this row's upper and lower half row
	switch {
	case to <= hi || from > lo:
		return ' '
	case from <= hi && to > lo:
		return '█'
	case from <= hi:
		return '▀'
	default:
		return '▄'
	}
}
