package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// Ask overlay (#46): the blocking option card the `ask` tool (#36) puts on
// screen when the model needs a decision it cannot make alone.
//
// The card is modal and lives above the composer: while it is open every key
// belongs to it (Up/Down select, 1-9 quick pick, Space toggles on a
// multi-choice card, Enter confirms, Esc — or the timeout, or a canceled
// context — skips). A skip is not an error: the caller falls back to the
// tool's headless policy (the recommended options).
//
// AskCard is safe to call from the agent goroutine: it parks the card on the
// App under the mutex and blocks on a channel the UI thread answers. It
// always resolves — with no host interest in the answer the card still
// closes on Esc/timeout, so nothing can hang on it.

// AskOption is one labelled choice on the card.
type AskOption struct {
	Label       string
	Description string
}

// AskRequest is one question.
type AskRequest struct {
	Question string
	Options  []AskOption
	// Multi allows several labels; Enter returns every toggled option.
	Multi bool
	// Recommended highlights an option to start on (the headless fallback
	// the tool would take on a timeout).
	Recommended []string
}

// AskAnswer carries the labels the user chose (empty after a skip).
type AskAnswer struct {
	Labels []string
}

// AskOps is the ask tool's TUI seam (#36): Show renders the card and returns
// the answer, ok=false on the skip/timeout path. cmd wires
// app.NewAskOps(settings.AskTimeout()) into the tool's card sink; hosts
// without a TUI keep the tool's headless sink, which is why this is a value
// the caller passes around rather than a dependency the App needs.
type AskOps struct {
	Show func(ctx context.Context, req AskRequest, timeout time.Duration) (AskAnswer, bool)
}

// NewAskOps returns the seam value for the ask tool: Show is AskCard.
func (a *App) NewAskOps(timeout time.Duration) *AskOps {
	return &AskOps{Show: func(ctx context.Context, req AskRequest, _ time.Duration) (AskAnswer, bool) {
		return a.AskCard(ctx, req, timeout)
	}}
}

// askState is the on-screen card (mu-guarded).
type askState struct {
	req   AskRequest
	sel   int
	multi map[int]bool
	ch    chan askResult
}

type askResult struct {
	labels []string
	ok     bool
	// notice is set by the draw path when the card had no room to paint:
	// AskCard puts it in the transcript, then takes the skip.
	notice string
}

// minAskCardWidth is the narrowest screen the card renders on; below it the
// question goes to the transcript instead (AskCard's notice path).
const minAskCardWidth = 12

// AskCard shows the blocking card and returns the answer. ok=false means the
// user skipped (Esc), the context was canceled, or the timeout elapsed.
func (a *App) AskCard(ctx context.Context, req AskRequest, timeout time.Duration) (AskAnswer, bool) {
	if len(req.Options) == 0 {
		return AskAnswer{}, false
	}
	a.mu.Lock()
	narrow := a.width < minAskCardWidth
	a.mu.Unlock()
	if narrow {
		// No room for the card: say the question in the transcript and take
		// the skip path, so the tool's headless policy answers instead of
		// the question vanishing.
		a.AddSystemBlock(askNotice(req, "window too narrow for the option card — using the recommended path"))
		return AskAnswer{}, false
	}
	st := &askState{req: req, ch: make(chan askResult, 1)}
	if req.Multi {
		st.multi = map[int]bool{}
	}
	st.sel = recommendedIndex(req)

	a.mu.Lock()
	a.ask = st
	a.mu.Unlock()
	a.poke()
	defer func() {
		a.mu.Lock()
		if a.ask == st {
			a.ask = nil
		}
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
	case res := <-st.ch:
		if res.notice != "" {
			// The draw path closed an unpaintable card; keep its record.
			a.AddSystemBlock(res.notice)
			return AskAnswer{}, false
		}
		return AskAnswer{Labels: res.labels}, res.ok
	case <-deadline:
		// The wait the headless policy would have run is already spent, so
		// record the question and let the caller answer from the card alone.
		a.AddSystemBlock(askNotice(req, fmt.Sprintf("no answer within %s — using the recommended path", timeout)))
		return AskAnswer{}, false
	case <-ctx.Done():
		return AskAnswer{}, false
	}
}

// recommendedIndex highlights the first recommended option (0 when none of
// them is on the card).
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

// AskPending reports whether the blocking card is on screen.
func (a *App) AskPending() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ask != nil
}

