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

	// showThinking renders model reasoning blocks in the transcript
	// (settings key `showThinking`, toggled by /settings; issue #20).
	showThinking bool

	width, height int

	// Wired by cmd: onSend runs the agent turn; onCancel aborts it; onQuit exits.
	ops               *SessionOps
	modelOps          *ModelOps                       // session lifecycle, wired by cmd (nil → notices)
	planOps           *PlanOps                        // /plan, wired by cmd (nil → notices)
	advisorOps        *AdvisorOps                     // /advisor, wired by cmd (nil → notices)
	memoryOps         *MemoryOps                      // /memory, wired by cmd (nil → notices)
	themeOps          *ThemeOps                       // /theme, wired by cmd (nil → notices)
	prewalkOps        *PrewalkOps                     // /prewalk, wired by cmd (nil → notices)
	spick             *sessionPicker                  // /resume selector (nil = closed)
	onPickerResume    func(id string)                 // wired by cmd: performs the resume
	onPickerSearch    func(query string) []PickerItem // wired by cmd: prompt-text matches (nil → local id+title filter)
	onPickerPinToggle func(id string)                 // wired by cmd: persists the pin sidecar
	onPickerDelete    func(id string) error           // wired by cmd: deletes JSONL + artifacts after confirmation
	tpick             *treeSelector                   // /tree selector (nil = closed)
	treeData          func() []TreeEntry              // entry snapshot, wired by cmd
	treeLabelLoad     func() map[string]string
	treeLabelSave     func(id, label string) error
	treeLabels        map[string]string // id→label snapshot, refreshed on open
	settingsOps       *SettingsOps      // /settings, wired by cmd (nil → notices)
	cwdLabel          string            // welcome top bar (last two path components)
	branch            string            // git branch for the welcome top bar ("" when none)
	commandDir        string            // markdown command discovery root
	pathRoot          string            // @-completion root (empty disables the menu)
	pathScan          func() []string   // shared FS-scan cache-backed file source
	extCommands       map[string]string // "/server:cmd" -> description
	extRun            ExtensionCommand
	renderers         map[string]RenderSpec   // tool name -> declarative render spec
	sessionBranch     func(args string) error // /branch to an entry id
	resumeList        func(cwd string) error  // /resume session listing
	onSend            func(text string)
	onCancel          func()
	onQuit            func()

	keyq      chan tcell.Event
	dirty     chan struct{}
	quitCh    chan struct{}
	lineCache map[blockKey][]line

	// Welcome-screen Game of Life backdrop (UI thread; guarded by mu).
	life       lifeGrid
	lifeTick   int
	sheenPhase int // welcome logo sheen sweep position (columns)
	// Mouse text selection: drag across transcript rows, release copies
	// to the clipboard. selRows is the last frame's rendered rows with
	// their screen origins, kept for hit-testing (UI thread; mu-guarded).
	selActive bool
	selAnchor selPoint
	selEnd    selPoint
	selRows   []selRow
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
		keyMap:       km,
		scr:          scr,
		th:           th,
		st:           Status{Model: model, SessionID: sessionID},
		showThinking: true,
		width:        w, height: h,
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

