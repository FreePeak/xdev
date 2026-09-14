package tui

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/theme"
)

// Status carries the status-line state. The running indicator's frames come
// from the theme (Symbols.SpinnerFrames → preset default, see
// theme.Theme.SpinnerFrames).
type Status struct {
	Model     string
	SessionID string
	TokensIn  int64
	TokensOut int64
	// Cost is the session spend in USD (0 when the provider reports none),
	// CtxWindow the model's context window (0 = unknown) and Rate the last
	// measured decode speed in output tokens/second (0 = never measured).
	// All three feed the optional HUD segments (statusLine.segments).
	Cost      float64
	CtxWindow int64
	// CtxUsed is the LIVE context occupancy: input+output of the most recent
	// completed request (the provider's own count, so it covers the system
	// prompt, the whole visible history and the tool schemas). The HUD's
	// context segment reads this against CtxWindow — deliberately NOT the
	// cumulative TokensIn/TokensOut, which count every turn the session ever
	// sent and so run far past the window.
	CtxUsed int64
	Rate    float64
	// Start anchors the HUD time segment: the moment the current session's
	// clock began (process start; cmd re-bases it on every session swap so
	// the segment shows total session time, not process uptime). Zero = the
	// segment hides.
	Start      time.Time
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
	// pickers is the modal-list stack: /model opens a roles+models
	// selector, and "set role" pushes a second list on top. Esc pops one
	// level; a selection pops them all. While non-empty the picker owns
	// every key event and the dropdown is closed.
	pickers []*picker
	keyMap  *KeyMap // remappable keybinding layer
	st      Status
	// The decode window of the message being streamed: the first and last
	// delta, and the runes between them. AddUsage closes the window and
	// turns it into st.Rate; starting a run discards an unfinished one.
	// Guarded by mu.
	deltaFirst, deltaLast time.Time
	deltaRunes            int64

	// statusSegs is the HUD segment order (settings statusLine.segments);
	// empty = defaultStatusSegments.
	statusSegs []string
	// ask is the blocking ask card (#46/#36); nil = closed.
	ask *askState

	// showThinking renders model reasoning blocks in the transcript
	// (settings key `showThinking`, toggled by /settings; issue #20).
	showThinking bool

	width, height int

	// Wired by cmd: onSend runs the agent turn; onCancel aborts it; onQuit exits.
	ops               *SessionOps
	modelOps          *ModelOps                              // session lifecycle, wired by cmd (nil → notices)
	planOps           *PlanOps                               // /plan, wired by cmd (nil → notices)
	advisorOps        *AdvisorOps                            // /advisor, wired by cmd (nil → notices)
	memoryOps         *MemoryOps                             // /memory, wired by cmd (nil → notices)
	themeOps          *ThemeOps                              // /theme, wired by cmd (nil → notices)
	prewalkOps        *PrewalkOps                            // /prewalk, wired by cmd (nil → notices)
	goalOps           *GoalOps                               // /goal, wired by cmd (nil → notices)
	vibeOps           *VibeOps                               // /vibe, wired by cmd (nil → notices)
	spick             *sessionPicker                         // /resume selector (nil = closed)
	onPickerResume    func(id string)                        // wired by cmd: performs the resume
	onPickerSearch    func(query string) []SessionPickerItem // wired by cmd: prompt-text matches (nil → local id+title filter)
	onPickerPinToggle func(id string)                        // wired by cmd: persists the pin sidecar
	onPickerDelete    func(id string) error                  // wired by cmd: deletes JSONL + artifacts after confirmation
	tpick             *treeSelector                          // /tree selector (nil = closed)
	treeData          func() []TreeEntry                     // entry snapshot, wired by cmd
	treeLabelLoad     func() map[string]string
	treeLabelSave     func(id, label string) error
	treeLabels        map[string]string // id→label snapshot, refreshed on open
	settingsOps       *SettingsOps      // /settings, wired by cmd (nil → notices)
	cwd               string            // working directory (the status row's left side)
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

	keyq   chan tcell.Event
	dirty  chan struct{}
	quitCh chan struct{}

	// UI-loop stall detection (stall.go). loopBeat is written from the loop
	// and read by the watchdog, so it is atomic rather than mutex-guarded: a
	// watchdog that took App.mu could not report a loop stuck holding it.
	loopBeat atomic.Int64
	stallDir string
	// rowIdx is the transcript's row layout and per-block render cache; the
	// per-frame cost is the viewport, not the session (see rowindex.go).
	rowIdx rowIndex

	// Welcome-screen Game of Life backdrop (UI thread; guarded by mu).
	life       lifeGrid
	lifeTick   int
	sheenPhase int // welcome logo sheen sweep position (columns)
	// Mouse text selection: drag anywhere, release copies to the clipboard.
	// selDown is the held button (a drag in flight); selShown keeps the
	// highlight up after the release until the next click, like a terminal's
	// own selection. Corners of a transcript gesture carry the document row
	// under the pointer (selTop is the viewport's first row, kept so the drag
	// can re-anchor after a scroll), and selCache holds the text of rows that
	// have since scrolled out of sight — that is what lets one drag cover more
	// than a screen. selRows is the last frame's transcript rows with their
	// screen origins; rows outside that capture are read back from the grid.
	// (UI thread; mu-guarded.)
	selDown    bool
	selShown   bool
	selDocMode bool
	selTop     int
	selAnchor  selCorner
	selEnd     selCorner
	selRows    []selRow
	selCache   map[int]selRow
	// selNotice is the copy confirmation (omp's showStatus for a copy); it
	// rides the composer divider until selNoticeUntil.
	selNotice      string
	selNoticeUntil time.Time
	// scrollHint is the ▲n▼n viewport hint, drawn on the composer's info
	// divider — never on row 0, where it overwrote scrolled-to content.
	scrollHint string
}

type blockKey struct {
	idx      int
	kind     BlockKind
	width    int
	tlen     int
	tool     string
	status   string
	stream   bool
	expanded bool // result box: the Ctrl+O state changed the row set
	age      int64
	trim     int8 // bounded middle trim: this block's render-window tier
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
		st:           Status{Model: model, SessionID: sessionID, Start: time.Now()},
		showThinking: true,
		width:        w, height: h,
		keyq:   make(chan tcell.Event, 64),
		dirty:  make(chan struct{}, 1),
		quitCh: make(chan struct{}),
		sm:     newScrollModel(),
	}
}

// SetLocation wires the working directory: the status row's left side, and
// the welcome top-bar location (cwd + git branch).
func (a *App) SetLocation(cwd string) {
	a.mu.Lock()
	a.cwd = cwd
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
	a.clearRenderCache()
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
func (a *App) SetPickerSearch(fn func(query string) []SessionPickerItem) { a.onPickerSearch = fn }

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
	a.noteDelta(delta)
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
	a.noteDelta(delta)
	a.mu.Unlock()
	a.poke()
}

// noteDelta extends the decode window with one streamed delta. Callers hold
// a.mu. Runes are counted for the live estimate only; the settled rate is the
// provider's own token count over the same window.
func (a *App) noteDelta(delta string) {
	now := time.Now()
	if a.deltaFirst.IsZero() {
		a.deltaFirst = now
	}
	a.deltaLast = now
	a.deltaRunes += int64(len([]rune(delta)))
}

