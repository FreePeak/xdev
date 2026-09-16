package tui

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// Ask card (#46): the blocking "the agent needs a decision" panel the `ask`
// tool (#36) puts on screen. Modelled on the two cards that hold this role in
// the tools on this machine — Claude Code's AskUserQuestion panel and Grok
// CLI's ask_user_question card — under xdev's own card rules: modal (it owns
// every key), floating above the composer, Esc skips, a click picks, and it
// always resolves so nothing can hang on it.
//
// What the baselines taught, and what this card copies:
//   - a batch of questions is ONE tabbed card, not N interruptions;
//   - every question carries a free-text row — a fixed option list is wrong
//     more often than a typed answer is, and both baselines let you type;
//   - "Chat about this" closes the card unanswered: the human wants to say it
//     in prose in the next turn rather than click a button;
//   - quick pick (1-9), vim movement (j/k rows, h/l questions), Tab/Shift-Tab
//     and ←/→ across questions, and a review step that shows what will
//     actually be submitted before it is;
//   - a card that cannot be painted closes rather than sit invisible owning
//     the keyboard (the 5b999f0 invariant).
//
// AskCard is safe to call from the agent goroutine: it parks the card on the
// App under the mutex and blocks on a channel the UI thread answers.

// AskOption is one labelled choice on the card.
type AskOption struct {
	Label       string
	Description string
}

// AskRequest is one question. ID names it in a batch answer (the ask tool's
// questions[].id) and labels its tab; it is optional.
type AskRequest struct {
	Question string
	Options  []AskOption
	// Multi allows several labels: Space toggles boxes, Enter returns the set.
	Multi bool
	// ID names this question's tab. Empty numbers the tabs "q1", "q2", …
	ID string
	// Recommended highlights the starting cursor (and pre-toggles the boxes on
	// a multi card); it is what the tool's headless policy falls back to.
	Recommended []string
}

// AskAnswer is one question's answer. Labels are the chosen options; Note is
// prose typed on the free-text row instead, and AskChatLabel as the Note is the
// card's escape hatch: the human wants to answer in the next turn, so the card
// closes with no option chosen.
type AskAnswer struct {
	Labels []string
	Note   string
}

// AskChatLabel is the free-text row's escape value: what the human picks when
// every option is wrong and they want to say why in prose. It rides AskAnswer.
// Note rather than Labels, so it can never be mistaken for an option — and a
// host that cannot carry free text still understands this one value.
const AskChatLabel = "Chat about this"

// AskOps is the ask tool's TUI seam (#36): Show renders the card and returns
// the answer, ok=false on the skip/timeout path. cmd wires
// app.NewAskOps(settings.AskTimeout()) into the tool's card sink; hosts without
// a TUI keep the tool's headless sink, which is why this is a value the caller
// passes around rather than a dependency the App needs.
//
// ShowBatch asks several questions as one tabbed card. A seam that leaves it
// nil (a test seam built with Show alone) falls back to one Show per question.
type AskOps struct {
	Show      func(ctx context.Context, req AskRequest, timeout time.Duration) (AskAnswer, bool)
	ShowBatch func(ctx context.Context, reqs []AskRequest, timeout time.Duration) ([]AskAnswer, bool)
}

// NewAskOps returns the seam value for the ask tool: Show is AskCard,
// ShowBatch is AskCardBatch. The timeout is captured here and the per-call
// argument is ignored, so a sink cannot wait a second, different length of time
// than the card actually gave.
func (a *App) NewAskOps(timeout time.Duration) *AskOps {
	return &AskOps{
		Show: func(ctx context.Context, req AskRequest, _ time.Duration) (AskAnswer, bool) {
			return a.AskCard(ctx, req, timeout)
		},
		ShowBatch: func(ctx context.Context, reqs []AskRequest, _ time.Duration) ([]AskAnswer, bool) {
			return a.AskCardBatch(ctx, reqs, timeout)
		},
	}
}

// Card geometry. minAskCardWidth is the card's own floor: below it the options
// cannot be read and the questions go to the transcript instead. askMaxVisible
// caps the option window so the quick-pick digits stay honest, and askMaxQRows
// caps the question text so a long question cannot push the options off.
const (
	minAskCardWidth = 24
	askMaxVisible   = 9
	askMaxQRows     = 4
	askFreeLabel    = "Type something…"

	// Row kinds. The row index is uniform (options, then free text, then chat)
	// so the key router and the painter cannot disagree about which row a "4"
	// is.
	askRowOption = iota
	askRowFree
	askRowChat
)

// askRow is one selectable line: an option, the free-text row, or the chat row.
type askRow struct {
	label string
	desc  string
	kind  int
}