// AskSelection returns the highlighted option index and the option count.
func (a *App) AskSelection() (sel, n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ask == nil {
		return 0, 0
	}
	return a.ask.sel, len(a.ask.req.Options)
}

// handleAskKey routes keys while the card is open. The card is modal: every
// key is swallowed (handled=true), so the composer never sees a keystroke
// aimed at the question.
func (a *App) handleAskKey(key *tcell.EventKey) (handled bool) {
	a.mu.Lock()
	st := a.ask
	a.mu.Unlock()
	if st == nil {
		return false
	}
	switch key.Key() {
	case tcell.KeyUp:
		a.moveAsk(-1)
	case tcell.KeyDown:
		a.moveAsk(1)
	case tcell.KeyEnter:
		a.resolveAsk(true)
	case tcell.KeyEsc, tcell.KeyCtrlC:
		// Skip: the caller falls back to the tool's recommended options.
		a.resolveAsk(false)
	case tcell.KeyRune:
		r := key.Rune()
		switch {
		case r >= '1' && r <= '9':
			a.quickAsk(int(r - '1'))
		case r == ' ':
			a.toggleAsk()
		}
	}
	return true
}

func (a *App) moveAsk(delta int) {
	a.mu.Lock()
	if st := a.ask; st != nil && len(st.req.Options) > 0 {
		n := len(st.req.Options)
		st.sel = (st.sel + delta + n) % n
	}
	a.mu.Unlock()
	a.poke()
}

// quickAsk is the 1-9 shortcut: a single-choice card confirms the digit, a
// multi-choice card toggles it (Enter confirms the toggled set).
func (a *App) quickAsk(idx int) {
	a.mu.Lock()
	st := a.ask
	if st == nil || idx < 0 || idx >= len(st.req.Options) {
		a.mu.Unlock()
		return
	}
	st.sel = idx
	multi := st.req.Multi
	a.mu.Unlock()
	if multi {
		a.toggleAsk()
		return
	}
	a.resolveAsk(true)
}

// toggleAsk flips the highlighted option on a multi-choice card.
func (a *App) toggleAsk() {
	a.mu.Lock()
	if st := a.ask; st != nil && st.req.Multi {
		st.multi[st.sel] = !st.multi[st.sel]
	}
	a.mu.Unlock()
	a.poke()
}

// resolveAsk answers the open card and wakes the parked AskCard. confirm=false
// is the skip path (Esc/timeout parity: no labels, ok=false).
func (a *App) resolveAsk(confirm bool) {
	a.mu.Lock()
	st := a.ask
	if st == nil {
		a.mu.Unlock()
		return
	}
	a.ask = nil
	var labels []string
	if confirm {
		if st.req.Multi {
			for i, opt := range st.req.Options {
				if st.multi[i] {
					labels = append(labels, opt.Label)
				}
			}
		}
		if len(labels) == 0 && st.sel >= 0 && st.sel < len(st.req.Options) {
			labels = []string{st.req.Options[st.sel].Label}
		}
	}
	ch := st.ch
	a.mu.Unlock()
	select {
	case ch <- askResult{labels: labels, ok: confirm && len(labels) > 0}:
	default:
	}
	a.poke()
}

// dropAskLocked closes the card from the draw path when it cannot paint — the
// 5b999f0 invariant (a modal that cannot paint must close) restated for ask:
// owning the keyboard while painting nothing looked like a dead UI. Callers
// hold a.mu, so AddSystemBlock/resolveAsk would re-lock and deadlock the UI
// thread; the parked AskCard is woken through its buffered channel instead and
// leaves the question in the transcript itself.
func (a *App) dropAskLocked(st *askState) {
	if a.ask != st {
		return
	}
	a.ask = nil
	select {
	case st.ch <- askResult{notice: askNotice(st.req, "no room above the composer for the option card — using the recommended path")}:
	default:
	}
}