// liveRate estimates the rate while a message is still streaming: the runes
// received so far, at four to a token, over the window they arrived in. It is
// replaced by the measured st.Rate the moment usage lands. Callers hold a.mu.
func (a *App) liveRate() float64 {
	window := a.deltaLast.Sub(a.deltaFirst)
	if window < 100*time.Millisecond || a.deltaRunes == 0 {
		return 0
	}
	return float64(a.deltaRunes/4) / window.Seconds()
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

// ToolOutcome is what the renderer needs from a finished tool call besides its
// text: the wall time plus the two structured facts a status footer shows —
// how the process ended, and whether the tool dropped output the model never
// saw. cmd flattens tool-specific Details into it so the transcript never has
// to type-switch over another package's payload.
type ToolOutcome struct {
	Dur       string // formatted wall time, e.g. "70ms" ("" = unknown)
	Exit      int    // process exit code; read only when HasExit
	HasExit   bool
	Truncated bool
}

// AddToolBlock appends one tool-call row in the running state, carrying the
// call's raw JSON arguments: the renderer reads the naming argument out of
// them (omp's `name · detail`), so no flattened preview is baked in here.
func (a *App) AddToolBlock(name, rawArgs string) {
	a.mu.Lock()
	a.blocks = append(a.blocks, &Block{
		Kind: KindTool, ToolName: name, Text: rawArgs,
		Status: "running", Ts: time.Now(),
	})
	a.mu.Unlock()
	a.poke()
}

// FinishTool marks the last running tool block done (ok/error) and appends
// the tool-result block carrying the full (sink-windowed) output.
func (a *App) FinishTool(name string, isErr bool, output string, out ToolOutcome) {
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
	// omp keeps the outcome in the footer rather than in the body: the
	// "[exit code N]" the tool appends for the model is dropped from the
	// render once the footer reports the same number.
	if out.HasExit && out.Exit != 0 {
		text = strings.TrimSuffix(text, fmt.Sprintf("\n[exit code %d]", out.Exit))
	}
	a.blocks = append(a.blocks, &Block{
		Kind: KindToolDone, ToolName: name, Text: text,
		Dur: out.Dur, Err: isErr, Exit: out.Exit, HasExit: out.HasExit,
		Truncated: out.Truncated,
	})
	a.mu.Unlock()
	a.poke()
}

// ToggleToolExpand flips the Ctrl+O state of the newest tool result, the one
// the user is looking at on a tail-following transcript. It reports whether
// there was a result to toggle, so the caller can stay silent instead of
// claiming to have expanded an empty transcript.
func (a *App) ToggleToolExpand() bool {
	a.mu.Lock()
	var found bool
	for i := len(a.blocks) - 1; i >= 0; i-- {
		if b := a.blocks[i]; b.Kind == KindToolDone {
			b.Expanded = !b.Expanded
			found = true
			break
		}
	}
	a.mu.Unlock()
	if found {
		a.poke()
	}
	return found
}

// AddUsage folds token usage into the status line and measures the decode
// rate the HUD's rate segment shows: the provider's own output-token count
// over the window in which deltas actually arrived (omp's per-message math,
// the same rule internal/dist/bench.go measures with). A message with no
// usable window — nothing streamed, or a sub-100ms burst — keeps the previous
// rate rather than inventing one.
// It also refreshes CtxUsed, the live occupancy behind the HUD's context
// segment.
func (a *App) AddUsage(in, out int64) {
	a.mu.Lock()
	a.st.TokensIn += in
	a.st.TokensOut += out
	a.st.CtxUsed = in + out
	if window := a.deltaLast.Sub(a.deltaFirst); out > 1 && window >= 100*time.Millisecond {
		a.st.Rate = float64(out) / window.Seconds()
	}
	a.deltaFirst, a.deltaLast, a.deltaRunes = time.Time{}, time.Time{}, 0
	a.mu.Unlock()
}

// AddCost folds provider-reported spend (USD) into the HUD cost segment.
func (a *App) AddCost(usd float64) {
	a.mu.Lock()
	a.st.Cost += usd
	a.mu.Unlock()
}

// SetContextWindow records the model's context window for the HUD context
// segment (0 = unknown: the segment hides).
func (a *App) SetContextWindow(tokens int64) {
	a.mu.Lock()
	a.st.CtxWindow = tokens
	a.mu.Unlock()
	a.poke()
}

// SetSessionStart re-anchors the HUD time segment. Wired by cmd on session
// swaps (/new, /drop, /resume, fork) so the clock follows the session, not
// the process.
func (a *App) SetSessionStart(t time.Time) {
	a.mu.Lock()
	a.st.Start = t
	a.mu.Unlock()
	a.poke()
}

// SetStatusSegments configures the HUD (settings statusLine.segments): the
// segment names to render, in order. Unknown names are skipped with a
// warning; nil/empty restores the shipped layout.
func (a *App) SetStatusSegments(segs []string) {
	known := make([]string, 0, len(segs))
	var unknown []string
	for _, s := range segs {
		name := strings.ToLower(strings.TrimSpace(s))
		if name == "" {
			continue
		}
		if _, ok := statusSegments[name]; ok {
			known = append(known, name)
			continue
		}
		unknown = append(unknown, name)
	}
	if len(unknown) > 0 {
		logx.Warnf("tui: statusLine.segments: unknown segment(s) %s skipped (known: %s)",
			strings.Join(unknown, ", "), strings.Join(statusSegmentNames(), ", "))
	}
	a.mu.Lock()
	a.statusSegs = known
	a.mu.Unlock()
	a.poke()
}

// SetRunning toggles the spinner state. Starting a run also discards a decode
// window the last one never closed — an aborted stream would otherwise make
// the next rate divide new tokens by old elapsed time.
func (a *App) SetRunning(r bool) {
	a.mu.Lock()
	a.st.Running = r
	if r {
		a.deltaFirst, a.deltaLast, a.deltaRunes = time.Time{}, time.Time{}, 0
	}
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

// RenameSession implements CommandAPI /rename: a manual title beats any
// generated one and survives the ai-title pass (the slot records the source).
func (a *App) RenameSession(title string) error {
	if a.ops == nil || a.ops.Rename == nil {
		return fmt.Errorf("session rename not wired")
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return fmt.Errorf("usage: /rename <new title>")
	}
	if err := a.ops.Rename(title); err != nil {
		return err
	}
	a.AddSystemBlock("· session renamed: " + title)
	return nil
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
	a.clearRenderCache()
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

// SetGoalOps wires the /goal command (the goal state lives in cmd).
func (a *App) SetGoalOps(ops *GoalOps) { a.goalOps = ops }

// Goal implements CommandAPI /goal: a bare /goal (or /goal view) shows the
// current goal and budget; the verbs create/resume/evidence/complete/drop
// drive the same state the goal tool owns, so an interactive session steers
// its own objective without a model turn. The args used to be dropped, which
// made `/goal create …` look like a dead command.
func (a *App) Goal(args string) error {
	block, err := a.goalOps.Dispatch(args)
	if err != nil {
		return err
	}
	a.AddSystemBlock(block)
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
	// "/theme list" is the same query as bare "/theme": the listing verb is
	// the obvious thing a user types, and erroring on it (while "list" is not
	// a theme name) is a dead end.
	if name == "" || strings.EqualFold(name, "list") {
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

// Memory implements CommandAPI /memory: view|stats|clear plus the
// backend-specific verbs (queue|sync|enqueue for the mnemopi store, diagnose
// for the remote Hindsight backend). The grammar lives in MemoryOps.Dispatch
// so every backend answers the same verbs through one place.
func (a *App) Memory(args string) error {
	block, err := a.memoryOps.Dispatch(args)
	if err != nil {
		return err
	}
	a.AddSystemBlock(block)
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

// ResumeSession implements CommandAPI /resume. With no argument it opens the
// session picker (the recent sessions for this directory); with a query it
// resolves immediately, so `/resume <id-prefix>` still works headlessly.
func (a *App) ResumeSession(query string) error {
	if a.ops == nil || a.ops.Resume == nil {
		return fmt.Errorf("session resume not wired")
	}
	if q := strings.TrimSpace(query); q != "" {
		return a.ops.Resume(q)
	}
	if a.ops.Recent == nil {
		return a.ops.Resume("")
	}
	opts := a.ops.Recent()
	if len(opts) == 0 {
		a.AddSystemBlock("no other sessions in this directory")
		return nil
	}
	items := make([]PickerItem, 0, len(opts))
	for _, o := range opts {
		items = append(items, PickerItem{
			Label: o.Title, Detail: o.Detail, Value: o.ID, Current: o.Current,
		})
	}
	resume := a.ops.Resume
	a.OpenPicker(PickerOptions{
		Title: "resume session",
		Views: []PickerView{{Name: "recent", Items: items, Action: "resume"}},
		OnSelect: func(id string) {
			if err := resume(id); err != nil {
				a.AddSystemBlock("error: " + err.Error())
			}
		},
	})
	return nil
}

// Reset clears the transcript (used by /clear): all blocks gone, viewport
// back to follow. Streaming state is untouched — callers must not be
// running a turn when they call this.
func (a *App) Reset() {
	a.mu.Lock()
	a.blocks = nil
	a.sm = newScrollModel()
	a.clearRenderCache()
	a.mu.Unlock()
	a.poke()
}

// SetSessionOps wires the session lifecycle (store lives in cmd). Nil ops
// degrade the /new /clear /drop commands to notices.
func (a *App) SetSessionOps(ops *SessionOps) { a.ops = ops }

// cycleModel advances the active model through the configured cycle
// patterns. It reports whether the chord was claimed: false means cycling is
// not wired or has nowhere to go, so the chord keeps its other meaning
// (menu-prev) instead of silently doing nothing.
func (a *App) cycleModel() bool {
	if a.modelOps == nil || a.modelOps.Cycle == nil {
		return false
	}
	next, ok := a.modelOps.Cycle()
	if !ok {
		return false
	}
	a.AddSystemBlock("active model: " + next)
	return true
}

// SwitchModel implements CommandAPI /model: with no argument it opens the
// interactive selector (roles + models); with an argument it switches
// directly, accepting anything the -model flag accepts (a concrete
// provider/model, a bare model id, or an @role[:effort] alias).
func (a *App) SwitchModel(args string) error {
	if a.modelOps == nil {
		return fmt.Errorf("model switching not wired")
	}
	if strings.TrimSpace(args) == "" {
		a.OpenModelPicker()
		return nil
	}
	if a.modelOps.Set == nil {
		return fmt.Errorf("model switching not wired")
	}
	ref := strings.TrimSpace(args)
	if err := a.modelOps.Set(ref); err != nil {
		return err
	}
	// Echo what is actually live, not what was typed: "/model @slow"
	// switching to onegw/dev must not claim the session runs "@slow".
	echo := ref
	if a.modelOps.Current != nil {
		if cur := a.modelOps.Current(); cur != "" {
			echo = cur
		}
	}
	a.AddSystemBlock("active model: " + echo)
	return nil
}

// --- modal picker stack ---

// OpenPicker pushes one modal list. The stack's top owns the keyboard until
// Esc pops it or a selection clears the whole stack.
func (a *App) OpenPicker(opts PickerOptions) {
	a.mu.Lock()
	a.pickers = append(a.pickers, newPicker(opts))
	a.smenu = nil // one modal at a time: the dropdown would draw under it
	a.mu.Unlock()
	a.poke()
}

// popPicker removes the top picker (Esc).
func (a *App) popPicker() {
	a.mu.Lock()
	if n := len(a.pickers); n > 0 {
		a.pickers = a.pickers[:n-1]
	}
	a.mu.Unlock()
	a.poke()
}

// closePickers clears the stack (a selection happened).
func (a *App) closePickers() {
	a.mu.Lock()
	a.pickers = nil
	a.mu.Unlock()
	a.poke()
}

// PickerOpen reports whether a modal list is showing (hosts and tests).
func (a *App) PickerOpen() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.pickers) > 0
}

// OpenModelPicker opens the /model selector: one view per provider's
// concrete models plus a roles view whose rows assign a model to a role.
// Also bound to Alt+M (omp's app.model.select).
func (a *App) OpenModelPicker() {
	if a.modelOps == nil || a.modelOps.Views == nil {
		cur := ""
		if a.modelOps != nil && a.modelOps.Current != nil {
			cur = a.modelOps.Current()
		}
		a.AddSystemBlock("active model: " + cur)
		return
	}
	views := a.modelOps.Views()
	for i := range views {
		if views[i].Action == "" {
			views[i].Action = "use"
		}
	}
	// A selector with no rows is the same dead end as an invisible one: with
	// nothing configured (or discovery dead) the panel would own the keyboard
	// over an empty list, so the answer lands in the transcript instead.
	rows := 0
	for i := range views {
		rows += len(views[i].Items)
	}
	if rows == 0 {
		cur := ""
		if a.modelOps.Current != nil {
			cur = a.modelOps.Current()
		}
		a.AddSystemBlock("no models to select — check ~/.xdev/agent/models.yml (active: " + cur + ")")
		return
	}
	// Model rows switch the session; a roles row overrides OnSelect to
	// assign instead (cmd wires that view). Routed through SwitchModel so a
	// pick reports the model that is actually live: the old callback called
	// Set and stayed silent, which read as "Enter did nothing" whenever the
	// panel had just closed. Errors still land in the transcript because the
	// picker is already gone by then.
	use := func(ref string) {
		if strings.TrimSpace(ref) == "" {
			return
		}
		if err := a.SwitchModel(ref); err != nil {
			a.AddSystemBlock("error: " + err.Error())
		}
	}
	a.OpenPicker(PickerOptions{Title: "model", Views: views, OnSelect: use})
}

// OpenRolePicker pushes the model list that assigns @role: Enter persists
// modelRoles.<role> to the global settings layer and switches this session
// to it, so the choice is visible immediately. Exported because cmd builds
// the roles view and points its Enter here.
func (a *App) OpenRolePicker(role string) {
	if a.modelOps == nil || a.modelOps.Models == nil || a.modelOps.SetRole == nil {
		a.AddSystemBlock("role assignment not wired")
		return
	}
	models := a.modelOps.Models()
	if len(models) == 0 {
		a.AddSystemBlock("no models configured — check ~/.xdev/agent/models.yml")
		return
	}
	a.OpenPicker(PickerOptions{
		Title:    "set @" + role + " to",
		Views:    []PickerView{{Name: "models", Action: "set", Items: models}},
		OnSelect: func(ref string) { a.setRole(role, ref) },
	})
}

// setRole persists modelRoles.<role> and, on success, switches this session
// to the model so the choice is immediately visible.
func (a *App) setRole(role, ref string) {
	if err := a.modelOps.SetRole(role, ref); err != nil {
		a.AddSystemBlock("error: " + err.Error())
		return
	}
	if a.modelOps.Set != nil {
		if err := a.modelOps.Set("@" + role); err != nil {
			a.AddSystemBlock("error: " + err.Error())
			return
		}
	}
	a.AddSystemBlock("@" + role + " → " + ref)
}

// handlePickerKey routes one key to the top picker. It reports whether the
// event was consumed: while a picker is open the editor, scrolling, and the
// quit chords are all inert, so a stray key can never leak into the
// transcript behind the panel.
func (a *App) handlePickerKey(key *tcell.EventKey) bool {
	a.mu.Lock()
	if len(a.pickers) == 0 {
		a.mu.Unlock()
		return false
	}
	p := a.pickers[len(a.pickers)-1]
	a.mu.Unlock()

	switch key.Key() {
	case tcell.KeyUp, tcell.KeyCtrlP:
		a.pickerMutate(func(p *picker) { p.move(-1) })
	case tcell.KeyDown, tcell.KeyCtrlN:
		a.pickerMutate(func(p *picker) { p.move(1) })
	case tcell.KeyPgUp:
		a.pickerMutate(func(p *picker) { p.move(-p.visible) })
	case tcell.KeyPgDn:
		a.pickerMutate(func(p *picker) { p.move(p.visible) })
	case tcell.KeyRight, tcell.KeyTab:
		a.pickerMutate(func(p *picker) { p.switchView(1) })
	case tcell.KeyLeft, tcell.KeyBacktab:
		a.pickerMutate(func(p *picker) { p.switchView(-1) })
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		a.pickerMutate(func(p *picker) { p.backspace() })
	case tcell.KeyEnter:
		act, ok := p.choose()
		if !ok {
			return true
		}
		it, _ := p.selected()
		a.closePickers()
		act(it.Value)
	case tcell.KeyEsc, tcell.KeyCtrlC, tcell.KeyCtrlD:
		a.popPicker()
	case tcell.KeyRune:
		if r := key.Rune(); r != ' ' {
			a.pickerMutate(func(p *picker) { p.typeFilter(r) })
		}
	}
	return true
}

// pickerMutate applies fn to the top picker and repaints.
func (a *App) pickerMutate(fn func(*picker)) {
	a.mu.Lock()
	if n := len(a.pickers); n > 0 {
		fn(a.pickers[n-1])
	}
	a.mu.Unlock()
	a.poke()
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
	a.clearRenderCache()
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
	ticks := 0
	a.beat()
	a.startStallWatchdog()

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
		a.beat()
		select {
		case <-a.quitCh:
			return
		case ev := <-a.keyq:
			a.handleKey(ev)
			a.draw()
		case <-a.dirty:
			a.draw()
		case <-tick.C:
			ticks++
			a.mu.Lock()
			running := a.st.Running
			if running {
				a.st.spinnerIdx = (a.st.spinnerIdx + 1) % len(a.th.SpinnerFrames())
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
			// A copy confirmation is timed, and an idle UI does not repaint:
			// the tick that finds it expired asks for the draw that drops it.
			if a.selNotice != "" && a.copyHint() == "" {
				animate = true
			}
			clock := a.hudHasClock()
			a.mu.Unlock()
			if running || animate {
				a.draw()
			} else if clock && ticks%30 == 0 {
				// The session clock must keep counting while the UI is
				// otherwise idle: repaint once a second (33ms × 30).
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
			a.clearRenderCache()
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
	menuOpen := a.smenu != nil && a.smenu.active()
	a.mu.Unlock()
	// The ask card (#46) is the topmost modal: it blocks the composer and
	// owns every key until it is answered or skipped.
	if a.handleAskKey(key) {
		return
	}
	// The hub roster owns navigation while open.
	if a.handleHubRosterKey(key) {
		return
	}
	// The session picker owns navigation while open (Up/Down/Enter/Esc).
	if a.handleSessionPickerKey(key) {
		return
	}
	if a.handlePickerKey(key) {
		return
	}
	// The tree selector is modal too: it owns every key while open
	// (filters, search, labels, Enter/Esc).
	if a.handleTreeKey(key) {
		return
	}

	// Claude-Code double-Esc rewind: idle with a draft in the composer,
	// the first Esc clears the draft; the next Esc (empty composer) opens
	// the tree selector, where a user row is rewind-and-re-prime. An open
	// slash/@-menu owns Esc first (close the menu), and the selector's own
	// Esc handling is modal.
	if key.Key() == tcell.KeyEsc && !running && !menuOpen {
		if strings.TrimSpace(a.ed.Text()) != "" {
			a.ed.Reset()
			a.poke()
			return
		}
		a.OpenTreeSelector()
		return
	}

	// Keymap-driven actions: the table /hotkeys prints is the table that
	// runs. Editor-adjacent actions are handled here; everything else is
	// decorative.
	action := a.keyMap.Resolve(key)

	// Slash dropdown owns navigation while open (grok slash_dropdown).
	if menuOpen {
		switch action {
		case "menu-accept":
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
		case "cancel": // close without changing the text
			a.mu.Lock()
			a.smenu = nil
			a.mu.Unlock()
			a.poke()
			return
		case "menu-prev", "menu-next", "model-cycle":
			// model-cycle rides the dropdown first: with the slash menu
			// open, Ctrl+P means "previous entry", which is what omp does
			// there too.
			delta := -1
			if action == "menu-next" {
				delta = 1
			}
			a.mu.Lock()
			a.smenu.move(delta)
			a.mu.Unlock()
			a.poke()
			return
		}
	}

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
		// Esc aborts a running turn; idle it is a no-op (the dropdown,
		// when open, was already closed by the menu branch above).
		if running {
			a.onCancel()
		}
		return
	case "model-select":
		a.OpenModelPicker()
		return
	case "app.agents.hub":
		// The same path /hub takes; an unwired hub says so in the
		// transcript rather than swallowing the chord.
		_ = a.HubRoster()
		return
	case "model-cycle":
		// Unwired cycling is a no-op, not an error: the chord is bound by
		// default and most sessions configure no pattern list.
		a.cycleModel()
		return
	case "quit":
		if running {
			a.onCancel()
			return
		}
		a.onQuit()
		return
	case "scroll-up":
		a.scroll(1, false)
		return
	case "scroll-down":
		a.scroll(1, true)
		return
	case "scroll-page-up":
		a.scrollPage(false)
		return
	case "scroll-page-down":
		a.scrollPage(true)
		return
	case "scroll-top":
		a.scrollTo(false)
		return
	case "scroll-bottom":
		a.scrollTo(true)
		return
	case "redraw":
		a.Invalidate()
		return
	case "expand":
		// omp's ctrl+o. No result to reveal is silence, not a notice: the
		// transcript must not claim to have expanded something.
		a.ToggleToolExpand()
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

	// Empty editor with nothing to recall: arrows scroll the transcript.
	// Once history exists, Up/Down belong to the editor (omp/Claude Code:
	// Up recalls the previous prompt whether or not the box is empty);
	// scrolling stays on Shift+arrow, PgUp/PgDn and the mouse wheel.
	if !running && strings.TrimSpace(a.ed.Text()) == "" && !menuOpen && !a.ed.HasHistory() {
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

// scrollPage moves the viewport a full page toward older (down=false) or
// newer (down=true) rows, keeping one line of overlap so context survives the
// jump (omp's ScrollView.page scrolls height-1, not half a screen — the old
// h/2 step needed two presses to clear one viewport and felt sluggish).
func (a *App) scrollPage(down bool) {
	a.mu.Lock()
	vp := normVP(a.viewportLinesLocked())
	total := a.totalLinesLocked()
	n := max(1, vp-1)
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

// totalLinesLocked is the transcript's row count. The row index keeps the
// running total, so a scroll key costs a stamp scan over blocks rather than a
// re-render of every row in the session.
func (a *App) totalLinesLocked() int {
	return int(a.sync(a.contentWidth()))
}

func (a *App) viewportLinesLocked() int {
	// scrollback + blank + composer (grows with the draft) + status row.
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

// blockLines returns block i's styled visual lines, rendering them only when
// the block's stamp moved. The render lives in the row index — one entry per
// block, a superseded render replaced in place rather than kept beside it, so
// a long session's memory stays flat while a turn streams.
func (a *App) blockLines(i int, b *Block, w int) []line {
	x := &a.rowIdx
	if x.w != w {
		x.w = w
		x.reset()
	}
	key := a.renderKey(i, b, w)
	if i < len(x.rend) && x.rend[i].key == key {
		return x.rend[i].lines
	}
	if i >= len(x.rend) {
		x.grow(i + 1)
	}
	x.markDirty(i)
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
		// Bounded middle trim: the newest thinking blocks keep the full render
		// window (PRD row budget); aged ones collapse to a head slice plus the
		// elided-row count. The full reasoning always stays in the session JSONL.
		bodySt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim)))
		rows := wrap(body, max(10, w-4))
		headRows, tailRows := thinkWindow(a.trimTier(i))
		if len(rows) > headRows+tailRows+1 {
			for _, wl := range rows[:headRows] {
				lines = append(lines, textline("  "+wl, bodySt))
			}
			lines = append(lines, textline(fmt.Sprintf("  … %d rows elided (full reasoning in the session log) …", len(rows)-headRows-tailRows), stThinkingHdr(a, false)))
			for _, wl := range rows[len(rows)-tailRows:] {
				lines = append(lines, textline("  "+wl, bodySt))
			}
		} else {
			for _, wl := range rows {
				lines = append(lines, textline("  "+wl, bodySt))
			}
		}
	case KindTool:
		// omp's call row: state bullet, bold tool name, and the naming
		// argument as a phrase — never the raw JSON the model sent. While the
		// call is in flight the bullet spins and the elapsed ticks; the
		// settled wall time belongs to the result frame's footer.
		name, detail := toolSummary(b, w)
		bullet, fg := "◈", theme.AccentTool
		switch b.Status {
		case "running":
			frames := a.th.SpinnerFrames()
			bullet, fg = frames[a.st.spinnerIdx%len(frames)], theme.AccentRunning
		case "error":
			bullet, fg = "✗", theme.AccentError
		case "ok":
			bullet, fg = "●", theme.AccentSuccess
		}
		ln := textline(bullet+" ", tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(fg))))
		ln.runs = append(ln.runs, cell{
			text:  name,
			style: tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.TextSecondary))).Bold(true),
		})
		if detail != "" {
			ln.runs = append(ln.runs, cell{
				text:  " · " + detail,
				style: tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.Gray))),
			})
		}
		if b.Status == "running" && !b.Ts.IsZero() {
			ln.runs = append(ln.runs, cell{
				text:  "  " + humanDur(time.Since(b.Ts)),
				style: tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim))),
			})
		}
		lines = append(lines, ln)
	case KindToolDone:
		lines = append(lines, a.toolBoxLines(i, b, w)...)
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
	x.rend[i] = blockRend{key: key, lines: lines, rows: int32(len(lines)) + 1}
	return lines
}

