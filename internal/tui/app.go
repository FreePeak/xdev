package tui

import (
	"fmt"
	"github.com/FreePeak/xdev/internal/logx"
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
	sm     scrollModel // transcript viewport (offset/follow), see scroll.go
	ed     Editor
	smenu  *slashMenu // "/" autocomplete dropdown (nil = closed)
	keyMap *KeyMap    // remappable keybinding layer
	st     Status

	width, height int

	// Wired by cmd: onSend runs the agent turn; onCancel aborts it; onQuit exits.
	ops         *SessionOps       // session lifecycle, wired by cmd (nil → notices)
	cwdLabel    string            // welcome top bar (last two path components)
	branch      string            // git branch for the welcome top bar ("" when none)
	commandDir  string            // markdown command discovery root
	pathRoot    string            // @-completion root (empty disables the menu)
	pathScan    func() []string   // shared FS-scan cache-backed file source
	extCommands map[string]string // "/server:cmd" -> description
	extRun      ExtensionCommand
	renderers   map[string]RenderSpec // tool name -> declarative render spec
	onSend      func(text string)
	onCancel    func()
	onQuit      func()

	keyq      chan tcell.Event
	dirty     chan struct{}
	quitCh    chan struct{}
	lineCache map[blockKey][]line

	// Welcome-screen Game of Life backdrop (UI thread; guarded by mu).
	life     lifeGrid
	lifeTick int
}

type blockKey struct {
	idx    int
	kind   BlockKind
	width  int
	tlen   int
	tool   string
	status string
	stream bool
}

// New creates the App over an initialized screen.
func New(scr tcell.Screen, th *theme.Theme, model, sessionID string) *App {
	w, h := scr.Size()
	km, err := LoadKeyMap()
	if err != nil {
		logx.Errorf("tui: keybindings.yml: %v (using defaults)", err)
		km = DefaultKeyMap()
	}
	return &App{
		keyMap: km,
		scr:    scr,
		th:     th,
		st:     Status{Model: model, SessionID: sessionID},
		width:  w, height: h,
		keyq:      make(chan tcell.Event, 64),
		dirty:     make(chan struct{}, 1),
		quitCh:    make(chan struct{}),
		sm:        newScrollModel(),
		lineCache: map[blockKey][]line{},
	}
}

// SetLocation sets the welcome top-bar location (cwd + git branch).
func (a *App) SetLocation(cwd string) {
	a.mu.Lock()
	a.cwdLabel = cwdShort(cwd)
	a.branch = gitBranch(cwd)
	a.mu.Unlock()
}

// SetHandlers wires the send/cancel/quit callbacks.
func (a *App) SetHandlers(onSend func(text string), onCancel, onQuit func()) {
	a.onSend, a.onCancel, a.onQuit = onSend, onCancel, onQuit
}

// Invalidate clears the render cache (resize, theme change).
func (a *App) Invalidate() {
	a.mu.Lock()
	a.lineCache = map[blockKey][]line{}
	a.mu.Unlock()
	a.poke()
}

// --- mutators (model thread / key thread) ---

// AddUserBlock appends a user prompt block.
func (a *App) AddUserBlock(text string) {
	a.mu.Lock()
	a.blocks = append(a.blocks, &Block{Kind: KindUser, Text: text, Ts: time.Now()})
	a.mu.Unlock()
	a.poke()
}

// AddSystemBlock appends a harness notice.
// KeyMap returns the active keybinding map.
func (a *App) KeyMap() *KeyMap { return a.keyMap }

func (a *App) AddSystemBlock(text string) {
	a.mu.Lock()
	a.blocks = append(a.blocks, &Block{Kind: KindSystem, Text: text})
	a.mu.Unlock()
	a.poke()
}

// BeginAssistant starts (or continues into) the streaming assistant block.
func (a *App) BeginAssistant() {
	a.mu.Lock()
	a.st.Running = true
	if n := len(a.blocks); n == 0 || a.blocks[n-1].Kind != KindAssistant || !a.blocks[n-1].stream {
		a.blocks = append(a.blocks, &Block{Kind: KindAssistant, stream: true, Ts: time.Now()})
	}
	a.mu.Unlock()
	a.poke()
}

