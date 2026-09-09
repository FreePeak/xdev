package tui

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// spinnerFrames are the braille spinner (Grok-style running indicator).
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Status carries the status-line state.
type Status struct {
	Model      string
	SessionID  string
	TokensIn   int64
	TokensOut  int64
	Running    bool
	spinnerIdx int
}

// App is the xdev TUI: scrollback of blocks + editor + status line, drawn
// with the Grok-style accent rail. The model thread mutates state through
// the exported mutators (mutex-guarded); the UI loop redraws at ~30fps and
// on every key event.
type App struct {
	scr    tcell.Screen
	th     *theme.Theme
	mu     sync.Mutex
	blocks []*Block
	offset int // lines scrolled up from the live tail (0 = follow)
	ed     Editor
	st     Status

	width, height int

	// Wired by cmd: onSend runs the agent turn; onCancel aborts it; onQuit exits.
	onSend   func(text string)
	onCancel func()
	onQuit   func()

	keyq      chan tcell.Event
	dirty     chan struct{}
	quitCh    chan struct{}
	lineCache map[blockKey][]string
}

type blockKey struct {
	idx    int
	kind   BlockKind
	width  int
	tlen   int
	tool   string
	status string
}

// New creates the App over an initialized screen.
func New(scr tcell.Screen, th *theme.Theme, model, sessionID string) *App {
	w, h := scr.Size()
	return &App{
		scr:   scr,
		th:    th,
		st:    Status{Model: model, SessionID: sessionID},
		width: w, height: h,
		keyq:      make(chan tcell.Event, 64),
		dirty:     make(chan struct{}, 1),
		quitCh:    make(chan struct{}),
		lineCache: map[blockKey][]string{},
	}
}

// SetHandlers wires the send/cancel/quit callbacks.
func (a *App) SetHandlers(onSend func(text string), onCancel, onQuit func()) {
	a.onSend, a.onCancel, a.onQuit = onSend, onCancel, onQuit
}

// Invalidate clears the render cache (resize, theme change).
func (a *App) Invalidate() {
	a.mu.Lock()
	a.lineCache = map[blockKey][]string{}
	a.mu.Unlock()
	a.poke()
}

// --- mutators (model thread / key thread) ---

// AddUserBlock appends a user prompt block.
func (a *App) AddUserBlock(text string) {
	a.mu.Lock()
	a.blocks = append(a.blocks, &Block{Kind: KindUser, Text: text})
	a.offset = 0
	a.mu.Unlock()
	a.poke()
}

// AddSystemBlock appends a harness notice.
func (a *App) AddSystemBlock(text string) {
	a.mu.Lock()
	a.blocks = append(a.blocks, &Block{Kind: KindSystem, Text: text})
	a.offset = 0
	a.mu.Unlock()
	a.poke()
}

// BeginAssistant starts (or continues into) the streaming assistant block.
func (a *App) BeginAssistant() {
	a.mu.Lock()
	a.st.Running = true
	if n := len(a.blocks); n == 0 || a.blocks[n-1].Kind != KindAssistant || !a.blocks[n-1].stream {
		a.blocks = append(a.blocks, &Block{Kind: KindAssistant, stream: true})
	}
	a.mu.Unlock()
	a.poke()
}

// AppendAssistant appends a text delta to the streaming assistant block.
func (a *App) AppendAssistant(delta string) {
	a.mu.Lock()
	if n := len(a.blocks); n > 0 && a.blocks[n-1].Kind == KindAssistant {
		a.blocks[n-1].Text += delta
		a.offset = 0
	}
	a.mu.Unlock()
	a.poke()
}

// EndAssistant closes the streaming block.
func (a *App) EndAssistant() {
	a.mu.Lock()
	if n := len(a.blocks); n > 0 && a.blocks[n-1].Kind == KindAssistant {
		a.blocks[n-1].stream = false
	}
	a.mu.Unlock()
	a.poke()
}

// BeginThinking adds a dim thinking block (collapsed while streaming).
func (a *App) BeginThinking() {
	a.mu.Lock()
	a.blocks = append(a.blocks, &Block{Kind: KindThinking, stream: true})
	a.mu.Unlock()
	a.poke()
}

// AppendThinking appends to the last streaming thinking block.
func (a *App) AppendThinking(delta string) {
	a.mu.Lock()
	for i := len(a.blocks) - 1; i >= 0; i-- {
		if a.blocks[i].Kind == KindThinking && a.blocks[i].stream {
			a.blocks[i].Text += delta
			break
		}
	}
	a.mu.Unlock()
	a.poke()
}