// askResult is one resolution of the card: the answers, or a skip carrying the
// transcript notice the parked caller must record (a painter may not lock).
type askResult struct {
	answers []AskAnswer
	ok      bool
	notice  string
}

// done reports whether this resolution ends the card. The zero value means
// "the key only moved something", and it must never reach the channel: the
// parked caller reads one result and that is the whole conversation.
func (r askResult) done() bool { return r.ok || r.notice != "" }

// askState is the live card (mu-guarded). Its methods run with a.mu held and
// never call back into the App.
type askState struct {
	reqs   []AskRequest
	sel    [][]string // chosen labels per question
	note   []string   // typed free text per question
	cur    []int      // cursor row per question
	q      int        // the question the tab strip is on
	top    int        // first painted row of the current question
	msg    string     // one-line feedback, cleared by navigation
	review bool       // showing the summary screen, not a question
	dead   bool       // resolved off the caller's hands: stop painting
	ch     chan askResult
	hit    askHit // this frame's row/tab map, for clicks
}

func newAskState(reqs []AskRequest, ch chan askResult) *askState {
	st := &askState{reqs: reqs, ch: ch}
	for i, r := range reqs {
		st.sel = append(st.sel, nil)
		st.note = append(st.note, "")
		st.cur = append(st.cur, recommendedIndex(r))
		if r.Multi {
			// Pre-toggle the recommended boxes, so a multi card shows the human
			// exactly what the headless policy would have answered.
			for _, o := range r.Options { // option order, not recommendation order
				if hasLabel(r.Recommended, o.Label) {
					st.sel[i] = append(st.sel[i], o.Label)
				}
			}
		}
	}
	return st
}

// OptionLabels is the request's option labels, in order.
func (r AskRequest) OptionLabels() []string {
	out := make([]string, 0, len(r.Options))
	for _, o := range r.Options {
		out = append(out, o.Label)
	}
	return out
}

// recommendedIndex is where the cursor starts: the first recommended option, or
// 0 when the card recommends nothing that is on it.
func recommendedIndex(req AskRequest) int {
	for _, want := range req.Recommended {
		for i, opt := range req.Options {
			if opt.Label == want {
				return i
			}
		}
	}
	return 0
}

// AskCard shows one question as the blocking card and returns the answer.
// ok=false means the human skipped (Esc), the context was canceled, nobody
// answered inside the timeout, or no card could be painted.
func (a *App) AskCard(ctx context.Context, req AskRequest, timeout time.Duration) (AskAnswer, bool) {
	if len(req.Options) == 0 {
		return AskAnswer{}, false // the tool validates; a card could draw only noise
	}
	ans, ok := a.AskCardBatch(ctx, []AskRequest{req}, timeout)
	if !ok || len(ans) == 0 {
		return AskAnswer{}, false
	}
	return ans[0], true
}

// AskCardBatch shows a batch of questions as ONE tabbed card: several questions
// are one interruption, not several. It always resolves — Esc, a timeout, or a
// canceled context return ok=false and the caller falls back to the tool's
// headless policy.
func (a *App) AskCardBatch(ctx context.Context, reqs []AskRequest, timeout time.Duration) ([]AskAnswer, bool) {
	// A question with no options has nothing to click: drop it rather than park
	// a card that hides the questions that do have choices.
	var keep []AskRequest
	for _, r := range reqs {
		if len(r.Options) > 0 {
			keep = append(keep, r)
		}
	}
	if len(keep) == 0 || ctx.Err() != nil {
		return nil, false
	}
	a.mu.Lock()
	narrow := a.width < minAskCardWidth
	a.mu.Unlock()
	if narrow {
		// No room for the card: say the questions in the transcript and take
		// the skip path, so the tool's headless policy answers instead of the
		// questions vanishing.
		a.askNotice(keep, "window too narrow for the option card — using the recommended path")
		return nil, false
	}

	ch := make(chan askResult, 1)
	st := newAskState(keep, ch)
	a.mu.Lock()
	if a.ask != nil { // one card at a time: a second ask releases the first
		a.ask.dead = true
		close(a.ask.ch)
	}
	a.ask = st
	a.askPause() // the wait for an answer is not work the session did
	a.mu.Unlock()
	a.poke()
	defer func() {
		a.mu.Lock()
		if a.ask == st {
			a.ask = nil
		}
		a.askResume()
		a.mu.Unlock()
		a.poke()
	}()

	var deadline <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		deadline = timer.C
	}
	select {
	case res := <-ch:
		if res.notice != "" {
			// A skip, or a card the painter had to close: the question stays on
			// the record, so the wait was never invisible (5b999f0).
			a.AddSystemBlock(res.notice)
			return nil, false
		}
		if !res.ok {
			return nil, false
		}
		return res.answers, true
	case <-deadline:
		// The wait the headless policy would have run is already spent, so
		// record the question and let the caller answer from it alone.
		a.askNotice(keep, fmt.Sprintf("no answer within %s — using the recommended path", timeout))
		return nil, false
	case <-ctx.Done():
		return nil, false
	}
}

