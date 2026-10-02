package tui

// The mid-turn submit queue (issue #157, "mid-turn queue semantics").
//
// Until this shipped, Enter on a new prompt while a turn was running was
// refused outright: runTurn's single `running` claim rejected the submit and
// printed "a turn is already running — Esc cancels it". A person watching a
// 40-tool refactor had exactly one way to talk to the model: kill the turn
// (losing its work) or type into a box that could not be sent.
//
// The queue is the deepseek-harness / Claude Code shape: a prompt typed while
// the model works is a pending entry above the composer, and it reaches the
// model at the next step boundary of the SAME run — never discarded, never
// silently reordered. The agent half already exists: Agent.Steer queues a
// message and the loop drains steering at the top of every turn
// (internal/agent/loop.go), persisting it as a user message. So the delivery
// mechanism is a queue entry plus one Steer call; everything here is the half
// that was missing, the half a person can SEE.
//
// Two delivery points, both explicit:
//
//   - Enter while working: queue the entry, hand it to the live agent's Steer
//     channel. The loop injects it before the next provider request.
//   - "send now" (F6, or the per-row button): interrupt the turn, acknowledge
//     it in the transcript, and run this message as a fresh turn. Interrupting
//     rather than injecting is the honest reading of "now" — the in-flight
//     provider request cannot be pre-empted, and pretending otherwise would
//     leave the user waiting a round trip they asked to skip.
//
// Ordering is append-only and never rewritten: the list is a transcript of
// intent, and a queue that re-sorts itself is a queue nobody trusts. Take-back
// (Esc on a row) moves an entry back to the composer, because a mistyped
// prompt is a normal event and losing it to a queue is not acceptable.

import (
	"fmt"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// queuedEntry is one pending prompt. Kind records how it will be delivered so
// the row can say so: a plain entry rides the steer channel, a bang entry is
// shell work that belongs to the next turn instead of the model, a slash
// entry is a command the queue holds until the turn ends.
type queuedEntry struct {
	text string
	// kind is "text" (the default), "shell" for "!<cmd>" and "command" for
	// "/<cmd>" — the two prefixes that mean something other than a prompt.
	kind string
	// session is the session that OWNS the entry (#157). The App is one
	// transcript with several open sessions behind it, so an unowned row could
	// be painted over a session that never typed it, taken back into the wrong
	// composer, retired by another session's steering drain, or flushed by
	// another session's run ending. Every read and write filters on this.
	session string
	// seq is the append order, used to keep the list stable if a future
	// policy ever needs it. Ordering is positional today; the field exists so
	// the invariant is testable rather than implied.
	seq int
}

const (
	queueKindText    = "text"
	queueKindShell   = "shell"
	queueKindCommand = "command"
)

// queueRowMax caps the painted rows: the list must never eat the transcript
// on a small terminal, and a queue nobody can see is not a queue.
const queueRowMax = 5

// queueLabel classifies a draft the way the composer does, so a queued row
// says what the entry actually is rather than showing a prompt that will be
// executed locally.
func queueLabel(text string) string {
	switch {
	case strings.HasPrefix(text, "!"):
		return queueKindShell
	case strings.HasPrefix(text, "/"):
		return queueKindCommand
	default:
		return queueKindText
	}
}

// appendQueued adds an entry owned by session sid and returns the index it
// landed at. Caller holds a.mu.
func (a *App) appendQueued(text, sid string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return -1
	}
	a.queueSeq++
	a.queue = append(a.queue, queuedEntry{text: text, kind: queueLabel(text), session: sid, seq: a.queueSeq})
	return len(a.queue) - 1
}

// takeQueued removes entry i and returns it. Caller holds a.mu.
func (a *App) takeQueued(i int) (queuedEntry, bool) {
	if i < 0 || i >= len(a.queue) {
		return queuedEntry{}, false
	}
	e := a.queue[i]
	a.queue = append(a.queue[:i], a.queue[i+1:]...)
	return e, true
}

// QueueAgain puts a popped entry back at the HEAD of the list — the head,
// because it is the oldest and the list's promise is delivery order. The
// run-end flush uses it when the turn slot turned out to be taken: the entry
// must not silently vanish between "the run ended" and "we tried to start the
// next turn".
func (a *App) QueueAgain(text, sid string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	a.queue = append([]queuedEntry{{text: text, kind: queueLabel(text), session: sid, seq: -1}}, a.queue...)
}