// AppendAssistant appends a text delta to the streaming assistant block.
func (a *App) AppendAssistant(delta string) {
	a.mu.Lock()
	if n := len(a.blocks); n > 0 && a.blocks[n-1].Kind == KindAssistant {
		a.blocks[n-1].Text += delta
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
	a.blocks = append(a.blocks, &Block{Kind: KindThinking, stream: true, Ts: time.Now()})
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

// EndThinking closes the last streaming thinking block, freezing its
// duration for the "Thought for Xs" header.
func (a *App) EndThinking() {
	a.mu.Lock()
	for i := len(a.blocks) - 1; i >= 0; i-- {
		if a.blocks[i].Kind == KindThinking && a.blocks[i].stream {
			a.blocks[i].stream = false
			a.blocks[i].thinkDur = time.Since(a.blocks[i].Ts)
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
	a.mu.Unlock()
	a.poke()
}

// AddToolBlock appends a tool-call summary block (status running).
func (a *App) AddToolBlock(name, argsPreview string) {
	a.mu.Lock()
	a.blocks = append(a.blocks, &Block{Kind: KindTool, ToolName: name, Text: argsPreview, Status: "running"})
	a.mu.Unlock()
	a.poke()
}

// FinishTool marks the last running tool block done (ok/error) and appends
// the tool-result block carrying the full (sink-windowed) output.
func (a *App) FinishTool(name string, isErr bool, output, dur string) {
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
	text := output
	if !isErr && len(a.renderers) > 0 {
		if spec, ok := a.renderers[name]; ok {
			if rendered, rok := renderToolOutput(spec, name, output); rok {
				text = rendered
			}
		}
	}
	a.blocks = append(a.blocks, &Block{Kind: KindToolDone, ToolName: name, Text: text, Dur: dur, Err: isErr})
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

// ForkSession implements CommandAPI by delegating to wired SessionOps.Fork.
func (a *App) ForkSession() error {
	if a.ops == nil || a.ops.Fork == nil {
		return fmt.Errorf("session fork not wired")
	}
	return a.ops.Fork()
}

// DumpSession implements CommandAPI by exporting the transcript.
func (a *App) DumpSession() error {
	if a.ops == nil || a.ops.Dump == nil {
		return fmt.Errorf("session dump not wired")
	}
	path, err := a.ops.Dump()
	if err != nil {
		return err
	}
	a.AddSystemBlock("transcript dumped to " + path)
	return nil
}

// ResumeSession implements CommandAPI by swapping to the resumed store.
func (a *App) ResumeSession(query string) error {
	if a.ops == nil || a.ops.Resume == nil {
		return fmt.Errorf("session resume not wired")
	}
	return a.ops.Resume(query)
}

// Reset clears the transcript (used by /clear): all blocks gone, viewport
// back to follow. Streaming state is untouched — callers must not be
// running a turn when they call this.
func (a *App) Reset() {
	a.mu.Lock()
	a.blocks = nil
	a.sm = newScrollModel()
	a.lineCache = map[blockKey][]line{}
	a.mu.Unlock()
	a.poke()
}

// SetSessionOps wires the session lifecycle (store lives in cmd). Nil ops
// degrade the /new /clear /drop commands to notices.
func (a *App) SetSessionOps(ops *SessionOps) { a.ops = ops }

// SendPrompt submits text through the normal send path (markdown commands).
func (a *App) SendPrompt(text string) {
	a.mu.Lock()
	a.blocks = append(a.blocks, &Block{Kind: KindUser, Text: text, Ts: time.Now()})
	a.sm.Bottom()
	a.mu.Unlock()
	if a.onSend != nil {
		a.onSend(text)
	}
	a.poke()
}

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
			// Game of Life on the welcome screen: step every 4th
			// 33ms tick (~8fps) and redraw only on those steps,
			// while it is visible (no blocks, nothing running) —
			// idle CPU stays near zero between steps.
			animate := false
			if !running && len(a.blocks) == 0 {
				gw, top, bot, ok := lifeArea(a.width, a.height)
				if ok {
					a.lifeTick = (a.lifeTick + 1) % 4
					if a.lifeTick == 0 {
						a.stepLife(gw, top, bot)
						animate = true
					}
				}
			}
			a.mu.Unlock()
			if running || animate {
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
			a.lineCache = map[blockKey][]line{}
			a.mu.Unlock()
		}
		// Mouse wheel scrolls the in-app transcript (tcell would otherwise
		// let the host terminal scroll its own pre-launch scrollback).
		if m, ok := ev.(*tcell.EventMouse); ok {
			switch m.Buttons() {
			case tcell.WheelUp:
				a.scroll(3, false)
			case tcell.WheelDown:
				a.scroll(3, true)
			}
		}
		return
	}
	a.mu.Lock()
	running := a.st.Running
	h := a.height
	menuOpen := a.smenu != nil && a.smenu.active()
	a.mu.Unlock()

	// Slash dropdown owns navigation while open (grok slash_dropdown).
	if menuOpen {
		switch key.Key() {
		case tcell.KeyTab:
			a.mu.Lock()
			if sel, ok := a.smenu.selected(); ok {
				text := sel.Name
				next := ""
				if sel.kind == kindPath {
					// Replace only the @token: the user's sentence stays.
					text = a.smenu.pathPrefix + "@" + sel.Name + " "
					next = text
				} else {
					next = strings.TrimPrefix(sel.Name, "/")
				}
				a.ed.Reset()
				for _, r := range text {
					a.ed.HandleKey(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
				}
				a.ed.HandleKey(tcell.NewEventKey(tcell.KeyEnd, 0, tcell.ModNone))
				if sel.kind == kindPath {
					if prefix, q, ok := pathToken(next); ok {
						a.smenu.openPaths(prefix, q, a.pathCandidates(q))
					} else {
						a.smenu = nil
					}
				} else {
					a.smenu.open(next, a.commandDir, a.extCommands)
				}
			}
			a.mu.Unlock()
			a.poke()
			return
		case tcell.KeyEsc: // close without changing the text
			a.mu.Lock()
			a.smenu = nil
			a.mu.Unlock()
			a.poke()
			return
		}
	}

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
	case tcell.KeyPgUp, tcell.KeyCtrlB:
		a.scroll(h/2, false)
		return
	case tcell.KeyPgDn, tcell.KeyCtrlF:
		a.scroll(h/2, true)
		return
	case tcell.KeyHome:
		a.scrollTo(false)
		return
	case tcell.KeyEnd:
		a.scrollTo(true)
		return
	case tcell.KeyCtrlL:
		a.Invalidate()
		return
	}

	// Slash dropdown navigation: while the menu is open the arrows move the
	// selection (not the transcript), Tab completes the selection into the
	// editor, and Enter still submits the typed text through dispatch.
	if menuOpen {
		switch key.Key() {
		case tcell.KeyUp:
			a.mu.Lock()
			a.smenu.move(-1)
			a.mu.Unlock()
			a.poke()
			return
		case tcell.KeyDown:
			a.mu.Lock()
			a.smenu.move(1)
			a.mu.Unlock()
			a.poke()
			return
		}
	}

	// Empty editor: arrows scroll the transcript. Non-empty: the editor
	// uses them for history recall.
	if !running && strings.TrimSpace(a.ed.Text()) == "" && !menuOpen {
		switch key.Key() {
		case tcell.KeyUp:
			a.scroll(1, false)
			return
		case tcell.KeyDown:
			a.scroll(1, true)
			return
		}
	}

	// Editor keys. Text is captured BEFORE HandleKey — the editor archives
	// and resets itself when it reports send.
	text := strings.TrimSpace(a.ed.Text())
	send := a.ed.HandleKey(key)
	a.syncSlashMenu()
	if send {
		// slash command routing (issue #11): a command is consumed by the
		// router — no user block, no agent run.
		if dispatch(a, text) {
			a.smenu = nil // editor reset — the dropdown is moot
			a.poke()
			return
		}
		a.mu.Lock()
		a.blocks = append(a.blocks, &Block{Kind: KindUser, Text: text})
		a.sm.Bottom()
		a.mu.Unlock()
		if a.onSend != nil {
			a.onSend(text)
		}
	}
	a.poke()
}

// syncSlashMenu opens, re-queries, or closes the "/" dropdown to match the
// editor text: open only while the first token is still being typed
// (no space yet). A closing menu keeps the current text untouched.
func (a *App) syncSlashMenu() {
	text := a.ed.Text()
	if strings.HasPrefix(text, "/") && !strings.ContainsAny(text, " \t\n") {
		if a.smenu == nil {
			a.smenu = newSlashMenu()
		}
		a.smenu.open(text[1:], a.commandDir, a.extCommands)
		return
	}
	if a.pathScan != nil {
		if prefix, query, ok := pathToken(text); ok {
			if a.smenu == nil {
				a.smenu = newSlashMenu()
			}
			a.smenu.openPaths(prefix, query, a.pathCandidates(query))
			return
		}
	}
	a.smenu = nil
}

// scroll moves the viewport n lines toward older (down=false) or newer
// (down=true) rows, clamped by the scroll model.
func (a *App) scroll(n int, down bool) {
	a.mu.Lock()
	total, vp := a.totalLinesLocked(), a.viewportLinesLocked()
	if down {
		a.sm.ScrollDown(n, total, vp)
	} else {
		a.sm.ScrollUp(n, total, vp)
	}
	a.mu.Unlock()
	a.poke()
}

// scrollTo jumps to the oldest (down=false) or newest (down=true) row.
func (a *App) scrollTo(down bool) {
	a.mu.Lock()
	if down {
		a.sm.Bottom()
	} else {
		a.sm.Top(a.totalLinesLocked(), a.viewportLinesLocked())
	}
	a.mu.Unlock()
	a.poke()
}

// totalLinesLocked counts rendered lines across all blocks.
func (a *App) totalLinesLocked() int {
	w := a.contentWidth()
	n := 0
	for i := range a.blocks {
		n += len(a.blockLines(i, a.blocks[i], w)) + 1 // separator
	}
	return n
}

func (a *App) viewportLinesLocked() int {
	return a.height - 4 // scrollback + composer(2) + shortcuts
}

// contentWidth is the scrollback text width (rail + padding removed).
func (a *App) contentWidth() int {
	w := a.width - 4 // rail(1) + gap(1) + right pad(2)
	if w < 10 {
		w = 10
	}
	return w
}

// blockLines renders a block to styled visual lines (cached per width/state).
func (a *App) blockLines(i int, b *Block, w int) []line {
	key := blockKey{idx: i, kind: b.Kind, width: w, tlen: len(b.Text), tool: b.ToolName, status: b.Status, stream: b.stream}
	if lines, ok := a.lineCache[key]; ok {
		return lines
	}
	var lines []line
	switch b.Kind {
	case KindUser:
		// Grok user prompt: ❯ prefix, text_primary body, bg-highlight band
		// across the full row; continuation lines indent past the prefix.
		band := a.cellColor(a.th.Get(theme.BgHighlight))
		pfxSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentUser)))
		bodySt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.TextPrimary)))
		text := strings.TrimRight(b.Text, "\n")
		wrapped := wrap(text, max(10, w-2))
		for j, wl := range wrapped {
			ln := line{bg: band}
			if j == 0 {
				ln.runs = append(ln.runs, cell{text: "❯ ", style: pfxSt})
			} else {
				ln.runs = append(ln.runs, cell{text: "  ", style: pfxSt})
			}
			ln.runs = append(ln.runs, cell{text: wl, style: bodySt})
			lines = append(lines, ln)
		}
		if text == "" {
			ln := textline("❯ ", pfxSt)
			ln.bg = band
			lines = append(lines, ln)
		}
	case KindThinking:
		// Grok thinking.rs: "Thinking…" (running, with braille spinner) or
		// "Thought for Xs" (done). Muted bold; body hidden (collapsed).
		var hdr string
		if b.stream {
			hdr = "⠹ Thinking…"
		} else if b.thinkDur > 0 {
			hdr = fmt.Sprintf("Thought for %.1fs", b.thinkDur.Seconds())
		} else {
			hdr = "Thought"
		}
		lines = append(lines, textline(hdr, stThinkingHdr(a, b.stream)))
	case KindTool:
		fg := theme.AccentTool
		switch b.Status {
		case "running":
			fg = theme.AccentRunning
		case "error":
			fg = theme.AccentError
		case "ok":
			fg = theme.AccentSuccess
		}
		// ◈ bullet colored by state; name+args in secondary text.
		ln := textline("◈ "+toolSummary(b, w), tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.TextSecondary))))
		ln.runs[0].style = tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(fg)))
		lines = append(lines, ln)
	case KindToolDone:
		hdrSt := a.mdStyle().muted
		st := a.mdStyle().muted
		state := "ok"
		if b.Err {
			state = "error"
			hdrSt = tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentError)))
			st = tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentError)))
		}
		hdr := "↳ " + state
		if b.Dur != "" {
			hdr += " (" + b.Dur + ")"
		}
		lines = append(lines, textline(hdr, hdrSt))
		// Body: the tool output as the model saw it (the tool layer bounds
		// it: bash 16KB head+tail per stream, 8MB combined kill cap). The
		// render window below keeps the resident line cache bounded (PRD
		// row budget): ponytail ceiling — beyond head+tail rows the full
		// text is only in the session JSONL, upgrade path is fold/expand.
		body := strings.TrimRight(b.Text, "\n")
		if body == "" {
			if !b.Err {
				lines = append(lines, textline("(no output)", a.mdStyle().muted))
			}
			break
		}
		const maxHeadRows, maxTailRows = 200, 50
		rows := wrap(body, max(10, w-2))
		draw := func(wl string) {
			lines = append(lines, textline("  "+wl, st))
		}
		if len(rows) > maxHeadRows+maxTailRows+1 {
			for _, wl := range rows[:maxHeadRows] {
				draw(wl)
			}
			lines = append(lines, textline(fmt.Sprintf("  … %d rows elided (full output in the session log) …", len(rows)-maxHeadRows-maxTailRows), a.mdStyle().muted))
			for _, wl := range rows[len(rows)-maxTailRows:] {
				draw(wl)
			}
		} else {
			for _, wl := range rows {
				draw(wl)
			}
		}
	case KindSystem:
		fg := theme.Gray
		if strings.Contains(strings.ToLower(b.Text), "error") || strings.Contains(strings.ToLower(b.Text), "canceled") {
			fg = theme.AccentError
		}
		lines = append(lines, textline(strings.TrimRight(b.Text, "\n"), tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(fg))).Italic(true)))
	case KindAssistant:
		for _, ln := range a.renderMarkdown(b.Text, w) {
			lines = append(lines, wrapLine(ln, w)...)
		}
		if b.stream && len(lines) > 0 {
			last := &lines[len(lines)-1]
			last.runs = append(last.runs, cell{text: "▍", style: a.mdStyle().muted})
		}
	default:
		for _, ln := range a.renderMarkdown(b.Text, w) {
			lines = append(lines, wrapLine(ln, w)...)
		}
	}
	a.lineCache[key] = lines
	return lines
}