// askNotice records why a card did not wait for an answer, with the questions
// and the recommendations, as one transcript block.
func (a *App) askNotice(reqs []AskRequest, why string) {
	a.AddSystemBlock(askNotice(reqs, why))
}

// AskPending reports whether the blocking card is on screen.
func (a *App) AskPending() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ask != nil
}

// AskSelection returns the highlighted option index and the option count of the
// question under the tab strip (0, 0 with no card). The index is an option, so
// a cursor parked on the free-text or chat row reads as the last one.
func (a *App) AskSelection() (sel, n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.ask
	if st == nil {
		return 0, 0
	}
	n = len(st.reqs[st.q].Options)
	if n == 0 {
		return 0, 0
	}
	return min(st.cur[st.q], n-1), n
}

// ------------------------------------------------------------------ card state

// rows lists the current question's selectable rows.
func (st *askState) rows() []askRow {
	r := st.reqs[st.q]
	rows := make([]askRow, 0, len(r.Options)+2)
	for _, o := range r.Options {
		rows = append(rows, askRow{label: o.Label, desc: o.Description, kind: askRowOption})
	}
	rows = append(rows,
		askRow{label: st.note[st.q], kind: askRowFree},
		askRow{label: AskChatLabel, kind: askRowChat})
	return rows
}

// cursor is the question's remembered row, clamped into the row list: a resize
// can shrink the window under it.
func (st *askState) cursor(rows []askRow) int {
	return clamp(st.cur[st.q], 0, len(rows)-1)
}

// setCursor parks the cursor on a row and keeps the window around it.
func (st *askState) setCursor(rows []askRow, i int) {
	st.cur[st.q] = clamp(i, 0, len(rows)-1)
	st.scroll(rows)
}

// scroll slides the painted window just enough to hold the cursor.
func (st *askState) scroll(rows []askRow) {
	vis := min(len(rows), askMaxVisible)
	if st.top > st.cur[st.q] {
		st.top = st.cur[st.q]
	}
	if st.top+vis <= st.cur[st.q] {
		st.top = max(0, st.cur[st.q]-vis+1)
	}
	st.top = clamp(st.top, 0, max(0, len(rows)-vis))
}

func (st *askState) move(rows []askRow, d int) {
	st.setCursor(rows, st.cursor(rows)+d)
}

// goQ steps the tab strip; false means the batch has no other question that
// way, which is how the router tells the end of a batch apart.
func (st *askState) goQ(d int) bool {
	q := clamp(st.q+d, 0, len(st.reqs)-1)
	if q == st.q {
		return false
	}
	st.gotoQ(q)
	return true
}

// gotoQ jumps to one question (a tab click, the review screen).
func (st *askState) gotoQ(q int) {
	st.q, st.review, st.msg = clamp(q, 0, len(st.reqs)-1), false, ""
}

// toggle flips an option's membership in the current answer (multi-select),
// keeping the set in option order: the answer reads as the card showed it, not
// as the order the boxes were clicked.
func (st *askState) toggle(i int, label string) {
	for k, l := range st.sel[st.q] {
		if l == label {
			st.sel[st.q] = append(st.sel[st.q][:k:k], st.sel[st.q][k+1:]...)
			return
		}
	}
	rank := 0 // the box's place among the checked ones, by option order
	for _, l := range st.sel[st.q] {
		if idx := st.optionIndex(l); idx >= 0 && idx < i {
			rank++
		}
	}
	tail := append([]string(nil), st.sel[st.q][rank:]...)
	st.sel[st.q] = append(st.sel[st.q][:rank:rank], append([]string{label}, tail...)...)
}

// optionIndex is where a label sits on the current question (-1 for typed text,
// which has no place in the option order).
func (st *askState) optionIndex(label string) int {
	for i, o := range st.reqs[st.q].Options {
		if o.Label == label {
			return i
		}
	}
	return -1
}

// answered reports whether the human put something on a question.
func (st *askState) answered(q int) bool {
	return len(st.sel[q]) > 0 || strings.TrimSpace(st.note[q]) != ""
}

