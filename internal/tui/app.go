package tui

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/config"
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
	// CtxUsed is the LIVE context occupancy: the token count of the most recent
	// completed request — every input token, cached or fresh, plus the output
	// (the provider's own total, so it covers the system prompt, the whole
	// visible history and the tool schemas). The HUD's context segment reads
	// this against CtxWindow — deliberately NOT the cumulative
	// TokensIn/TokensOut, which count every turn the session ever sent and so
	// run far past the window.
	CtxUsed int64
	Rate    float64
	// Work is the session's accumulated ACTIVE time — the spans the agent spent
	// thinking, streaming and running tools. The idle gaps between turns are
	// never counted, and neither is the time a question card sat waiting for
	// the human, so an idle agent shows a frozen number instead of a wall clock
	// that keeps running. SetWork re-bases it when a store with history is
	// adopted (/resume, /fork).
	Work time.Duration
	// runStart stamps the work span in flight; zero means none is open.
	runStart time.Time
	// askWaits counts the question cards on screen. While it is above zero the
	// clock stands still: the pause belongs to the human, not to the session.
	askWaits   int
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
	// escDraft is the prompt the last Esc took out of the composer and
	// escUsed says that exact text has already been given back: the ladder is
	// clear → restore → clear → open the selector, so repeated Esc always
	// ends at the tree instead of blinking the same text forever, while an
	// edit made after a restore still gets its own undo. UI-thread-only like
	// the editor itself (no lock); the stash is a copy.
	escDraft []rune
	escUsed  bool
	smenu    *slashMenu // "/" autocomplete dropdown (nil = closed)
	// pickers is the modal-list stack: /model opens a models selector, and
	// a nested list pushes on top of it. Esc pops one level; a selection
	// pops them all. While non-empty the picker owns every key event and
	// the dropdown is closed.
	pickers []*picker
	// mouseBtnDown tracks the primary button across events, so a press is acted
	// on once: tcell strips the SGR motion bit and exposes no accessor, so
	// without this edge a single click would look like a press at every drag
	// report the terminal sends along the way.
	mouseBtnDown bool
	keyMap       *KeyMap // remappable keybinding layer
	st           Status
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
	connectOps        *ConnectOps                            // /connect, wired by cmd (nil → notices)
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
	treeLabels        map[string]string        // id→label snapshot, refreshed on open
	settingsOps       *SettingsOps             // /settings, wired by cmd (nil → notices)
	thinkingOps       *ThinkingOps             // /thinking, wired by cmd (nil → notices)
	cwd               string                   // working directory (the status row's left side)
	branch            string                   // git branch for the top bar ("" when none)
	commandDir        string                   // markdown command discovery root
	pathRoot          string                   // @-completion root (empty disables the menu)
	pathList          func(string) []PathEntry // one directory's entries (the only source)
	extCommands       map[string]string        // "/server:cmd" -> description
	extRun            ExtensionCommand
	renderers         map[string]RenderSpec   // tool name -> declarative render spec
	sessionBranch     func(args string) error // /branch to an entry id
	resumeList        func(cwd string) error  // /resume session listing
	onSend            func(text string)
	// onSendImages is the multimodal send: the draft with every attached
	// image's chip stripped, plus the payloads in prompt order. It returns
	// true when it took the turn. False (or nil) means the attachments cannot
	// go out — and then the draft must come back to the composer rather than
	// being sent as text: a chip reaching the model as the word "[Image …]"
	// is a user believing a screenshot was read that no pixel of was sent.
	onSendImages func(text string, imgs []PasteImage) bool
	onCancel     func()
	onQuit       func()
	// onRetry re-runs the current session's last turn with no new prompt (the
	// F5 recovery for a stream a dropped connection cut short). Wired by cmd;
	// nil degrades the chord to a notice.
	onRetry func()

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

	sheenPhase int // welcome logo sheen sweep position (columns)
	// startupNotice is a line of the welcome screen's own chrome — what
	// loaded, what broke (#272). It is deliberately NOT a transcript block:
	// any block ends the welcome screen, so the report used to replace the
	// start screen. grok keeps the same split — startup warnings and the tip
	// row are slots of the welcome layout, never scrollback content.
	startupNotice string
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
	// Held-drag edge auto-scroll (selection.go): selEdge is the direction a drag
	// parked on the transcript's first (-1) or last (+1) row is scrolling,
	// selEdgeAt when it first arrived there. The UI tick keeps scrolling once
	// selEdgeDelay has passed — the pointer itself reports nothing while it sits
	// still, which is the whole problem. selThumbDrag is a drag that took the
	// scrollbar instead of the text; selGrab is the grip taken on the thumb, so
	// thumb keeps its grip while it moves: selGrab is how far below the thumb's
	// top the finger landed, so a thumb grabbed in the middle stays under it.
	// (UI thread; mu-guarded.)
	selEdge      int
	selEdgeAt    time.Time
	selThumbDrag bool
	selGrab      int
	// selBar* is the scrollbar's geometry as the painter last drew it, published
	// every frame like the picker's hit table: a press is tested against the bar
	// that is on screen rather than a re-derivation that could disagree with it.
	// (UI thread; mu-guarded.)
	selBarOn    bool
	selBarW     int // the bar's column width (the last screen column, or 0)
	selBarVP    int // visible transcript rows the bar spans
	selBarTotal int // transcript rows at paint time
	selBarThumb int // thumb rows at paint time
	selBarPos   int // thumb's first track row at paint time
	// selNotice is the copy confirmation (omp's showStatus for a copy); it
	// rides the composer divider until selNoticeUntil.
	selNotice      string
	selNoticeUntil time.Time
	// scrollHint is the ▲n▼n viewport hint, drawn on the composer's info
	// divider — never on row 0, where it overwrote scrolled-to content.
	scrollHint string
	// Pasted images (paste.go): the payloads the chips in the composer name,
	// the bracketed-paste window, and the clipboard reader. All three are
	// UI-thread-owned exactly like the editor beside them — no lock covers
	// them, and nothing off the UI loop may touch them.
	images    pendingImages
	paste     pasteState
	clipImage func() ([]byte, string, error)
	// vision reports whether the live model takes image input (cmd wires it
	// from models.yml `vision:` for the model in use). nil is unknown, which
	// pastes anyway: a model that cannot read the attachment answers with
	// text, and a wrong "no" would block the feature on a field nobody filled.
	vision func() bool
	// dock is the context dock panel (#291 §1): its display policy, its fold
	// state, its built rows. Nil until a source is wired; every reader of it
	// tolerates that, because a session with no dock is a normal session.
	dock *dockState
	// dockSetMode persists a display policy the human changed with Alt+s
	// (settings `sidebarMode`); nil = this session cannot persist it.
	dockSetMode func(mode string)
}

type blockKey struct {
	idx      int
	kind     BlockKind
	width    int
	tlen     int
	tool     string
	status   string
	stream   bool
	expanded bool // box rows: the Ctrl+O state changed the row set
	age      int64
	trim     int8 // bounded middle trim: this block's render-window tier
	dlen     int  // result box: a diff changes the row set without touching Text
	thinkOff int  // reasoning box: the box's own scroll position
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
		keyq:   make(chan tcell.Event, 64),
		dirty:  make(chan struct{}, 1),
		quitCh: make(chan struct{}),
		sm:     newScrollModel(),
	}
}

