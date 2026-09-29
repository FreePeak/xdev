package tui

import "testing"

// scrollSteps: Start = total - vp - offset (tail-anchored; offset counts
// rows above the tail). Indicator(up, down) = (start, total - end).
func scrollSteps() []struct {
	name           string
	setup          func(m *scrollModel)
	total, vp      int
	wantStart      int
	wantFollow     bool
	wantUp, wantDn int
} {
	return []struct {
		name           string
		setup          func(m *scrollModel)
		total, vp      int
		wantStart      int
		wantFollow     bool
		wantUp, wantDn int
	}{
		{"content fits", func(m *scrollModel) {}, 10, 20, 0, true, 0, 0},
		{"at tail tall content", func(m *scrollModel) {}, 100, 20, 80, true, 80, 0},
		{"scrolled up 5", func(m *scrollModel) { m.ScrollUp(5, 100, 20) }, 100, 20, 75, false, 75, 5},
		{"up then down back", func(m *scrollModel) { m.ScrollUp(5, 100, 20); m.ScrollDown(5, 100, 20) }, 100, 20, 80, true, 80, 0},
		{"up then partial down", func(m *scrollModel) { m.ScrollUp(9, 100, 20); m.ScrollDown(4, 100, 20) }, 100, 20, 75, false, 75, 5},
		{"top", func(m *scrollModel) { m.Top(100, 20) }, 100, 20, 0, false, 0, 80},
		{"top then bottom", func(m *scrollModel) { m.Top(100, 20); m.Bottom() }, 100, 20, 80, true, 80, 0},
		{"clamp huge up", func(m *scrollModel) { m.ScrollUp(1_000, 100, 20) }, 100, 20, 0, false, 0, 80},
		{"clamp huge down", func(m *scrollModel) { m.ScrollUp(30, 100, 20); m.ScrollDown(1_000, 100, 20) }, 100, 20, 80, true, 80, 0},
		{"degenerate vp 0", func(m *scrollModel) { m.ScrollUp(5, 100, 0) }, 100, 0, 94, false, 94, 5},
		{"degenerate total 0", func(m *scrollModel) { m.ScrollUp(5, 0, 20) }, 0, 20, 0, true, 0, 0},
		{"negative n ignored", func(m *scrollModel) { m.ScrollUp(-5, 100, 20) }, 100, 20, 80, true, 80, 0},
		{"viewport bigger than content stays follow", func(m *scrollModel) { m.ScrollUp(3, 10, 20) }, 10, 20, 0, true, 0, 0},
	}
}

func TestScrollModelSteps(t *testing.T) {
	for _, tc := range scrollSteps() {
		t.Run(tc.name, func(t *testing.T) {
			m := newScrollModel()
			if tc.setup != nil {
				tc.setup(&m)
			}
			m.NewContent(tc.total, tc.vp)
			if got := m.Start(tc.total, tc.vp); got != tc.wantStart {
				t.Fatalf("Start = %d, want %d", got, tc.wantStart)
			}
			if got := m.Following(); got != tc.wantFollow {
				t.Fatalf("Following = %v, want %v", got, tc.wantFollow)
			}
			up, down := m.Indicator(tc.total, tc.vp)
			if up != tc.wantUp || down != tc.wantDn {
				t.Fatalf("Indicator = (%d,%d), want (%d,%d)", up, down, tc.wantUp, tc.wantDn)
			}
		})
	}
}

func TestNewContentPreservesPosition(t *testing.T) {
	// ScrollUp(10) from the tail pins the window to rows 70..89. Appending
	// 10 rows below must keep exactly that window in view: the offset grows
	// by the row delta (10 → 20), so Start stays 70.
	m := newScrollModel()
	m.NewContent(100, 20)
	m.ScrollUp(10, 100, 20) // start = 100-20-10 = 70
	m.NewContent(110, 20)   // +10 rows appended below
	if got := m.Start(110, 20); got != 70 {
		t.Fatalf("Start after append = %d, want 70 (window rows 70..89 kept)", got)
	}
	if m.Following() {
		t.Fatal("still following after append while scrolled")
	}
	// Shrink below the offset: clamps, never negative.
	m.NewContent(15, 20)
	if got := m.Start(15, 20); got != 0 {
		t.Fatalf("Start after shrink = %d, want 0", got)
	}
}