// firstUnanswered is the question the review screen points at (-1 when none is).
func (st *askState) firstUnanswered() int {
	for i := range st.reqs {
		if !st.answered(i) {
			return i
		}
	}
	return -1
}

// name labels a question in the tab strip and the review rows: the tool's id
// when it carries one, else its ordinal.
func (st *askState) name(q int) string {
	switch {
	case st.reqs[q].ID != "":
		return st.reqs[q].ID
	case len(st.reqs) == 1:
		return "answer"
	default:
		return fmt.Sprintf("q%d", q+1)
	}
}

// summary is a question's answer in one line, as the review screen shows it.
func (st *askState) summary(q int) string {
	if st.note[q] == AskChatLabel {
		return AskChatLabel // the escape hatch, not a chosen option
	}
	parts := append([]string(nil), st.sel[q]...)
	if text := strings.TrimSpace(st.note[q]); text != "" {
		parts = append(parts, text)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ", ")
}

// pickAt records row i and reports whether that makes the question DONE. A
// multi-select toggle never ends a question, and empty free text is not an
// answer either.
func (st *askState) pickAt(i int, rows []askRow) bool {
	if i < 0 || i >= len(rows) {
		return false
	}
	switch rows[i].kind {
	case askRowFree:
		return strings.TrimSpace(st.note[st.q]) != ""
	case askRowChat:
		// The escape hatch is a form of typed answer, not a fake option: it
		// rides the Note, so no card label can ever be "not one of the
		// options" to the tool.
		st.sel[st.q] = nil
		st.note[st.q] = AskChatLabel
		return true
	default:
		if st.reqs[st.q].Multi {
			st.toggle(i, rows[i].label)
			return false
		}
		st.sel[st.q] = []string{rows[i].label}
		st.note[st.q] = ""
		return true
	}
}

// confirm is Enter: on a multi-select it closes the question with the toggled
// set (nothing checked is nothing answered, so the card stays open rather than
// invent one), everywhere else it takes the cursor's row.
func (st *askState) confirm(rows []askRow, cur int) askResult {
	if st.reqs[st.q].Multi && rows[cur].kind == askRowOption {
		if !st.answered(st.q) {
			return askResult{}
		}
		return st.afterPick()
	}
	if !st.pickAt(cur, rows) {
		return askResult{}
	}
	return st.afterPick()
}

// afterPick resolves what a DONE question means: the card is submitted for a
// single question or a chat escape, otherwise it steps to the next question
// still open, and shows the review when none is.
func (st *askState) afterPick() askResult {
	if len(st.reqs) == 1 || st.note[st.q] == AskChatLabel {
		return st.submit()
	}
	if q := st.firstUnanswered(); q >= 0 {
		st.gotoQ(q)
		return askResult{}
	}
	st.review, st.msg = true, "" // every tab has an answer: show them
	return askResult{}
}

// submit resolves the card with what the human chose. Questions left empty keep
// empty answers, so the tool's headless policy covers those (and only those)
// instead of the card inventing a reply for them.
func (st *askState) submit() askResult {
	out := make([]AskAnswer, len(st.reqs))
	for i := range st.reqs {
		out[i] = AskAnswer{
			Labels: append([]string(nil), st.sel[i]...),
			Note:   strings.TrimSpace(st.note[i]),
		}
	}
	return askResult{answers: out, ok: true}
}

// skip drops the card without an answer. Partial choices are still worth one
// transcript line: the human saw the question, and the next turn should know
// what was already picked before the Esc.
func (st *askState) skip(why string) askResult {
	var chosen []string
	for q := range st.reqs {
		if s := st.summary(q); s != "" {
			chosen = append(chosen, fmt.Sprintf("%s=%s", st.name(q), s))
		}
	}
	note := "ask " + why
	if len(chosen) > 0 {
		note += " · already chosen: " + strings.Join(chosen, ", ")
	}
	return askResult{notice: askNotice(st.reqs, strings.TrimPrefix(note, "ask "))}
}

// askNotice renders the questions, options and the reason as one transcript
// block: the fallback when no card fits, and the record of a skip or timeout.
func askNotice(reqs []AskRequest, why string) string {
	var b strings.Builder
	b.WriteString("ask: " + why)
	for _, r := range reqs {
		b.WriteString("\n" + r.Question)
		if r.Multi {
			b.WriteString(" (choose all that apply)")
		}
		for _, o := range r.Options {
			b.WriteString("\n  - " + o.Label)
			if o.Description != "" {
				b.WriteString(": " + o.Description)
			}
			if hasLabel(r.Recommended, o.Label) {
				b.WriteString(" (recommended)")
			}
		}
	}
	return b.String()
}

