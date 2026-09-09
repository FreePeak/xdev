package tui

import (
	"strings"

	"github.com/gdamore/tcell/v2"
)

// Editor is the prompt input: one logical line (soft multi-line via
// Alt+Enter), history recall with Up/Down when the buffer has no embedded
// newline.
type Editor struct {
	buf     []rune
	cur     int
	history []string
	histIdx int // len(history) = not browsing
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
// (multi-line drafts are not clobbered by history navigation).
func (e *Editor) recall(dir int) {
	if strings.ContainsRune(e.Text(), '\n') {
		return
	}
	if dir < 0 {
		if e.histIdx == 0 {
			return
		}
		if e.histIdx == len(e.history) && e.Text() != "" {
			return // don't clobber a fresh draft
		}
		e.histIdx--
	} else {
		if e.histIdx >= len(e.history) {
			return
		}
		e.histIdx++
	}
	if e.histIdx < len(e.history) {
		e.buf = []rune(e.history[e.histIdx])
	} else {
		e.buf = nil
	}
	e.cur = len(e.buf)
}