// stThinkingHdr styles the thinking header: muted bold, per grok thinking.rs.
func stThinkingHdr(a *App, running bool) tcell.Style {
	fg := a.th.Get(theme.Gray)
	if running {
		fg = a.th.Get(theme.AccentThinking)
	}
	return tcell.StyleDefault.Foreground(a.cellColor(fg)).Bold(true)
}

// --- drawing ---

func (a *App) draw() {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.scr
	w, h := a.width, a.height
	s.Clear()

	// Empty transcript: the welcome screen (grok welcome/mod.rs — logo,
	// menu, shortcuts) instead of a blank void.
	if len(a.blocks) == 0 {
		a.drawWelcome(s, w, h)
		a.drawSlashDropdown(h - 3)
		a.drawComposer(h - 3)
		a.drawShortcuts(h - 1)
		s.Show()
		return
	}

	// Grok layout: scrollback rows [0, h-4), blank row, composer box (2
	// rows: input + info divider), shortcuts row at the bottom.
	vp := h - 4
	if vp < 1 {
		vp = 1
	}
	contentW := a.contentWidth()

	type row struct {
		ln    line
		rail  string
		railS tcell.Style
		ts    string // right-aligned timestamp (first row of user/assistant)
	}
	var rows []row
	for i := range a.blocks {
		b := a.blocks[i]
		lines := a.blockLines(i, b, contentW)
		var railCh string
		var railS tcell.Style
		switch b.Kind {
		case KindUser:
			railCh = "" // user rows carry their own ❯ band, no rail
		case KindThinking:
			railCh = "┃"
			railS = tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentThinking)))
		case KindTool, KindToolDone:
			railCh = "┃"
			railS = tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentTool)))
		case KindSystem:
			railCh = "┃"
			railS = tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentError)))
		default: // assistant
			railCh = "┃"
			railS = tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentAssistant)))
		}
		for j, ln := range lines {
			r := row{ln: ln, rail: railCh, railS: railS}
			if j == 0 && !b.Ts.IsZero() && (b.Kind == KindUser || b.Kind == KindAssistant) {
				r.ts = b.Ts.Format("3:04 PM")
			}
			rows = append(rows, r)
		}
		rows = append(rows, row{}) // separator
	}

	// Feed the new row count through the model every frame: while following
	// it stays pinned to the tail; while scrolled it preserves the user's
	// position against streaming output.
	a.sm.NewContent(len(rows), vp)
	start := a.sm.Start(len(rows), vp)
	end := start + vp
	if end > len(rows) {
		end = len(rows)
	}
	for y, r := range rows[start:end] {
		if r.ln.bg != 0 {
			// Band row (user prompt / code fence): fill the full width so
			// the band reads as one continuous row (grok semantic band).
			for bx := 0; bx < w; bx++ {
				s.SetContent(bx, y, ' ', nil, tcell.StyleDefault.Background(r.ln.bg))
			}
		}
		x := 3 // rail(1) + pad(2); user bands start their runs at x=0
		if r.ln.bg != 0 && len(r.ln.runs) > 0 && r.ln.runs[0].text == "❯ " {
			x = 0
		}
		if r.rail != "" {
			drawText(s, 0, y, r.rail, r.railS)
		}
		for _, run := range r.ln.runs {
			drawText(s, x, y, run.text, run.style)
			x += width(run.text)
		}
		// Right-aligned dim timestamp (grok draws these on first rows).
		if r.ts != "" {
			tsSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim)))
			if r.ln.bg != 0 {
				tsSt = tsSt.Background(r.ln.bg)
			}
			drawText(s, w-width(r.ts)-2, y, r.ts, tsSt)
		}
	}
	// Scroll indicator (grok-style ▲n ▼n): rows hidden above/below.
	if up, down := a.sm.Indicator(len(rows), vp); up > 0 || down > 0 {
		hint := fmt.Sprintf("▲ %d ▼ %d", up, down)
		drawText(s, w-len(hint)-1, 0, hint,
			tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.Gray))))
	}

	a.drawSlashDropdown(h - 3)
	a.drawComposer(h - 3)
	a.drawShortcuts(h - 1)
	s.Show()
}