// ------------------------------------------------------------------- key router

// handleAskKey routes keys while the card is open. The card is modal: every key
// is swallowed (handled=true), so the composer never sees a keystroke aimed at
// the question. The parked AskCardBatch does the teardown and records notices,
// because it, not the key path, owns the transcript.
func (a *App) handleAskKey(key *tcell.EventKey) bool {
	a.mu.Lock()
	st := a.ask
	if st == nil || st.dead {
		a.mu.Unlock()
		return false
	}
	res := st.key(key)
	if !res.done() { // navigation only: the card stays open, nothing is sent
		a.mu.Unlock()
		a.poke()
		return true
	}
	st.dead = true
	a.ask = nil
	a.mu.Unlock()
	st.ch <- res // buffer 1, and the caller always reads exactly one result
	a.poke()
	return true
}

// key is the card's whole keyboard, run with a.mu held. The zero askResult
// means "stayed open".
func (st *askState) key(key *tcell.EventKey) askResult {
	if st.review {
		return st.reviewKey(key)
	}
	rows := st.rows()
	cur := st.cursor(rows)
	switch key.Key() {
	case tcell.KeyUp, tcell.KeyCtrlP:
		st.move(rows, -1)
	case tcell.KeyDown, tcell.KeyCtrlN:
		st.move(rows, 1)
	case tcell.KeyPgUp:
		st.move(rows, -askMaxVisible)
	case tcell.KeyPgDn:
		st.move(rows, askMaxVisible)
	case tcell.KeyLeft:
		st.goQ(-1)
	case tcell.KeyRight:
		st.goQ(1)
	case tcell.KeyTab, tcell.KeyBacktab:
		return st.tabKey(key.Key() == tcell.KeyTab)
	case tcell.KeyEnter:
		return st.confirm(rows, cur)
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if rows[cur].kind == askRowFree {
			st.note[st.q] = chopLastRune(st.note[st.q])
		}
	case tcell.KeyEscape:
		return st.skip("skipped")
	case tcell.KeyCtrlC, tcell.KeyCtrlD:
		return st.skip("cancelled")
	case tcell.KeyRune:
		return st.runeKey(key.Rune(), rows, cur)
	}
	return askResult{}
}

// tabKey walks the questions; at the end of the batch Tab opens the review
// (Claude Code submits from there) rather than wrapping around silently.
func (st *askState) tabKey(forward bool) askResult {
	d := 1
	if !forward {
		d = -1
	}
	if st.goQ(d) || len(st.reqs) == 1 {
		return askResult{}
	}
	if forward {
		st.review, st.msg = true, ""
	}
	return askResult{}
}

// runeKey routes a printable character. On the free-text row it types; anywhere
// else the key belongs to the card.
func (st *askState) runeKey(r rune, rows []askRow, cur int) askResult {
	if rows[cur].kind == askRowFree {
		if r == '\t' {
			return askResult{}
		}
		st.note[st.q] += string(r) // Space included: typed answers are sentences
		return askResult{}
	}
	n := len(st.reqs[st.q].Options)
	switch {
	case r == 'k':
		st.move(rows, -1)
	case r == 'j':
		st.move(rows, 1)
	case r == 'h':
		st.goQ(-1)
	case r == 'l':
		st.goQ(1)
	case r == ' ': // Space toggles a box and stays put; elsewhere it confirms
		if st.reqs[st.q].Multi && rows[cur].kind == askRowOption {
			st.pickAt(cur, rows)
			return askResult{}
		}
		return st.confirm(rows, cur)
	case r == 'z': // jump to the free-text row and type (Grok's z)
		st.setCursor(rows, n)
	case r == 'x': // the chat escape hatch, without hunting for its row
		st.pickAt(len(rows)-1, rows)
		return st.afterPick()
	case r >= '1' && r <= '9':
		i := int(r - '1')
		if i >= len(rows) {
			return askResult{}
		}
		st.setCursor(rows, i)
		if rows[i].kind == askRowFree {
			return askResult{} // landing on the text row only starts typing
		}
		if !st.pickAt(i, rows) {
			return askResult{}
		}
		return st.afterPick()
	default:
		// Any other plain character takes over the free-text row: the card must
		// never swallow a prose answer, and typing at it is how you give one.
		if typable(r) {
			st.setCursor(rows, n)
			st.note[st.q] += string(r)
			return askResult{}
		}
	}
	return askResult{}
}