// EndThinking closes the last streaming thinking block.
func (a *App) EndThinking() {
	a.mu.Lock()
	for i := len(a.blocks) - 1; i >= 0; i-- {
		if a.blocks[i].Kind == KindThinking && a.blocks[i].stream {
			a.blocks[i].stream = false
			break
		}
	}
	a.mu.Unlock()
	a.poke()
}

// AddAssistantBlock appends a complete (non-streaming) assistant block —
// used for resumed-history replay.
func (a *App) AddAssistantBlock(text string) {
	a.mu.Lock()
	a.blocks = append(a.blocks, &Block{Kind: KindAssistant, Text: text})
	a.offset = 0
	a.mu.Unlock()
	a.poke()
}

// AddToolBlock appends a tool-call summary block (status running).
func (a *App) AddToolBlock(name, argsPreview string) {
	a.mu.Lock()
	a.blocks = append(a.blocks, &Block{Kind: KindTool, ToolName: name, Text: argsPreview, Status: "running"})
	a.offset = 0
	a.mu.Unlock()
	a.poke()
}

// FinishTool marks the last running tool block done (ok/error) and appends
// a one-line result preview.
func (a *App) FinishTool(name string, isErr bool, resultPreview string) {
	a.mu.Lock()
	for i := len(a.blocks) - 1; i >= 0; i-- {
		b := a.blocks[i]
		if b.Kind == KindTool && b.ToolName == name && b.Status == "running" {
			if isErr {
				b.Status = "error"
			} else {
				b.Status = "ok"
			}
			break
		}
	}
	kind := KindToolDone
	if isErr {
		kind = KindSystem
	}
	a.blocks = append(a.blocks, &Block{Kind: kind, ToolName: name, Text: resultPreview})
	a.offset = 0
	a.mu.Unlock()
	a.poke()
}

// AddUsage folds token usage into the status line.
func (a *App) AddUsage(in, out int64) {
	a.mu.Lock()
	a.st.TokensIn += in
	a.st.TokensOut += out
	a.mu.Unlock()
}

// SetRunning toggles the spinner state.
func (a *App) SetRunning(r bool) {
	a.mu.Lock()
	a.st.Running = r
	a.mu.Unlock()
	a.poke()
}

// FinishRun clears running state (after cancel or completion).
func (a *App) FinishRun() { a.SetRunning(false) }

// Quit terminates the UI loop.
func (a *App) Quit() { close(a.quitCh) }

func (a *App) poke() {
	select {
	case a.dirty <- struct{}{}:
	default:
	}
}

// --- UI loop ---

// Run drives the UI until Quit. Callers own screen init/fini.
func (a *App) Run() {
	a.width, a.height = a.scr.Size()
	tick := time.NewTicker(33 * time.Millisecond) // ~30fps
	defer tick.Stop()

	// Feed tcell events into keyq.
	go func() {
		for {
			ev := a.scr.PollEvent()
			if ev == nil {
				return
			}
			select {
			case a.keyq <- ev:
			case <-a.quitCh:
				return
			}
		}
	}()

	a.draw()
	for {
		select {
		case <-a.quitCh:
			return
		case ev := <-a.keyq:
			a.handleKey(ev)
			a.draw()
		case <-a.dirty:
			a.draw()
		case <-tick.C:
			a.mu.Lock()
			running := a.st.Running
			if running {
				a.st.spinnerIdx = (a.st.spinnerIdx + 1) % len(spinnerFrames)
			}
			a.mu.Unlock()
			if running {
				a.draw()
			}
		}
	}
}