func TestNewContentFollowsTail(t *testing.T) {
	m := newScrollModel()
	for total := 10; total <= 100; total += 10 {
		m.NewContent(total, 20)
	}
	if got := m.Start(100, 20); got != 80 {
		t.Fatalf("Start while following = %d, want 80", got)
	}
	// Bottom + NewContent resumes following after scrolling away.
	m.ScrollUp(30, 100, 20)
	m.Bottom()
	m.NewContent(150, 20)
	if !m.Following() || m.Start(150, 20) != 130 {
		t.Fatalf("Bottom+NewContent: follow=%v start=%d, want true/130", m.Following(), m.Start(150, 20))
	}
}

func TestScrollModelNoPanicDegenerate(t *testing.T) {
	m := newScrollModel()
	m.ScrollUp(-1, -5, 0)
	m.ScrollDown(5, -5, 0)
	m.Top(-5, 0)
	m.Bottom()
	m.NewContent(-5, 0)
	m.Start(-5, 0)
	m.Indicator(-5, 0)
}

func TestScrollbar(t *testing.T) {
	// Fits: no bar.
	m := newScrollModel()
	if _, _, ok := m.Scrollbar(15, 20); ok {
		t.Fatal("scrollbar shown when content fits the viewport")
	}
	// total=100 vp=20 → the track is 2*vp=40 half rows and the thumb is
	// 2*vp*vp/total = 8 half rows (4 rows), maxOff = 80. Geometry is in half
	// rows — the unit opencode's slider works in.
	m = newScrollModel()
	start, end, ok := m.Scrollbar(100, 20) // offset 0 = tail
	if !ok || end != 40 || end-start != 8 {
		t.Fatalf("at tail: got [%d,%d) ok=%v, want an 8-half-row thumb ending at 40", start, end, ok)
	}
	m.Top(100, 20) // offset = maxOff
	start, end, _ = m.Scrollbar(100, 20)
	if start != 0 || end != 8 {
		t.Fatalf("at top: got [%d,%d), want [0,8)", start, end)
	}
	// Monotonic: scrolling up from the tail walks the thumb toward half row 0.
	m = newScrollModel()
	prev := 1 << 30
	for off := 0; off <= 80; off += 20 {
		m.offset = off
		s, e, ok := m.Scrollbar(100, 20)
		if !ok {
			t.Fatalf("bar vanished at offset %d", off)
		}
		if s > prev {
			t.Fatalf("thumb moved down while scrolling up at offset %d (%d>%d)", off, s, prev)
		}
		if e-s != 8 {
			t.Fatalf("thumb length = %d half rows at offset %d, want 8", e-s, off)
		}
		prev = s
	}
	// Half-row resolution: the thumb's position comes out of the offset by
	// flooring, so a whole-row track could only ever show vp-thumb distinct
	// spots while the scrollback was swept — every row of a long transcript
	// moved the bar the same single step. The half-row track shows twice as
	// many, which is the resolution opencode's slider works in.
	m = newScrollModel()
	seen := map[int]bool{}
	for off := 0; off <= 200-20; off++ {
		m.offset = off
		s, _, _ := m.Scrollbar(200, 20)
		seen[s] = true
	}
	if len(seen) <= 20-(2*20*20/200) {
		t.Fatalf("a sweep of the scrollback visited %d thumb positions, want more than the %d a whole-row track can show",
			len(seen), 20-(2*20*20/200))
	}
	// A taller viewport than the thumb budget still shows at least one half row.
	if s, e, ok := m.Scrollbar(1000, 30); !ok || e-s < 1 {
		t.Fatalf("degenerate thumb [%d,%d) ok=%v", s, e, ok)
	}
}

// TestSBGlyph pins the three cells a scrollbar column can hold: the full block
// where the thumb covers the row, the half block where it covers one half, and
// the bare groove where it does not touch. opencode's slider draws exactly
// these three states, which is what lets its thumb land between rows.
func TestSBGlyph(t *testing.T) {
	if got := sbGlyph(3, 6, 8); got != '█' {
		t.Fatalf("row 3 of thumb [6,8) = %q, want a full block", got)
	}
	if got := sbGlyph(3, 6, 7); got != '▀' {
		t.Fatalf("row 3 of thumb [6,7) = %q, want the upper half block", got)
	}
	if got := sbGlyph(3, 7, 8); got != '▄' {
		t.Fatalf("row 3 of thumb [7,8) = %q, want the lower half block", got)
	}
	for row := range 5 {
		if got := sbGlyph(row, 9, 9); got != ' ' {
			t.Fatalf("row %d outside the thumb = %q, want the bare groove", row, got)
		}
	}
}
