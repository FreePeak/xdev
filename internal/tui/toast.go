package tui

import (
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// A toast is the transient notice opencode puts in the top-right corner:
// information and errors that must be read but belong to neither the
// transcript nor the prompt. It replaces the composer divider as the notice
// channel, because the divider shares its right end with the model name and
// the viewport hint — a long error there is either cut short or dropped
// whole.
//
// Everything the divider carried rides here now: the copy confirmation, a
// failed chord (paste, link, a read-only settings row, a refused image) and
// the MCP connect report that lands off the UI thread.

// ToastLevel is a toast's severity: it picks the accent and the lifetime,
// nothing else. The text is the text either way.
type ToastLevel int

const (
	// ToastInfo is feedback the user asked for (a copy, a chord that
	// landed). ToastError is something that failed on its own (a server that
	// never answered, a gesture the app declined).
	ToastInfo ToastLevel = iota
	ToastError
)

const (
	// toastInfoGrace is how long an info toast lives: long enough to read,
	// short enough that it is not a status line.
	toastInfoGrace = 5 * time.Second
	// toastErrorGrace is longer. The user did not ask for an error, and a
	// missing tool set only becomes visible much later, when the model works
	// around a tool it never had.
	toastErrorGrace = 8 * time.Second
	// toastMaxRows is how many stack before the oldest is dropped. Four
	// broken MCP servers is a report, not a wall of chrome.
	toastMaxRows = 3
	// toastPad is the plate's margin: glyph, space, text, space.
	toastPad = 1
)

// toast is one queued notice: what to say, how to say it, until when.
type toast struct {
	level ToastLevel
	text  string
	until time.Time
}

// Toast shows text in the top-right corner for d and then drops it — the
// channel every notice answers on. Safe from any goroutine: it takes the
// lock, unlike the UI-thread setNotice beside it. d <= 0 takes the level's
// own grace.
func (a *App) Toast(level ToastLevel, text string, d time.Duration) {
	text = oneLine(text)
	if text == "" {
		return
	}
	if d <= 0 {
		d = toastGrace(level)
	}
	a.mu.Lock()
	a.toasts = pushToast(a.toasts, toast{level, text, time.Now().Add(d)})
	a.mu.Unlock()
	a.poke()
}

// setNotice / setError are the UI-thread twins of Toast: no lock, no poke, and
// the level's own grace. Every UI-thread notice producer calls one of them
// (paste.go, selection.go, msgmenu.go, settings.go, app.go). The caller holds
// a.mu or is the UI loop, and paint() repaints, so nothing else is needed.
func (a *App) setNotice(s string) { a.toast(ToastInfo, s) }
func (a *App) setError(s string)  { a.toast(ToastError, s) }

func (a *App) toast(level ToastLevel, text string) {
	text = oneLine(text)
	if text == "" {
		return
	}
	a.toasts = pushToast(a.toasts, toast{level, text, time.Now().Add(toastGrace(level))})
}

// pushToast appends one notice, dropping the oldest past the cap. The oldest
// goes first: the newest is what the user is still reading, and four broken
// servers in a stack would cover the transcript they are talking about.
func pushToast(q []toast, t toast) []toast {
	if len(q) >= toastMaxRows {
		q = q[len(q)-toastMaxRows+1:]
	}
	return append(q, t)
}

func toastGrace(l ToastLevel) time.Duration {
	if l == ToastError {
		return toastErrorGrace
	}
	return toastInfoGrace
}

// oneLine collapses a notice to the single row a toast is: tabs expanded and
// control bytes dropped by sanitizeOutput (a raw control rune paints as
// garbage), then the whitespace runs squeezed, so a multi-line error does not
// break the stack's geometry.
func oneLine(s string) string {
	return strings.Join(strings.Fields(sanitizeOutput(s)), " ")
}

// liveToasts drops the toasts whose deadline has passed and returns what is
// left. An expired one is deleted here, so no later frame can resurrect it.
// Caller holds a.mu.
func (a *App) liveToasts() []toast {
	now := time.Now()
	keep := a.toasts[:0]
	for _, t := range a.toasts {
		if now.Before(t.until) {
			keep = append(keep, t)
		}
	}
	a.toasts = keep
	return keep
}

// expireToasts pushes every deadline into the past, so a test can age the
// stack without sleeping. It is the shape of the tick's own question — the
// one repaint that happens with no event behind it.
func (a *App) expireToasts() {
	for i := range a.toasts {
		a.toasts[i].until = time.Now().Add(-time.Millisecond)
	}
}

// toastText is the newest live toast's text, or "" — how a test reads what a
// gesture announced without depending on where it painted. Caller holds a.mu.
func (a *App) toastText() string {
	if live := a.liveToasts(); len(live) > 0 {
		return live[len(live)-1].text
	}
	return ""
}

// toastsExpiring reports whether any toast is past its deadline — the only
// thing that makes an idle UI repaint, since nothing else moves once the
// screen is quiet and the frame that drops the last toast is the one the
// tick has to ask for. Caller holds a.mu.
func (a *App) toastsExpiring() bool {
	now := time.Now()
	for _, t := range a.toasts {
		if !now.Before(t.until) {
			return true
		}
	}
	return false
}

// drawToasts paints the live toasts in the top-right corner, newest at the
// top, each on its own deadline. The plate is filled with BgHighlight so a
// toast reads as a surface over the frame rather than as a line of text
// floating in it — the same reasoning as the "↓ n new" jump chip.
//
// Caller holds a.mu. The corner is the main pane's (rightEdge), so a toast
// with the context dock open stops at the panel's edge instead of painting
// over it, and the stack starts at transcriptTop() so it clears the session
// strip when that row is showing.
func (a *App) drawToasts(s tcell.Screen) {
	live := a.liveToasts()
	edge := a.rightEdge()
	// A terminal with no corner to give keeps its text: a toast squeezed into
	// a cell or two says nothing, and the rows it would cover are content.
	if len(live) == 0 || edge < 24 || len(live) > a.height-2 {
		return
	}
	top := a.transcriptTop()
	// The queue is oldest-first, the stack is newest-first: a new notice
	// arrives at the top where the eye already is, and the ones below it age
	// out on their own clocks.
	for i := len(live) - 1; i >= 0; i-- {
		t := live[i]
		fg := theme.Gray
		if t.level == ToastError {
			fg = theme.AccentError
		}
		st := tcell.StyleDefault.
			Background(a.cellColor(a.th.Get(theme.BgHighlight))).
			Foreground(a.cellColor(a.th.Get(fg)))
		row := toastIcon(t.level) + " " + truncateCells(t.text, edge-2*toastPad-2, "…")
		x := edge - width(row) - toastPad
		y := top + len(live) - 1 - i
		fillPanelRows(s, y, y, x, edge-toastPad, st)
		drawText(s, x+toastPad, y, row, st)
	}
}

// toastIcon is the glyph that carries the level, so the paint is not the only
// thing saying "error": red and green are exactly the pair a color-blind
// reader cannot separate (theme.ApplyColorBlindMode remaps both).
func toastIcon(l ToastLevel) string {
	if l == ToastError {
		return "✗"
	}
	return "•"
}