// toolBoxLines renders one finished tool result in omp's frame: a rounded box
// of output, closed by footer rows that carry the outcome — the wall time and
// exit code omp prints as `⟦Wall: 0.07s | Exit: 9⟧`, the truncation warning,
// and the hidden-row notice with its Ctrl+O affordance. The top border names
// the tool only when no call row above already did. Errors tint the frame.
// The result block carries no rail (draw), so the border is the line.
func (a *App) toolBoxLines(i int, b *Block, w int) []line {
	borderCol, bodyCol := theme.AccentTool, theme.TextSecondary
	if b.Err {
		borderCol, bodyCol = theme.AccentError, theme.AccentError
	}
	box := a.th.Box()
	border := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(borderCol)))
	bodySt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(bodyCol)))
	dimSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim)))
	mutedSt := a.mdStyle().muted

	inner := max(1, w-4) // side borders + one pad cell each

	// The call row that omp folds into this box is a block of its own here, so
	// the label is only needed when this result stands alone.
	label := ""
	if b.ToolName != "" && (i == 0 || a.blocks[i-1].Kind != KindTool || a.blocks[i-1].ToolName != b.ToolName) {
		label = b.ToolName
	}
	top := line{runs: []cell{{text: box.TopLeft, style: border}}}
	switch {
	case label == "":
		top.runs = append(top.runs, cell{text: strings.Repeat(box.Horizontal, max(1, w-2)) + box.TopRight, style: border})
	default:
		label = truncateCells(label, max(1, w-6), "…")
		top.runs = append(top.runs,
			cell{text: box.Horizontal + " ", style: border},
			cell{text: label, style: border.Bold(true)},
			cell{text: " " + strings.Repeat(box.Horizontal, max(1, w-5-width(label))) + box.TopRight, style: border})
	}

	// row wraps one line to the frame, padded so every right border lands on
	// the same column.
	row := func(s string, st tcell.Style) line {
		return line{runs: []cell{
			{text: box.Vertical + " ", style: border},
			{text: fitWidth(s, inner), style: st},
			{text: " " + box.Vertical, style: border},
		}}
	}

	var out []line
	out = append(out, top)

	// Body: the tool output as the model saw it (the tool layer bounds it:
	// bash 16KB head+tail per stream, 8MB combined kill cap). Collapsed, the
	// render window keeps the resident line cache bounded (PRD row budget);
	// Ctrl+O drops the window and prints everything the tool kept, which is
	// bounded by that sink cap rather than by this renderer.
	body := strings.TrimRight(b.Text, "\n")
	switch {
	case body == "" && !b.Err:
		out = append(out, row("(no output)", mutedSt))
	case body != "":
		// Aged results collapse to a head+tail window: the middle of the
		// output is trimmed before anything else in the transcript is.
		headRows, tailRows := toolWindow(a.trimTier(i))
		rows := wrap(body, inner)
		if b.Expanded || len(rows) <= headRows+tailRows+1 {
			for _, wl := range rows {
				out = append(out, row(wl, bodySt))
			}
		} else {
			for _, wl := range rows[:headRows] {
				out = append(out, row(wl, bodySt))
			}
			// The notice sits at the hole it describes, between the head and
			// the tail — omp prints its hidden-line count the same way.
			out = append(out, row(fmt.Sprintf("… %d lines hidden (Ctrl+O to expand)", len(rows)-headRows-tailRows), dimSt))
			for _, wl := range rows[len(rows)-tailRows:] {
				out = append(out, row(wl, bodySt))
			}
		}
	}

	// Status footer: only the facts this result has. A signalled command
	// reports no exit code (tool.Outcome says so), so it never shows a signal
	// dressed up as a status.
	var notes []string
	if b.Dur != "" {
		notes = append(notes, "Wall: "+b.Dur)
	}
	if b.HasExit && b.Exit != 0 {
		notes = append(notes, fmt.Sprintf("Exit: %d", b.Exit))
	}
	if b.Truncated {
		notes = append(notes, "output truncated")
	}
	if len(notes) > 0 {
		notesSt := dimSt
		if b.Err {
			notesSt = tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentError)))
		}
		out = append(out, row("⟦"+strings.Join(notes, " | ")+"⟧", notesSt))
	}

	out = append(out, textline(box.BottomLeft+strings.Repeat(box.Horizontal, max(1, w-2))+box.BottomRight, border))
	return out
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
	// Per-frame facts about the viewport: a frame that draws no transcript
	// (welcome, /clear) must not keep last frame's scroll hint, nor its
	// selection capture — rows recorded before /clear would copy text that is
	// no longer on screen.
	a.scrollHint, a.selRows = "", nil

	// Empty transcript: the welcome screen (grok welcome/mod.rs — logo,
	// menu, shortcuts) instead of a blank void.
	if len(a.blocks) == 0 {
		composerTop := h - 1 - a.composerRows()
		a.drawWelcome(s, w, h)
		a.drawSessionPicker(composerTop)
		a.drawHubRoster(composerTop)
		a.drawTreeSelector(composerTop)
		a.drawPicker(composerTop)
		a.drawSlashDropdown(composerTop)
		a.drawAskCard(composerTop)
		a.drawComposer(composerTop)
		a.drawStatusRow(h - 1)
		s.Show()
		return
	}

	// Grok layout: scrollback, blank row, composer box (grows with the
	// draft's wrapped line count), status row at the bottom.
	cRows := a.composerRows()
	vp := h - cRows - 2
	if vp < 1 {
		vp = 1
	}
	contentW := a.contentWidth()

	// The row index is this frame's plan: sync() re-renders only the blocks
	// whose stamp moved and knows the first row of every block, so painting
	// costs the viewport instead of the session.
	total := int(a.sync(contentW))

	// Feed the new row count through the model every frame: while following
	// it stays pinned to the tail; while scrolled it preserves the user's
	// position against streaming output.
	a.sm.NewContent(total, vp)
	start := a.sm.Start(total, vp)
	end := start + vp
	if end > total {
		end = total
	}

	// Proportional right-edge scrollbar (omp's ScrollView): while the
	// transcript overflows the viewport, the last screen column carries a
	// track/thumb that gives continuous position feedback. Reserve it so band
	// rows never render a cell under the bar.
	sbStart, sbEnd, sbOk := a.sm.Scrollbar(total, vp)
	bandLim := w
	if sbOk {
		bandLim = w - 1
	}
	selRows := make([]selRow, 0, end-start)
	for y, r := range a.viewRows(int32(start), int32(end)) {
		if r.ln.bg != 0 {
			// Band row (user prompt / code fence): fill the full width so
			// the band reads as one continuous row (grok semantic band).
			for bx := 0; bx < bandLim; bx++ {
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
	// Paint the scrollbar over the reserved column, spanning the visible
	// rows: the thumb marks the current window, the track fills the rest.
	if sbOk {
		trackSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim)))
		thumbSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.Gray)))
		for y := range end - start {
			ch, st := "│", trackSt
			if y >= sbStart && y < sbEnd {
				ch, st = "█", thumbSt
			}
			drawText(s, w-1, y, ch, st)
		}
	}
	a.selRows, a.selTop = selRows, start
	if a.selDown {
		a.selCacheRows(start) // keep the text a held drag has already passed
	}
	a.drawSelection()
	// Scroll indicator (grok-style ▲n▼n): rows hidden above/below. It rides the
	// composer's info divider — at y=0 it overwrote whatever content scrolled
	// to the top row (a long thinking line, or the last prompt) and any
	// right-aligned timestamp there, so the first line showed a hint glued to
	// the text that never scrolled away. The divider is chrome: never content.
	if up, down := a.sm.Indicator(total, vp); up > 0 || down > 0 {
		a.scrollHint = fmt.Sprintf("▲ %d ▼ %d", up, down)
	} else {
		a.scrollHint = ""
	}
	// The composer's first input row sits below the transcript; the box
	// occupies composerRows() rows above the status line.
	composerTop := h - 1 - cRows
	a.drawSessionPicker(composerTop)
	a.drawHubRoster(composerTop)
	a.drawTreeSelector(composerTop)
	a.drawPicker(composerTop)
	a.drawSlashDropdown(composerTop)
	a.drawAskCard(composerTop)
	a.drawComposer(composerTop)
	a.drawStatusRow(h - 1)
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
	popup := a.th.Box()
	popupSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.PromptBorderActive)))
	drawText(s, 2, y, popup.TopLeft+strings.Repeat(popup.Horizontal, min(w-4, nameW+44))+popup.TopRight,
		popupSt)
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
	drawText(s, 2, y, popup.BottomLeft+strings.Repeat(popup.Horizontal, min(w-4, nameW+44))+popup.BottomRight,
		popupSt)
}