// handleKey applies one key event (editor, scrolling, control keys).
func (a *App) handleKey(ev tcell.Event) {
	key, ok := ev.(*tcell.EventKey)
	if !ok {
		if r, ok := ev.(*tcell.EventResize); ok {
			a.mu.Lock()
			a.width, a.height = r.Size()
			a.lineCache = map[blockKey][]string{}
			a.mu.Unlock()
		}
		return
	}
	a.mu.Lock()
	running := a.st.Running
	h := a.height
	a.mu.Unlock()

	switch key.Key() {
	case tcell.KeyCtrlC, tcell.KeyCtrlD:
		if running {
			a.onCancel()
			return
		}
		a.onQuit()
		return
	case tcell.KeyEsc:
		if running {
			a.onCancel()
		}
		return
	case tcell.KeyPgUp:
		a.scroll(-h / 2)
		return
	case tcell.KeyPgDn:
		a.scroll(h / 2)
		return
	case tcell.KeyCtrlL:
		a.Invalidate()
		return
	}

	if key.Key() == tcell.KeyUp && !running {
		a.scroll(-1)
		return
	}
	if key.Key() == tcell.KeyDown && !running {
		a.scroll(1)
		return
	}

	// Editor keys. Text is captured BEFORE HandleKey — the editor archives
	// and resets itself when it reports send.
	text := strings.TrimSpace(a.ed.Text())
	send := a.ed.HandleKey(key)
	a.offset = 0
	if send {
		a.mu.Lock()
		a.blocks = append(a.blocks, &Block{Kind: KindUser, Text: text})
		a.offset = 0
		a.mu.Unlock()
		if a.onSend != nil {
			a.onSend(text)
		}
	}
	a.poke()
}

func (a *App) scroll(lines int) {
	a.mu.Lock()
	total := a.totalLinesLocked()
	vp := a.viewportLinesLocked()
	if vp < 1 {
		vp = 1
	}
	maxOff := total - vp
	if maxOff < 0 {
		maxOff = 0
	}
	a.offset -= lines // scroll-up (negative lines) moves the viewport up
	if a.offset > maxOff {
		a.offset = maxOff
	}
	if a.offset < 0 {
		a.offset = 0
	}
	a.mu.Unlock()
	a.poke()
}

// totalLinesLocked counts rendered lines across all blocks.
func (a *App) totalLinesLocked() int {
	w := a.contentWidth()
	n := 0
	for i := range a.blocks {
		n += len(a.blockLinesLocked(i, a.blocks[i], w)) + 1 // separator
	}
	return n
}

func (a *App) viewportLinesLocked() int {
	return a.height - 3 // scrollback + status + editor
}

// contentWidth is the scrollback text width (rail + padding removed).
func (a *App) contentWidth() int {
	w := a.width - 4 // rail(1) + gap(1) + right pad(2)
	if w < 10 {
		w = 10
	}
	return w
}

// blockLinesLocked renders a block to visual lines (cached per width/state).
func (a *App) blockLinesLocked(i int, b *Block, w int) []string {
	key := blockKey{idx: i, kind: b.Kind, width: w, tlen: len(b.Text), tool: b.ToolName, status: b.Status}
	if lines, ok := a.lineCache[key]; ok {
		return lines
	}
	var lines []string
	switch b.Kind {
	case KindTool:
		lines = wrap(toolSummary(b, w), w)
	case KindToolDone:
		prev := strings.Join(strings.Fields(b.Text), " ")
		if prev == "" {
			prev = "(no output)"
		}
		if len(prev) > w*3 {
			prev = prev[:w*3] + "…"
		}
		lines = wrap("↳ "+prev, w)
	default:
		lines = wrap(strings.TrimRight(b.Text, "\n"), w)
		if b.stream && (b.Kind == KindThinking || b.Kind == KindAssistant) {
			if n := len(lines); n > 0 {
				lines[n-1] += " ▍"
			}
		}
	}
	a.lineCache[key] = lines
	return lines
}

// --- drawing ---

