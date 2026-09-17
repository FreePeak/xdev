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

// Scrollbar computes the thumb region for a right-edge scrollbar. It returns
// the viewport rows [start,end) — 0-based from the first visible row — that
// should carry the thumb glyph. ok is false when the whole transcript fits,
// so no bar is drawn at all.
//
// The thumb length is proportional to viewport/total (omp's ScrollView:
// floor(vp*vp/total), never shorter than one row, never longer than the
// track); its position tracks the current offset, thumb at the bottom means
// following the tail and at the top means viewing the oldest row.
func (m *scrollModel) Scrollbar(total, vp int) (start, end int, ok bool) {
	vp = normVP(vp)
	if total <= vp {
		return 0, 0, false
	}
	thumb := vp * vp / total
	if thumb < 1 {
		thumb = 1
	}
	if thumb > vp {
		thumb = vp
	}
	// offset counts rows above the tail: 0 = tail, maxOff = oldest row.
	maxOff := total - vp
	pos := vp - thumb // default: thumb pinned to the bottom (at the tail)
	if maxOff > 0 {
		pos = (maxOff - m.offset) * (vp - thumb) / maxOff
	}
	if pos < 0 {
		pos = 0
	}
	if pos > vp-thumb {
		pos = vp - thumb
	}
	return pos, pos + thumb, true
}