// drawPicker renders the top modal picker above the composer: a titled box
// with optional view tabs, the row window (section headers included), and a
// footer carrying the filter, the position, and the key hints. It borrows
// the dropdown's surface so both read as the same chrome.
func (a *App) drawPicker(yComposerTop int) {
	if len(a.pickers) == 0 {
		return
	}
	p := a.pickers[len(a.pickers)-1]
	w := a.width
	// Never open-and-invisible (the 5b999f0 invariant, restated for the
	// picker stack): an unusable terminal width or no room above the
	// composer must close the modal, not leave it owning the keyboard
	// while painting nothing.
	if w < 24 {
		// Callers hold a.mu (draw does), so this pops directly —
		// popPicker would re-lock and self-deadlock the UI thread.
		a.pickers = a.pickers[:len(a.pickers)-1]
		return
	}
	x0, x1 := 2, w-3
	inner := x1 - x0 - 1
	rows := p.visible
	if rows > pickerMaxRows {
		rows = pickerMaxRows
	}
	tabs := p.viewCount() > 1
	// chrome = top border + bottom border + footer, plus the tab row.
	chrome := 3
	if tabs {
		chrome++
	}
	// The panel floats above the composer and never covers it.
	if avail := yComposerTop - 1; avail-chrome < 1 {
		a.pickers = a.pickers[:len(a.pickers)-1]
		return
	} else if rows > avail-chrome {
		rows = avail - chrome
	}
	p.visible = rows // paging in handlePickerKey follows the drawn window
	lines, start, selLine := p.window(rows)

	borderS := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.PromptBorderActive)))
	rowBg := tcell.StyleDefault.Background(a.cellColor(a.th.Get(theme.BgBase)))
	selBg := tcell.StyleDefault.Background(a.cellColor(a.th.Get(theme.BgHighlight)))
	detailCol := a.cellColor(a.th.Get(theme.GrayDim))
	curCol := a.cellColor(a.th.Get(theme.AccentSuccess))
	footS := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim)))

	height := chrome + len(lines)
	yTop := yComposerTop - 1 - height

	// Top border with the title embedded: ╭─ model ──…──╮
	title := " " + p.opts.Title + " "
	drawText(a.scr, x0, yTop, "╭"+title, borderS)
	drawn := 1 + width(title)
	if pad := inner - drawn; pad > 0 {
		drawText(a.scr, x0+drawn, yTop, strings.Repeat("─", pad), borderS)
	}
	drawText(a.scr, x1, yTop, "╮", borderS)
	y := yTop + 1

	// View tabs (omp's per-provider views): the active one is bright.
	if tabs {
		a.drawPickerTabRow(p, y, x0, inner, rowBg, tcell.StyleDefault.Foreground(detailCol))
		y++
	}

	// Rows. The label column is aligned across the visible window so the
	// detail column reads as a table.
	labelW := 0
	for _, ln := range lines {
		if !ln.header {
			labelW = max(labelW, width(ln.item.Label))
		}
	}
	labelW = min(labelW+2, 28)
	for i, ln := range lines {
		st := rowBg
		if start+i == selLine {
			st = selBg
		}
		for x := x0 + 1; x < x1; x++ {
			a.scr.SetContent(x, y, ' ', nil, st)
		}
		a.scr.SetContent(x0, y, '│', nil, borderS)
		a.scr.SetContent(x1, y, '│', nil, borderS)
		if ln.header {
			drawText(a.scr, x0+3, y, clip(ln.text, inner-4), st.Foreground(a.cellColor(a.th.Get(theme.Gray))))
			y++
			continue
		}
		marker := "  "
		if start+i == selLine {
			marker = "▶ "
		}
		drawText(a.scr, x0+2, y, marker, st.Foreground(a.cellColor(a.th.Get(theme.AccentAssistant))))
		if ln.item.Current {
			drawText(a.scr, x0+4, y, "●", st.Foreground(a.cellColor(a.th.Get(theme.AccentSuccess))))
		}
		drawText(a.scr, x0+6, y, clip(ln.item.Label, labelW-1), st.Foreground(a.cellColor(a.th.Get(theme.AccentUser))))
		if ln.item.Detail != "" {
			cell := x0 + 6 + labelW
			if room := x1 - 2 - cell; room > 4 {
				col := detailCol
				if ln.item.Current {
					col = curCol
				}
				drawText(a.scr, cell, y, clip(ln.item.Detail, room-1), st.Foreground(col))
			}
		}
		y++
	}

	// Footer: position/filter left, key hints right. The row is cleared first
	// — the row loop above fills its own cells, but the footer never did, so
	// transcript glyphs showed straight through the panel ("T1/9 itemsich and
	// I'll go." was the live symptom).
	left, right := p.footer()
	for x := x0 + 1; x < x1; x++ {
		a.scr.SetContent(x, y, ' ', nil, rowBg)
	}
	drawText(a.scr, x0, y, "│", borderS)
	drawText(a.scr, x1, y, "│", borderS)
	drawText(a.scr, x0+2, y, clip(left, inner-2), footS)
	if rw := width(right) + 2; rw < inner-width(left) {
		drawText(a.scr, x1-1-rw, y, right, footS)
	}
	y++
	drawText(a.scr, x0, y, "╰"+strings.Repeat("─", inner)+"╯", borderS)
}