// SetLocation wires the working directory: the status row's left side, and
// the top bar's git branch.
func (a *App) SetLocation(cwd string) {
	a.mu.Lock()
	a.cwd = cwd
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

// SetHandlers wires the send/cancel/quit callbacks. A text-only caller uses
// this; a mode that can carry image attachments adds SetImageSend.
func (a *App) SetHandlers(onSend func(text string), onCancel, onQuit func()) {
	a.onSend, a.onCancel, a.onQuit = onSend, onCancel, onQuit
}

// SetImageSend wires the multimodal send path (see App.onSendImages).
func (a *App) SetImageSend(fn func(text string, imgs []PasteImage) bool) {
	a.onSendImages = fn
}

// SetVision wires "can the live model take an image?", which cmd answers from
// models.yml `vision:` for the model in use — so a /model switch changes the
// answer. Unwired (tests, modes with no model) is unknown, which pastes.
func (a *App) SetVision(fn func() bool) { a.vision = fn }

// SetRetry wires the F5 retry path (see App.onRetry).
func (a *App) SetRetry(fn func()) { a.onRetry = fn }

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

// SetStartupNotice reports a startup fact (#272: what loaded, what broke)
// where the user will actually read it. With an empty transcript it becomes
// a line of the welcome screen — a transcript block would end that screen,
// which is how "· task agents: 5" came to replace the start screen outright.
// Once a transcript exists (a resumed session) there is no welcome to hang it
// on, so it goes to the scrollback instead. Either way it is one line the user
// sees before the first turn, and never both.
func (a *App) SetStartupNotice(text string) {
	a.mu.Lock()
	empty := len(a.blocks) == 0
	if empty {
		a.startupNotice = text
	}
	a.mu.Unlock()
	if !empty {
		a.AddSystemBlock(text)
	}
	a.poke()
}

// SetNotice shows text on the composer divider for d and then drops it, the
// channel a failed chord already answers on (paste.go setNotice). For a caller
// off the UI thread — the MCP connect that lands mid-session — so it takes the
// lock, unlike the UI-thread setNotice it shares the slot with.
//
// ponytail: one slot, so a copy confirmation inside d overwrites this notice
// (and vice versa). Fixing that means a second row of chrome on the divider;
// worth it only if a real report of a lost notice shows up.
func (a *App) SetNotice(text string, d time.Duration) {
	a.mu.Lock()
	a.selNotice = text
	a.selNoticeUntil = time.Now().Add(d)
	a.mu.Unlock()
	a.poke()
}

// BeginAssistant starts (or continues into) the streaming assistant block. A
// stream that arrives with no turn opened (a feed that outlived its cancel)
// opens its work span here too, so the time segment never loses the seconds
// the model was actually answering.
func (a *App) BeginAssistant() {
	a.mu.Lock()
	a.markRun(true)
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
	Diff      string // unified diff of the file change, when the tool made one
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
		Truncated: out.Truncated, Diff: out.Diff,
	})
	a.mu.Unlock()
	a.poke()
}

