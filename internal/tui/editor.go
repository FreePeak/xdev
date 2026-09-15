package tui

import (
	"strings"

	"github.com/gdamore/tcell/v2"
)

// Editor is the prompt input: soft multi-line (Ctrl+J / Alt+Enter).
// Up/Down walk the composer's visual rows (App routes through moveLine);
// at the buffer edge they recall history — and never clobber a draft that
// holds a hard newline.
type Editor struct {
	buf     []rune
	cur     int
	history []string
	histIdx int // len(history) = not browsing
	// draft holds the text that was in the box when browsing began, so
	// Down past the newest entry returns to it instead of dropping it —
	// the readline/zsh behavior that lets an Up recall be undone.
	draft []rune
	// wantCol is the sticky desired column for repeated Up/Down (vim/omp
	// #dt/#He): a clamped row-to-row move keeps the column the pointer
	// asked for so alternating arrows don't drift left. 0 = unset (column 0
	// never needs stickiness, so the zero value is correct); any horizontal
	// key through HandleKey clears it.
	wantCol int
}

// Text returns the current input.
func (e *Editor) Text() string { return string(e.buf) }

// Reset clears the buffer and history browsing position.
func (e *Editor) Reset() {
	e.buf = e.buf[:0]
	e.cur = 0
	e.histIdx = len(e.history)
}

// PushHistory records a sent prompt (dedup of immediate repeat).
func (e *Editor) PushHistory(s string) {
	s = strings.TrimSpace(s)
	if s == "" || (len(e.history) > 0 && e.history[len(e.history)-1] == s) {
		return
	}
	e.history = append(e.history, s)
	if len(e.history) > 500 {
		e.history = e.history[len(e.history)-500:]
	}
	e.histIdx = len(e.history)
}

// finishSend archives the current input and clears the editor.
func (e *Editor) finishSend() {
	e.PushHistory(strings.TrimSpace(e.Text()))
	e.Reset()
}

// HandleKey applies an editing key. send=true means Enter finished a
// non-empty prompt; the editor has already archived and reset itself, so
// callers that need the sent text must capture Text() before calling.
func (e *Editor) HandleKey(ev *tcell.EventKey) (send bool) {
	if ev.Key() != tcell.KeyUp && ev.Key() != tcell.KeyDown {
		e.wantCol = 0 // horizontal motion and edits end the vertical-walk memory
	}
	switch ev.Key() {
	case tcell.KeyRune:
		switch {
		case ev.Rune() == '\n' || ev.Rune() == '\r':
			// Alt/Option+Enter inserts a newline; plain Enter sends.
			if ev.Modifiers()&tcell.ModAlt != 0 {
				e.insert('\n')
			} else if strings.TrimSpace(e.Text()) != "" {
				e.finishSend()
				send = true
			}
		default:
			e.insert(ev.Rune())
		}
	case tcell.KeyEnter:
		if strings.TrimSpace(e.Text()) != "" {
			e.finishSend()
			send = true
		}
	case tcell.KeyCtrlJ:
		// Ctrl+J inserts a newline in the box (claude/omp/grok parity);
		// Alt/Option+Enter already did this on mac. Never sends.
		e.insert('\n')
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if e.cur > 0 {
			e.buf = append(e.buf[:e.cur-1], e.buf[e.cur:]...)
			e.cur--
		}
	case tcell.KeyDelete:
		if e.cur < len(e.buf) {
			e.buf = append(e.buf[:e.cur], e.buf[e.cur+1:]...)
		}
	case tcell.KeyLeft:
		if e.cur > 0 {
			e.cur--
		}
	case tcell.KeyRight:
		if e.cur < len(e.buf) {
			e.cur++
		}
	case tcell.KeyHome, tcell.KeyCtrlA:
		e.cur = 0
	case tcell.KeyEnd, tcell.KeyCtrlE:
		e.cur = len(e.buf)
	case tcell.KeyCtrlK:
		e.buf = e.buf[:e.cur]
	case tcell.KeyCtrlU:
		e.buf = e.buf[e.cur:]
		e.cur = 0
	case tcell.KeyCtrlW:
		// Delete the word before the cursor.
		i := e.cur
		for i > 0 && e.buf[i-1] == ' ' {
			i--
		}
		for i > 0 && e.buf[i-1] != ' ' {
			i--
		}
		e.buf = append(e.buf[:i], e.buf[e.cur:]...)
		e.cur = i
	case tcell.KeyUp:
		e.recall(-1)
	case tcell.KeyDown:
		e.recall(1)
	}
	return send
}

func (e *Editor) insert(r rune) {
	e.buf = append(e.buf, 0)
	copy(e.buf[e.cur+1:], e.buf[e.cur:])
	e.buf[e.cur] = r
	e.cur++
}

