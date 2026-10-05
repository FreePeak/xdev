package tui

// The status row's two dsh pills, and the popup a click on one opens
// (deepseek-harness StatsPills.tsx: the TimePill / UsagePill pair on the
// composer dock, each a button whose dialog breaks the inline reading out).
//
// The row is the loudest surface in the TUI and the shortest one: it shares
// the bottom line with the working directory, and every segment that lands
// there costs the path a column. dsh solved it by keeping TWO pills and moving
// everything else behind a click — the pill reads a headline (counts · speed,
// total · cache hit) and its dialog carries the breakdown. That is the split
// this file implements, and it is why the default layout shrank to two
// segments: the numbers the popups show are not lost, they are one click away.
//
// Every figure in a popup comes from usageSnapshot, the same read /usage
// renders, so a pill and its report can never disagree.

import (
	"fmt"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// The two pill segments. They are the only names the default row uses, and
// the only two with a popup; every other segment stays available through
// settings statusLine.segments as a plain reading.
const (
	pillTime  = "time"
	pillToken = "tokens"
)

// statusPopup is the open detail panel. It is anchored to the pill that
// opened it (ax, ay) and repaints every frame from a fresh snapshot, so a
// number that moves while the panel is up is the number on screen.
type statusPopup struct {
	// pill names the segment whose breakdown this is: pillTime renders
	// the timing block, pillToken the token block.
	pill string
	// ax/ay is the pill's left column and the status row, which is where
	// the panel is clamped: a popup on a bottom row opens upward.
	ax, ay int
	// x, y, w, h are published by the painter each frame, so the
	// click-outside test answers against the geometry that was on screen
	// rather than a recomputation that can drift (the msgMenu contract).
	x, y, w, h int
}

// statusHit is one pill's on-screen rectangle, published by drawHUD every
// frame and consumed by the press that opens its popup. A pill that is not
// on screen this frame has no entry, so it cannot be clicked — the same
// contract a.jump (the "↓ n new" chip) keeps.
type statusHit struct {
	name string
	rect panelRect
}

// statusPills is the clickable vocabulary: the two segments whose text is a
// dsh pill's own label, and therefore the two a click can break out. Every
// other segment stays a plain reading, opt-in through settings
// statusLine.segments.
var statusPills = map[string]bool{
	pillTime:  true,
	pillToken: true,
}

// openStatusPopup puts the panel up for the pill under (x, y). It toggles:
// a click on the pill whose panel is already open closes it, which is what
// makes the popup dismissible without a second surface to click.
// Caller holds a.mu.
func (a *App) openStatusPopup(name string, x, y int) {
	if a.statusPop != nil && a.statusPop.pill == name {
		a.statusPop = nil
	} else {
		a.statusPop = &statusPopup{pill: name, ax: x, ay: y}
	}
	a.poke()
}

// statusHitAt returns the pill whose published rect covers (x, y), or "".
// Caller holds a.mu.
func (a *App) statusHitAt(x, y int) string {
	for _, hit := range a.statusHits {
		if hit.name != "" && hit.rect.contains(x, y) {
			return hit.name
		}
	}
	return ""
}

// handleStatusPopupMouse gives the open popup the mouse before the
// transcript sees it. It answers true when the event belonged to the panel,
// so a click that dismisses it cannot also anchor a selection on the row
// underneath.
func (a *App) handleStatusPopupMouse(m *tcell.EventMouse, press bool) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.statusPop == nil {
		return false
	}
	x, y := m.Position()
	if m.Buttons()&tcell.Button1 != 0 && press {
		// A press ON another pill switches panels instead of dismissing
		// and reopening: the two pills are one control surface, so moving
		// between them reads as one gesture.
		if name := a.statusHitAt(x, y); name != "" {
			a.openStatusPopup(name, x, y)
			return true
		}
	}
	// Only a NEW press dismisses. The release and the drag reports of the
	// gesture that opened the panel arrive at the pill's own cell, which is
	// the row BELOW the panel it just put up — tested against the panel's own
	// rectangle they closed it again on the button-up, so one click flashed
	// the breakdown and a second click could not leave it up: the popup read
	// as a press-and-hold peek. A report that is not a press is a click
	// somewhere else, and that is what dismisses.
	if press && !a.statusPopupBounds().contains(x, y) {
		a.statusPop = nil
		a.poke()
	}
	return true
}