// reviewKey routes the summary screen: the cursor walks the answers, Enter
// submits (or drops into the first question still open), Esc goes back to the
// questions with every answer intact.
func (st *askState) reviewKey(key *tcell.EventKey) askResult {
	step := func(d int) askResult {
		st.q = clamp(st.q+d, 0, len(st.reqs)-1)
		return askResult{}
	}
	switch key.Key() {
	case tcell.KeyUp, tcell.KeyLeft:
		return step(-1)
	case tcell.KeyDown, tcell.KeyRight:
		return step(1)
	case tcell.KeyTab:
		if st.q == len(st.reqs)-1 {
			return st.submitOrNudge() // Tab off the last row submits
		}
		return step(1)
	case tcell.KeyBacktab:
		return step(-1)
	case tcell.KeyEnter:
		return st.submitOrNudge()
	case tcell.KeyEscape:
		st.review, st.msg = false, ""
	case tcell.KeyCtrlC, tcell.KeyCtrlD:
		return st.skip("cancelled")
	case tcell.KeyRune:
		switch key.Rune() {
		case 'k', 'h':
			return step(-1)
		case 'j', 'l':
			return step(1)
		case 'e': // Claude Code's "edit answers"
			st.review, st.msg = false, ""
		case 'x':
			return st.skip("cancelled")
		}
	}
	return askResult{}
}

// submitOrNudge submits the review, or points at the question still empty.
func (st *askState) submitOrNudge() askResult {
	if q := st.firstUnanswered(); q >= 0 {
		st.gotoQ(q)
		st.msg = st.name(q) + " still needs an answer"
		return askResult{}
	}
	return st.submit()
}

// handleAskMouse routes the wheel and a click while the card is open: the wheel
// moves the cursor, a click takes the row under it, a click on a tab jumps to
// that question — what omp's lists and both baselines do.
func (a *App) handleAskMouse(m *tcell.EventMouse, press bool) bool {
	a.mu.Lock()
	st := a.ask
	if st == nil || st.dead {
		a.mu.Unlock()
		return false
	}
	wheel := 0
	switch m.Buttons() {
	case tcell.WheelUp:
		wheel = -1
	case tcell.WheelDown:
		wheel = 1
	}
	if wheel == 0 && !press {
		a.mu.Unlock()
		return false
	}
	x, y := m.Position()
	res := st.mouse(x, y, wheel)
	if !res.done() {
		a.mu.Unlock()
		a.poke()
		return true
	}
	st.dead = true
	a.ask = nil
	a.mu.Unlock()
	st.ch <- res
	a.poke()
	return true
}

// mouse applies one gesture to the frame the painter published.
func (st *askState) mouse(x, y, wheel int) askResult {
	rows := st.rows()
	if wheel != 0 {
		st.move(rows, wheel)
		return askResult{}
	}
	if q, ok := st.hit.tabAt(y, x); ok {
		st.gotoQ(q)
		return askResult{}
	}
	i, ok := st.hit.rowAt(y)
	if !ok {
		return askResult{}
	}
	if st.review {
		st.gotoQ(i) // a click on a review row edits that question
		return askResult{}
	}
	st.setCursor(rows, i)
	if rows[i].kind == askRowFree {
		return askResult{} // a click on the text row starts typing
	}
	if !st.pickAt(i, rows) {
		return askResult{}
	}
	return st.afterPick()
}

// typable reports whether a plain character should start a typed answer rather
// than be read as a card command.
func typable(r rune) bool {
	switch r {
	case '\t', '\n', '\r':
		return false
	}
	return utf8.ValidRune(r) && r >= ' '
}

// chopLastRune deletes one rune (the free-text row's Backspace).
func chopLastRune(s string) string {
	if s == "" {
		return ""
	}
	_, n := utf8.DecodeLastRuneInString(s)
	return s[:len(s)-n]
}