// drawSlashDropdown renders the "/" autocomplete popup above the composer
// (grok slash_dropdown.rs): aligned name column + dim description, selected
// row highlighted. Rows sit on the composer's background so the popup reads
// as one surface with the prompt box.
func (a *App) drawSlashDropdown(yComposerTop int) {
	if a.smenu == nil || !a.smenu.active() {
		return
	}
	rows := a.smenu.rows()
	if len(rows) == 0 || yComposerTop < len(rows)+1 {
		return
	}
	w := a.width
	s := a.scr
	selName, _ := a.smenu.selected()
	sel := tcell.StyleDefault.Background(a.cellColor(a.th.Get(theme.BgHighlight)))
	rowBg := tcell.StyleDefault.Background(a.cellColor(a.th.Get(theme.BgBase)))

	// Aligned name column (grok: label cap 40, gap 2).
	nameW := 0
	for _, r := range rows {
		nameW = max(nameW, width(r.Name)+width(r.Tag))
	}
	nameW = min(nameW+2, 42)

	y := yComposerTop - len(rows) - 2 // popup = rows + 2 border rows
	drawText(s, 2, y, "╭"+strings.Repeat("─", min(w-4, nameW+44))+"╮",
		tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.PromptBorderActive))))
	y++
	for _, r := range rows {
		selected := r.Name == selName.Name
		st := rowBg
		if selected {
			st = sel
		}
		for x := 2; x < w-2; x++ {
			s.SetContent(x, y, ' ', nil, st)
		}
		nameCol := st.Foreground(a.cellColor(a.th.Get(theme.AccentUser)))
		drawText(s, 4, y, r.Name, nameCol)
		x := 4 + nameW
		drawText(s, x, y, r.Description, st.Foreground(a.cellColor(a.th.Get(theme.GrayDim))))
		if r.Tag != "" {
			drawText(s, w-6-width(r.Tag), y, "["+r.Tag+"]", st.Foreground(a.cellColor(a.th.Get(theme.AccentThinking))))
		}
		y++
	}
	drawText(s, 2, y, "╰"+strings.Repeat("─", min(w-4, nameW+44))+"╯",
		tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.PromptBorderActive))))
}