// DropQueued removes the pending entry with this text from session sid, oldest
// match first, and reports whether one went. The send-now path calls it once
// the HOST has really taken the message: a row that survived the interrupt
// would be delivered twice — once as the interrupted turn's replacement prompt,
// and again by the queue's run-end flush when that turn ends. Not removing it
// is the difference between "delivered now" and "delivered now and again".
func (a *App) DropQueued(sid, text string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.removeQueuedByTextLocked(sid, text)
}

// queuePrompt records a pending submit and hands the text to the host's
// mid-turn delivery seam. It reports whether the entry was queued: a host
// whose onQueue refused it (a guest room that cannot forward into a live
// turn, say) must not leave a row on screen that will never be delivered, so
// the entry is removed again and the caller falls back to the plain send
// path, which prints the host's own refusal.
//
// Caller: UI thread, outside a.mu.
func (a *App) queuePrompt(text string) bool {
	a.mu.Lock()
	sid := a.st.SessionID
	idx := a.appendQueued(text, sid)
	a.mu.Unlock()
	if idx < 0 {
		return false
	}
	if a.onQueue == nil || a.onQueue(text) {
		return true
	}
	a.mu.Lock()
	a.removeQueuedByTextLocked(sid, text)
	a.mu.Unlock()
	return false
}

// oldestQueued returns the first pending entry's text, or "" when the queue is
// empty. Delivery order is oldest-first, so this is what an immediate-delivery
// action sends: the message that has been waiting longest goes first and the
// rest keep their place behind it.
func (a *App) oldestQueued() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.queueForLocked(a.st.SessionID) {
		return e.text
	}
	return ""
}

// queueForLocked is one session's pending entries in delivery order. Caller
// holds a.mu. The App is one transcript over many open sessions, so every read
// of the queue is a read of ONE session's rows — never the whole list.
func (a *App) queueForLocked(sid string) []queuedEntry {
	if sid == "" {
		return nil
	}
	out := make([]queuedEntry, 0, len(a.queue))
	for _, e := range a.queue {
		if e.session == sid {
			out = append(out, e)
		}
	}
	return out
}

// RetireDelivered turns the pending rows for texts the agent just injected
// into session sid's conversation into ordinary transcript rows. The loop's
// steering drain is the delivery point (#157), so the host calls this from
// there and a queued row becomes a ❯ row exactly when its message became part
// of the conversation — not when the steer call returned, which only proves
// the text reached a channel.
//
// The transcript row and the turn count are the point. A delivered prompt
// whose row simply vanished was a prompt the person could not see the model
// had, and one run that swallowed three of them counted a single turn. Each
// matched row is one accepted user turn, counted as it is delivered.
//
// It is pinned to one session: two sessions drain steering at the same moment,
// and each must only ever retire its own rows.
//
// Matching is by value, oldest first, on purpose: a person who typed the same
// sentence twice has two entries and two deliveries, and retiring the newest
// one would leave the oldest row looking pending while the message behind it
// is already in the transcript. A text that is not in THIS session's queue is
// ignored, so a steering message from an extension or the mailbox — which never
// had a row — is a no-op rather than a corruption.
//
// Caller: the agent goroutine, so the lock is taken here.
func (a *App) RetireDelivered(sid string, texts []string) {
	a.mu.Lock()
	delivered := 0
	for _, t := range texts {
		if !a.removeQueuedByTextLocked(sid, t) {
			continue // never had a row: not something the person typed
		}
		delivered++
		a.blocks = append(a.blocks, &Block{Kind: KindUser, Text: t, Ts: time.Now()})
	}
	if delivered > 0 {
		// Each delivered row is one accepted user turn. The count used to
		// move once per finished run, so three prompts steered into a single
		// run read as one turn (#157).
		a.st.Turns += delivered
		a.sm.Bottom()
	}
	// The painted list is the current session's; a parked session's queue
	// stays invisible exactly like its transcript.
	n := len(a.queueForLocked(a.st.SessionID))
	a.mu.Unlock()
	if n > 0 || delivered > 0 {
		a.poke()
	}
}

// TakeOldestQueued removes and returns session sid's first pending entry's
// text, or "" when it has none. This is the queue's own delivery order: the
// message that has been waiting longest goes first and the rest keep their
// place. It is keyed by session so one run's flush can never pick up another's
// row.
//
// The host's run-end flush uses it for a row that was never delivered — a
// prompt typed just as the run ended, or one the turn could not steer. A row
// that survived the run is a message the model never saw, so the honest
// outcome is to run it, not to leave it pending forever.
func (a *App) TakeOldestQueued(sid string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, e := range a.queue {
		if e.session == sid {
			a.queue = append(a.queue[:i], a.queue[i+1:]...)
			return e.text
		}
	}
	return ""
}