func hasLabel(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// clamp pins v to [lo, hi] (the card's own: the package has no general helper,
// and scrollModel.clamp is a method on a viewport).
func clamp(v, lo, hi int) int {
	switch {
	case v < lo:
		return lo
	case v > hi:
		return hi
	}
	return v
}

// ---------------------------------------------------------------------- painter

// drawAskCard renders the card floating above the composer (yComposerTop is the
// composer's first input row). Topmost modal: draw() calls it last among the
// overlays. Callers hold a.mu (draw does), so it must not re-lock: a card it
// cannot paint resolves through the channel and the parked caller records why.
func (a *App) drawAskCard(yComposerTop int) {
	st := a.ask
	if st == nil || st.dead {
		return
	}
	st.hit = askHit{}
	w := a.width
	rows := st.rows()
	// Publish the clamp the key path uses too, so a resize cannot leave the
	// cursor and the highlight on different rows.
	st.cur[st.q] = st.cursor(rows)
	st.scroll(rows)

	visible := min(len(rows), askMaxVisible)
	qLines := askQuestionLines(st.reqs[st.q].Question, w-8)
	tabs := 0
	if len(st.reqs) > 1 {
		tabs = 1
	}
	body := len(qLines) + visible
	if st.review {
		body = len(st.reqs) + 1 // the answers, plus the line saying what Enter does
	}
	if st.msg != "" {
		body++
	}
	height := 2 + tabs + body + 1 // borders, tabs, body, footer
	yTop := yComposerTop - 1 - height
	// The card may overlap the composer's own top border row (like the other
	// cards) but never its input rows. An unpaintable card closes: a modal that
	// owns the keyboard while drawing nothing reads as a frozen terminal.
	if w < minAskCardWidth || yTop < 1 {
		st.dead = true
		a.ask = nil
		select {
		case st.ch <- askResult{notice: askNotice(st.reqs,
			"no room above the composer for the option card — using the recommended path")}:
		default:
		}
		return
	}

	x0, x1 := 2, w-3 // border columns; content is x0+1..x1-1
	inner := x1 - x0 - 1
	box := a.th.Box()
	rowSt := tcell.StyleDefault.Background(a.cellColor(a.th.Get(theme.BgBase)))
	textSt := rowSt.Foreground(a.cellColor(a.th.Get(theme.TextPrimary)))
	dimSt := textSt.Foreground(a.cellColor(a.th.Get(theme.GrayDim)))
	selSt := textSt.Background(a.cellColor(a.th.Get(theme.BgHighlight))).Bold(true)
	borderSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.PromptBorderActive)))
	paint := func(y, x int, s string, st0 tcell.Style) {
		drawText(a.scr, x, y, truncateCells(s, x1-x, "…"), st0)
	}
	fill := func(y int) {
		for x := x0 + 1; x < x1; x++ {
			a.scr.SetContent(x, y, ' ', nil, rowSt)
		}
	}
	edge := func(y int) { drawText(a.scr, x1, y, box.Vertical, borderSt) }

	// Top border with the card's identity embedded: ╭─ ask · 1/2 ────…──╮
	title := " ask"
	if tabs > 0 {
		title += fmt.Sprintf(" · %d/%d", st.q+1, len(st.reqs))
	}
	if st.review {
		title += " · review"
	} else if st.reqs[st.q].Multi {
		title += " · multi"
	}
	fill(yTop)
	drawText(a.scr, x0, yTop, box.TopLeft+title, borderSt)
	if pad := inner - width(title); pad > 0 {
		drawText(a.scr, x0+2+width(title), yTop, strings.Repeat(box.Horizontal, pad)+box.TopRight, borderSt)
	} else {
		drawText(a.scr, x1, yTop, box.TopRight, borderSt)
	}
	y := yTop + 1

	if tabs > 0 { // the question strip: one chip per tab, ✓ on the answered ones
		fill(y)
		x := x0 + 2
		for q := range st.reqs {
			name, style := st.name(q), textSt
			if st.summary(q) != "" {
				name, style = name+" ✓", dimSt
			}
			if q == st.q {
				style = selSt
			}
			if x+width(name) > x1-1 {
				break // a batch too long for one row keeps the tabs it fits
			}
			st.hit.addTab(y, x, q)
			paint(y, x, name, style)
			x += width(name) + 2
		}
		edge(y)
		y++
	}

	if st.review {
		for q := range st.reqs {
			line, style := st.name(q)+": ", textSt
			if q == st.q {
				line, style = "❯ "+line, selSt
			} else {
				line = "  " + line
			}
			if sum := st.summary(q); sum != "" {
				line += sum
			} else {
				line, style = line+"(no answer)", dimSt
			}
			fill(y)
			paint(y, x0+1, line, style)
			edge(y)
			st.hit.addRow(y, q)
			y++
		}
		note := "Enter submit answers · ←/→ edit a question · Esc back"
		if q := st.firstUnanswered(); q >= 0 {
			note = "Enter answers " + st.name(q) + " · ↑/↓ pick · Esc back"
		}
		fill(y)
		paint(y, x0+1, note, dimSt)
		edge(y)
	} else {
		for _, line := range qLines {
			fill(y)
			paint(y, x0+1, line, textSt.Bold(true))
			edge(y)
			y++
		}
		for i := st.top; i < st.top+visible; i++ {
			style, label := rowSt, askRowText(st, rows[i], i)
			switch {
			case i == st.cur[st.q]:
				style = selSt
			case rows[i].kind != askRowOption && rows[i].label == "":
				style = dimSt
			case rows[i].kind == askRowChat:
				style = dimSt
			}
			fill(y)
			paint(y, x0+1, label, style)
			edge(y)
			st.hit.addRow(y, i)
			y++
		}
		if st.msg != "" { // the one thing the human just did wrong, in one line
			fill(y)
			paint(y, x0+1, "  "+st.msg, textSt.Foreground(a.cellColor(a.th.Get(theme.AccentError))))
			edge(y)
			y++
		}
	}

	// Footer: the keys that work on this screen, then the bottom border.
	fill(y)
	paint(y, x0+1, a.askFooter(st, rows), textSt.Foreground(a.cellColor(a.th.Get(theme.Gray))))
	edge(y)
	y++
	fill(y)
	drawText(a.scr, x0, y, box.BottomLeft+strings.Repeat(box.Horizontal, inner)+box.BottomRight, borderSt)
}