// ToggleBoxExpand flips the Ctrl+O state of the newest boxed block — a finished
// tool result or a reasoning box — the one the user is looking at on a
// tail-following transcript. It reports whether there was a box to toggle, so
// the caller can stay silent instead of claiming to have expanded an empty
// transcript.
func (a *App) ToggleBoxExpand() bool {
	a.mu.Lock()
	var found bool
	for i := len(a.blocks) - 1; i >= 0; i-- {
		if b := a.blocks[i]; b.Kind == KindToolDone || b.Kind == KindThinking {
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
// segment, with total: the provider's token count for this request, cached
// input included. ctx is what sits in the window, not only what the window had
// to re-read — a prompt-cache hit still occupies those tokens, and an
// input+output sum reads 90 % low on a cached conversation (the same total
// agent.ContextTokens and compaction trigger on). total <= 0 (a provider that
// reports none) falls back to in+out.
func (a *App) AddUsage(in, out, total int64) {
	a.mu.Lock()
	a.st.TokensIn += in
	a.st.TokensOut += out
	if total <= 0 {
		total = in + out // a provider that reports no total gets the floor
	}
	a.st.CtxUsed = total
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

// SetContextReplay measures a replayed transcript for the HUD's context
// segment. A resumed, switched-to or rewound session rebuilds its history
// without sending a request, so AddUsage has not run and the live number is
// still zero. Callers pass the agent's own context measure (agent.ContextTokens
// of the rebuilt messages) — the same number compaction triggers on, so HUD and
// loop can never disagree. It only ever raises the number: a request that just
// answered knows better than a replay of what led to it. n <= 0 (an empty
// history) leaves the segment hidden rather than claiming zero.
func (a *App) SetContextReplay(n int64) {
	a.mu.Lock()
	if n > a.st.CtxUsed {
		a.st.CtxUsed = n
	}
	a.mu.Unlock()
	a.poke()
}

// SetContextWindow records the model's context window for the HUD context
// segment (0 = unknown: the segment hides).
func (a *App) SetContextWindow(tokens int64) {
	a.mu.Lock()
	a.st.CtxWindow = tokens
	a.mu.Unlock()
	a.poke()
}

// SetWork re-bases the HUD time segment with the work a session has already
// banked. Wired by cmd when a store is adopted (/new, /drop, /resume, fork):
// the turns already on disk were active time even though this process never
// watched them happen.
func (a *App) SetWork(d time.Duration) {
	a.mu.Lock()
	a.st.Work = max(d, 0)
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

// SetRunning toggles the spinner state and opens/closes the work span the
// HUD's time segment measures. Starting a run also discards a decode window
// the last one never closed — an aborted stream would otherwise make the next
// rate divide new tokens by old elapsed time.
func (a *App) SetRunning(r bool) {
	a.mu.Lock()
	a.markRun(r)
	if r {
		a.deltaFirst, a.deltaLast, a.deltaRunes = time.Time{}, time.Time{}, 0
	}
	a.mu.Unlock()
	a.poke()
}

// markRun opens or closes the work span (caller holds a.mu). It is idempotent
// per state, so every streaming hook may claim the run: time between turns —
// reading output, deciding the next prompt — is never counted. An open span
// outlives a question card, whose wait is discounted where the card opens
// (askPause), so a stream that stops to ask is not ended by the asking.
func (a *App) markRun(r bool) {
	if r == a.st.Running {
		return
	}
	a.st.Running = r
	if r {
		if a.st.runStart.IsZero() {
			a.st.runStart = time.Now()
		}
		return
	}
	if !a.st.runStart.IsZero() {
		a.st.Work += time.Since(a.st.runStart)
		a.st.runStart = time.Time{}
	}
}

// askPause stops the clock while a question card waits for the human: it banks
// the span a live run had open — the turn is still in flight, it is the answer
// that is missing — and closes it, so nothing accrues while somebody reads.
// askResume reopens it. Caller holds a.mu.
func (a *App) askPause() {
	a.st.askWaits++
	if !a.st.runStart.IsZero() {
		a.st.Work += time.Since(a.st.runStart)
		a.st.runStart = time.Time{}
	}
}

// askResume ends one card's claim on the clock; the span reopens only when the
// last waiting card is gone and the run that asked is still in flight.
// Caller holds a.mu.
func (a *App) askResume() {
	if a.st.askWaits > 0 {
		a.st.askWaits--
	}
	if a.st.askWaits == 0 && a.st.Running && a.st.runStart.IsZero() {
		a.st.runStart = time.Now()
	}
}

// activeWork is the time segment's reading: the banked spans plus the live
// one. Idle, it is a frozen total — the wall clock between turns belongs to
// the user, not to the session.
func (a *App) activeWork() time.Duration {
	live := a.st.Work
	if !a.st.runStart.IsZero() {
		live += time.Since(a.st.runStart)
	}
	return live
}

// FinishRun closes the run, folding its span into the active-time total.
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

// SetConnectOps wires the /connect command (the catalog lives in cmd/config).
func (a *App) SetConnectOps(ops *ConnectOps) { a.connectOps = ops }

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
	if arg == "show" {
		return a.planShow()
	}
	var on bool
	switch arg {
	case "":
		on = !cur
	case "on":
		on = true
	case "off":
		on = false
	default:
		return fmt.Errorf("plan: use /plan, /plan on, /plan off, or /plan show")
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

// planShow is /plan show: the proposed document read as a document, plus the
// phase list it was written against (#291 §2). The dock is the ambient surface
// for it; this is the one you can scroll back to, and the one that works in a
// narrow terminal where the panel has folded away.
func (a *App) planShow() error {
	if a.planOps.Show == nil {
		return fmt.Errorf("plan: show not wired")
	}
	text := strings.TrimSpace(a.planOps.Show())
	if text == "" {
		a.AddSystemBlock("no pending plan — /plan on, have the model propose one")
		return nil
	}
	a.AddSystemBlock(text)
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

// Reset clears the transcript (used by /clear, /new, /drop, /resume and tree
// navigation): all blocks gone, viewport back to follow, and the context
// segment's number goes with them — an emptied transcript occupies nothing, so
// leaving the previous session's measurement on the row would lie. The time
// segment follows the same rule: the work on a blanked history is 0 until a
// replayed session banks its path's spans back in with SetWork. A replay also
// measures the history back in with SetContextReplay. Streaming state is
// untouched — callers must not be running a turn when they call this.
func (a *App) Reset() {
	a.mu.Lock()
	a.st.Work = 0
	a.blocks = nil
	a.sm = newScrollModel()
	a.st.CtxUsed = 0
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
// interactive selector; with an argument it switches directly, accepting
// anything the -model flag accepts (a concrete provider/model, a bare model
// id, or a model with an inline ":effort" suffix).
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
	// Echo what is actually live, not what was typed: "/model dev"
	// switching to onegw/dev must not claim the session runs "dev".
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
// concrete models. Also bound to Alt+M (omp's app.model.select).
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

// handlePickerMouse routes a mouse event into the open modal list, with the
// semantics omp's SelectList has: the wheel moves the selection one row per
// notch, one click on a row takes it and chooses it (clickItem calls onSelect
// straight away), and a click on a view tab switches view. Returns true when the
// picker consumed the event, so the transcript neither scrolls nor starts a text
// selection underneath the panel. Caller: UI thread; it takes a.mu itself.
func (a *App) handlePickerMouse(m *tcell.EventMouse, press bool) bool {
	wheel := 0
	switch m.Buttons() {
	case tcell.WheelUp:
		wheel = -1
	case tcell.WheelDown:
		wheel = 1
	}
	x, y := m.Position()

	a.mu.Lock()
	if len(a.pickers) == 0 {
		a.mu.Unlock()
		return false
	}
	p := a.pickers[len(a.pickers)-1]
	var (
		act    func(string)
		value  string
		choose bool
	)
	switch {
	case wheel != 0:
		p.move(wheel)
	case press && y == p.tabY:
		if _, view := p.pickAt(x, y); view >= 0 {
			p.switchView(view - p.view)
		}
	case press:
		item, _ := p.pickAt(x, y)
		if item < 0 {
			// A header, border or blank cell inside the panel: consume it so
			// the click cannot start a selection underneath the modal.
			a.mu.Unlock()
			a.poke()
			return true
		}
		if p.selectItem(item) {
			if f, ok := p.choose(); ok {
				if it, ok := p.selected(); ok {
					act, value, choose = f, it.Value, true
				}
			}
		}
	default:
		a.mu.Unlock()
		return false
	}
	a.mu.Unlock()
	a.poke()
	if !choose {
		return true
	}
	a.closePickers()
	act(value)
	return true
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
	// Page by the drawn window. visible is 0 until the first paint
	// measures the terminal (a key can beat the first frame), so page by
	// at least one row rather than swallowing the key.
	case tcell.KeyPgUp:
		a.pickerMutate(func(p *picker) { p.move(-max(1, p.visible)) })
	case tcell.KeyPgDn:
		a.pickerMutate(func(p *picker) { p.move(max(1, p.visible)) })
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

// SetThinkingOps wires the /thinking command and the Shift-Tab toggle to the
// live request-side level (cmd owns the provider holder and the settings
// write). nil ops leave the toggle reporting that it is unwired.
func (a *App) SetThinkingOps(ops *ThinkingOps) { a.thinkingOps = ops }

// ThinkingLevel implements CommandAPI /thinking: bare reports the level in
// force, "on" is the alias for "auto" (the Shift-Tab toggle's other half), and
// any level in config.ThinkingLevels applies to the next turn — unlike
// /settings showThinking, which only touches the display.
func (a *App) ThinkingLevel(args string) error {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		a.AddSystemBlock("thinking " + a.currentThinkingLevel())
		return nil
	}
	if len(fields) > 1 {
		return fmt.Errorf("usage: /thinking [off|auto|%s]", strings.Join(config.ThinkingLevels[2:], "|"))
	}
	want := fields[0]
	if want == "on" || want == "true" {
		want = "auto"
	}
	if !slices.Contains(config.ThinkingLevels, want) {
		return fmt.Errorf("usage: /thinking [off|auto|%s]", strings.Join(config.ThinkingLevels[2:], "|"))
	}
	if a.thinkingOps == nil || a.thinkingOps.Set == nil {
		return fmt.Errorf("thinking is not wired in this build")
	}
	if err := a.thinkingOps.Set(want); err != nil {
		return err
	}
	a.AddSystemBlock("thinking " + want)
	return nil
}

// ToggleThinking is the Shift-Tab chord: off ⇄ auto, the Claude Code Alt+T
// shape (the toggle never lands on a pinned budget — it turns reasoning off or
// hands the decision back to the model role).
func (a *App) ToggleThinking() {
	want := "off"
	if a.currentThinkingLevel() == "off" {
		want = "auto"
	}
	if err := a.ThinkingLevel(want); err != nil {
		a.AddSystemBlock("thinking: " + err.Error())
	}
}

func (a *App) currentThinkingLevel() string {
	if a.thinkingOps != nil && a.thinkingOps.Current != nil {
		return a.thinkingOps.Current()
	}
	return "auto"
}

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
	if fields[0] == "sidebarMode" {
		return a.setDockModeSetting(fields[1:])
	}
	if fields[0] != "showThinking" {
		return fmt.Errorf("unknown setting %q (want showThinking|sidebarMode)", fields[0])
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

// setDockModeSetting is /settings sidebarMode [auto|show|hide]: the context
// dock's display policy (#291 §1). The App owns the state, so the flip works
// with unwired ops; persisting is the config seam's job when it exists.
func (a *App) setDockModeSetting(fields []string) error {
	// Bare form asks, it does not write: a report that also persisted would make
	// the panel's own explanation of itself a side effect.
	if len(fields) == 0 {
		a.AddSystemBlock("sidebarMode " + a.DockMode() + " — " + a.DockState())
		return nil
	}
	if len(fields) > 1 {
		return fmt.Errorf("usage: /settings sidebarMode [auto|show|hide]")
	}
	want := a.DockMode()
	switch fields[0] {
	case DockAuto, DockShow, DockHide:
		want = fields[0]
	default:
		return fmt.Errorf("usage: /settings sidebarMode [auto|show|hide]")
	}
	confirm := "sidebarMode " + want
	if a.settingsOps != nil && a.settingsOps.SetSidebar != nil {
		if err := a.settingsOps.SetSidebar(want); err != nil {
			return err
		}
		confirm += " (saved to " + a.settingsOps.Path + ")"
	}
	a.SetDockMode(want)
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
			// A paste window whose end marker never arrived must still close,
			// or the keys held inside it would never reach the user again
			// (paste.go). Before the lock and outside it: the paste state and
			// the editor are UI-thread-owned, and the flush may read the file
			// a pasted path named.
			stuck := a.flushStuckPaste()
			a.mu.Lock()
			running := a.st.Running
			if running {
				a.st.spinnerIdx = (a.st.spinnerIdx + 1) % len(a.th.SpinnerFrames())
			}
			// Welcome animation. The logo sheen advances one column
			// per 33ms tick and redraws with it — a smooth sweep at
			// ~30fps, the cadence omarchy's own About animation runs
			// at (25ms frames) — while the welcome is visible (no
			// blocks, nothing running). It stops once a block exists,
			// so the only idle cost is the draw itself.
			animate := false
			if !running && len(a.blocks) == 0 {
				a.sheenPhase++
				animate = true
			}
			// A copy confirmation is timed, and an idle UI does not repaint:
			// the tick that finds it expired asks for the draw that drops it.
			if a.selNotice != "" && a.copyHint() == "" {
				animate = true
			}
			// A held drag parked on the transcript's edge is the one mouse
			// gesture with no events of its own, so the tick is its clock
			// (selection.go selEdgeTick).
			if a.selEdgeTick() {
				animate = true
			}
			a.mu.Unlock()
			// Nothing repaints for the HUD: a running draw keeps the time
			// segment live, and idle, the number is a frozen total that needs
			// no tick to stay correct.
			if running || animate || stuck {
				a.draw()
			}
		}
	}
}

// handleKey applies one key event (editor, scrolling, control keys) or one
// paste event.
func (a *App) handleKey(ev tcell.Event) {
	// Bracketed paste before the EventKey cast: the markers are their own
	// event type, and the keys between them are payload, not commands — a
	// pasted CR must not read as Enter. A window can also close by handing
	// back its text WITH the event still to be handled (a control key ended
	// it), so both halves are acted on: the draft lands in the composer and
	// Ctrl-C / Esc still do what they were pressed for.
	consumed, payload := a.paste.feedPaste(ev)
	if payload != "" {
		a.handlePaste(payload)
	}
	if consumed {
		return // the UI loop repaints after handleKey
	}
	key, ok := ev.(*tcell.EventKey)
	if !ok {
		if r, ok := ev.(*tcell.EventResize); ok {
			a.mu.Lock()
			a.width, a.height = r.Size()
			a.clearRenderCache()
			a.mu.Unlock()
		}
		// Mouse wheel scrolls the in-app transcript (tcell would otherwise let
		// the host terminal scroll its own pre-launch scrollback).
		if m, ok := ev.(*tcell.EventMouse); ok {
			// One press edge, computed here, so every consumer agrees on which
			// event began the gesture (tcell does not report the motion bit).
			held := m.Buttons()&tcell.Button1 != 0
			a.mu.Lock()
			press := held && !a.mouseBtnDown
			// A wheel report carries no button bits at all, so reading one as
			// "the button came up" would make the next motion of a held drag
			// look like a fresh press and restart the selection under it.
			if m.Buttons()&(tcell.WheelUp|tcell.WheelDown|tcell.WheelLeft|tcell.WheelRight) == 0 {
				a.mouseBtnDown = held
			}
			a.mu.Unlock()
			// A modal owns the mouse first: omp's lists move the selection on
			// the wheel and choose the row under a click, so nothing underneath
			// them should scroll or start a text selection. The ask card
			// outranks the rest — while it is up it takes the wheel and the
			// click, or the human scrolls the transcript underneath a question
			// they were trying to answer.
			if a.handleAskMouse(m, press) || a.handlePickerMouse(m, press) || a.handleHubRosterMouse(m, press) {
				return // the UI loop repaints after handleKey
			}
			switch m.Buttons() {
			case tcell.WheelUp:
				if !a.scrollThinkBox(m, false) {
					a.scroll(3, false)
				}
			case tcell.WheelDown:
				if !a.scrollThinkBox(m, true) {
					a.scroll(3, true)
				}
			default:
				a.mu.Lock()
				a.handleMouse(m, press)
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

	// Claude-Code double-Esc rewind, with one rung first: idle with a draft,
	// the first Esc clears it — and keeps it, so the next Esc on the empty
	// composer gives the prompt back instead of opening the tree selector.
	// Only with nothing left to undo does Esc open the selector, where a user
	// row is rewind-and-re-prime. Clearing a draft you cannot get back is not
	// an undo, it is a delete, and the text was the expensive part.
	//
	// An open slash/@-menu owns Esc first (close the menu), and the selector's
	// own Esc handling is modal. The stash always holds what the LAST Esc
	// cleared — a newer draft re-stashes on its own clear — and a send retires
	// it: a prompt resurfacing after the user moved on is worse than one lost.
	if key.Key() == tcell.KeyEsc && !running && !menuOpen {
		if strings.TrimSpace(a.ed.Text()) != "" {
			// The stash still names THIS text and was already handed back
			// once: that draft spent its undo, so clear it for real. Anything
			// else — new text, an edit after the hand-back (a whitespace
			// change counts) — gets its own undo, so Esc stays one gesture:
			// "undo what the last Esc took".
			if strings.TrimSpace(string(a.escDraft)) == strings.TrimSpace(a.ed.Text()) && a.escUsed {
				a.ed.Reset()
				a.escDraft, a.escUsed = nil, false
			} else {
				a.escDraft, a.escUsed = a.ed.Clear(), false
			}
			a.poke()
			return
		}
		if len(a.escDraft) > 0 && !a.escUsed {
			a.ed.SetBuffer(string(a.escDraft))
			a.escUsed = true
			a.poke()
			return
		}
		a.escDraft, a.escUsed = nil, false
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
					// Replace only the @token: the user's sentence stays. A
					// directory keeps its slash and reopens the menu inside it;
					// anything else ends the mention with a space.
					text = pathMention(a.smenu.pathPrefix, a.smenu.pathDir, sel.Name)
					if !strings.HasSuffix(text, "/") {
						text += " "
					}
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
		a.ToggleBoxExpand()
		return
	case "paste-image":
		// omp's app.clipboard.pasteImage: the one paste no terminal can hand
		// us, because a bitmap never reaches an app through a keystroke. The
		// read shells out, like the text copy on this path already does.
		a.pasteClipboard()
		return
	case "retry":
		// F5: re-run the current session's last turn. The host owns the
		// running / nothing-to-retry guards (it holds the turn and the
		// store); the chord is a bare notice only when the mode never wired
		// a retry at all.
		if a.onRetry != nil {
			a.onRetry()
			a.poke()
			return
		}
		a.AddSystemBlock("retry is not wired in this build")
		return
	case "thinking-toggle":
		// Shift-Tab. Like the dock chords this runs after every modal
		// handler, so an open picker keeps first claim on the key (the model
		// picker binds Shift-Tab to "previous tab").
		a.ToggleThinking()
		return
	case "dock-cycle", "dock-fold":
		// The context dock's own two chords (#291 §1), handled here — after
		// every modal handler — so a card or a picker open on screen keeps
		// first claim on the key. The panel is chrome, never a modal's input.
		a.dockKey(action)
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

	// Multi-row drafts: Up/Down walk the composer's visual rows first, the
	// same order omp's editor uses (cursorUp/cursorDown move a row; history
	// is only reached at the buffer edge). moveLine reports false when the
	// cursor is already on the first/last row — or the draft fits one row —
	// and HandleKey below keeps the history-recall contract there.
	switch key.Key() {
	case tcell.KeyUp, tcell.KeyDown:
		dir := 1
		if key.Key() == tcell.KeyUp {
			dir = -1
		}
		if a.ed.moveLine(dir, a.composerAvail()) {
			a.poke()
			return
		}
	}

	// Editor keys. Text is captured BEFORE HandleKey — the editor archives
	// and resets itself when it reports send.
	draft := strings.TrimSpace(a.ed.Text())
	// Which images ride with this message is read from the same snapshot, for
	// the same reason: sending resets the buffer, and the buffer is what says
	// which payloads are live (paste.go).
	imgs := a.ImagesFor(draft)
	send := a.ed.HandleKey(key)
	// A chip the key removed retires its payload: nothing may be sent that the
	// composer no longer shows.
	a.dropStalePastes()
	a.syncSlashMenu()
	if send {
		// The prompt is on its way: an Esc-cleared draft from before it is no
		// longer "the last thing I undid", and resurfacing it after a send
		// would put two prompts in the composer that were never both there.
		a.escDraft, a.escUsed = nil, false
		// slash command routing (issue #11): a command is consumed by the
		// router — no user block, no agent run. A command sees the draft as
		// typed, chips included: its argument is not a place to lose a name.
		if dispatch(a, draft) {
			a.smenu = nil // editor reset — the dropdown is moot
			a.poke()
			return
		}
		// The wire text drops an attached image's chip, because the image part
		// is its rendering. The transcript row keeps the draft, so the screen
		// shows what was composed.
		text := draft
		if len(imgs) > 0 {
			text = a.expandPastes(draft)
		}
		a.mu.Lock()
		a.blocks = append(a.blocks, &Block{Kind: KindUser, Text: draft})
		a.sm.Bottom()
		a.mu.Unlock()
		if len(imgs) > 0 {
			// Attachments make this the multimodal path's alone. A declined
			// or unwired send returns the draft to the composer: the text-only
			// handler cannot carry the bytes, and sending the chip as a word
			// would let a model answer a picture it never received.
			if a.onSendImages != nil && a.onSendImages(text, imgs) {
				a.poke()
				return
			}
			a.returnDraft(draft, imgs)
			return
		}
		if a.onSend != nil {
			a.onSend(text)
		}
	}
	a.poke()
}

// returnDraft puts a draft that could not be sent back in the composer and
// says why on the divider. It exists because an attached image has no text
// fallback: the alternative is a cleared box, a lost screenshot, and a user who
// does not know either happened. The chips and the payloads go back together,
// so one Enter retries.
func (a *App) returnDraft(draft string, imgs []PasteImage) {
	if a.ed.Text() == "" {
		a.ed.InsertText(draft) // the send path has only just emptied it
	}
	a.images.keep(imgs)
	a.images.drop(a.ed.Text())
	a.mu.Lock()
	running := a.st.Running
	a.mu.Unlock()
	why := "this mode cannot send images"
	switch {
	case a.onSendImages != nil && running:
		why = "a turn is already running — Esc cancels it, then send again"
	case a.onSendImages != nil:
		why = "the send declined the attachment (a guest room forwards text only)"
	}
	a.setNotice(fmt.Sprintf("not sent: %d image(s) — %s", len(imgs), why))
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
	if a.pathList != nil {
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
	// top bar (transcript only) + scrollback + blank + composer (grows with
	// the draft) + status row.
	return a.height - a.composerRows() - 2 - a.transcriptTop()
}

// contentWidth is the scrollback text width (rail + padding removed), and with
// the context dock open everything to its right edge: the transcript's lines
// wrap where the panel begins, so no row is ever cut mid-glyph by the border.
func (a *App) contentWidth() int {
	w := a.rightEdge() - 4 // rail(1) + gap(1) + right pad(2)
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
		// "Thought for Xs" (done), in the same rounded frame a finished tool
		// result gets. Reasoning is the one block that can outgrow any screen,
		// so the frame shows a fixed window the wheel scrolls and Ctrl+O drops
		// (issue #20).
		if !a.showThinking {
			break
		}
		lines = a.thinkBoxLines(b, w)
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

// boxTop is an outlined frame's opening row: the left corner, the horizontal
// rule, and an optional bold label set into it (a tool's name, a reasoning
// block's state). boxBottom closes the same frame; both are shared by every
// boxed surface so a second frame cannot drift from the first.
func boxTop(box theme.BoxChars, st tcell.Style, label string, w int) line {
	top := line{runs: []cell{{text: box.TopLeft, style: st, chrome: true}}}
	if label == "" {
		top.runs = append(top.runs, cell{text: strings.Repeat(box.Horizontal, max(1, w-2)) + box.TopRight, style: st, chrome: true})
		return top
	}
	label = truncateCells(label, max(1, w-6), "…")
	// The label is content and the rule around it is frame: a drag over the
	// top border copies the tool's name, not the glyphs framing it.
	top.runs = append(top.runs,
		cell{text: box.Horizontal + " ", style: st, chrome: true},
		cell{text: label, style: st.Bold(true)},
		cell{text: " " + strings.Repeat(box.Horizontal, max(1, w-5-width(label))) + box.TopRight, style: st, chrome: true})
	return top
}

func boxBottom(box theme.BoxChars, st tcell.Style, w int) line {
	// A closing rule carries no label: every cell of it is frame, so the row
	// copies as the blank line it looks like.
	return line{runs: []cell{{text: box.BottomLeft + strings.Repeat(box.Horizontal, max(1, w-2)) + box.BottomRight, style: st, chrome: true}}}
}

// boxRow frames one interior row: side borders, one pad cell each, and the text
// padded to the interior width so every right border lands on the same column.
func boxRow(box theme.BoxChars, border, st tcell.Style, s string, inner int) line {
	body := strings.TrimRight(fitWidth(s, inner), " ")
	return line{runs: []cell{
		{text: box.Vertical + " ", style: border, chrome: true},
		{text: body, style: st},
		{text: strings.Repeat(" ", inner-width(body)) + " " + box.Vertical, style: border, chrome: true},
	}}
}

// boxSelectable is the inverse of the frame builders: from a row's painted text
// and the column that text starts at, it returns what a selection should copy
// and the column that copy starts at. x0 moves with every dropped cell because
// the highlight (drawSelection) and the copy (cellSlice) both measure from it —
// stripping a border without moving x0 would shift every slice two cells left.
// A box's side borders and the pad cells inside them go, and so does the pad a
// body row is filled out to its right border with, so a copied box reads as its
// text and not as its frame. A rule row keeps only the label set into it
// (`╭─ bash ───╮` copies as `bash`; a bare `╰───╯` as nothing): the label is
// content, the rule around it is the frame.
//
// ponytail: this path recognises a frame by its glyphs, because the surfaces
// that need it — the composer, the picker panel — paint their borders cell by
// cell rather than as runs, so there is no run to mark. Transcript rows never
// come through here (they are captured as runs, where cell.chrome is exact), so
// the only text it can misread is a cell-by-cell surface whose content happens
// to open with `│ ` and close with ` │`. If such a row ever appears, the upgrade
// path is to paint that surface as runs like the frame builders do and let
// cell.chrome carry it.
func boxSelectable(box theme.BoxChars, s string, x0 int) (string, int) {
	// A boxed surface paints its frame inset from the grid's left edge, so the
	// blanks before it are frame too; a row that is not boxed at all (the top
	// bar, welcome, the status row) is returned untouched, indent and all.
	n := len(s) - len(strings.TrimLeft(s, " "))
	t := s[n:]
	for _, c := range []string{box.TopLeft, box.BottomLeft} {
		if rest, ok := strings.CutPrefix(t, c); ok {
			return ruleLabel(box, rest, x0+n+width(c))
		}
	}
	rest, ok := strings.CutPrefix(t, box.Vertical+" ")
	if !ok {
		return s, x0
	}
	body, ok := strings.CutSuffix(strings.TrimRight(rest, " "), " "+box.Vertical)
	if !ok {
		return s, x0
	}
	return strings.TrimRight(body, " "), x0 + n + width(box.Vertical+" ")
}

// ruleLabel is the label a rule row carries: what sits between the corner and
// the horizontals, or between two runs of them. Nothing but horizontals is a
// close, and nothing at all is a bare rule — both copy as no text.
func ruleLabel(box theme.BoxChars, rest string, x0 int) (string, int) {
	for strings.HasPrefix(rest, box.Horizontal) {
		rest, x0 = rest[len(box.Horizontal):], x0+width(box.Horizontal)
	}
	label := rest
	if i := strings.Index(label, box.Horizontal); i >= 0 {
		label = label[:i]
	}
	if label == box.TopRight || label == box.BottomRight {
		label = ""
	}
	x0 += len(label) - len(strings.TrimLeft(label, " "))
	return strings.TrimSpace(label), x0
}

// thinkRows is a reasoning block's body, wrapped to the frame's interior width.
// Empty reasoning (a block that has not streamed a delta yet) has no rows: the
// frame still opens, so the reader sees that reasoning began.
func (a *App) thinkRows(b *Block, w int) []string {
	body := strings.TrimRight(b.Text, "\n")
	if body == "" {
		return nil
	}
	return wrap(sanitizeOutput(body), max(1, w-4))
}

// thinkMaxOff is the largest offset a reasoning box's window can use: past it
// the window is already at the oldest thought, so a wheel there has nothing
// left to scroll. One definition, because the render and the wheel must agree
// on where the box stops.
func thinkMaxOff(n int) int { return max(0, n-thinkBoxRows) }

// thinkWindow slices a block's wrapped reasoning to the box's window: `off`
// rows above the newest thought, at most thinkBoxRows tall. The window is
// tail-anchored, the same way the transcript counts its own offset, so the
// newest thought is what a reader following the turn sees. An offset past
// either end reads as that end, never as an empty frame.
func thinkWindow(n, off int) (start, end int) {
	end = n - clamp(off, 0, thinkMaxOff(n))
	return max(0, end-thinkBoxRows), end
}

// thinkBoxLines renders one reasoning block in the same rounded frame a result
// gets: the top border carries the state ("⠹ Thinking…" while it streams,
// "Thought for Xs" once it settles) and the body shows a fixed window of it —
// at most thinkBoxRows rows, scrolled by the wheel over the box
// (Block.ThinkOff) and dropped entirely by Ctrl+O. The full reasoning always
// stays in the session JSONL, so the window is a view, never the record.
func (a *App) thinkBoxLines(b *Block, w int) []line {
	box := a.th.Box()
	border := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentThinking)))
	bodySt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim)))
	inner := max(1, w-4) // side borders + one pad cell each

	hdr := "Thought"
	switch {
	case b.stream:
		hdr = "⠹ Thinking…"
	case b.thinkDur > 0:
		hdr = fmt.Sprintf("Thought for %.1fs", b.thinkDur.Seconds())
	}

	rows := a.thinkRows(b, w)
	start, end := 0, len(rows)
	if !b.Expanded {
		start, end = thinkWindow(len(rows), b.ThinkOff)
	}
	// The hidden-row notice leads the window the way it does in a result box:
	// following the tail, what is elided is the head. Scrolled up, the count
	// also covers the rows the wheel has yet to come back to — one notice
	// beats two at this height.
	out := []line{boxTop(box, border, hdr, w)}
	if hidden := len(rows) - (end - start); hidden > 0 {
		out = append(out, boxRow(box, border, bodySt, fmt.Sprintf("… %d rows hidden (Ctrl+O to expand)", hidden), inner))
	}
	for _, wl := range rows[start:end] {
		out = append(out, boxRow(box, border, bodySt, wl, inner))
	}
	return append(out, boxBottom(box, border, w))
}

// scrollThinkBox routes a wheel notch to the reasoning box under the pointer:
// over the box the notch moves the box's own window (Block.ThinkOff) instead of
// the transcript, so the wheel reads the reasoning it is pointing at. It reports
// whether the notch was consumed — at either end of the box's own content the
// wheel falls through to the transcript, which is the way back out.
func (a *App) scrollThinkBox(m *tcell.EventMouse, down bool) bool {
	_, y := m.Position()
	a.mu.Lock()
	top, vp := a.selViewport() // syncs the layout this hit-test reads
	hdr := a.transcriptTop()
	var b *Block
	if bi := a.rowIdx.blockAt(int32(top + y - hdr)); vp > 0 && y >= hdr && y < hdr+vp && bi >= 0 {
		b = a.blocks[bi]
	}
	if b == nil || b.Kind != KindThinking {
		a.mu.Unlock()
		return false
	}
	// ThinkOff counts rows above the newest thought, so the wheel's own sense
	// inverts here: rolling up walks back through the reasoning, rolling down
	// returns to the live edge. A notch that cannot move the window is not a
	// scroll, and reporting it unconsumed hands it back to the transcript.
	step := -1
	if !down {
		step = 1
	}
	off := clamp(b.ThinkOff+step, 0, thinkMaxOff(len(a.thinkRows(b, a.contentWidth()))))
	consumed := off != b.ThinkOff
	b.ThinkOff = off
	a.mu.Unlock()
	if consumed {
		a.poke()
	}
	return consumed
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
	top := boxTop(box, border, label, w)

	// cellRow frames an already-wrapped styled row: boxRow's border and
	// padding, but the padding keeps each run's own style so a coloured row's
	// cells land on the same column as a plain one. Runs past the interior
	// budget are cut here rather than allowed to push the right border out of
	// alignment.
	cellRow := func(ln line) line {
		framed := line{runs: []cell{{text: box.Vertical + " ", style: border, chrome: true}}}
		col := 0
		for _, r := range ln.runs {
			if col >= inner {
				break
			}
			if col+width(r.text) > inner {
				r.text = truncateCells(r.text, inner-col, "")
			}
			framed.runs = append(framed.runs, r)
			col += width(r.text)
		}
		if col < inner {
			framed.runs = append(framed.runs, cell{text: strings.Repeat(" ", inner-col), chrome: true})
		}
		framed.runs = append(framed.runs, cell{text: " " + box.Vertical, style: border, chrome: true})
		return framed
	}

	// The render window: Ctrl+O drops it and prints everything the tool kept
	// (bounded by the tool's own sink cap, not by this renderer).
	headRows, tailRows := toolWindow(a.trimTier(i))

	// A file change paints its diff: the coloured rows replace the plain
	// preview the model was shown (same change, plus the context around it),
	// and the tool's own first line survives above them as the header, so the
	// box still names what it did. Bash gets the same treatment only when its
	// output actually reads as a unified diff — painting a list whose rows
	// happen to start with "-" as a change would be a lie, which is why the
	// structured path (b.Diff, written by a tool that made the change) needs
	// no such test.
	body := sanitizeOutput(strings.TrimRight(b.Text, "\n"))
	diff, header := b.Diff, ""
	switch {
	case diff == "" && b.ToolName == "bash" && DiffLooksUnified(body):
		diff, body = body, ""
	case diff != "":
		header, body = splitDiffHeader(body)
	}

	var rows []line
	if diff != "" {
		rows = a.diffCells(sanitizeOutput(diff), inner)
	} else {
		for _, wl := range wrap(body, inner) {
			rows = append(rows, textline(wl, bodySt))
		}
	}

	var out []line
	out = append(out, top)
	if header != "" {
		out = append(out, boxRow(box, border, bodySt, header, inner))
	}
	switch {
	case len(rows) == 0 && !b.Err:
		out = append(out, boxRow(box, border, mutedSt, "(no output)", inner))
	case len(rows) == 0:
		// An error result with nothing to say: the frame and footer carry it.
	case b.Expanded || len(rows) <= headRows+tailRows+1:
		// Aged results collapse to a head+tail window: the middle of the
		// output is trimmed before anything else in the transcript is.
		for _, ln := range rows {
			out = append(out, cellRow(ln))
		}
	default:
		for _, ln := range rows[:headRows] {
			out = append(out, cellRow(ln))
		}
		// The notice sits at the hole it describes, between the head and
		// the tail — omp prints its hidden-line count the same way.
		out = append(out, boxRow(box, border, dimSt, fmt.Sprintf("… %d lines hidden (Ctrl+O to expand)", len(rows)-headRows-tailRows), inner))
		for _, ln := range rows[len(rows)-tailRows:] {
			out = append(out, cellRow(ln))
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
		out = append(out, boxRow(box, border, notesSt, "⟦"+strings.Join(notes, " | ")+"⟧", inner))
	}

	out = append(out, boxBottom(box, border, w))
	return out
}

// splitDiffHeader keeps the part of a tool's output that the diff cannot say:
// its leading one-line reports (the moved notice, the [path#TAG] snapshot, the
// write summary). It stops where the render window starts — the "N:text" rows
// a file preview is made of — because those restate the change the coloured
// rows now carry. body returns the dropped remainder.
func splitDiffHeader(body string) (header, rest string) {
	if body == "" {
		return "", ""
	}
	var keep []string
	lines := strings.Split(body, "\n")
	for i, ln := range lines {
		if isWindowRow(ln) {
			return strings.Join(keep, "\n"), strings.Join(lines[i:], "\n")
		}
		keep = append(keep, ln)
	}
	return strings.Join(keep, "\n"), ""
}

// isWindowRow reports a render-window row: a 1-based line number and a colon.
func isWindowRow(s string) bool {
	i := 0
	for i < len(s) && '0' <= s[i] && s[i] <= '9' {
		i++
	}
	return i > 0 && i < len(s) && s[i] == ':'
}

// --- drawing ---

// draw paints the state into the screen buffer (paint, under a.mu) and then
// flushes to the tty WITHOUT the lock. The flush is a synchronous write a
// suspended terminal or full tty buffer can block for minutes — measured
// 12m9s (#283 RCA §1: held across it, that freeze starved every provider
// delta waiting on a.mu and let the stream watchdog kill a healthy stream).
// With Show outside the lock, a frozen tty costs display freshness only;
// the agent goroutine is never blocked by it.
func (a *App) draw() {
	a.paint()
	a.scr.Show()
}

// paint renders the current state into the screen buffer. a.mu guards the
// state reads; it must NEVER be held across the Show() flush (see draw).
func (a *App) paint() {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.scr
	w, h := a.width, a.height
	s.Clear()
	// Per-frame facts about the viewport: a frame that draws no transcript
	// (welcome, /clear) must not keep last frame's scroll hint, nor its
	// selection capture — rows recorded before /clear would copy text that is
	// no longer on screen.
	// The scrollbar's geometry is the same per-frame fact: a welcome frame that
	// draws no bar must not leave last frame's grab live on the last column.
	a.scrollHint, a.selRows, a.selBarOn = "", nil, false

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
		// A gesture made before the first block exists — the composer is the
		// only selectable surface there — is highlighted here too; the branch
		// returns, so it never reaches the call at the end of paint().
		a.drawSelection()
		return
	}

	// Grok layout: top bar, scrollback, blank row, composer box (grows
	// with the draft's wrapped line count), status row at the bottom.
	// The top bar is chrome: the transcript viewport starts below it.
	top := a.transcriptTop()
	cRows := a.composerRows()
	vp := h - cRows - 2 - top
	if vp < 1 {
		vp = 1
	}
	a.drawTopBar(s, w, true)
	// The panel is built before the transcript's width is computed: with it open
	// the lines wrap at its left edge, and a frame that painted the transcript
	// first would have to redo every render cache entry it drew.
	a.dockBuild()
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
	// The dock's left edge is the transcript's right edge: band fills, the
	// scrollbar and the right-aligned timestamps all stop there, or a row that
	// scrolled under the panel would paint through its border.
	edge := a.rightEdge()
	bandLim := edge
	if sbOk {
		bandLim = edge - 1
	}
	selRows := make([]selRow, 0, end-start)
	for row, r := range a.viewRows(int32(start), int32(end)) {
		y := row + top
		// A banded row (user prompt / code fence) carries one background
		// that the fill, the rail, the runs and the timestamp must all
		// share: tcell's zero background is ColorDefault, so a style that
		// omits it resets the cell under the glyphs to the terminal's own
		// colour instead of the band.
		banded := r.ln.bg != 0
		if banded {
			// Fill the full width so the band reads as one continuous row
			// (grok semantic band).
			for bx := range bandLim {
				s.SetContent(bx, y, ' ', nil, tcell.StyleDefault.Background(r.ln.bg))
			}
		}
		x := 3 // rail(1) + pad(2); user bands start their runs at x=0
		if banded && len(r.ln.runs) > 0 && r.ln.runs[0].text == "❯ " {
			x = 0
		}
		if r.rail != "" {
			railS := r.railS
			if banded {
				railS = railS.Background(r.ln.bg)
			}
			drawText(s, 0, y, r.rail, railS)
		}
		// What this row contributes to a selection leaves the chrome out: a
		// frame's border and padding are painted, never copied, and the copy
		// starts at the column its first content cell occupies, which is the
		// column the highlight measures from — so what is painted is what is
		// copied. A row that is chrome end to end (a bare rule) records no
		// text and copies as the blank line it looks like.
		startX, content := -1, strings.Builder{}
		for _, run := range r.ln.runs {
			st := run.style
			if banded {
				// Runs carry the band too, or the row fill survives only
				// in the gaps between the glyphs.
				st = st.Background(r.ln.bg)
			}
			drawText(s, x, y, run.text, st)
			if !run.chrome {
				if startX < 0 {
					startX = x
				}
				content.WriteString(run.text)
			}
			x += width(run.text)
		}
		if startX < 0 {
			startX = 0
		}
		// Selection hit-testing works off this text (the streaming
		// cursor is decoration, not content).
		selRows = append(selRows, selRow{text: strings.TrimSuffix(content.String(), "▍"), x0: startX})
		// Right-aligned dim timestamp (grok draws these on first rows).
		if r.ts != "" {
			tsSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim)))
			if banded {
				tsSt = tsSt.Background(r.ln.bg)
			}
			drawText(s, edge-width(r.ts)-2, y, r.ts, tsSt)
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
			drawText(s, edge-1, y+top, ch, st)
		}
	}
	// Publish the bar's geometry for the mouse hit-test (selection.go): the
	// press that grabs the thumb and the drag that moves it act on exactly the
	// bar painted here — and on no bar at all when the transcript fits, since
	// sbOk false is what keeps grab off a column that carries content.
	a.selBarOn, a.selBarVP, a.selBarPos = sbOk, vp, sbStart
	a.selBarW, a.selBarTotal, a.selBarThumb = 0, total, sbEnd-sbStart
	if sbOk {
		a.selBarW = 1
	}
	a.selRows, a.selTop = selRows, start
	if a.selDown {
		a.selCacheRows(start) // keep the text a held drag has already passed
	}
	// The highlight is not painted here: the composer, the status row and every
	// overlay below repaint their own cells in normal video, and a highlight
	// painted under them is wiped by the same frame (a drag over the composer
	// copied text and showed nothing, since the composer is exactly the row the
	// user drags first). It is a screen overlay, so paint() draws it last.
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
	// The composer's first input row sits below the transcript; it occupies
	// composerRows() rows above the status line.
	composerTop := h - 1 - cRows
	// The panel first, so every overlay below paints over it: the dock is chrome
	// beside the transcript, never a surface a modal has to negotiate with.
	if a.dockOn() {
		dtop, dh := a.dockGrid()
		a.drawDock(s, w-dockCols, dtop, dh)
	}
	a.drawSessionPicker(composerTop)
	a.drawHubRoster(composerTop)
	a.drawTreeSelector(composerTop)
	a.drawPicker(composerTop)
	a.drawSlashDropdown(composerTop)
	a.drawAskCard(composerTop)
	a.drawComposer(composerTop)
	a.drawStatusRow(h - 1)
	// Last, so it paints over every surface the frame just drew: see the note
	// where the selection geometry is published above.
	a.drawSelection()
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
	// The row window follows the terminal, not a fixed 12: the panel
	// floats above the composer, so the room above it is the real budget
	// (clamped below), and the selection scrolls the list past the window.
	rows := pickerRowCeiling(a.height)
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

	// View tabs (omp's per-provider views): the active one is bright. The tab
	// strip's row is published for the mouse hit-test.
	p.tabY, p.tabAt = -1, nil
	if tabs {
		p.tabY = y
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
	// The name column is the terminal's leftovers: the box, the 6 cells of
	// marker/dot/indent at the row's head, and the detail column the row
	// keeps. The old fixed 28 wrap made a wide terminal ellipsize names it
	// had room to print, and a narrow one spend half the row on a detail
	// that was then clipped away. The floor keeps a name legible when the
	// row is too narrow for both — the detail is the part that drops (the
	// room>4 guard below), not the thing being picked.
	labelW = min(labelW+2, max(12, inner-6-pickerDetailCols))
	// Publish the row map the mouse router hit-tests against, so a click lands
	// on exactly the row the user saw.
	p.hitY0, p.hitItem = y, make([]int, len(lines))
	for i, ln := range lines {
		p.hitItem[i] = -1 // a section header is not a target
		if !ln.header {
			p.hitItem[i] = ln.itemIdx
		}
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
		// Each tab owns its label plus the gap after it, so a click anywhere
		// in that span switches to the view it names.
		p.tabAt = append(p.tabAt, x)
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

// composerAvail is the editor's text width in cells inside the prompt box
// (border+pad+prefix+right pad+border).
func (a *App) composerAvail() int {
	avail := a.width - 7
	if avail < 4 {
		avail = 4
	}
	return avail
}

// composerBudget is the most input rows the box may paint: the screen minus
// everything else it shares the terminal with (top bar, one transcript row,
// the box's own borders, the status row) — draw()'s viewport arithmetic
// solved for the composer. Without a ceiling the box grew past the screen:
// its top edge climbed above row 0, drawComposer bailed on yTop < 1, and a
// large paste showed NOTHING while the full draft sat in the buffer — the
// "composer is empty but Enter sends it all" bug.
func (a *App) composerBudget() int {
	n := a.height - 5 - a.transcriptTop()
	if n < 1 {
		n = 1
	}
	return n
}

// composerView returns the input rows the box paints plus the cursor's cell
// within them and the draft rows hidden above/below the window. An embedded
// newline is a hard row break (Ctrl+J / Alt+Enter), a long line wraps at the
// available width so the box grows instead of truncating, and past the
// budget it pages to the cursor (rowWindow) instead of leaving the screen.
// The same wrapRows geometry backs the Up/Down walk in Editor.moveLine:
// arrows traverse exactly what is painted, and walking off the painted edge
// scrolls the window with the cursor.
func (a *App) composerView() (lines []string, curRow, curCol, above, below int) {
	rows := wrapRows(a.ed.buf, a.composerAvail())
	full, col := cursorCell(a.ed.buf, rows, a.ed.cur)
	lo, hi := rowWindow(len(rows), full, a.composerBudget())
	for _, rs := range rows[lo:hi] {
		lines = append(lines, string(a.ed.buf[rs.start:rs.end]))
	}
	return lines, full - lo, col, lo, len(rows) - hi
}

// composerInputLines is the painted window without the scroll counts.
func (a *App) composerInputLines() (lines []string, curRow, curCol int) {
	lines, curRow, curCol, _, _ = a.composerView()
	return
}

// composerRows is the total height of the prompt box (top border, painted
// input rows, bottom divider).
func (a *App) composerRows() int {
	lines, _, _, _, _ := a.composerView()
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
	// above/below count the draft rows the window hides, and decide both
	// where the ❯ prefix belongs and what the divider's hint says.
	lines, curRow, curCol, above, below := a.composerView()
	promptStyle := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentUser))).Bold(true)
	vert := boxRune(box.Vertical)
	for i, ln := range lines {
		y := yTop + i
		a.scr.SetContent(1, y, vert, nil, bs)
		a.scr.SetContent(w-2, y, vert, nil, bs)
		if i == 0 && above == 0 {
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
		if x == 5 && a.ed.Text() == "" {
			// grok's "Type a message…": an empty prompt is not a blank void.
			// Drawn here, after the text row, because the fill below would
			// otherwise blank it back to spaces.
			ph := "Type a message…"
			drawText(a.scr, 5, yTop+i, ph, ms.body.Foreground(a.cellColor(a.th.Get(theme.GrayDim))))
			x += width(ph)
		}
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
	// The viewport hint rides this divider's right end. It used to be
	// painted on transcript row 0, where it overwrote whatever content had
	// scrolled to the top: a long thinking line, or the last prompt, looked
	// like it had gone static in the first line. The divider is chrome, so it
	// takes the pixels instead; when the divider is too narrow for both, the
	// hint is dropped rather than eating the model name. Three hints want the
	// slot, in this order: a fresh copy confirmation (the only proof the mouse
	// gesture did anything, since the app holds the mouse and the terminal
	// stays quiet), then the draft's own hidden rows — text the user is
	// composing right now beats scrollback they already read — then the
	// transcript's ▲n▼n.
	hint := a.copyHint()
	if hint == "" {
		hint = draftHint(above, below)
	}
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

	// Cursor: blinking block at the editor position inside the painted window.
	cx := 5 + curCol
	a.scr.ShowCursor(min(cx, w-3), yTop+curRow)
}

// draftHint names the composer rows the window hides, if any.
func draftHint(above, below int) string {
	switch {
	case above > 0 && below > 0:
		return fmt.Sprintf("draft ▲%d ▼%d", above, below)
	case above > 0:
		return fmt.Sprintf("draft ▲%d", above)
	case below > 0:
		return fmt.Sprintf("draft ▼%d", below)
	}
	return ""
}

// drawStatusRow renders the bottom row: the working directory on the left,
// the configured HUD segments (settings statusLine.segments) right-aligned
// (caller holds a.mu). The keyboard chords used to live on the left; /hotkeys
// and the welcome menu carry them now, which frees the room the metrics need
// on a small terminal.
func (a *App) drawStatusRow(y int) {
	parts := a.hudParts()
	// The work timer and the decode rate are what the row is for during a
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
// and model first, the work timer and the rate last).
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

// defaultStatusSegments is the shipped layout: the work timer, the token
// counters, the live context total and the decode speed, right-aligned (the
// timer reads leftmost so the rate's own " │ " stays the row's right edge).
// The context segment is the number: what this session's context costs against
// the model's window (ctx 92k/200k), read from Status.CtxUsed — the last
// request's provider-reported total, cached input included, which covers the
// system prompt, the visible history and the tool schemas. It hides while
// either half is unknown (an undiscovered window, or a session that has not
// answered yet), so a fresh run keeps a clean row. The model keeps its
// composer divider slot, which is chrome rather than a segment.
var defaultStatusSegments = []string{"time", "tokens", "context", "rate"}

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
		// Total time spent WORKING: the banked spans plus the live one. An idle
		// agent — and an agent parked on a question card — shows a frozen
		// number; the wall clock between turns belongs to the user, not to the
		// session. "0s" is a reading, so the segment never hides.
		return humanDur(a.activeWork()), theme.StatusLineSpend
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
// narrow row take: the work timer and the decode rate, separated by a
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
// runs out of width. The decode rate and the work timer are what the
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