// statusPopupBounds is the rectangle the open panel occupies, clamped to
// what the frame can show. A panel with no published geometry yet (the click
// that opened it arrived before the next paint) falls back to the anchor
// cell, which is enough for a click-outside test.
// Caller holds a.mu.
func (a *App) statusPopupBounds() panelRect {
	p := a.statusPop
	if p == nil {
		return panelRect{}
	}
	if p.w == 0 || p.h == 0 {
		return panelRect{x: p.ax, y: p.ay, w: 1, h: 1}
	}
	return panelRect{x: p.x, y: p.y, w: p.w, h: p.h}
}

// StatusPopupOpen reports whether a pill's detail panel is up.
func (a *App) StatusPopupOpen() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.statusPop != nil
}

// closeStatusPopup dismisses the panel. Caller holds a.mu.
func (a *App) closeStatusPopupLocked() {
	if a.statusPop == nil {
		return
	}
	a.statusPop = nil
	a.poke()
}

// --- painting ---

// drawStatusPopup paints the open panel above the status row, anchored to
// the pill that opened it and clamped inside the terminal: the anchor is the
// pill's own cell, and the row is the last line, so the panel opens UP and a
// pill near the right edge slides left rather than pushing off screen.
func (a *App) drawStatusPopup() {
	p := a.statusPop
	if p == nil {
		return
	}
	title, rows := a.statusPopupBody(p.pill)
	if len(rows) == 0 {
		return
	}
	s := a.scr
	labelW := 0
	for _, r := range rows {
		labelW = max(labelW, width(r[0]))
	}
	valueW := 0
	for _, r := range rows {
		valueW = max(valueW, width(r[1]))
	}
	// Rows + the two border rows, and the label/value columns plus the two
	// pad cells and the two borders.
	w := min(a.rightEdge()-4, labelW+valueW+6)
	h := len(rows) + 2
	// The panel opens UP from the status row: its bottom sits one row above
	// the pill that opened it, and a panel taller than the space above the
	// row is pinned to the top of the terminal rather than shifted down onto
	// the row — the row it is explaining is the one thing the panel must never
	// cover. It may overlap the composer; the msgmenu makes the same trade
	// for a panel anchored lower down, and a metrics answer is worth a
	// partially visible prompt box.
	x := min(max(2, p.ax), max(2, a.rightEdge()-w-2))
	y := max(0, p.ay-h)

	brdSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.StatusLineSep)))
	lblSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim)))
	valSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.TextPrimary)))
	// The field is the terminal's own background (SGR 49), the call #495 made
	// for the sidebar: theme.BgBase (#141414) painted a grey band under text
	// cells that resolve to the terminal's black anyway, which is the same
	// striping #466 built out of the diff popup. On a dark terminal that is
	// black; on the light theme it is light, so the label/value inks are
	// never stranded on a black box.
	bg := tcell.StyleDefault
	box := a.th.Box()

	fillPanelRows(s, y, y+h-1, x, x+w, bg)
	cx := x
	for _, r := range boxTop(box, brdSt, title, w).runs {
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
	// Publish before the rows are drawn, so a click arriving before the next
	// paint still hit-tests against real geometry.
	p.x, p.y, p.w, p.h = x, y, w, h

	inner := w - 2
	for i, row := range rows {
		ry := y + 1 + i
		drawText(s, x+2, ry, truncateCells(row[0], inner, "…"), lblSt)
		drawText(s, x+3+labelW, ry, truncateCells(row[1], max(0, inner-1-labelW), "…"), valSt)
	}
}