// drawPickerTabRow draws the tab strip, brightening the active view.
func (a *App) drawPickerTabRow(p *picker, y, x0, inner int, bg, dim tcell.Style) {
	borderCol := a.cellColor(a.th.Get(theme.PromptBorderActive))
	a.scr.SetContent(x0, y, '│', nil, tcell.StyleDefault.Foreground(borderCol))
	a.scr.SetContent(x0+inner+1, y, '│', nil, tcell.StyleDefault.Foreground(borderCol))
	for x := x0 + 1; x <= x0+inner; x++ {
		a.scr.SetContent(x, y, ' ', nil, bg)
	}
	x := x0 + 2
	active := tcell.StyleDefault.Background(a.cellColor(a.th.Get(theme.BgHighlight))).
		Foreground(a.cellColor(a.th.Get(theme.AccentAssistant))).Bold(true)
	for i, v := range p.opts.Views {
		if x+width(v.Name) > x0+inner {
			break
		}
		st := dim
		if i == p.view {
			st = active
		}
		drawText(a.scr, x, y, v.Name, st)
		x += width(v.Name) + 3
	}
}

// clip truncates s to max cells, marking the cut with an ellipsis.
func clip(s string, maxCells int) string {
	if maxCells <= 1 {
		return ""
	}
	if width(s) <= maxCells {
		return s
	}
	var b strings.Builder
	w := 0
	for _, r := range s {
		rw := width(string(r))
		if w+rw > maxCells-1 {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	return b.String() + "…"
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

// drawComposer renders the prompt box: themed outline (theme.Box), ❯ prefix,
// editor text, blinking block cursor; the model + running spinner ride the
// info divider, tinted with the statusLine tokens.
func (a *App) drawComposer(yTop int) {
	w := a.width
	if w < 6 || yTop < 1 {
		return
	}
	box := a.th.Box()
	bs := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.PromptBorderActive)))
	// The divider is the status line: a theme that sets statusLineBg fills
	// the row ("" = terminal default = transparent, today's look).
	infoBg, hasInfoBg := a.th.Slot(theme.StatusLineBg)
	divSt := bs
	if hasInfoBg {
		divSt = divSt.Background(a.cellColor(infoBg))
	}
	ms := a.mdStyle()

	// Top border: ╭────╮ (1-cell inset on each side, like grok's box).
	drawText(a.scr, 1, yTop-1, box.TopLeft, bs)
	for x := 2; x < w-2; x++ {
		a.scr.SetContent(x, yTop-1, boxRune(box.Horizontal), nil, bs)
	}
	drawText(a.scr, w-2, yTop-1, box.TopRight, bs)

	// Input rows: │ ❯ first…│ then continuation rows aligned under the text.
	lines, curRow, curCol := a.composerInputLines()
	promptStyle := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentUser))).Bold(true)
	vert := boxRune(box.Vertical)
	for i, ln := range lines {
		y := yTop + i
		a.scr.SetContent(1, y, vert, nil, bs)
		a.scr.SetContent(w-2, y, vert, nil, bs)
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

	// Info divider bottom border: ╰─ model · ⠋ ─────── ▲n▼n ─╯
	yBottom := yTop + len(lines)
	info := " " + a.st.Model
	if a.vibeOps != nil && a.vibeOps.Active != nil && a.vibeOps.Active() {
		info += " · Vibe"
	}
	if a.st.Running {
		frames := a.th.SpinnerFrames() // theme frames, braille by default
		a.st.spinnerIdx = a.st.spinnerIdx % len(frames)
		info += " · " + frames[a.st.spinnerIdx]
	}
	drawText(a.scr, 1, yBottom, box.BottomLeft, divSt)
	for x := 2; x < w-2; x++ {
		a.scr.SetContent(x, yBottom, boxRune(box.Horizontal), nil, divSt)
	}
	if info != " " {
		infoSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.StatusLineModel)))
		if hasInfoBg {
			infoSt = infoSt.Background(a.cellColor(infoBg))
		}
		drawText(a.scr, 2, yBottom, info, infoSt)
	}
	// The viewport hint (▲n▼n) rides this divider's right end. It used to be
	// painted on transcript row 0, where it overwrote whatever content had
	// scrolled to the top: a long thinking line, or the last prompt, looked
	// like it had gone static in the first line. The divider is chrome, so it
	// takes the pixels instead; when the divider is too narrow for both, the
	// hint is dropped rather than eating the model name. A fresh copy
	// confirmation outranks it — that message is the only proof the mouse
	// gesture did anything, since the app holds the mouse and the terminal
	// stays quiet.
	hint := a.copyHint()
	if hint == "" {
		hint = a.scrollHint
	}
	if hint != "" {
		hx := w - 3 - width(hint)
		if hx > 2+width(info) {
			hintSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.Gray)))
			if hasInfoBg {
				hintSt = hintSt.Background(a.cellColor(infoBg))
			}
			drawText(a.scr, hx, yBottom, hint, hintSt)
		}
	}
	drawText(a.scr, w-2, yBottom, box.BottomRight, divSt)

	// Cursor: blinking block at the editor position inside the wrapped grid.
	cx := 5 + curCol
	a.scr.ShowCursor(min(cx, w-3), yTop+curRow)
}