// TakeQueued removes and returns the OLDEST pending entry with this text in
// session sid. It is the named form of TakeOldestQueued: a host flushing a
// specific entry (an extension retry, a hook) wants that one, not the head of
// the queue.
func (a *App) TakeQueued(sid, text string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, e := range a.queue {
		if e.session == sid && e.text == text {
			a.queue = append(a.queue[:i], a.queue[i+1:]...)
			return e.text, true
		}
	}
	return "", false
}

// QueuedCount reports how many entries the session on screen has pending.
func (a *App) QueuedCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.queueForLocked(a.st.SessionID))
}

// PendingTexts returns the session on screen's pending entries in delivery
// order, oldest first. Exported for tests and for a host that reports the
// queue on quit.
func (a *App) PendingTexts() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []string{}
	for _, e := range a.queueForLocked(a.st.SessionID) {
		out = append(out, e.text)
	}
	return out
}

// removeQueuedByTextLocked drops the oldest entry of session sid whose text
// matches and reports whether one went. The take-back path is text-keyed
// rather than index-keyed because the click that triggered it is a screen
// coordinate, and the queue may have grown between the frame that published the
// hit table and the click itself. Caller holds a.mu.
func (a *App) removeQueuedByTextLocked(sid, text string) bool {
	for i, e := range a.queue {
		if e.session == sid && e.text == text {
			a.queue = append(a.queue[:i], a.queue[i+1:]...)
			return true
		}
	}
	return false
}

// queueRows is the number of rows the session on screen's list occupies, 0
// when it is empty.
func (a *App) queueRows() int {
	n := len(a.queueForLocked(a.st.SessionID))
	if n > queueRowMax {
		n = queueRowMax
	}
	return n
}

// queueTop is the first screen row the list occupies: directly above the
// composer's top border, inside the main pane (so the sidebar's columns stay
// the panel's). Caller holds a.mu.
func (a *App) queueTop(composerTop int) int {
	y := composerTop - 1 - a.queueRows()
	if y < a.transcriptTop() {
		return a.transcriptTop()
	}
	return y
}

// queueRowText flattens an entry to the one line a queued row paints: control
// bytes stripped, newlines and tabs collapsed. A queue entry is a draft the
// user may still take back, and a row that painted a raw newline would tear
// the box in half — sanitizeOutput is the same discipline tool output goes
// through before any width math (blocks.go).
func queueRowText(s string) string {
	s = sanitizeOutput(s)
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	return strings.TrimSpace(s)
}

// drawQueue paints the pending entries above the composer and publishes the
// hit table the mouse reads. Each row is an ordinal, a kind mark, the text,
// and a [send now] button whose pixels are the hit target — so the button a
// person aims at is the button they get. The rows are chrome, not transcript,
// so they paint over the transcript's tail, immediately above the composer.
//
// Caller holds a.mu.
func (a *App) drawQueue(composerTop int) {
	a.queueHits = nil // no list, nothing to hit
	shown := a.queueForLocked(a.st.SessionID)
	if len(shown) == 0 {
		return
	}
	s := a.scr
	w := a.rightEdge()
	rows := a.queueRows()
	y0 := a.queueTop(composerTop)
	if y0 <= 0 || w < 20 {
		return
	}
	hidden := len(shown) - rows
	dim := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim)))
	textSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.TextPrimary)))
	mark := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentUser))).Bold(true)
	for i := 0; i < rows; i++ {
		y := y0 + i
		// Row i shows entry (hidden+i): the OLDEST first, so the list reads in
		// delivery order rather than in reverse.
		e := shown[hidden+i]
		ord := fmt.Sprintf("%d", hidden+i+1)
		drawText(s, 1, y, ord, dim)
		drawText(s, 3, y, queueKindMark(e.kind), mark)
		// The right end carries the immediate-delivery button. Painted
		// before the text so the text knows how much room is left.
		btn := "[" + sendNowLabel + "]"
		bx := w - 2 - width(btn)
		drawText(s, bx, y, btn, dim)
		// Ordinal, mark, button, and one cell of pad on each side of the
		// text. The ordinal is 3 wide ("12 "), the mark 2, so text starts at
		// column 5 and the button owns everything past bx.
		avail := bx - 5
		if avail < 4 {
			avail = 4
		}
		line := queueRowText(e.text)
		if line == "" {
			line = "…"
		}
		line = truncateCells(line, avail, "…")
		drawText(s, 5, y, line, textSt)
		// Blank the gap between the text and the button, or a longer entry
		// from an earlier frame shows through this row's tail.
		for x := 5 + width(line); x < bx; x++ {
			s.SetContent(x, y, ' ', nil, textSt)
		}
		a.queueHits = append(a.queueHits, queueHit{
			sid:  a.st.SessionID,
			text: e.text,
			row:  panelRect{x: 1, y: y, w: w - 1, h: 1},
			btn:  panelRect{x: bx, y: y, w: width(btn), h: 1},
		})
	}
	if hidden > 0 {
		// The overflow is named, never silent: a queue whose tail is invisible
		// looks like a queue that lost messages.
		drawText(s, 1, y0-1, fmt.Sprintf("+%d more queued", hidden), dim)
	}
}