// statusPopupBody is the panel's title and its label/value rows, built from
// the one session snapshot. Zero means nobody made the measurement, so the
// row is dropped rather than drawn as a zero: an unwired cost must not read
// as a billing bug, and an unmeasured speed must not read as a stall.
// Caller holds a.mu (the painter takes it once per frame), so it reads the
// snapshot through the locked twin rather than taking the lock again.
func (a *App) statusPopupBody(pill string) (string, [][2]string) {
	r := a.usageSnapshotLocked()
	switch pill {
	case pillTime:
		rows := make([][2]string, 0, 8)
		// dsh's stats.counts, the pill's own label: the turn/step shape of
		// the session. Hidden at zero for the same reason as the rest.
		if r.turns > 0 || r.steps > 0 {
			rows = append(rows, [2]string{"turns / steps", fmt.Sprintf("%d / %d", r.turns, r.steps)})
		}
		if r.llmWork > 0 {
			rows = append(rows, [2]string{"LLM time", humanDur(r.llmWork)})
		}
		if r.toolWork > 0 {
			rows = append(rows, [2]string{"tool time", humanDur(r.toolWork)})
		}
		if r.ttftCount > 0 {
			avg := (time.Duration(r.ttftSum/r.ttftCount) * time.Millisecond).Round(10 * time.Millisecond)
			rows = append(rows, [2]string{"avg time to first token", humanDur(avg)})
		}
		if r.rate > 0 {
			rows = append(rows, [2]string{"decode speed", fmt.Sprintf("%.1f t/s", r.rate)})
		}
		if r.calls > 0 {
			call := fmt.Sprintf("%d", r.calls)
			if r.errors > 0 {
				call = fmt.Sprintf("%d (%d failed)", r.calls, r.errors)
			}
			rows = append(rows, [2]string{"tool calls", call})
		}
		// The active clock is the one figure that always has a reading:
		// a fresh session has genuinely worked for 0s.
		rows = append(rows, [2]string{"active time", humanDur(r.work)})
		return "Session statistics", rows
	case pillToken:
		rows := make([][2]string, 0, 8)
		if r.cache > 0 && r.in+r.cache > 0 {
			rows = append(rows, [2]string{"cache hit", fmt.Sprintf("%d%%", 100*r.cache/(r.in+r.cache))})
		}
		rows = append(rows, [2]string{"uncached input", groupTokens(r.in)})
		rows = append(rows, [2]string{"cached input", groupTokens(r.cache)})
		if r.cacheWrite > 0 {
			rows = append(rows, [2]string{"cache writes", groupTokens(r.cacheWrite)})
		}
		rows = append(rows, [2]string{"output", groupTokens(r.out)})
		if r.think > 0 {
			rows = append(rows, [2]string{"of which reasoning", groupTokens(r.think)})
		}
		if r.cost > 0 {
			rows = append(rows, [2]string{"cost", fmt.Sprintf("$%.4f", r.cost)})
		}
		if r.ctx > 0 && r.window > 0 {
			rows = append(rows, [2]string{"context", fmt.Sprintf("%s/%s (%d%%)",
				HumanTokens(r.ctx), HumanTokens(r.window), int(100*r.ctx/r.window))})
		}
		return "Token usage", rows
	}
	return "", nil
}

// statusPopupRows exposes the panel's rows to a caller that already holds a.mu
// (the tests walk the same table the painter draws). Zero there means the
// painter is still to run for this frame, which is the honest answer.
func (a *App) statusPopupRows() [][2]string {
	_, rows := a.statusPopupBody(a.statusPop.pill)
	return rows
}

// statusHitRect returns the on-screen rect the painter published for a pill,
// and false when that pill was not on screen this frame. Caller holds a.mu.
func (a *App) statusHitRect(name string) (panelRect, bool) {
	for _, hit := range a.statusHits {
		if hit.name == name {
			return hit.rect, true
		}
	}
	return panelRect{}, false
}

// --- the pills themselves ---

// pillLabel renders one pill's inline reading: the headline dsh puts on the
// button, and nothing else. The breakdown lives in the popup, which is the
// whole point of the pair.
//
// The time pill is dsh's TimePill: "{turns} turns {steps} steps" beside the
// decode speed, with the work timer ahead of it because that is the one
// figure xdev's row has always led with and a fresh session's honest reading
// is a 0s, not a blank.
//
// No leading glyph. dsh draws an icon per pill because it draws a row of
// buttons in a browser; a terminal row is a fixed-cell grid whose every
// column is contested, and the icon only said which family a figure belonged
// to — a distinction the value's own unit already makes.
func (a *App) pillLabel(name string) string {
	switch name {
	case pillTime:
		var b strings.Builder
		b.WriteString(humanDur(a.activeWork()))
		if a.st.Turns > 0 || a.st.Steps > 0 {
			// "12t·34s" is turns and STEPS, and the two-letter unit is what
			// keeps the row narrow. The g is there for exactly that: "s"
			// alone reads as seconds, and seconds is the other reading on
			// this pill.
			fmt.Fprintf(&b, " · %dt·%dg", a.st.Turns, a.st.Steps)
		}
		if a.st.Rate > 0 {
			fmt.Fprintf(&b, " · %.1f t/s", a.st.Rate)
		}
		return b.String()
	case pillToken:
		total := a.st.TokensIn + a.st.TokensOut + a.st.TokensCache + a.st.TokensCacheWrite
		if total == 0 {
			return ""
		}
		s := HumanTokens(total)
		if a.st.TokensCache > 0 && a.st.TokensIn+a.st.TokensCache > 0 {
			s += fmt.Sprintf(" · %d%%", 100*a.st.TokensCache/(a.st.TokensIn+a.st.TokensCache))
		}
		return s
	}
	return ""
}