// SetStatusModel updates the status-line model name (wired by cmd on
// /model switch).
func (a *App) SetStatusModel(m string) {
	a.mu.Lock()
	a.st.Model = m
	a.mu.Unlock()
	a.poke()
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

// SetSessionBranch wires /branch to the store. The callback must rebuild
// history and replay the transcript (like swapStoreTo) so the user sees the
// new branch's content.
// SetPickerResume wires what Enter on a picker row does: cmd performs
// the actual store swap (same path as /resume <id>).
func (a *App) SetPickerResume(fn func(id string)) { a.onPickerResume = fn }

// SetPickerSearch wires prompt-text search: cmd returns ranked rows for
// the query (may include matches from session JSONL bodies); nil falls
// back to the picker's local id+title token filter.
func (a *App) SetPickerSearch(fn func(query string) []PickerItem) { a.onPickerSearch = fn }

// SetPickerPinToggle wires Ctrl+P in the picker (bare 'p' stays search
// text): cmd persists session-pins.json (nil → pin toggle is a no-op).
func (a *App) SetPickerPinToggle(fn func(id string)) { a.onPickerPinToggle = fn }

// SetPickerDelete wires the confirmed delete (Backspace-on-empty twice):
// cmd removes the session JSONL + artifacts (nil → delete disabled).
func (a *App) SetPickerDelete(fn func(id string) error) { a.onPickerDelete = fn }

func (a *App) SetResumeList(fn func(cwd string) error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resumeList = fn
}

func (a *App) SetSessionBranch(fn func(args string) error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessionBranch = fn
}

// BranchSession implements CommandAPI by switching the leaf pointer.
func (a *App) BranchSession(args string) error {
	if a.sessionBranch == nil {
		return fmt.Errorf("session branch not wired")
	}
	return a.sessionBranch(args)
}

// ListSessions implements CommandAPI /resume listing.
func (a *App) ListSessions(cwd string) error {
	if a.resumeList != nil {
		return a.resumeList(cwd)
	}
	return fmt.Errorf("no session listing available")
}

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
// With thinking display off the block is never created, so the transcript
// stays exactly as if the model had not reasoned at all.
func (a *App) BeginThinking() {
	a.mu.Lock()
	if !a.showThinking {
		a.mu.Unlock()
		return
	}
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
// SetModelOps wires the /model command (active state lives in cmd).
func (a *App) SetModelOps(ops *ModelOps) { a.modelOps = ops }

// SetPlanOps wires the /plan command (plan state lives in cmd).
func (a *App) SetPlanOps(ops *PlanOps) { a.planOps = ops }

// SetMemoryOps wires the /memory command (the backend lives in cmd).
func (a *App) SetMemoryOps(ops *MemoryOps) { a.memoryOps = ops }

// SetTheme swaps the palette live (custom-theme reload, /theme switch)
// and repaints.
func (a *App) SetTheme(th *theme.Theme) {
	if th == nil {
		return
	}
	a.mu.Lock()
	a.th = th
	a.lineCache = map[blockKey][]line{}
	a.mu.Unlock()
	a.poke()
}

// SetPrewalkOps wires the /prewalk command (the target lives in cmd).
func (a *App) SetPrewalkOps(ops *PrewalkOps) { a.prewalkOps = ops }

// Prewalk implements CommandAPI /prewalk: "", on, off, or "into <ref>".
func (a *App) Prewalk(args string) error {
	if a.prewalkOps == nil {
		return fmt.Errorf("prewalk not wired")
	}
	switch fields := strings.Fields(strings.TrimSpace(args)); {
	case len(fields) == 0, fields[0] == "on", fields[0] == "off", fields[0] == "into":
		on := len(fields) == 0 || fields[0] != "off"
		into := ""
		if len(fields) == 3 && fields[0] == "into" {
			into = fields[1] + " " + fields[2] // "into <ref>" joins back
		} else if len(fields) >= 2 && fields[0] == "into" {
			into = strings.Join(fields[1:], " ")
		}
		if a.prewalkOps.Set == nil {
			return fmt.Errorf("prewalk toggle not wired")
		}
		if err := a.prewalkOps.Set(on, into); err != nil {
			return err
		}
		a.AddSystemBlock(a.prewalkOps.Status())
	default:
		return fmt.Errorf("prewalk: use /prewalk, /prewalk on|off, or /prewalk into <ref>")
	}
	return nil
}

// SetThemeOps wires the /theme command (theme resolution lives in cmd).
func (a *App) SetThemeOps(ops *ThemeOps) { a.themeOps = ops }

// Theme implements CommandAPI /theme: list or switch.
func (a *App) Theme(args string) error {
	if a.themeOps == nil {
		return fmt.Errorf("theme switching not wired")
	}
	name := strings.TrimSpace(args)
	if name == "" {
		lines := []string{"active theme: " + a.themeOps.Current(), "available:"}
		if a.themeOps.List != nil {
			for _, t := range a.themeOps.List() {
				lines = append(lines, "  "+t)
			}
		}
		a.AddSystemBlock(strings.Join(lines, "\n"))
		return nil
	}
	if a.themeOps.Set == nil {
		return fmt.Errorf("theme switching not wired")
	}
	if err := a.themeOps.Set(name); err != nil {
		return err
	}
	a.AddSystemBlock("theme: " + name)
	return nil
}

// Memory implements CommandAPI /memory: view|stats|clear.
func (a *App) Memory(args string) error {
	if a.memoryOps == nil {
		return fmt.Errorf("memory not wired (set memory: local in settings)")
	}
	switch strings.TrimSpace(args) {
	case "", "view":
		if a.memoryOps.View == nil {
			return fmt.Errorf("memory view not wired")
		}
		a.AddSystemBlock(a.memoryOps.View())
	case "stats":
		if a.memoryOps.Stats == nil {
			return fmt.Errorf("memory stats not wired")
		}
		a.AddSystemBlock(a.memoryOps.Stats())
	case "clear":
		if a.memoryOps.Clear == nil {
			return fmt.Errorf("memory clear not wired")
		}
		if err := a.memoryOps.Clear(); err != nil {
			return err
		}
		a.AddSystemBlock("memory cleared")
	default:
		return fmt.Errorf("memory: use view|stats|clear")
	}
	return nil
}

// SetAdvisorOps wires the /advisor command (advisor state lives in cmd).
func (a *App) SetAdvisorOps(ops *AdvisorOps) { a.advisorOps = ops }

// Advisor implements CommandAPI /advisor: on|off|status|dump.
func (a *App) Advisor(args string) error {
	if a.advisorOps == nil {
		return fmt.Errorf("advisor not wired")
	}
	switch strings.TrimSpace(args) {
	case "on", "off":
		on := strings.TrimSpace(args) == "on"
		if a.advisorOps.Set == nil {
			return fmt.Errorf("advisor toggle not wired")
		}
		if err := a.advisorOps.Set(on); err != nil {
			return err
		}
		a.AddSystemBlock(fmt.Sprintf("advisor %s", strings.TrimSpace(args)))
	case "", "status":
		st := "unavailable"
		if a.advisorOps.Status != nil {
			st = a.advisorOps.Status()
		}
		a.AddSystemBlock("advisor: " + st)
	case "dump":
		if a.advisorOps.Dump == nil {
			return fmt.Errorf("advisor dump not wired")
		}
		a.AddSystemBlock(a.advisorOps.Dump())
	default:
		return fmt.Errorf("advisor: use on|off|status|dump")
	}
	return nil
}

// PlanMode implements CommandAPI /plan: no args toggles, "on"/"off" set
// explicitly, and every transition announces itself in the transcript.
func (a *App) PlanMode(args string) error {
	if a.planOps == nil || a.planOps.Get == nil || a.planOps.Set == nil {
		return fmt.Errorf("plan mode not wired")
	}
	arg := strings.TrimSpace(args)
	cur := a.planOps.Get()
	var on bool
	switch arg {
	case "":
		on = !cur
	case "on":
		on = true
	case "off":
		on = false
	default:
		return fmt.Errorf("plan: use /plan, /plan on, or /plan off")
	}
	if err := a.planOps.Set(on); err != nil {
		return err
	}
	if on {
		a.AddSystemBlock("plan mode ON — read-only research; call propose with the plan to exit")
	} else {
		a.AddSystemBlock("plan mode OFF — full toolset restored")
	}
	return nil
}

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

// SwitchModel implements CommandAPI /model: with no argument it prints the
// current model and the available refs; with an argument it switches.
func (a *App) SwitchModel(args string) error {
	if a.modelOps == nil {
		return fmt.Errorf("model switching not wired")
	}
	if strings.TrimSpace(args) == "" {
		cur := ""
		if a.modelOps.Current != nil {
			cur = a.modelOps.Current()
		}
		var lines []string
		if cur != "" {
			lines = append(lines, "active model: "+cur)
		}
		lines = append(lines, "available:")
		if a.modelOps.List != nil {
			for _, m := range a.modelOps.List() {
				lines = append(lines, "  "+m)
			}
		}
		a.AddSystemBlock(strings.Join(lines, "\n"))
		return nil
	}
	if a.modelOps.Set == nil {
		return fmt.Errorf("model switching not wired")
	}
	if err := a.modelOps.Set(strings.TrimSpace(args)); err != nil {
		return err
	}
	a.AddSystemBlock("active model: " + strings.TrimSpace(args))
	return nil
}

// SetShowThinking toggles transcript rendering of reasoning blocks and
// drops any already-rendered thinking blocks when turning it off, so the
// transcript matches what a session started with the flag off shows.
func (a *App) SetShowThinking(on bool) {
	a.mu.Lock()
	a.showThinking = on
	if !on {
		kept := a.blocks[:0]
		for _, b := range a.blocks {
			if b.Kind != KindThinking {
				kept = append(kept, b)
			}
		}
		a.blocks = kept
	}
	a.lineCache = map[blockKey][]line{}
	a.mu.Unlock()
	a.poke()
}

// SetSettingsOps wires the /settings command (settings live in cmd).
func (a *App) SetSettingsOps(ops *SettingsOps) { a.settingsOps = ops }

// Thinking reports whether reasoning output is currently displayed.
func (a *App) Thinking() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.showThinking
}

// Blocks snapshots the transcript blocks (read-only; for tests and hosts
// asserting on replayed transcript shape).
func (a *App) Blocks() []Block {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Block, len(a.blocks))
	for i, b := range a.blocks {
		out[i] = *b
	}
	return out
}