// askFooter names the keys that work here, and only the ones that fit.
func (a *App) askFooter(st *askState, rows []askRow) string {
	if st.review {
		return "Enter submit · ↑/↓ or j/k review · Esc back · x skip"
	}
	if rows[st.cursor(rows)].kind == askRowFree {
		return "type the answer · Enter submit · Backspace edit · ↑ leaves the text"
	}
	parts := []string{"↑/↓ select", "Enter confirm"}
	if st.reqs[st.q].Multi {
		parts[0] = "↑/↓ move · Space toggle"
	}
	// The batch hint leads because it is the one a human cannot guess: two
	// questions are on the card and nothing else says how to reach the next.
	// Quick pick reads off the row numbers, so it is last of the optional set.
	if len(st.reqs) > 1 {
		parts = append(parts, "Tab/←→ questions")
	}
	parts = append(parts, "1-9 quick pick", "z type an answer", "x chat instead", "Esc skip")
	return strings.Join(parts, " · ")
}

// askQuestionLines wraps the question, capped so a long question cannot push
// the options off the card (Grok truncates for exactly this reason).
func askQuestionLines(text string, cells int) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	cells = max(cells, 20)
	lines := wrap(text, cells)
	if len(lines) > askMaxQRows {
		lines = lines[:askMaxQRows]
		lines[askMaxQRows-1] += "…"
	}
	return lines
}

// askRowText renders one row: cursor, box, ordinal, label, description. The
// ordinal doubles as the quick-pick hint, so the free-text and chat rows get
// one too.
func askRowText(st *askState, r askRow, i int) string {
	box, here := "  ", " " // two fixed columns: the cursor never shifts a label
	if r.kind == askRowOption {
		box = "☐ "
		if st != nil && hasLabel(st.sel[st.q], r.label) {
			box = "☑ "
		}
	}
	if st != nil && i == st.cur[st.q] {
		here = "❯"
	}
	label := r.label
	switch r.kind {
	case askRowFree:
		if label == "" {
			label = askFreeLabel
		}
		label = `"` + label + `" ▍` // the text row reads as a field, cursor included
	}
	row := fmt.Sprintf("%s%s%d. %s", here, box, i+1, label)
	if r.desc != "" {
		row += " — " + r.desc
	}
	if st != nil && hasLabel(st.reqs[st.q].Recommended, r.label) {
		row += "  (recommended)"
	}
	return row
}

// ---------------------------------------------------------------- mouse hit map

// askHit is the row/tab map the painter publishes each frame, so a click lands
// on exactly what the user saw (the picker and the hub roster do the same).
type askHit struct {
	rowY map[int]int // screen row → card row index
	tabY int
	tabX []int
	tabQ []int
}

func (h *askHit) addRow(y, i int) {
	if h.rowY == nil {
		h.rowY = map[int]int{}
	}
	h.rowY[y] = i
}

func (h *askHit) addTab(y, x, q int) {
	h.tabY = y
	h.tabX = append(h.tabX, x)
	h.tabQ = append(h.tabQ, q)
}

// rowAt maps a click's row to the card row it landed on.
func (h askHit) rowAt(y int) (int, bool) {
	i, ok := h.rowY[y]
	return i, ok
}

// tabAt maps a click on the strip to the question it names: the chip starting
// at or before x, the latest such one wins.
func (h askHit) tabAt(y, x int) (int, bool) {
	if h.tabY != y || len(h.tabX) == 0 {
		return 0, false
	}
	best := -1
	for i, tx := range h.tabX {
		if x >= tx && (best < 0 || tx >= h.tabX[best]) {
			best = i
		}
	}
	if best < 0 {
		return 0, false
	}
	return h.tabQ[best], true
}