// queueKindMark is the glyph a row wears, so a "!" shell entry and a "/cmd"
// entry are recognizable in the list without reading them.
func queueKindMark(kind string) string {
	switch kind {
	case queueKindShell:
		return "! "
	case queueKindCommand:
		return "/ "
	default:
		return "❯ "
	}
}

// sendNowLabel is the button's own text. Kept as a constant so the painter and
// the docs agree on what the user is being offered.
const sendNowLabel = "send now"

// queueHit is one painted row: the session that owns it (so a click taken after
// a tab switch still names the right conversation), the text (the key, since a
// click is a screen coordinate and the list may have changed) and the two rects.
type queueHit struct {
	sid  string
	text string
	row  panelRect
	btn  panelRect
}

// handleQueueMouse answers a click on a queued row. It returns true when it
// consumed the gesture, so the caller never also anchors a selection on the
// transcript row underneath.
//
// send-now: interrupt the live turn and run this message immediately. The
// acknowledgement is printed by cmd (it owns the turn), so this only arms the
// action and runs it outside the lock — the same discipline as the message
// menu: nothing here mutates session state under a.mu.
//
// Esc on a row: take it back into the composer. This one is UI-thread-only
// (it touches the editor), so it is answered here under the lock the way every
// other composer affordance is.
//
// Caller: UI thread, a.mu held.
func (a *App) handleQueueMouse(x, y int, press bool) bool {
	if len(a.queueHits) == 0 {
		return false
	}
	for _, h := range a.queueHits {
		if !h.row.contains(x, y) {
			continue
		}
		if h.btn.contains(x, y) {
			if press {
				a.queueFire = a.buildQueueSendNow(h.sid, h.text)
			}
			return true
		}
		// A click on the row body is a take-back: put the text back where it
		// came from so it can be edited before it is sent. Nothing runs
		// under the lock beyond the editor write.
		if press {
			a.ed.SetBuffer(h.text)
			if a.removeQueuedByTextLocked(h.sid, h.text) {
				a.poke()
			}
		}
		return true
	}
	return false
}

// buildQueueSendNow returns the closure a click on the send-now button runs,
// or nil when the host wired no immediate-delivery path (a headless build, a
// test): an unwired button must do nothing rather than pretend to deliver. The
// row's OWNING session rides into the callback, so the host delivers the
// message to the session that queued it even if the screen has moved on.
func (a *App) buildQueueSendNow(sid, text string) func() {
	if a.onSendNow == nil {
		return nil
	}
	return func() { a.onSendNow(sid, text) }
}

// queueMouse is handleQueueMouse's caller: it takes the lock the row geometry
// and the editor both live under, answers the gesture, and leaves the armed
// send-now action for the caller to run UNLOCKED — a send-now interrupts a
// turn and starts another, which must never happen while a.mu is held,
// because the transcript paints under that lock.
func (a *App) queueMouse(m *tcell.EventMouse, press bool) bool {
	x, y := m.Position()
	a.mu.Lock()
	consumed := a.handleQueueMouse(x, y, press)
	a.mu.Unlock()
	return consumed
}

// runPendingQueueAction runs the send-now action a click armed, if any. The
// caller is the UI loop, right after the lock was released.
func (a *App) runPendingQueueAction() {
	a.mu.Lock()
	fire := a.queueFire
	a.queueFire = nil
	a.mu.Unlock()
	if fire != nil {
		fire()
	}
}