// drawComposer renders the grok prompt box: rounded border, ❯ prefix,
// editor text, blinking block cursor; model info line on the bottom border.
func (a *App) drawComposer(yTop int) {
	w := a.width
	if w < 6 || yTop < 1 {
		return
	}
	border := a.th.Get(theme.PromptBorderActive)
	bs := tcell.StyleDefault.Foreground(a.cellColor(border))
	ms := a.mdStyle()

	// Top border: ╭────╮ (1-cell inset on each side, like grok's box).
	drawText(a.scr, 1, yTop-1, "╭", bs)
	for x := 2; x < w-2; x++ {
		a.scr.SetContent(x, yTop-1, '─', nil, bs)
	}
	drawText(a.scr, w-2, yTop-1, "╮", bs)

	// Input row: │ ❯ text…│
	a.scr.SetContent(1, yTop, '│', nil, bs)
	a.scr.SetContent(w-2, yTop, '│', nil, bs)
	drawText(a.scr, 3, yTop, "❯ ", tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentUser))).Bold(true))

	text := a.ed.Text()
	cur := a.ed.cur
	avail := w - 7 // inner: border(1)+pad(1)+prefix(2)+right pad(2)+border(1)
	if avail < 2 {
		avail = 2
	}
	vis := []rune(text)
	if width(text) > avail {
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
	drawText(a.scr, 5, yTop, string(vis), ms.body)

	// Info divider bottom border: ╰─ model · ⠋ ───────────╯
	info := " " + a.st.Model
	if a.st.Running {
		a.st.spinnerIdx = a.st.spinnerIdx % len(spinnerFrames)
		info += " · " + spinnerFrames[a.st.spinnerIdx]
	}
	drawText(a.scr, 1, yTop+1, "╰", bs)
	for x := 2; x < w-2; x++ {
		a.scr.SetContent(x, yTop+1, '─', nil, bs)
	}
	if info != " " {
		drawText(a.scr, 2, yTop+1, info, tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim))))
	}
	drawText(a.scr, w-2, yTop+1, "╯", bs)

	// Cursor: blinking block at the editor position.
	cx := 5 + width(string(vis[:min(cur, len(vis))]))
	a.scr.ShowCursor(min(cx, w-3), yTop)
}

// drawShortcuts renders the bottom hint row: bold keys, gray labels,
// dim │ separators (grok shortcuts_bar.rs).
func (a *App) drawShortcuts(y int) {
	keyStyle := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.TextSecondary))).Bold(true)
	lblStyle := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.Gray)))
	sepStyle := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim)))

	type hint struct{ key, label string }
	hints := []hint{
		{"Enter", "send"},
		{"Esc", "cancel"},
		{"Ctrl+C", "quit"},
	}
	x := 2
	for i, hh := range hints {
		if i > 0 {
			drawText(a.scr, x, y, "  │  ", sepStyle)
			x += 5
		}
		drawText(a.scr, x, y, hh.key, keyStyle)
		x += width(hh.key)
		drawText(a.scr, x, y, ":", lblStyle)
		x++
		drawText(a.scr, x, y, hh.label, lblStyle)
		x += width(hh.label)
	}
	// Right-aligned token counter (status_line style: muted segments).
	if a.st.TokensIn > 0 || a.st.TokensOut > 0 {
		right := fmt.Sprintf("↑%s │ ↓%s", humanTokens(a.st.TokensIn), humanTokens(a.st.TokensOut))
		drawText(a.scr, a.width-width(right)-2, y, right, lblStyle)
	}
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