func (a *App) draw() {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.scr
	s.Clear()
	w, h := a.width, a.height

	// Layout: scrollback rows [0, h-3), status line h-2, editor h-1.
	vp := h - 3
	if vp < 1 {
		vp = 1
	}
	contentW := a.contentWidth()

	// Flatten block lines into (text, style, railStyle) rows.
	type row struct {
		text  string
		rail  string
		style tcell.Style
		railS tcell.Style
	}
	var rows []row
	for i := range a.blocks {
		b := a.blocks[i]
		lines := a.blockLinesLocked(i, b, contentW)
		var st tcell.Style
		var railCh string
		var railS tcell.Style
		switch b.Kind {
		case KindUser:
			st = tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentUser)))
			for j, ln := range lines {
				prefix := "  "
				if j == 0 {
					prefix = "❯ "
				}
				rows = append(rows, row{text: prefix + ln, style: st})
			}
			rows = append(rows, row{text: "", style: st})
			continue
		case KindThinking:
			st = tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.Gray)))
			railCh = "│"
			railS = tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim)))
		case KindTool:
			railCh = "│"
			fg := a.th.Get(theme.AccentTool)
			if b.Status == "running" {
				fg = a.th.Get(theme.AccentRunning)
			}
			if b.Status == "error" {
				fg = a.th.Get(theme.AccentError)
			}
			st = tcell.StyleDefault.Foreground(a.cellColor(fg)).Bold(b.Status != "running")
			railS = tcell.StyleDefault.Foreground(a.cellColor(fg))
		case KindToolDone:
			st = tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.Gray)))
			railCh = " "
			railS = st
		case KindSystem:
			fg := a.th.Get(theme.Gray)
			if strings.Contains(strings.ToLower(b.Text), "error") || strings.Contains(b.Text, "canceled") {
				fg = a.th.Get(theme.AccentError)
			}
			st = tcell.StyleDefault.Foreground(a.cellColor(fg)).Italic(true)
			railCh = " "
			railS = st
		default: // assistant
			railCh = "│"
			st = tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.TextPrimary)))
			railS = tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentAssistant)))
		}
		for _, ln := range lines {
			rows = append(rows, row{text: ln, rail: railCh, style: st, railS: railS})
		}
		rows = append(rows, row{text: "", style: st, rail: " ", railS: railS}) // separator
	}

	// Viewport: show rows[offset .. offset+vp) from the bottom.
	start := len(rows) - vp - a.offset
	if start < 0 {
		start = 0
	}
	end := start + vp
	if end > len(rows) {
		end = len(rows)
	}
	for y, r := range rows[start:end] {
		// Rail column.
		if r.rail != "" {
			drawText(s, 0, y, r.rail, r.railS)
		}
		// Content, indented under the rail (user prompts flush-left).
		x := 2
		if r.rail == "" && r.text != "" && strings.HasPrefix(r.text, "❯") {
			x = 0
		}
		drawText(s, x, y, r.text, r.style)
	}

	// Scroll indicator when scrolled up.
	if a.offset > 0 {
		hint := fmt.Sprintf("▲ %d", a.offset)
		drawText(s, w-len(hint)-1, 0, hint,
			tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.Gray))))
	}

	a.drawStatus(h - 2)
	a.drawEditor(h - 1)
	s.Show()
}

func (a *App) drawStatus(y int) {
	a.st.spinnerIdx = a.st.spinnerIdx % len(spinnerFrames) // safety
	sp := ""
	if a.st.Running {
		sp = " " + spinnerFrames[a.st.spinnerIdx]
	}
	left := fmt.Sprintf(" %s · %s · ↑%s ↓%s tokens%s",
		a.st.Model, shortID(a.st.SessionID),
		humanTokens(a.st.TokensIn), humanTokens(a.st.TokensOut), sp)
	st := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.Gray)))
	drawText(a.scr, 0, y, left, st)

	hint := "Enter send · Esc cancel · Ctrl+C quit "
	drawText(a.scr, a.width-width(hint), y, hint,
		tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim))))
}

// drawEditor renders the prompt line. Caller (draw) holds a.mu.
func (a *App) drawEditor(y int) {
	text := a.ed.Text()
	cur := a.ed.cur

	prompt := "❯ "
	promptStyle := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentUser))).Bold(true)
	textStyle := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.TextPrimary)))

	drawText(a.scr, 0, y, prompt, promptStyle)
	// Render the tail of the buffer that fits (long drafts scroll).
	avail := a.width - width(prompt) - 1
	vis := []rune(text)
	if width(text) > avail {
		// Keep the cursor visible: show the window ending at cursor.
		start := cur
		shown := 0
		for start > 0 && shown < avail-3 {
			start--
			shown += width(string(vis[start]))
		}
		vis = append(vis[start:], []rune("…")...)
		cur = cur - start
		if cur > len(vis) {
			cur = len(vis)
		}
	}
	drawText(a.scr, width(prompt), y, string(vis), textStyle)

	// Cursor position within the visible window.
	cx := width(prompt) + width(string(vis[:min(cur, len(vis))]))
	a.scr.ShowCursor(cx, y)
}

// drawText writes s at (x, y); wide runes handled by tcell.
func drawText(s tcell.Screen, x, y int, text string, st tcell.Style) {
	for _, r := range text {
		s.SetContent(x, y, r, nil, st)
		x += width(string(r))
	}
}

func (a *App) cellColor(c theme.Color) tcell.Color {
	return tcell.NewRGBColor(int32(c.R), int32(c.G), int32(c.B))
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// humanTokens renders 1234 as "1.2k".
func humanTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