// drawStatusRow renders the bottom row: the working directory on the left,
// the configured HUD segments (settings statusLine.segments) right-aligned
// (caller holds a.mu). The keyboard chords used to live on the left; /hotkeys
// and the welcome menu carry them now, which frees the room the metrics need
// on a small terminal.
func (a *App) drawStatusRow(y int) {
	parts := a.hudParts()
	// The session clock and the decode rate are what the row is for during a
	// run, so they claim the space first: the path is what shrinks.
	budget := a.width - 2 - hudEssentialWidth(parts) - 1
	lbl := pathDisplay(a.cwd, budget-2)
	if lbl != "" {
		pathSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.Gray)))
		drawText(a.scr, 2, y, lbl, pathSt)
	}
	a.drawHUD(y, 2+width(lbl), parts)
}

// drawHUD renders the configured status segments right-aligned on the
// status row (caller holds a.mu). Segment colors come from the
// statusLine* tokens, the separators from statusLineSep, and statusLineBg
// fills the row when the theme sets one. The metrics win a narrow row:
// the path is drawn first against the space the essential segments need,
// and any segment that still does not fit is dropped by keep-rank (theme
// and model first, the session clock and the rate last).
func (a *App) drawHUD(y, leftEnd int, parts []hudPart) {
	if len(parts) == 0 {
		return
	}
	const sep = " │ "
	widthOf := func(ps []hudPart) int {
		n := 0
		for i, p := range ps {
			if i > 0 {
				n += width(sep)
			}
			n += width(p.text)
		}
		return n
	}
	end := a.width - 2
	for len(parts) > 0 && end-widthOf(parts) < leftEnd+1 {
		drop := 0
		for i, p := range parts {
			if statusKeepRank[p.name] < statusKeepRank[parts[drop].name] {
				drop = i
			}
		}
		parts = append(parts[:drop], parts[drop+1:]...)
	}
	if len(parts) == 0 {
		return
	}
	bgSt := tcell.StyleDefault
	if bg, ok := a.th.Slot(theme.StatusLineBg); ok {
		bgSt = bgSt.Background(a.cellColor(bg))
	}
	sepSt := bgSt.Foreground(a.cellColor(a.th.Get(theme.StatusLineSep)))
	x := end - widthOf(parts)
	if _, ok := a.th.Slot(theme.StatusLineBg); ok {
		for bx := x; bx < end; bx++ {
			a.scr.SetContent(bx, y, ' ', nil, bgSt)
		}
	}
	for i, p := range parts {
		if i > 0 {
			drawText(a.scr, x, y, sep, sepSt)
			x += width(sep)
		}
		drawText(a.scr, x, y, p.text, bgSt.Foreground(a.cellColor(a.th.Get(p.token))))
		x += width(p.text)
	}
}