// SettingsView implements CommandAPI /settings: bare lists the resolved
// settings; "/settings showThinking [on|off]" turns the reasoning display
// (and only the display — the ":effort" budget still decides whether the
// model thinks) for the rest of the session and persists it to the global
// layer. Omitting on|off flips the current state.
func (a *App) SettingsView(args string) error {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		if a.settingsOps == nil {
			return fmt.Errorf("settings not wired")
		}
		var lines []string
		if a.settingsOps.List != nil {
			lines = a.settingsOps.List()
		}
		a.AddSystemBlock(strings.Join(lines, "\n"))
		return nil
	}
	if fields[0] != "showThinking" {
		return fmt.Errorf("unknown setting %q (want showThinking)", fields[0])
	}
	on := !a.Thinking()
	if len(fields) == 2 {
		switch fields[1] {
		case "on", "true":
			on = true
		case "off", "false":
			on = false
		default:
			return fmt.Errorf("usage: /settings showThinking on|off")
		}
	} else if len(fields) != 1 {
		return fmt.Errorf("usage: /settings showThinking [on|off]")
	}
	// The display flip is App-local state, so it works with unwired ops;
	// persisting needs the config layer, silently skipped when absent.
	confirm := "showThinking "
	if on {
		confirm += "on"
	} else {
		confirm += "off"
	}
	if a.settingsOps != nil && a.settingsOps.SetThinking != nil {
		if err := a.settingsOps.SetThinking(on); err != nil {
			return err
		}
		confirm += " (saved to " + a.settingsOps.Path + ")"
	}
	a.SetShowThinking(on)
	a.AddSystemBlock(confirm)
	return nil
}

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
			// Welcome animation. The logo sheen advances one column
			// per 33ms tick and redraws with it — a smooth sweep at
			// ~30fps, the cadence omarchy's own About animation runs
			// at (25ms frames) — while the welcome is visible (no
			// blocks, nothing running). The Life backdrop still steps
			// every 4th tick (~8fps), which is all it needs; both
			// stop once a block exists, so the only idle cost is the
			// draw itself.
			animate := false
			if !running && len(a.blocks) == 0 {
				gw, top, bot, ok := lifeArea(a.width, a.height)
				if ok {
					a.lifeTick = (a.lifeTick + 1) % 4
					a.sheenPhase++
					animate = true
					if a.lifeTick == 0 {
						a.stepLife(gw, top, bot)
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
			default:
				a.mu.Lock()
				a.handleMouse(m)
				a.mu.Unlock()
			}
		}
		return
	}
	a.mu.Lock()
	running := a.st.Running
	h := a.height
	menuOpen := a.smenu != nil && a.smenu.active()
	a.mu.Unlock()

	// The session picker owns navigation while open (Up/Down/Enter/Esc).
	if a.handlePickerKey(key) {
		return
	}
	// The tree selector is modal too: it owns every key while open
	// (filters, search, labels, Enter/Esc).
	if a.handleTreeKey(key) {
		return
	}

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
			return
		}
		// Double-escape rewind: Esc on an empty composer opens the tree
		// selector; a second Esc (handled by the selector) closes it.
		if strings.TrimSpace(a.ed.Text()) == "" {
			a.OpenTreeSelector()
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

	// Keymap-driven actions: the table /hotkeys prints is the table that
	// runs, so a remapped chord (keybindings.yml) works rather than being
	// decorative. Editor-adjacent actions are handled here; everything
	// else falls through to the editor's own key handling.
	if action := a.keyMap.Resolve(key); action != "" {
		switch action {
		case "submit":
			// Fall through to the editor: Enter also completes an open
			// slash menu, which only the editor path knows about.
		case "newline":
			// The editor is UI-thread-owned (same discipline as the
			// HandleKey path below): no lock, no cross-goroutine sharing.
			a.ed.insert('\n')
			a.poke()
			return
		case "clear-input":
			a.ed.Reset()
			a.poke()
			return
		case "cancel":
			if running {
				a.onCancel()
			}
			return
		case "app.session.tree":
			if running {
				a.onCancel()
				return
			}
			a.OpenTreeSelector()
			return
		case "quit":
			if running {
				a.onCancel()
				return
			}
			a.onQuit()
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
	// scrollback + blank + composer (grows with the draft) + shortcuts.
	return a.height - a.composerRows() - 2
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
		// "Thought for Xs" (done). Muted bold header; the reasoning body
		// renders dimmed underneath while showThinking is on (issue #20).
		if !a.showThinking {
			break
		}
		var hdr string
		if b.stream {
			hdr = "⠹ Thinking…"
		} else if b.thinkDur > 0 {
			hdr = fmt.Sprintf("Thought for %.1fs", b.thinkDur.Seconds())
		} else {
			hdr = "Thought"
		}
		lines = append(lines, textline(hdr, stThinkingHdr(a, b.stream)))
		body := strings.TrimRight(b.Text, "\n")
		if body == "" {
			break
		}
		// Row budget mirrors the tool-output window (PRD row budget): the
		// full reasoning always stays in the session JSONL.
		bodySt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim)))
		rows := wrap(body, max(10, w-4))
		const thinkHeadRows, thinkTailRows = 100, 40
		if len(rows) > thinkHeadRows+thinkTailRows+1 {
			for _, wl := range rows[:thinkHeadRows] {
				lines = append(lines, textline("  "+wl, bodySt))
			}
			lines = append(lines, textline(fmt.Sprintf("  … %d rows elided (full reasoning in the session log) …", len(rows)-thinkHeadRows-thinkTailRows), stThinkingHdr(a, false)))
			for _, wl := range rows[len(rows)-thinkTailRows:] {
				lines = append(lines, textline("  "+wl, bodySt))
			}
		} else {
			for _, wl := range rows {
				lines = append(lines, textline("  "+wl, bodySt))
			}
		}
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
		st := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(fg))).Italic(true)
		// Multi-line notices (/help, /model list, /settings) render one row
		// per line; embedded \n used to collapse into a single unreadable row.
		body := strings.TrimRight(b.Text, "\n")
		if body == "" {
			lines = append(lines, textline("", st))
			break
		}
		for _, wl := range wrap(body, max(10, w-2)) {
			lines = append(lines, textline(wl, st))
		}
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
		composerTop := h - 1 - a.composerRows()
		a.drawWelcome(s, w, h)
		a.drawSessionPicker(composerTop)
		a.drawTreeSelector(composerTop)
		a.drawSlashDropdown(composerTop)
		a.drawComposer(composerTop)
		a.drawShortcuts(h - 1)
		s.Show()
		return
	}

	// Grok layout: scrollback, blank row, composer box (grows with the
	// draft's wrapped line count), shortcuts row at the bottom.
	cRows := a.composerRows()
	vp := h - cRows - 2
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
	selRows := make([]selRow, 0, end-start)
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
		startX, content := x, strings.Builder{}
		for _, run := range r.ln.runs {
			drawText(s, x, y, run.text, run.style)
			content.WriteString(run.text)
			x += width(run.text)
		}
		// Selection hit-testing works off this text (the streaming
		// cursor is decoration, not content).
		selRows = append(selRows, selRow{text: strings.TrimSuffix(content.String(), "▍"), x0: startX})
		// Right-aligned dim timestamp (grok draws these on first rows).
		if r.ts != "" {
			tsSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim)))
			if r.ln.bg != 0 {
				tsSt = tsSt.Background(r.ln.bg)
			}
			drawText(s, w-width(r.ts)-2, y, r.ts, tsSt)
		}
	}
	a.selRows = selRows
	a.drawSelection()
	// Scroll indicator (grok-style ▲n ▼n): rows hidden above/below.
	if up, down := a.sm.Indicator(len(rows), vp); up > 0 || down > 0 {
		hint := fmt.Sprintf("▲ %d ▼ %d", up, down)
		drawText(s, w-len(hint)-1, 0, hint,
			tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.Gray))))
	}

	// The composer's first input row sits below the transcript; the box
	// occupies composerRows() rows above the shortcuts line.
	composerTop := h - 1 - cRows
	a.drawSessionPicker(composerTop)
	a.drawTreeSelector(composerTop)
	a.drawSlashDropdown(composerTop)
	a.drawComposer(composerTop)
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