// recall moves through history; only when the buffer holds no newline
// (multi-line drafts are not clobbered by history navigation). An in-progress
// draft never blocks recall — the baseline (omp/Claude Code) recalls on Up
// whatever is in the box — it is stashed in e.draft instead and restored when
// browsing comes back past the newest entry.
func (e *Editor) recall(dir int) {
	if strings.ContainsRune(e.Text(), '\n') {
		return
	}
	if dir < 0 {
		if e.histIdx == 0 || len(e.history) == 0 {
			return
		}
		if e.histIdx == len(e.history) {
			e.draft = append([]rune(nil), e.buf...) // stash before the first move
		}
		e.histIdx--
	} else {
		if e.histIdx >= len(e.history) {
			return
		}
		e.histIdx++
	}
	switch {
	case e.histIdx < len(e.history):
		e.buf = []rune(e.history[e.histIdx])
	default:
		e.buf = e.draft // back at the newest slot: the draft returns
		e.draft = nil
	}
	e.cur = len(e.buf)
	e.wantCol = 0 // the buffer swapped: the walk memory resets too
}

// HasHistory reports whether anything can be recalled (the app keeps
// Up/Down for transcript scrolling until the first prompt is sent).
func (e *Editor) HasHistory() bool { return len(e.history) > 0 }

// HistoryPrev/HistoryNext recall older/newer prompts. They exist so the
// keymap's history-prev/history-next actions reach the same machinery the
// arrow keys use (the /hotkeys table must not advertise dead chords).
func (e *Editor) HistoryPrev() { e.recall(-1) }

func (e *Editor) HistoryNext() { e.recall(1) }

// Visual-row geometry shared with the composer painter
// (App.composerInputLines): what the arrows walk is exactly what is drawn.

// rowSpan is one visual row: the rune offsets [start,end), newline excluded.
type rowSpan struct{ start, end int }

// wrapRows partitions buf into the composer's visual rows: '\n' is a hard
// break, and a row breaks before a rune that would overflow wrap cells.
// Always returns at least one row.
func wrapRows(buf []rune, wrap int) []rowSpan {
	if wrap < 4 {
		wrap = 4
	}
	var rows []rowSpan
	start, col := 0, 0
	for i, r := range buf {
		if r == '\n' {
			rows = append(rows, rowSpan{start, i})
			start, col = i+1, 0
			continue
		}
		rw := width(string(r))
		if col+rw > wrap {
			rows = append(rows, rowSpan{start, i})
			start, col = i, 0
		}
		col += rw
	}
	return append(rows, rowSpan{start, len(buf)})
}

// cursorCell maps a rune offset to (row, cell column) within rows.
func cursorCell(buf []rune, rows []rowSpan, cur int) (int, int) {
	r := len(rows) - 1
	for i, rs := range rows {
		if rs.start <= cur {
			r = i
		}
	}
	rs := rows[r]
	col := 0
	for _, ch := range buf[rs.start:min(cur, rs.end)] {
		col += width(string(ch))
	}
	return r, col
}

// offsetFor is the rune offset where the cursor sits at cell column col,
// clamped to the row's end when the row is shorter.
func offsetFor(buf []rune, rs rowSpan, col int) int {
	w := 0
	for i := rs.start; i < rs.end; i++ {
		rw := width(string(buf[i]))
		if w+rw > col {
			return i
		}
		w += rw
	}
	return rs.end
}

// moveLine walks the cursor one visual row in direction dir across a box
// wrap cells wide, preserving the cell column as far as the target row
// allows. It reports false when no such row exists — the cursor is already
// on the first/last row, and the caller keeps the history-recall contract.
func (e *Editor) moveLine(dir, wrap int) bool {
	rows := wrapRows(e.buf, wrap)
	r, c := cursorCell(e.buf, rows, e.cur)
	if e.wantCol > 0 {
		c = e.wantCol
	}
	tr := r + dir
	if tr < 0 || tr >= len(rows) {
		return false
	}
	e.cur = offsetFor(e.buf, rows[tr], c)
	// Stay sticky while either edge of this step is shorter than the
	// desired column, so the walk can return to the column it asked for.
	if rowWidth(e.buf, rows[r]) < c || rowWidth(e.buf, rows[tr]) < c {
		e.wantCol = c
	} else {
		e.wantCol = 0
	}
	return true
}

// rowWidth is the display width of one visual row.
func rowWidth(buf []rune, rs rowSpan) int {
	w := 0
	for _, ch := range buf[rs.start:rs.end] {
		w += width(string(ch))
	}
	return w
}