// statusSegments is the HUD segment vocabulary (settings
// statusLine.segments): model, tokens, context, cost, rate, theme, time.
var statusSegments = map[string]bool{
	"model":   true,
	"tokens":  true,
	"context": true,
	"cost":    true,
	"rate":    true,
	"theme":   true,
	"time":    true,
}

// defaultStatusSegments is the shipped layout: the session clock, the token
// counters and the decode speed, right-aligned (the clock reads leftmost so
// the rate's own " │ " stays the row's right edge). The model keeps its
// composer divider slot, which is chrome rather than a segment.
var defaultStatusSegments = []string{"time", "tokens", "rate"}

// hudHasClock reports whether the effective HUD layout renders the time
// segment (caller holds a.mu). When it does, the UI loop repaints at 1 Hz
// even while idle so the clock stays live.
func (a *App) hudHasClock() bool {
	segs := a.statusSegs
	if len(segs) == 0 {
		segs = defaultStatusSegments
	}
	for _, s := range segs {
		if s == "time" {
			return true
		}
	}
	return false
}

func statusSegmentNames() []string {
	out := make([]string, 0, len(statusSegments))
	for name := range statusSegments {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// hudSegment renders one segment: its text and the statusLine token that
// colors it. An empty text means the segment has nothing to show (hidden,
// not blank) — an unwired cost or context never draws an empty cell.
func (a *App) hudSegment(name string) (text, token string) {
	switch name {
	case "model":
		return a.st.Model, theme.StatusLineModel
	case "tokens":
		if a.st.TokensIn == 0 && a.st.TokensOut == 0 {
			return "", ""
		}
		return fmt.Sprintf("↑%s │ ↓%s", humanTokens(a.st.TokensIn), humanTokens(a.st.TokensOut)), theme.StatusLineSpend
	case "context":
		// used/total of the LIVE context: what the next request costs against
		// the model's window. Hidden until both halves are known — an
		// undiscovered window (0) or a session that never ran makes no claim.
		if a.st.CtxWindow <= 0 || a.st.CtxUsed == 0 {
			return "", ""
		}
		return fmt.Sprintf("ctx %s/%s", humanTokens(a.st.CtxUsed), humanTokens(a.st.CtxWindow)), theme.StatusLineContext
	case "cost":
		if a.st.Cost <= 0 {
			return "", ""
		}
		return fmt.Sprintf("$%.4f", a.st.Cost), theme.StatusLineCost
	case "time":
		if a.st.Start.IsZero() {
			return "", ""
		}
		return humanDur(time.Since(a.st.Start)), theme.StatusLineSpend
	case "rate":
		// omp's ⚡ tok/s: the decode speed of the last completed message, or a
		// live estimate from the deltas arriving right now. Never measured is
		// never displayed — the segment hides rather than show a fake zero.
		rate := a.st.Rate
		if a.st.Running {
			if live := a.liveRate(); live > 0 {
				rate = live
			}
		}
		if rate <= 0 {
			return "", ""
		}
		return fmt.Sprintf("⚡ %.1f t/s", rate), theme.StatusLineSpend
	case "theme":
		return a.th.Name, theme.StatusLineSep
	}
	return "", ""
}

// hudPart is one rendered HUD segment, carrying the segment name the
// keep-rank drop loop keys on.
type hudPart struct {
	name, text, token string
}

// hudSep separates HUD segments on the status row.
const hudSep = " │ "

// hudParts renders the configured segments (settings statusLine.segments),
// in order, dropping the ones with nothing to show (an unwired cost or
// context window hides rather than drawing an empty cell).
func (a *App) hudParts() []hudPart {
	segs := a.statusSegs
	if len(segs) == 0 {
		segs = defaultStatusSegments
	}
	parts := make([]hudPart, 0, len(segs))
	for _, name := range segs {
		text, token := a.hudSegment(name)
		if text != "" {
			parts = append(parts, hudPart{name, text, token})
		}
	}
	return parts
}

// hudEssentialWidth is the space the metrics that must always survive a
// narrow row take: the session clock and the decode rate, separated by a
// HUD separator. The rest of the HUD (and the hotkeys) give way to them.
func hudEssentialWidth(parts []hudPart) int {
	essential := make([]hudPart, 0, 2)
	for _, p := range parts {
		if statusKeepRank[p.name] >= 5 {
			essential = append(essential, p)
		}
	}
	if len(essential) == 0 {
		return 0
	}
	n := 0
	for i, p := range essential {
		if i > 0 {
			n += width(hudSep)
		}
		n += width(p.text)
	}
	return n
}

// statusKeepRank orders segments by how essential they are when the row
// runs out of width. The decode rate and the session clock are what the
// user reads during a run, so they are dropped last.
var statusKeepRank = map[string]int{
	"theme":   0,
	"model":   1,
	"tokens":  2,
	"context": 3,
	"cost":    4,
	"time":    5,
	"rate":    6,
}

// pathDisplay renders the working directory for the status row: home
// abbreviated ("~/work/proj"), and when the row is too narrow it keeps the
// trailing components — the ones that identify the project — behind a
// leading ellipsis ("…/freepeak/xdev").
func pathDisplay(cwd string, maxW int) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(cwd, home) {
		cwd = "~" + strings.TrimPrefix(cwd, home)
	}
	if maxW <= 0 || width(cwd) <= maxW {
		return cwd
	}
	parts := strings.Split(cwd, "/")
	for i := 1; i < len(parts); i++ {
		t := strings.Join(parts[i:], "/")
		if t == "" {
			continue
		}
		if w := width("…/" + t); w <= maxW {
			return "…/" + t
		}
	}
	return truncateCells(cwd, maxW, "…")
}

// boxRune is the first rune of a themed box glyph ("" = a space).
func boxRune(glyph string) rune {
	for _, r := range glyph {
		return r
	}
	return ' '
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

// humanTokens renders 1234 as "1.2k" and a round 200000 as "200k": the HUD
// meter shows round windows and round spend, where ".0" is pure noise in a
// row that is always competing with the working-directory path for width.
func humanTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		s := fmt.Sprintf("%.1f", float64(n)/1_000_000)
		return strings.TrimSuffix(s, ".0") + "M"
	case n >= 1000:
		s := fmt.Sprintf("%.1f", float64(n)/1000)
		return strings.TrimSuffix(s, ".0") + "k"
	default:
		return fmt.Sprintf("%d", n)
	}
}

// humanDur renders elapsed time compactly: "45s", "12m03s", "3h05m".
func humanDur(d time.Duration) string {
	s := int64(d.Seconds())
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	default:
		return fmt.Sprintf("%dh%02dm", s/3600, (s%3600)/60)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