// drawAskCard renders the blocking question above the composer. Callers hold
// a.mu (draw does), so this must not re-lock.
func (a *App) drawAskCard(yComposerTop int) {
	st := a.ask
	if st == nil || len(st.req.Options) == 0 {
		return
	}
	w, s := a.width, a.scr
	if w < minAskCardWidth {
		a.dropAskLocked(st)
		return
	}
	box := a.th.Box()
	header := box.Horizontal + " ask"
	if st.req.Multi {
		header += " · space toggles"
	}
	footer := "↑/↓ select · 1-9 quick pick · Enter confirm · Esc skip"
	if st.req.Multi {
		footer = "↑/↓ select · Space toggle · 1-9 quick pick · Enter confirm · Esc skip"
	}
	question := wrapAsk(st.req.Question, max(8, w-10))

	rows := make([]string, 0, len(st.req.Options))
	for i, opt := range st.req.Options {
		mark := "  "
		switch {
		case i == st.sel && st.multi[i]:
			mark = "❯✚"
		case i == st.sel:
			mark = "❯ "
		case st.multi[i]:
			mark = " ✚"
		}
		row := fmt.Sprintf("%s %d. %s", mark, i+1, opt.Label)
		if opt.Description != "" {
			row += "  " + opt.Description
		}
		if containsLabel(st.req.Recommended, opt.Label) {
			row += "  (recommended)"
		}
		rows = append(rows, row)
	}

	// inner is the cell count between the card's two verticals: the header,
	// every content row and the bottom border measure from it, so the right
	// edge lands on one column.
	inner := width(header)
	for _, q := range question {
		inner = max(inner, width(q)+2)
	}
	for _, r := range rows {
		inner = max(inner, width(r)+2)
	}
	inner = max(inner, width(footer)+2)
	inner = min(inner, w-6)
	right := 3 + inner // column of the right border glyph

	// The card floats above the composer and may overlap the composer's own
	// top border row (like the other cards), never its input rows.
	y := yComposerTop - len(rows) - len(question) - 3
	if y < 1 {
		a.dropAskLocked(st)
		return
	}
	borderSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.PromptBorderActive)))
	rowSt := tcell.StyleDefault.Background(a.cellColor(a.th.Get(theme.BgBase)))
	selSt := tcell.StyleDefault.Background(a.cellColor(a.th.Get(theme.BgHighlight))).Bold(true)
	qSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.TextPrimary)))
	footSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.Gray)))

	drawText(s, 2, y, box.TopLeft+header+strings.Repeat(box.Horizontal, max(0, inner-width(header)))+box.TopRight, borderSt)
	y++
	for _, q := range question {
		a.askFill(rowSt, y, w)
		drawText(s, 3, y, q, qSt)
		drawText(s, right, y, box.Vertical, borderSt)
		y++
	}
	for i, r := range rows {
		rowStyle := rowSt
		if i == st.sel {
			rowStyle = selSt
		}
		a.askFill(rowStyle, y, w)
		drawText(s, 3, y, r, rowStyle.Foreground(a.cellColor(a.th.Get(theme.TextPrimary))))
		drawText(s, right, y, box.Vertical, borderSt)
		y++
	}
	a.askFill(rowSt, y, w)
	drawText(s, 3, y, footer, footSt)
	drawText(s, right, y, box.Vertical, borderSt)
	y++
	drawText(s, 2, y, box.BottomLeft+strings.Repeat(box.Horizontal, inner)+box.BottomRight, borderSt)
}

// askFill paints one card row's background from x=2 to the right edge.
func (a *App) askFill(st tcell.Style, y, w int) {
	for x := 2; x < w-2; x++ {
		a.scr.SetContent(x, y, ' ', nil, st)
	}
}

// wrapAsk hard-wraps a question to at most cells cells per line.
func wrapAsk(text string, cells int) []string {
	if cells <= 0 {
		cells = 20
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return []string{""}
	}
	var out []string
	line := ""
	for _, wd := range words {
		switch {
		case line == "":
			line = wd
		case len([]rune(line))+1+len([]rune(wd)) <= cells:
			line += " " + wd
		default:
			out = append(out, line)
			line = wd
		}
	}
	return append(out, line)
}

// askNotice renders a question as a transcript block: the record kept when
// the card did not get an answer (no room, or nobody answered in time).
func askNotice(req AskRequest, why string) string {
	var b strings.Builder
	b.WriteString("ask: " + req.Question)
	for _, o := range req.Options {
		b.WriteString("\n  - " + o.Label)
		if o.Description != "" {
			b.WriteString(": " + o.Description)
		}
	}
	if len(req.Recommended) > 0 {
		b.WriteString("\n  recommended: " + strings.Join(req.Recommended, ", "))
	}
	b.WriteString("\n  (" + why + ")")
	return b.String()
}

func containsLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}