// composerInputLines returns the wrapped input rows for the editor text
// plus the column of the cursor within that wrapped grid. An embedded
// newline is a hard row break (Ctrl+J / Alt+Enter), and a long line wraps
// at the available width so the box grows instead of truncating.
func (a *App) composerInputLines() (lines []string, curRow, curCol int) {
	text := a.ed.Text()
	cur := a.ed.cur
	avail := a.width - 7 // inner width: border+pad+prefix+right pad+border
	if avail < 4 {
		avail = 4
	}
	// Convert the rune-cursor into (row, col) while wrapping.
	row, col := 0, 0
	flush := func(line string) {
		lines = append(lines, line)
		row++
		col = 0
	}
	line := strings.Builder{}
	for i, r := range []rune(text) {
		if i == cur {
			curRow, curCol = row, col
		}
		switch {
		case r == '\n':
			flush(line.String())
			line.Reset()
		default:
			if col+width(string(r)) > avail {
				flush(line.String())
				line.Reset()
			}
			line.WriteRune(r)
			col += width(string(r))
		}
	}
	if cur >= len([]rune(text)) {
		curRow, curCol = row, col
	}
	flush(line.String())
	return lines, curRow, curCol
}

// composerRows is the total height of the prompt box (top border, wrapped
// input rows, bottom divider).
func (a *App) composerRows() int {
	lines, _, _ := a.composerInputLines()
	return len(lines) + 2
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

	// Input rows: │ ❯ first…│ then continuation rows aligned under the text.
	lines, curRow, curCol := a.composerInputLines()
	promptStyle := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentUser))).Bold(true)
	for i, ln := range lines {
		y := yTop + i
		a.scr.SetContent(1, y, '│', nil, bs)
		a.scr.SetContent(w-2, y, '│', nil, bs)
		if i == 0 {
			drawText(a.scr, 3, y, "❯ ", promptStyle)
		} else {
			drawText(a.scr, 3, y, "  ", promptStyle)
		}
		drawText(a.scr, 5, y, ln, ms.body)
	}
	// Blank the space between the last text row and the right border so a
	// short line cannot leave stale cells from a previous longer draft.
	for i, ln := range lines {
		x := 5 + width(ln)
		for ; x < w-2; x++ {
			a.scr.SetContent(x, yTop+i, ' ', nil, ms.body)
		}
	}

	// Info divider bottom border: ╰─ model · ⠋ ───────────╯
	yBottom := yTop + len(lines)
	info := " " + a.st.Model
	if a.st.Running {
		a.st.spinnerIdx = a.st.spinnerIdx % len(spinnerFrames)
		info += " · " + spinnerFrames[a.st.spinnerIdx]
	}
	drawText(a.scr, 1, yBottom, "╰", bs)
	for x := 2; x < w-2; x++ {
		a.scr.SetContent(x, yBottom, '─', nil, bs)
	}
	if info != " " {
		drawText(a.scr, 2, yBottom, info, tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim))))
	}
	drawText(a.scr, w-2, yBottom, "╯", bs)

	// Cursor: blinking block at the editor position inside the wrapped grid.
	cx := 5 + curCol
	a.scr.ShowCursor(min(cx, w-3), yTop+curRow)
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
		{"Ctrl+J", "newline"},
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
