package tui

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
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
// TabInfo is one open session the status row and tab cycle expose.
// cmd owns the set; the App only paints what it is given.
type TabInfo struct {
	ID, Title                string
	Running, Unread, Current bool
}

type Status struct {
	Model     string
	SessionID string
	// TokensIn is the session's FRESH input — the provider's input
	// exclusive of cache reads, which is the split every provider
	// normalizes onto (input + output + cacheRead = totalTokens) and
	// the split compaction prices against. It is NOT the prompt the
	// model saw: on a cached conversation that is TokensCache too, and
	// the "tokens" segment shows both.
	TokensIn  int64
	TokensOut int64
	// TokensCache is the prompt-cache reads billed across the session,
	// and TokensThink the reasoning already counted inside TokensOut
	// (completion_tokens is inclusive of reasoning_tokens on every
	// wire xdev speaks). Both are tracked so the status row can show
	// the split the glyphs claim instead of one half of it.
	TokensCache int64
	TokensThink int64
	// ToolCalls counts the tool calls this session opened, ToolErrors the
	// ones that came back failed, and ToolWork the wall time they took.
	// AddToolBlock counts a call, FinishTool settles it. These are the
	// /usage panel's third block: the status row answers "how much", the
	// panel answers "spent how long and on what".
	ToolCalls  int
	ToolErrors int
	ToolWork   time.Duration
	// TokensCacheWrite is the prompt-cache WRITES billed across the
	// session, kept beside TokensCache (the reads) because the wire
	// reports them as separate buckets and the token pill's dialog breaks
	// out all three. Zero on a provider that reports no cache writes.
	TokensCacheWrite int64
	// Turns and Steps count what the session asked for: one turn per
	// finished run, one step per provider request inside it — dsh's
	// TimePill "{turns} turns {steps} steps". They are rendered by the
	// pill alone, which is the only place that has the room.
	Turns int
	Steps int
	// LLMWork is the wall time the provider's own messages took
	// (msg.DurationMS — request to last token, queue included), and
	// TTFTCount/TTFTSum the turns' time-to-first-token so /usage can report
	// an average instead of the last reading the status row shows.
	LLMWork   time.Duration
	TTFTSum   int64
	TTFTCount int64
	// Cost is the session spend in USD (0 when the provider reports none),
	// CtxWindow the model's context window (0 = unknown) and Rate the last
	// measured decode speed in output tokens/second (0 = never measured).
	// All three feed the optional HUD segments (statusLine.segments).
	Cost      float64
	CtxWindow int64
	// TTFT is the last completed turn's time-to-first-token in ms
	// (0 = no turn has finished). SetTTFT writes it; the "⌚ ttft"
	// HUD segment reads it.
	TTFT int64
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
	scr tcell.Screen
	th  *theme.Theme
	// md/mdFor/mdSet are mdStyle's memo (markdown.go). mdFor is the theme the
	// memo was built from, so SetTheme — which swaps a.th — rebuilds it.
	md     mdStyle
	mdFor  *theme.Theme
	mdSet  bool
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
	// debugMouse renders every mouse event on the status bar
	// (settings `tui.debugMouse`, off by default): the button, the
	// press/drag/release edge, the wheel direction and the coordinates.
	// It is the only way to see what the terminal is actually sending —
	// tcell strips the SGR motion bit, so a held drag looks like a
	// press at every report, and the gesture a user thinks they made
	// is not always the one that arrives.
	// (UI thread; mu-guarded.)
	debugMouse     bool
	debugMouseLine string
	logMu          sync.Mutex
	logFile        *os.File
	keyMap         *KeyMap // remappable keybinding layer
	st             Status
	tabs           []TabInfo // open sessions (cmd owns the set; App paints)
	onTabCycle     func(dir int, onlyUnread bool)
	onTabPick      func(id string) error // /tabs row: focus that open session
	// onTabClose closes one open session (opencode session_delete): the
	// session.delete chord and a click on a tab's × both land here, and a
	// nil degrades both to a notice.
	onTabClose func(id string) error
	// tabHits is the tab strip's clickable geometry, published each frame by
	// the painter (tabstrip.go).
	tabHits []tabHit
	// The decode window of the message being streamed: the first and last
	// delta. AddUsage closes the window and turns it into st.Rate;
	// BeginMessage (one EventStart) discards one a dead turn left open, so
	// the next message's rate is never divided by the previous one's elapsed
	// time. Guarded by mu.
	deltaFirst, deltaLast time.Time

	// cmd is the active tool call the session is running (set by
	// the agent loop via BeginActiveCommand, displayed left of the
	// path). cmdMu guards cmd/cmdActive/cmdDepth separately from st
	// so the drawer never reads a half-written command. cmdDepth is
	// a COUNT, not a flag: same-batch calls run concurrently, so the
	// first of two to finish must not clear the indicator while the
	// other is still in flight.
	cmd       string
	cmdActive bool
	cmdDepth  int
	cmdMu     sync.Mutex
	// statusSegs is the HUD segment order (settings statusLine.segments);
	// empty = defaultStatusSegments.
	statusSegs []string
	// ask is the blocking ask card (#46/#36); nil = closed.
	ask *askState

	// statusPop is the detail panel a click on a status-row pill opens,
	// and statusHits the pills' rectangles the painter published this
	// frame — the same publish-then-hit-test contract a.jump keeps, so a
	// pill that is not on screen cannot be clicked (statuspill.go).
	statusPop  *statusPopup
	statusHits []statusHit

	// diffOv is the full-width diff overlay shown when the user
	// clicks a changed file in the dock; nil when closed.
	diffOv *diffOverlay

	// msgm is the user-message menu a click on a ❯ row opens; nil = closed.
	msgm *msgMenu
	// msgv is the read-only message surface behind the menu's "jump" row.
	msgv *msgView
	// msgArmed records that a press landed on a user prompt. The menu opens on
	// the no-motion RELEASE of that press, not on the press itself, so a drag
	// that starts on a prompt still selects text (see msgmenu.go).
	msgArmed bool
	// msgFire is a menu action built under a.mu and run after unlocking — the
	// session rewind replays the transcript and must not run locked.
	msgFire func()

	// showThinking renders model reasoning blocks in the transcript
	// (settings key `showThinking`, toggled by /settings; issue #20).
	showThinking bool

	// renderMermaid draws a ```mermaid fence as a diagram instead of a code
	// band (settings key `renderMermaid`, default on). It is display-only and
	// per-render, so flipping it re-stamps nothing but the render cache: the
	// next frame redraws the same blocks as source.
	renderMermaid bool

	width, height int

	// Wired by cmd: onSend runs the agent turn; onCancel aborts it; onQuit exits.
	ops           *SessionOps
	modelOps      *ModelOps      // session lifecycle, wired by cmd (nil → notices)
	planOps       *PlanOps       // /plan, wired by cmd (nil → notices)
	autoAnswerOps *AutoAnswerOps // /auto-answer, wired by cmd (nil → notices)
	// version is the build this process is (`xdev version`), painted in the
	// context dock's footer; empty (a dev build) paints nothing.
	version            string
	advisorOps         *AdvisorOps                            // /advisor, wired by cmd (nil → notices)
	memoryOps          *MemoryOps                             // /memory, wired by cmd (nil → notices)
	themeOps           *ThemeOps                              // /theme, wired by cmd (nil → notices)
	connectOps         *ConnectOps                            // /connect, wired by cmd (nil → notices)
	prewalkOps         *PrewalkOps                            // /prewalk, wired by cmd (nil → notices)
	goalOps            *GoalOps                               // /goal, wired by cmd (nil → notices)
	scheduleOps        *ScheduleOps                           // /schedule, wired by cmd
	vibeOps            *VibeOps                               // /vibe, wired by cmd (nil → notices)
	spick              *sessionPicker                         // /resume selector (nil = closed)
	onPickerResume     func(id string)                        // wired by cmd: performs the resume
	onPickerSearch     func(query string) []SessionPickerItem // wired by cmd: prompt-text matches (nil → local id+title filter)
	onPickerPinToggle  func(id string)                        // wired by cmd: persists the pin sidecar
	onPickerDelete     func(id string) error                  // wired by cmd: deletes JSONL + artifacts after confirmation
	tpick              *treeSelector                          // /tree selector (nil = closed)
	treeData           func() []TreeEntry                     // entry snapshot, wired by cmd
	treeLabelLoad      func() map[string]string
	treeLabelSave      func(id, label string) error
	treeLabels         map[string]string        // id→label snapshot, refreshed on open
	settingsOps        *SettingsOps             // /settings, wired by cmd (nil → notices)
	settingsOverlayOps *SettingsOverlayOps      // /settings overlay, wired by cmd (nil → disabled)
	thinkingOps        *ThinkingOps             // /thinking, wired by cmd (nil → notices)
	cwd                string                   // working directory (the status row's left side)
	branch             string                   // git branch for the top bar ("" when none)
	commandDir         string                   // markdown command discovery root
	pathRoot           string                   // @-completion root (empty disables the menu)
	pathList           func(string) []PathEntry // one directory's entries (the only source)
	pathIdx            pathIndex                // background whole-cwd file index (pathindex.go)
	extCommands        map[string]string        // "/server:cmd" -> description
	extRun             ExtensionCommand
	renderers          map[string]RenderSpec   // tool name -> declarative render spec
	sessionBranch      func(args string) error // /branch to an entry id
	resumeList         func(cwd string) error  // /resume session listing
	onSend             func(text string)
	// onSendImages is the multimodal send: the draft with every attached
	// image's chip stripped, plus the payloads in prompt order. It returns
	// true when it took the turn. False (or nil) means the attachments cannot
	// go out — and then the draft must come back to the composer rather than
	// being sent as text: a chip reaching the model as the word "[Image …]"
	// is a user believing a screenshot was read that no pixel of was sent.
	onSendImages func(text string, imgs []PasteImage) bool
	onCancel     func()
	onQuit       func()
	// onQuitRunning is the quit path when a turn is in flight: true means
	// "detach it and leave" (opencode default), false means "kill it".
	// Wired by cmd from settings tui.exitDetach. nil falls back to kill
	// (onCancel then onQuit), matching the pre-detach behaviour.
	onQuitRunning func() bool
	// onRetry re-runs the current session's last turn with no new prompt (the
	// F5 recovery for a stream a dropped connection cut short). Wired by cmd;
	// nil degrades the chord to a notice.
	onRetry func()
	// onQueue is the mid-turn submit: a prompt typed while a turn is running
	// becomes a pending entry (queue.go) instead of being refused, and this
	// handler decides what "delivered" means for the host. cmd hands it to the
	// live agent's Steer channel, so it reaches the model at the next step
	// boundary of the SAME run rather than after it. A false return means the
	// host declined it (a guest room cannot forward into a live turn), and the
	// queued row is withdrawn rather than left promising a delivery that will
	// never happen. nil (a headless host, a test) means a mid-turn submit is
	// refused again, which is the pre-queue behavior and never a silent drop:
	// the draft comes back to the composer.
	onQueue func(text string) bool
	// onSendNow is the immediate-delivery path: interrupt the live turn,
	// acknowledge it, and run this message now (the row's [send now] button
	// and the F6 chord). It is a separate seam from onQueue because the two
	// have genuinely different outcomes — one joins the run in flight, the
	// other replaces it — and a host that wires only the first must still be
	// able to say "send now is not wired in this build" rather than silently
	// queueing something the user asked to have sent.
	onSendNow func(text string)
	// queue holds the pending mid-turn submits in delivery order and queueSeq
	// is the append counter (queue.go). queueHits is the last painted frame's
	// geometry — the mouse hit-tests against what was on screen, not against
	// geometry recomputed on the spot, the same contract the message menu
	// uses. queueFire is an armed send-now action, run AFTER a.mu is
	// released: interrupting a turn and starting another is not something to
	// do while holding the lock the transcript paints under.
	queue     []queuedEntry
	queueSeq  int
	queueHits []queueHit
	queueFire func()

	keyq     chan tcell.Event
	dirty    chan struct{}
	quitCh   chan struct{}
	quitOnce sync.Once

	// UI-loop stall detection (stall.go). loopBeat is written from the loop
	// and read by the watchdog, so it is atomic rather than mutex-guarded: a
	// watchdog that took App.mu could not report a loop stuck holding it.
	loopBeat atomic.Int64
	stallDir string
	// stallExitAfter is how long a condemned episode may stay condemned
	// before the watchdog gives up on it and ends the session
	// (stall.go). 0 disables that. Set with SetStallExitAfter.
	stallExitAfter time.Duration
	// wedged records that the loop stopped beating. The quit chord can only
	// be honoured by the loop itself, so a loop wedged in a blocking tty
	// write cannot read a key: the chord is served here instead, off the
	// loop, by the same goroutine that feeds keyq.
	wedged atomic.Bool
	// frameDropped is the screen's own dropped-frame flag
	// (tty_deadline.go): set when a frame write ran out of time, i.e. the
	// terminal behind this pane could not keep up. Nil when the screen is
	// not a deadline-bounded one, and then nothing is repaired because
	// nothing was dropped. Wired by SetFrameDropped.
	frameDropped *atomic.Bool
	// restoreTty puts the terminal back when the watchdog gives up on the
	// loop (stall.go). Wired to scr.Fini; nil leaves the exit to whatever
	// the caller restores.
	restoreTty func()
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
	// The thumb is measured in HALF ROWS (opencode's slider unit, scroll.go
	// Scrollbar): selBarPos/selBarEnd are its span on a 2*selBarVP-tall track, so
	// a grab can land on the half row the finger is really over.
	// (UI thread; mu-guarded.)
	selBarOn    bool
	selBarX     int // the screen column the bar was painted in (the transcript's last column, or 0)
	selBarVP    int // visible transcript rows the bar spans
	selBarTotal int // transcript rows at paint time
	selBarPos   int // thumb's first half row on the track at paint time
	selBarEnd   int // thumb's half-row end at paint time (exclusive)
	// The sticky header's geometry at paint time. stickyHdr is how many rows of
	// the transcript's top the header owns (its prompt plus the gap under it),
	// stickyVis how many of those are the prompt's own rows, stickyBlock the
	// prompt it pinned (-1 when none) and stickyDoc the document row its first
	// painted row shows.
	//
	// Both numbers matter because a header row is NOT the document row its
	// screen position names: below the header, screen row y is document row
	// start+(y-top), exactly as it always was, but a header row shows a row of
	// the pinned prompt, which the viewport has already scrolled past.
	// Published every frame with the rest of the geometry; (UI-thread;
	// mu-guarded.)
	stickyHdr   int
	stickyVis   int
	stickyBlock int
	stickyDoc   int32
	// toasts is the live notice stack (toast.go): the copy confirmation, a
	// failed chord, a failed MCP server — everything transient, painted in
	// the top-right corner and dropped on its own deadline.
	toasts               []toast
	selClickTime         time.Time
	selClickCount        int
	selClickX, selClickY int
	selClickLocked       bool // re-entry guard (no mu needed on the App)
	// Double/triple-click detection: selClickTime/selClickCount/selClickX/Y
	// track the last primary-button release so rapid repeats on the same
	// position expand the gesture — double-click selects the word,
	// triple-click selects the whole line. Window and tolerance are pinned
	// constants (clickWordWindow, clickWordTol), not settings.
	// Guarded by mu; reset by clearClick.
	// thinkFocus is the reasoning box a click has aimed the wheel at: clicking a
	// box focuses it and a click anywhere else lets it go, so the wheel scrolls
	// the transcript by default instead of whatever box happens to sit under the
	// pointer. -1 = no box focused (app.go scrollThinkBox, selection.go press).
	thinkFocus int
	// focusFade is the 0→1 progress of the reasoning box's focus transition:
	// 1 once focused (a tween that overshoots its target is a bug, so it
	// clamps), 0 once not. Set by selection.go on a focus change and advanced
	// one step per UI tick (app.go tick), so the border eases into its
	// brightened ink instead of snapping — the same ~300ms ease an animated
	// border gets elsewhere. -1 means "no transition in flight", which is the
	// steady state, so an idle transcript spends nothing on it.
	focusFade float64
	// scrollHint is the ▲n▼n viewport hint, drawn on the composer's info
	// divider — never on row 0, where it overwrote scrolled-to content.
	scrollHint string
	// jump is the "↓ n new" chip the painter draws over the transcript
	// whenever rows are hidden below the viewport — cleared every frame, so
	// a click is tested against the chip that is actually on screen
	// (selection.go press).
	jump panelRect
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
	// selDockRows is the dock panel's rows, populated by paint()
	// while the dock is on, so a selection drag over the panel
	// copies the dock text (session id, task name) instead of
	// the transcript rows painted underneath. nil when the dock
	// is off. Callers hold a.mu.
	selDockRows []selRow
	// linkHits is the clickable region of the last painted frame. It follows
	// resize, scroll, and dock geometry because paint rebuilds it from the exact
	// run positions it draws.
	linkHits []linkHit
	// linkOpen is the platform browser opener; tests inject a recorder. The
	// default is nil so the function stays a small seam rather than a new
	// subsystem.
	linkOpen func(string) error
	// linkClick is the target under the press that began the current primary
	// gesture. A release opens it only when the pointer never moved.
	linkClick string
	// dockSetMode persists a display policy the human changed with Alt+s
	// (settings `sidebarMode`); nil = this session cannot persist it.
	dockSetMode func(mode string)
}

// SetFrameDropped wires the dropped-frame flag draw() consults after every
// flush. Pass the flag NewDeadlineScreen returned; nil leaves draw() with
// nothing to repair, which is correct for a screen whose writes cannot time
// out. Call before Run.
func (a *App) SetFrameDropped(flag *atomic.Bool) { a.frameDropped = flag }

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
	focused  bool // reasoning box: the wheel is aimed at it (border brightens)
	// fade quantizes App.focusFade for the render cache: the tween changes the
	// border's colour without changing a row's text or length, so a stamp that
	// did not carry it would repaint the same cached border for the whole fade
	// and the box would jump at the end. Eight buckets is 12.5% steps — finer
	// than the 1/6 per tick the tween actually advances, so no step is skipped.
	fade int8
	// mermaid stamps whether a ```mermaid fence in this block drew as a
	// diagram. Flipping the setting changes every block's rows without any of
	// them changing length, so the stamp has to say which way it rendered or
	// a toggled transcript would keep the cache it should have dropped.
	mermaid bool
	// live stamps a result box whose text is still growing. Length alone is
	// not enough there: a tool that rewrites the same window of bytes (a
	// progress bar, a counter) keeps the tail the same size while the text
	// inside it changes.
	live uint64
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
		keyMap:        km,
		scr:           scr,
		th:            th,
		st:            Status{Model: model, SessionID: sessionID},
		showThinking:  true,
		renderMermaid: true,
		width:         w, height: h,
		keyq:   make(chan tcell.Event, 64),
		dirty:  make(chan struct{}, 1),
		quitCh: make(chan struct{}),
		sm:     newScrollModel(),
		// No box is focused until a click names one: the wheel is the
		// transcript's from the first frame.
		thinkFocus: -1,
		// -1 = no tween in flight. A box that has never been focused never
		// animates, so the welcome screen and an idle transcript cost nothing.
		focusFade: -1,
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

// SetVersion records the build this process is (`xdev version`): the context
// dock's footer row, so "which build is this" has an answer on screen. Empty
// is a dev build and paints nothing.
func (a *App) SetVersion(v string) {
	a.mu.Lock()
	a.version = strings.TrimSpace(v)
	a.mu.Unlock()
	a.DockBump()
}

// BeginActiveCommand notes that a tool call started, so the status row's left
// side ("● name") shows the work. Nested calls are counted, not replaced: two
// concurrent calls to the same tool must not clear the indicator when the
// first one finishes.
func (a *App) BeginActiveCommand(name string) {
	a.cmdMu.Lock()
	a.cmd, a.cmdActive = name, true
	a.cmdDepth++
	a.cmdMu.Unlock()
	a.poke()
}

// EndActiveCommand notes that a started call finished. The indicator clears
// only when the last one does.
func (a *App) EndActiveCommand() {
	a.cmdMu.Lock()
	if a.cmdDepth > 0 {
		a.cmdDepth--
	}
	if a.cmdDepth == 0 {
		a.cmdActive = false
		a.cmd = ""
	}
	a.cmdMu.Unlock()
	a.poke()
}

// hudCommand renders the left side of the status row while a
// tool is running: "● name". Empty when nothing is running so
// callers skip it; safe for narrow terminals because the
// segment block absorbs the squeeze.
func (a *App) hudCommand() string {
	a.cmdMu.Lock()
	name := a.cmd
	active := a.cmdActive
	a.cmdMu.Unlock()
	if !active || name == "" {
		return ""
	}
	return "● " + name
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

// SetQuitRunning wires the detach-on-quit path. When a turn is running and
// the user hits the quit chord, this is called instead of onCancel+onQuit.
// Returning true means the turn was (or will be) detached and the UI should
// exit without cancelling; false means fall through to cancel-then-quit.
func (a *App) SetQuitRunning(fn func() bool) { a.onQuitRunning = fn }

// SetImageSend wires the multimodal send path (see App.onSendImages).
func (a *App) SetImageSend(fn func(text string, imgs []PasteImage) bool) {
	a.onSendImages = fn
}

// SetQueueHandlers wires the mid-turn submit path (issue #157): onQueue for a
// prompt typed while a turn is running — it joins the run in flight through
// the host's steer channel — and onSendNow for the immediate-delivery button
// and its chord, which interrupts the turn and runs the message instead. Both
// are separate from SetHandlers because they are a different outcome, not a
// variation on sending. A host that wires only onQueue still gets the queue;
// a host that wires neither keeps the pre-queue behavior, so the submit is
// refused with a reason instead of vanishing.
func (a *App) SetQueueHandlers(onQueue func(text string) bool, onSendNow func(text string)) {
	a.onQueue, a.onSendNow = onQueue, onSendNow
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
// a.mu.
func (a *App) noteDelta(delta string) {
	a.extendWindow()
}

// noteToolDelta extends the window for a tool call's argument stream, which
// the provider counts in output_tokens even though the transcript never
// renders it. Callers hold a.mu.
func (a *App) noteToolDelta() {
	a.extendWindow()
}

// extendWindow stamps the decode window with now: the first delta opens it,
// every delta extends it. Callers hold a.mu.
func (a *App) extendWindow() {
	now := time.Now()
	if a.deltaFirst.IsZero() {
		a.deltaFirst = now
	}
	a.deltaLast = now
}

// BeginMessage closes the decode window the previous message left open. Every
// provider opens a message with exactly one EventStart (anthropic.go:365 and
// its three siblings), which is the only message boundary the hook layer can
// rely on: a turn that died into the retry ladder never emits EventDone, so
// without this its window stays open and becomes the next turn's denominator
// — a rate several times too low that never corrects itself.
func (a *App) BeginMessage() {
	a.mu.Lock()
	a.closeWindow()
	a.mu.Unlock()
}

// NoteToolDelta extends the decode window for a tool call's argument stream.
// The exported twin of noteDelta, for a hook layer: output_tokens counts
// tool-argument JSON that the transcript never renders, so the window has to
// span it or the rate divides every token the provider billed by a window
// that stopped at the last text delta.
func (a *App) NoteToolDelta() {
	a.mu.Lock()
	a.noteToolDelta()
	a.mu.Unlock()
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
	Dur string // formatted wall time, e.g. "70ms" ("" = unknown)
	// Elapsed is Dur's raw value, for the /usage tool-time total: the string
	// is one row's footer, and summing formatted strings is how a report ends
	// up minutes off. 0 = the caller did not measure.
	Elapsed   time.Duration
	Exit      int // process exit code; read only when HasExit
	HasExit   bool
	Truncated bool
	Diff      string // unified diff of the file change, when the tool made one
}

// AddToolBlock appends one tool-call row in the running state, carrying the
// call's raw JSON arguments: the renderer reads the naming argument out of
// them (omp's `name · detail` row), so no flattened preview is baked in here.
// callID is the provider's id for this call — the half that says WHICH call a
// result belongs to when two calls share a name. Empty is fine for a caller
// that pairs add and finish back to back (replay, bang mode).
func (a *App) AddToolBlock(callID, name, rawArgs string) {
	a.mu.Lock()
	// The /usage report's "tool calls" line. A replayed history replays the
	// same call, so this counts what the user watched, not what the
	// provider billed (billing is the ↑⇢↓ counters' business).
	a.st.ToolCalls++
	a.blocks = append(a.blocks, &Block{
		Kind: KindTool, CallID: callID, ToolName: name, Text: rawArgs,
		Status: "running", Ts: time.Now(),
	})
	a.mu.Unlock()
	a.poke()
}

// FinishTool closes the running row for THIS call (ok/error) and appends the
// tool-result block carrying the full (sink-windowed) output.
//
// The row is matched by call id. Name-only matching was the bug: same-batch
// calls run concurrently (agent MaxToolWorkers) and finish in whatever order
// the shells do, so "the newest running row with this name" was a guess — with
// two `bash` calls it closed the wrong row, so one command's output painted
// under the other command's name and the pairing read as "the command never
// ran". An id also means a replayed history (no ids) cannot steal a live row.
// A nameless call id still falls back to the name, so a caller that only knows
// the name keeps working.
//
// A live box opened by the first streamed chunk is REPLACED, not left behind:
// the settled text is the same output, windowed the same way, and two boxes
// for one command is the transcript lying about what ran.
func (a *App) FinishTool(callID, name string, isErr bool, output string, out ToolOutcome) {
	a.mu.Lock()
	for i := len(a.blocks) - 1; i >= 0; i-- {
		b := a.blocks[i]
		if b.Kind != KindTool || b.ToolName != name || b.Status != "running" {
			continue
		}
		// An id that is known on both sides must match: a row carrying a
		// different id belongs to another call and is not ours to close. A
		// row with no id (replayTranscript) is still eligible, which keeps a
		// resumed history pairing while a live call is in flight.
		if callID != "" && b.CallID != "" && b.CallID != callID {
			continue
		}
		if isErr {
			b.Status = "error"
		} else {
			b.Status = "ok"
		}
		break
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
	// A live box is REPLACED, not left behind: the settled text is the same
	// output, windowed the same way, and two boxes for one command is the
	// transcript lying about what ran. The box's OWN slot is rewritten —
	// the box sits under its call row, so appending would hoist the result
	// above every call that started after it.
	for i := len(a.blocks) - 1; i >= 0; i-- {
		b := a.blocks[i]
		if b.Kind != KindToolDone || !b.Live || b.ToolName != name {
			continue
		}
		if callID != "" && b.CallID != "" && b.CallID != callID {
			continue
		}
		a.blocks[i] = &Block{
			Kind: KindToolDone, CallID: callID, ToolName: name, Text: text,
			Dur: out.Dur, Err: isErr, Exit: out.Exit, HasExit: out.HasExit,
			Truncated: out.Truncated, Diff: out.Diff,
		}
		// /usage's "tool time": the calls' own wall time, one sample per
		// finished call, counted whether the call succeeded or not.
		if isErr {
			a.st.ToolErrors++
		}
		a.st.ToolWork += out.Elapsed
		a.mu.Unlock()
		a.poke()
		return
	}
	a.blocks = append(a.blocks, &Block{
		Kind: KindToolDone, CallID: callID, ToolName: name, Text: text,
		Dur: out.Dur, Err: isErr, Exit: out.Exit, HasExit: out.HasExit,
		Truncated: out.Truncated, Diff: out.Diff,
	})
	// /usage's "tool time": the calls' own wall time, one sample per
	// finished call, counted whether the call succeeded or not.
	if isErr {
		a.st.ToolErrors++
	}
	a.st.ToolWork += out.Elapsed
	a.mu.Unlock()
	a.poke()
}

// livePaint is the fastest a live output box repaints. A command that emits
// thousands of lines a second is not readable at that rate, and every frame
// the terminal cannot show is a frame spent walking the transcript: chunks
// coalesce into the newest bytes and the box repaints ten times a second.
const livePaint = 100 * time.Millisecond

// liveTailBytes bounds what a live box keeps. A live view is "what is
// happening now"; the settled result is windowed by the tool's own sink, so
// nothing is lost here — it is just not kept twice.
const liveTailBytes = 64 << 10

// liveRows is how many rows a live box paints. It matches toolRecentHead, the
// window a settled result keeps: a running command is worth the same screen
// real estate as a finished one, and a box that grows past the viewport is a
// box the rest of the conversation has scrolled out of.
const liveRows = toolRecentHead

// liveBoxLocked is this call's live box, opening one if the call is running
// and has produced its first bytes. A tool that streams nothing (read, grep,
// every MCP tool) never opens one, so the transcript gains no empty frame it
// has nothing to put in.
//
// The new box is spliced in directly under ITS OWN call row, not appended: a
// batch's calls run concurrently, so "the end of the transcript" is whatever
// started last, and a box there would paint one command's bytes under a
// different call's name.
func (a *App) liveBoxLocked(callID, name string) *Block {
	row := -1
	for i := len(a.blocks) - 1; i >= 0; i-- {
		b := a.blocks[i]
		if b.Kind == KindToolDone && b.Live && b.ToolName == name &&
			(callID == "" || b.CallID == "" || b.CallID == callID) {
			return b
		}
		if row < 0 && b.Kind == KindTool && b.ToolName == name && b.Status == "running" &&
			(callID == "" || b.CallID == "" || b.CallID == callID) {
			row = i
		}
	}
	if row < 0 {
		return nil
	}
	box := &Block{Kind: KindToolDone, CallID: callID, ToolName: name, Live: true}
	a.blocks = append(a.blocks, nil)
	copy(a.blocks[row+2:], a.blocks[row+1:])
	a.blocks[row+1] = box
	// No markDirty needed: the splice inserts a block whose render key can
	// not match the render cached at that index (a live result box is never
	// a call row), so sync re-renders from the insertion point on — which
	// recomputes the row offsets the shift invalidated.
	return box
}

// AppendToolOutput adds one chunk to this call's live box, opening that box
// on the first chunk. Chunks are the tool's own read boundaries, so they may
// split a line: the box keeps them as they came, and the renderer windows
// what it paints. A chunk for a call that never ran, or that has already
// settled, is dropped rather than opening a box under a command the
// transcript is done with.
func (a *App) AppendToolOutput(callID, name, chunk string) {
	if chunk == "" {
		return
	}
	a.mu.Lock()
	b := a.liveBoxLocked(callID, name)
	if b == nil {
		a.mu.Unlock()
		return
	}
	b.Text += chunk
	if len(b.Text) > liveTailBytes {
		b.Text = b.Text[len(b.Text)-liveTailBytes:]
	}
	// The stamp ages per livePaint, not per chunk: the box re-renders on the
	// next frame at most, however fast the tool talks.
	if now := time.Now(); now.Sub(b.liveAt) >= livePaint {
		b.liveAt = now
		b.liveSeq++
		a.poke()
	}
	a.mu.Unlock()
}

// taskToolName is the one tool whose children the transcript shows. The
// name lives in the agent package, which the TUI deliberately does not
// import, so the host passes the rows in and the TUI only has to know which
// call row they belong to.
const taskToolName = "task"

// AddTaskChild records that a `task` call started a child. The child is
// attached to the newest running `task` row — the same call a result would
// close — and a child that arrives after the call settled is dropped: the
// row it belonged to is gone, and inventing one would paint a child under
// whatever call came next.
func (a *App) AddTaskChild(callID, label, agent, model string) {
	a.mu.Lock()
	b := a.runningToolLocked(callID, taskToolName)
	if b != nil {
		b.Sub = append(b.Sub, &SubActivity{
			Label: label, Agent: agent, Model: model, Status: "running", Ts: time.Now(),
		})
	}
	a.mu.Unlock()
	a.poke()
}

// UpdateTaskChild records what a child just did. Unknown children (a host
// that only forwards tool events) are adopted rather than dropped, so the
// row still says something is running.
func (a *App) UpdateTaskChild(callID, label, toolName, rawArgs, status string) {
	a.mu.Lock()
	b := a.runningToolLocked(callID, taskToolName)
	if b != nil {
		c := findSubLocked(b, label)
		if c == nil {
			c = &SubActivity{Label: label, Ts: time.Now()}
			b.Sub = append(b.Sub, c)
		}
		c.Tool, c.Args, c.Status = toolName, rawArgs, status
		c.Calls++
	}
	a.mu.Unlock()
	a.poke()
}

// FinishTaskChild settles one child with its terminal status and wall time.
func (a *App) FinishTaskChild(callID, label, status string, d time.Duration) {
	a.mu.Lock()
	b := a.runningToolLocked(callID, taskToolName)
	if b != nil {
		if c := findSubLocked(b, label); c != nil {
			c.Status, c.Dur = status, d
		}
	}
	a.mu.Unlock()
	a.poke()
}

// runningToolLocked is the running call row for callID/name, newest first.
// Callers hold a.mu.
func (a *App) runningToolLocked(callID, name string) *Block {
	for i := len(a.blocks) - 1; i >= 0; i-- {
		b := a.blocks[i]
		if b.Kind != KindTool || b.ToolName != name || b.Status != "running" {
			continue
		}
		if callID != "" && b.CallID != "" && b.CallID != callID {
			continue
		}
		return b
	}
	return nil
}

// findSubLocked is the child with this label, or nil. Labels are unique
// per batch: a batch that named two children the same way already reports
// two indistinguishable sections, and merging their rows would be a lie.
func findSubLocked(b *Block, label string) *SubActivity {
	for _, c := range b.Sub {
		if c.Label == label {
			return c
		}
	}
	return nil
}

// subVisible splits a call's children into the rows the transcript paints
// and the count it reports for the rest. A batch runs up to 8 children and
// the block keeps them all (a handful of small structs, and dropping them
// would make a late settle land on the wrong child); the CAP is a paint
// decision, so it belongs here, not in the state.
func (b *Block) subVisible() (rows []*SubActivity, more int) {
	if len(b.Sub) > subRowsMax {
		return b.Sub[:subRowsMax], len(b.Sub) - subRowsMax
	}
	return b.Sub, 0
}

// RunningTaskCallID is the call id of the newest running `task` row, or ""
// when none is in flight. A host that receives a child event with no call
// id of its own asks this to place it: the spawn that is blocking the turn
// is the one whose row is still spinning.
func (a *App) RunningTaskCallID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if b := a.runningToolLocked("", taskToolName); b != nil {
		return b.CallID
	}
	return ""
}

// subLines renders a `task` call's children as continuation rows under the
// call row. Each row is `⎿ <label> · <what it last did>`, dim: it is
// narration inside someone else's call, not a call of its own, and it must
// not compete with the parent row for attention.
//
// The naming argument comes from toolDetail — the SAME precedence the
// parent's own call rows use — so a child row and a call row read alike and
// the rules live in one place.
func (a *App) subLines(b *Block, w int) []line {
	rows, more := b.subVisible()
	if len(rows) == 0 && more == 0 {
		return nil
	}
	dim := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim)))
	nameSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.Gray))).Bold(true)
	var out []line
	emit := func(text string) {
		ln := textline("  ⎿ ", dim)
		ln.runs = append(ln.runs, cell{text: text, style: nameSt})
		out = append(out, ln)
	}
	for _, c := range rows {
		phrase := c.Tool
		if detail := toolDetail(c.Args); detail != "" {
			if phrase == "" {
				phrase = detail
			} else {
				phrase += " · " + detail
			}
		}
		if phrase == "" {
			phrase = "starting"
		}
		row := c.Label + " · " + phrase
		// No per-child elapsed while the call is in flight: the call row
		// right above already counts the wall time, and two clocks on one
		// row is noise. The settled summary carries the child's own time.
		if budget := subRow(w); width(row) > budget {
			row = truncateCells(row, budget, "…")
		}
		emit(row)
	}
	if more > 0 {
		emit(fmt.Sprintf("+%d more running", more))
	}
	return out
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
// over the window in which deltas actually arrived (omp's per-message math).
// The window is per message, not per run: the first delta of a message opens
// it and EVERY delta extends it — text, thinking, and the tool-call argument
// deltas that output_tokens counts but the transcript never renders. A window
// that no EventDone closed (a turn that died into the retry ladder) is
// discarded by the next message's first delta rather than inherited as its
// denominator. A message with no usable window — nothing streamed, or a
// sub-100ms burst — measures no rate at all, and the segment hides rather
// than carry a previous turn's number as if it were this one's.
// It also refreshes CtxUsed, the live occupancy behind the HUD's context
// segment, with total: the provider's token count for this request, cached
// input included. ctx is what sits in the window, not only what the window had
// to re-read — a prompt-cache hit still occupies those tokens, and an
// input+output sum reads 90 % low on a cached conversation (the same total
// agent.ContextTokens and compaction trigger on). total <= 0 (a provider that
// reports none) falls back to in+out+cache.
//
// cache and thinking arrive as their own arguments because the split is the
// point: in is the fresh input only and out already CONTAINS the reasoning, so
// a row that printed just those two claimed a smaller prompt and a larger
// answer than the provider billed. On a cached turn the ↑ glyph read 479 for a
// 65054-token prompt (99 % of it invisible, cacheRead 64575) and the ↓ glyph
// read 1770 for 506 tokens of visible text (71 % of it thinking).
func (a *App) AddUsage(in, out, cache, thinking, total int64) {
	a.mu.Lock()
	a.st.TokensIn += in
	a.st.TokensOut += out
	a.st.TokensCache += cache
	a.st.TokensThink += thinking
	if total <= 0 {
		total = in + out + cache // a provider that reports no total gets the floor
	}
	a.st.CtxUsed = total
	if window := a.deltaLast.Sub(a.deltaFirst); out > 1 && window >= 100*time.Millisecond {
		a.st.Rate = float64(out) / window.Seconds()
	} else {
		a.st.Rate = 0 // an unmeasurable message measures nothing, and says so
	}
	a.closeWindow()
	a.mu.Unlock()
}

// closeWindow drops the decode window so the next message's first delta
// opens a fresh one instead of extending this message's. Callers hold a.mu.
func (a *App) closeWindow() {
	a.deltaFirst, a.deltaLast = time.Time{}, time.Time{}
}

// AddCost folds provider-reported spend (USD) into the HUD cost segment.
func (a *App) AddCost(usd float64) {
	a.mu.Lock()
	a.st.Cost += usd
	a.mu.Unlock()
}

// AddCacheWrite banks the prompt-cache WRITES one request billed. The wire
// reports them as their own bucket (Anthropic's cache creation input tokens,
// the write side of OpenAI's prompt details), and neither /usage's token
// block nor the token pill's total had anywhere to put them: a session that
// warms a long prefix bills those tokens and never showed them. A separate
// setter rather than a sixth AddUsage argument, so the existing AddUsage
// call sites (every provider, every test) stay as they are and a provider
// that reports no writes is simply never called.
func (a *App) AddCacheWrite(n int64) {
	a.mu.Lock()
	a.st.TokensCacheWrite += max(n, 0)
	a.mu.Unlock()
	a.poke()
}

// AddStep counts one provider request — the step a dsh TimePill reports
// beside its turn count. BeginMessage is the seam: every provider opens a
// message with exactly one EventStart, so a turn of five tool calls counts
// six steps, and a turn that died into the retry ladder still counts the
// steps it actually sent.
func (a *App) AddStep() {
	a.mu.Lock()
	a.st.Steps++
	a.mu.Unlock()
}

// SetTTFT stores the last completed turn's time-to-first-token (ms)
// for the HUD ⌚ ttft segment. Zero clears it.
func (a *App) SetTTFT(ms int64) {
	a.mu.Lock()
	a.st.TTFT = ms
	a.mu.Unlock()
	a.poke()
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
// watched them happen. It also re-bases /usage's own re-based counters, since
// a replayed history is the only source of them.
func (a *App) SetWork(d time.Duration) {
	a.mu.Lock()
	a.st.Work = max(d, 0)
	a.mu.Unlock()
	a.poke()
}

// SetLLMTime re-bases /usage's LLM time and average-TTFT from a rebuilt
// history (/resume, /fork, tree navigation) the way SetWork re-bases the
// timer. It REPLACES rather than adds: the replayed path measures the whole
// adopted history in one call, and adding would double-count it.
func (a *App) SetLLMTime(d time.Duration, ttftSum int64, ttftCount int64) {
	a.mu.Lock()
	a.st.LLMWork = max(d, 0)
	a.st.TTFTSum = max(ttftSum, 0)
	a.st.TTFTCount = max(ttftCount, 0)
	a.mu.Unlock()
	a.poke()
}

// AddLLMTime banks one finished provider request's wall time (msg.DurationMS
// — request sent to last token, gateway queue included) and, when the turn
// carried one, its time-to-first-token. /usage reports LLM time and the
// average TTFT; the status row's ⚡ and ⌚ stay last-turn readings.
//
// 0 durations are ignored rather than added: a message persisted before
// durations were recorded contributes nothing, which is what the replay path
// already assumes when it banks work.
func (a *App) AddLLMTime(dur time.Duration, ttftMS int64) {
	a.mu.Lock()
	a.st.LLMWork += max(dur, 0)
	if ttftMS > 0 {
		a.st.TTFTSum += ttftMS
		a.st.TTFTCount++
	}
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
// rate divide new tokens by old elapsed time. Per message, BeginMessage is
// what does that; this is the coarser run-level backstop.

// SetTabs publishes the open-session snapshot for the status row. cmd
// calls it after every switch and whenever a parked session raises its
// unread badge.
func (a *App) SetTabs(tabs []TabInfo) {
	a.mu.Lock()
	a.tabs = tabs
	a.mu.Unlock()
	a.poke()
}

// SetTabCycle wires Alt+]/Alt+[ (and the unread variants). nil degrades
// the chords to a notice.
func (a *App) SetTabCycle(fn func(dir int, onlyUnread bool)) {
	a.onTabCycle = fn
}

// SetTabClose wires what "close this session" means. cmd owns the tabset, so
// the chord and the tab strip's × both name a session id and let it decide
// (park, abort, close the store, focus a neighbour).
func (a *App) SetTabClose(fn func(id string) error) { a.onTabClose = fn }

// SetTabPick wires what Enter on a /tabs row does. nil degrades /tabs to
// a notice.
func (a *App) SetTabPick(fn func(id string) error) { a.onTabPick = fn }

// SetSessionID updates the status-row / dock identity after a switch.
func (a *App) SetSessionID(id string) {
	a.mu.Lock()
	a.st.SessionID = id
	a.mu.Unlock()
	a.DockBump()
}

func (a *App) SetRunning(r bool) {
	a.mu.Lock()
	a.markRun(r)
	if r {
		a.closeWindow()
	}
	a.mu.Unlock()
	a.poke()
}

// AddTurn counts one finished run — the turn a dsh TimePill reports beside
// its step count. SetRunning's falling edge is the seam: the run that
// actually ended is the turn, whatever it ended with, and an idle re-render
// never counts.
func (a *App) AddTurn() {
	a.mu.Lock()
	a.st.Turns++
	a.mu.Unlock()
	a.poke()
}

// SetSessionCounts re-bases the pill's turn/step counts from a rebuilt
// history (/resume, /fork, tree navigation) the way SetWork re-bases the
// timer: it REPLACES rather than adds, because the replayed path measures the
// whole adopted history in one call and adding would double-count it. A zero
// pair (a history with no counted steps) leaves the counts out of the pill
// rather than claiming a session that ran nothing.
func (a *App) SetSessionCounts(turns, steps int) {
	a.mu.Lock()
	a.st.Turns, a.st.Steps = max(turns, 0), max(steps, 0)
	a.mu.Unlock()
	a.poke()
}

// SetSessionUsage re-bases the session's token buckets and its spend from a
// rebuilt history (/resume, /fork, tab focus, tree navigation) the way
// SetWork re-bases the timer: it REPLACES rather than adds, because the
// replayed path measures the whole adopted history in one call and adding
// would double-count it.
//
// CtxUsed and Rate are deliberately NOT re-based here. The first is the LIVE
// context occupancy — SetContextReplay measures the rebuilt messages, and
// AddUsage owns it for a live turn — and the second is the last decoded
// message's speed, which a history cannot re-measure: carrying a stale rate
// forward is what Reset() clears it to prevent.
func (a *App) SetSessionUsage(in, out, cache, think, cacheWrite int64, cost float64) {
	a.mu.Lock()
	a.st.TokensIn, a.st.TokensOut = max(in, 0), max(out, 0)
	a.st.TokensCache, a.st.TokensThink = max(cache, 0), max(think, 0)
	a.st.TokensCacheWrite = max(cacheWrite, 0)
	a.st.Cost = max(cost, 0)
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

// Quit terminates the UI loop. It is idempotent: a terminal can deliver the
// quit chord more than once in one burst, and the UI loop drains those events
// before it returns to its select. Closing the channel a second time would
// panic the process instead of exiting it.
func (a *App) Quit() { a.quitOnce.Do(func() { close(a.quitCh) }) }

// quitOrCancel is what the quit chord (Ctrl+C, Ctrl+D) does: exit, and take
// the live turn down with it. It used to cancel a running turn and stop, which
// reads as "the exit key is dead" whenever the turn is slow to unwind — the
// turn's own recovery ladder, an in-flight tool, an MCP call still draining.
// The second press landed in the same branch, so the chord had no way out at
// all: session 2750b48c, where C-c and C-d did nothing for the whole of an
// unbroken turn.
//
// Cancelling is not dropped: onQuit's own teardown stops the agent, and the
// turn goroutine is abandoned if it outlives the loop. A turn that keeps
// running after the UI is gone is the same leak every /quit had, and it never
// outlives the process. Esc keeps cancel-only semantics for "stop and stay".
func (a *App) quitOrCancel(running bool) {
	// Detach-on-quit (settings tui.exitDetach, default on): a live turn is
	// handed off so closing the TUI does not kill the work. Esc stays
	// cancel-only; only the quit chord reaches here.
	if running && a.onQuitRunning != nil {
		if a.onQuitRunning() {
			if a.onQuit != nil {
				a.onQuit()
			}
			return
		}
	}
	if running && a.onCancel != nil {
		a.onCancel()
	}
	if a.onQuit != nil {
		a.onQuit()
	}
}

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
	// The title is the panel's title slot, and the panel is already built: a
	// rename that did not bump would leave the old name up there until something
	// else moved. The same invalidation a published plan uses, from the same
	// place — the write that changed what the panel shows.
	a.DockBump()
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

// Goal implements CommandAPI /goal: `/goal <objective>` names the session's
// objective and starts working on it; a bare /goal shows the current goal and
// budget, and complete/drop close it.
func (a *App) Goal(args string) error {
	block, err := a.goalOps.Dispatch(args)
	if err != nil {
		return err
	}
	a.AddSystemBlock(block)
	return nil
}

// SetScheduleOps wires /schedule to the session-local reminder state.
func (a *App) SetScheduleOps(ops *ScheduleOps) { a.scheduleOps = ops }

// Schedule implements CommandAPI /schedule.
func (a *App) Schedule(args string) error {
	out, err := a.scheduleOps.Dispatch(args)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "" {
		a.AddSystemBlock(out)
	}
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

// SetAutoAnswerOps wires the /auto-answer command (the ask policy lives in cmd).
func (a *App) SetAutoAnswerOps(ops *AutoAnswerOps) { a.autoAnswerOps = ops }

// SetAdvisorOps wires the /advisor command (advisor state lives in cmd).
func (a *App) SetAdvisorOps(ops *AdvisorOps) { a.advisorOps = ops }

// AutoAnswer implements CommandAPI /auto-answer: "yes" turns the policy on,
// "no" turns it off, and no argument toggles. A bare toggle is the whole
// reason this is a command rather than a settings row — a human deciding
// mid-session to stop answering their own questions must not have to leave
// the transcript to say so.
//
// The vocabulary is yes|no AND on|off: `yes` is what was asked for, and
// `on`/`off` is what every other toggle here (/plan, /advisor, /prewalk)
// already accepts. Anything else is a usage error, never a silent toggle — a
// typo must not be the thing that answers a question for them.
func (a *App) AutoAnswer(args string) error {
	if a.autoAnswerOps == nil || a.autoAnswerOps.Current == nil || a.autoAnswerOps.Set == nil {
		return fmt.Errorf("auto-answer not wired")
	}
	cur := a.autoAnswerOps.Current()
	var on bool
	fields := strings.Fields(strings.TrimSpace(args))
	switch {
	case len(fields) == 0:
		on = !cur
	case len(fields) == 1:
		switch fields[0] {
		case "yes", "on", "true":
			on = true
		case "no", "off", "false":
			on = false
		default:
			return fmt.Errorf("usage: /auto-answer yes|no")
		}
	default:
		return fmt.Errorf("usage: /auto-answer yes|no")
	}
	if err := a.autoAnswerOps.Set(on); err != nil {
		return err
	}
	confirm := "auto-answer no — an unanswered question waits for you"
	if on {
		confirm = "auto-answer yes — an unanswered question takes the recommended option"
	}
	if a.autoAnswerOps.Path != "" {
		confirm += " (saved to " + a.autoAnswerOps.Path + ")"
	}
	a.AddSystemBlock(confirm)
	return nil
}

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

// TabsPicker implements CommandAPI /tabs: the open-session set as a modal
// list, so a session can be found by its title instead of cycled blind
// (opencode's session list). It reads the snapshot SetTabs already
// publishes — the status row's own data — so there is no second source of
// truth to keep in step, and Enter hands the chosen id back to cmd, which
// focuses the tab.
func (a *App) TabsPicker() error {
	if a.onTabPick == nil {
		return fmt.Errorf("session tabs are not wired in this build")
	}
	a.mu.Lock()
	tabs := append([]TabInfo(nil), a.tabs...)
	a.mu.Unlock()
	if len(tabs) == 0 {
		a.AddSystemBlock("no sessions open")
		return nil
	}
	items := make([]PickerItem, 0, len(tabs))
	for _, t := range tabs {
		items = append(items, PickerItem{
			Label:   tabLabel(t),
			Detail:  shortID(t.ID),
			Value:   t.ID,
			Current: t.Current,
		})
	}
	pick := a.onTabPick
	a.OpenPicker(PickerOptions{
		Title: "open sessions",
		Views: []PickerView{{Name: "open", Items: items, Action: "switch"}},
		OnSelect: func(id string) {
			if err := pick(id); err != nil {
				a.AddSystemBlock("error: " + err.Error())
			}
		},
	})
	return nil
}

// tabLabel names one open session: its title, or the short id when the
// title is still the mechanical one. Running and unread ride the label, so
// the row itself says what is running where — the whole reason this list
// exists instead of the status row's "2 tabs" count.
func tabLabel(t TabInfo) string {
	label := t.Title
	if strings.TrimSpace(label) == "" {
		label = shortID(t.ID)
	}
	if t.Running {
		return "✦ " + label + " ·running"
	}
	if t.Unread {
		return "✦ " + label
	}
	return label
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
// measures the history back in with SetContextReplay.
//
// The ↑ ↓ ⇢ ˟ counters, the spend, the decode rate and the ttft are
// per-session too, and Reset is where that boundary is: without clearing
// them, /resume and /fork drew the previous session's numbers beside the
// freshly replayed transcript, and a new session opened showing a ⚡ it had
// never measured. Streaming state is otherwise untouched — callers must
// not be running a turn when they call this.
// too, and Reset is where that boundary is: without clearing them, /resume
// and /fork drew the previous session's numbers beside the freshly replayed
// transcript, and a new session opened showing a ⚡ it had never measured.
// Streaming state is otherwise untouched — callers must not be running a
// turn when they call this.
func (a *App) Reset() {
	a.mu.Lock()
	a.st.Work = 0
	a.st.TokensIn, a.st.TokensOut = 0, 0
	a.st.TokensCache, a.st.TokensThink = 0, 0
	a.st.TokensCacheWrite = 0
	a.st.Turns, a.st.Steps = 0, 0
	a.st.Cost = 0
	a.st.Rate = 0
	a.st.TTFT = 0
	a.st.CtxUsed = 0
	a.st.ToolCalls, a.st.ToolErrors = 0, 0
	a.st.ToolWork, a.st.LLMWork = 0, 0
	a.st.TTFTSum, a.st.TTFTCount = 0, 0
	a.closeWindow()
	a.blocks = nil
	a.thinkFocus = -1  // the focused box went with them
	a.focusFade = -1   // no tween: the box that was fading is gone with its blocks
	a.msgArmed = false // so did the armed menu row: its block is gone
	a.msgm = nil       // a menu over replayed-away blocks is not a menu
	a.msgv = nil       // likewise the read-only surface naming one
	a.statusPop = nil  // a panel over the previous session's transcript
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
		a.thinkFocus = -1 // a dropped box cannot stay the wheel's target
		a.focusFade = -1  // nor its half-drawn border
	}
	a.clearRenderCache()
	a.mu.Unlock()
	a.poke()
}

// SetRenderMermaid turns mermaid diagram rendering on or off (settings key
// `renderMermaid`). It is the one display setting that does not drop content:
// with it off a ```mermaid fence paints as the code band it was before the
// feature existed, so the render cache is dropped and the next frame redraws
// the same blocks from source.
func (a *App) SetRenderMermaid(on bool) {
	a.mu.Lock()
	a.renderMermaid = on
	a.clearRenderCache()
	a.mu.Unlock()
	a.poke()
}

// Mermaid reports whether mermaid fences render as diagrams.
func (a *App) Mermaid() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.renderMermaid
}

// SetDebugMouse enables rendering of every mouse event on the
// status bar (settings `tui.debugMouse`). Off by default: the log
// is opt-in so a normal session does not scroll the HUD with
// pointer noise.
func (a *App) SetDebugMouse(on bool) {
	a.mu.Lock()
	a.debugMouse = on
	if !on {
		a.debugMouseLine = ""
	}
	a.mu.Unlock()
	a.poke()
}

// SetLogFile opens <f> for writing a TUI screen transcript
// (one text dump per paint frame). Off by default (nil).
func (a *App) SetLogFile(f *os.File) {
	a.logMu.Lock()
	a.logFile = f
	a.logMu.Unlock()
}

// logFrame writes a text dump of the screen buffer to
// the log file (if open). It snapshots the pointer under
// logMu, then writes without any lock so a stalled terminal
// cannot hold up the UI loop.
func (a *App) logFrame() {
	a.logMu.Lock()
	f := a.logFile
	a.logMu.Unlock()
	if f == nil {
		return
	}
	_, _ = fmt.Fprintf(f, "\n=== frame %d ===\n", time.Now().UnixNano())
	for y := 0; y < a.height; y++ {
		for x := 0; x < a.width; x++ {
			text, _, _ := a.scr.Get(x, y)
			if text != "" {
				_, _ = f.WriteString(text)
			}
		}
		_, _ = f.WriteString("\n")
	}
	_ = f.Sync()
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

// thinkingLevel is the request-side level as the chrome shows it: the bare
// rung /thinking takes, so the readout sits beside the model it applies to
// without naming the mechanism ("model · high" is one request; "model ·
// thinking high" repeated the command beside the answer). Empty when the
// seam is unwired — a host that never wired /thinking has no level to report,
// and an invented "auto" would name a budget nothing chose (the same rule the
// dock's version row follows).
func (a *App) thinkingLevel() string {
	if a.thinkingOps == nil || a.thinkingOps.Current == nil {
		return ""
	}
	return strings.TrimSpace(a.thinkingOps.Current())
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
		return a.setDockModeSetting("sidebarMode", fields[1:])
	}
	if fields[0] == "renderMermaid" {
		return a.setMermaidSetting(fields[1:])
	}
	if fields[0] != "showThinking" {
		return fmt.Errorf("unknown setting %q (want showThinking|sidebarMode|renderMermaid)", fields[0])
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

// setDockModeSetting is the one place a sidebar policy is written: the dock's
// display policy (#291 §1), reached by /settings sidebarMode and by /sidebar.
// The App owns the state, so the flip works with unwired ops; persisting is
// the config seam's job when it exists. label is the key the caller writes,
// so the confirmation names the setting and not whichever command typed it.
func (a *App) setDockModeSetting(label string, fields []string) error {
	// Bare form asks, it does not write: a report that also persisted would make
	// the panel's own explanation of itself a side effect.
	if len(fields) == 0 {
		a.AddSystemBlock(label + " " + a.DockMode() + " — " + a.DockState())
		return nil
	}
	if len(fields) > 1 {
		return fmt.Errorf("usage: /%s [auto|show|hide]", label)
	}
	want := a.DockMode()
	switch fields[0] {
	case DockAuto, DockShow, DockHide:
		want = fields[0]
	default:
		return fmt.Errorf("usage: /%s [auto|show|hide]", label)
	}
	confirm := label + " " + want
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

// setMermaidSetting is /settings renderMermaid [on|off]: the same shape as
// setDockModeSetting — a bare form reports rather than writes, and the value
// lands on the one persisted key (renderMermaid) through the same seam the
// settings panel uses, so the two cannot disagree.
func (a *App) setMermaidSetting(fields []string) error {
	if len(fields) == 0 {
		state := "off"
		if a.Mermaid() {
			state = "on"
		}
		a.AddSystemBlock("renderMermaid " + state + " — ```mermaid fences draw as diagrams; a diagram that will not fit falls back to source")
		return nil
	}
	if len(fields) > 1 {
		return fmt.Errorf("usage: /settings renderMermaid [on|off]")
	}
	var on bool
	switch fields[0] {
	case "on", "true":
		on = true
	case "off", "false":
		on = false
	default:
		return fmt.Errorf("usage: /settings renderMermaid [on|off]")
	}
	confirm := "renderMermaid " + fields[0]
	if a.settingsOps != nil && a.settingsOps.SetMermaid != nil {
		if err := a.settingsOps.SetMermaid(on); err != nil {
			return err
		}
		confirm += " (saved to " + a.settingsOps.Path + ")"
	}
	a.SetRenderMermaid(on)
	a.AddSystemBlock(confirm)
	return nil
}

// Sidebar implements CommandAPI /sidebar [show|hide|auto]: the two-state
// show/hide a human reaches for mid-session. Alt+S already walks the policy
// through all three states, but that is a mode, not a switch — a bare toggle
// is what "hide the sidebar, now" means — and it lands on the same persisted
// key through the same seam /settings sidebarMode uses, so the two cannot
// disagree.
func (a *App) Sidebar(args string) error {
	fields := strings.Fields(args)
	if len(fields) > 1 {
		return fmt.Errorf("usage: /sidebar [show|hide|auto]")
	}
	want := ""
	if len(fields) == 1 {
		switch fields[0] {
		case DockShow, DockHide, DockAuto:
			want = fields[0]
		default:
			return fmt.Errorf("usage: /sidebar [show|hide|auto]")
		}
	} else {
		// The toggle asks what is ON SCREEN, not what the policy says: a
		// terminal that auto-closed the panel is not what the human is
		// asking to take away, so a bare call opens it rather than
		// persisting a hide for a panel that was never there.
		a.mu.Lock()
		shown := a.dockOn()
		a.mu.Unlock()
		if shown {
			want = DockHide
		} else {
			want = DockShow
		}
	}
	return a.setDockModeSetting("sidebarMode", []string{want})
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
			// A tty read error is terminal, and only here: tcell's
			// inputLoop posts one EventError and returns, so nothing will
			// ever be read from the terminal again (tscreen.go). Polling on
			// would leave the UI alive and deaf — no key, no resize, no
			// quit chord, with the tty still in raw mode and the alt
			// screen up. Leaving the loop instead lets runTUI's defers
			// restore the terminal; the user relaunches.
			if e, ok := ev.(*tcell.EventError); ok {
				logx.Errorf("tui: terminal read failed, leaving the UI loop: %v", e)
				a.Quit()
				return
			}
			// A wedged loop cannot read the key that would end it, so the quit
			// chord is served here, where a key still lands: the 2026-09-30
			// report (session 00b1c5a0) was exactly this — the loop blocked
			// 17 minutes inside a tty write, so Ctrl+C and Ctrl+D were read
			// by nobody and the session could not be exited. Only the chord
			// and only while the loop is wedged: every other key is the
			// loop's to apply, and a chord resolved through the keymap (a
			// user remap) is left exactly as it was.
			if a.wedged.Load() {
				if k, ok := ev.(*tcell.EventKey); ok && a.keyMap.Resolve(k) == "quit" {
					a.quitOrCancel(true)
					return
				}
			}
			select {
			case a.keyq <- ev:
			case <-a.quitCh:
				return
			}
		}
	}()

	a.draw()
	a.logFrameAfterDraw()

	for {
		iter := time.Now()
		select {
		case <-a.quitCh:
			return
		case ev := <-a.keyq:
			a.handleKey(ev)
			a.drainKeys()
			a.draw()
			a.logFrameAfterDraw()
			a.beatDone("event", iter)
		case <-a.dirty:
			a.draw()
			a.logFrameAfterDraw()
			a.beatDone("dirty", iter)
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
			// A toast is timed, and an idle UI does not repaint: the tick that
			// finds one expired asks for the draw that drops it.
			if a.toastsExpiring() {
				animate = true
			}
			// A held drag parked on the transcript's edge is the one mouse
			// gesture with no events of its own, so the tick is its clock
			// (selection.go selEdgeTick).
			if a.selEdgeTick() {
				animate = true
				// The reasoning box's focus tween advances one step per tick and
				// keeps asking for a frame until it lands. -1 is the steady state
				// (no box was ever focused), so an idle transcript stays at zero
				// repaint cost — the same discipline as the sheen above it.
				if a.focusFade >= 0 {
					a.focusFade += focusFadeStep
					if a.focusFade >= 1 {
						a.focusFade = 1 // clamp: an overshoot is a stuck mid-fade
					}
					animate = true
				}
			}
			// An armed leader prefix is the same kind of timer: the pair has
			// to stop waiting on its own, because Resolve alone would keep it
			// armed until the next keystroke (keymap.go). Checked under the
			// lock this tick already holds.
			if a.keyMap.ExpirePending(time.Now()) {
				animate = true
			}
			a.mu.Unlock()
			// The whole-cwd file index landing (pathindex.go): re-query an
			// OPEN @-dropdown so the recursive hits appear without another
			// keystroke. takeBuilt is a CAS, so this fires once per build.
			if a.pathList != nil && a.pathIdx.takeBuilt() {
				if _, _, ok := pathToken(a.ed.Text()); ok {
					a.syncSlashMenu()
					animate = true
				}
			}
			// Nothing repaints for the HUD: a running draw keeps the time
			// segment live, and idle, the number is a frozen total that needs
			// no tick to stay correct.
			if running || animate || stuck {
				a.draw()
				a.logFrameAfterDraw()
			}
			a.beatDone("tick", iter)
		}
	}
}

// keyBurst bounds how many already-queued input events one drain applies before
// the loop paints. A burst (a wheel flick, a held key, a paste) arrives faster
// than a frame paints, and drawing once per event queued a whole burst of
// frames ahead of whatever the user typed next — 600 wheel notches became ~900
// Show() calls, and the keystroke behind them waited a flush per queued event
// (~200 ms at 4 ms/frame: the "type after scrolling and the box lags" report).
// Every event is still applied, in order, so no chord or paste marker is
// dropped; only the redundant frames go. The budget lets a continuous stream
// (a held key) still reach the screen.
const keyBurst = 256

// drainKeys applies the input already waiting in keyq, so a burst costs one
// frame instead of one per event.
func (a *App) drainKeys() {
	for range keyBurst {
		select {
		case ev := <-a.keyq:
			a.handleKey(ev)
		default:
			return
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
			// debugMouse renders the event on the status bar when opted in.
			if a.debugMouse {
				a.debugMouseLine = mouseDebugLine(m, press)
			}
			a.mu.Unlock()
			// A modal owns the mouse first: omp's lists move the selection on
			// the wheel and choose the row under a click, so nothing underneath
			// them should scroll or start a text selection. The ask card
			// outranks the rest — while it is up it takes the wheel and the
			// click, or the human scrolls the transcript underneath a question
			// they were trying to answer. The status popup is last of the
			// chain: it is a panel over the transcript, so anything modal
			// above it must win.
			if a.handleAskMouse(m, press) || a.handlePickerMouse(m, press) || a.handleHubRosterMouse(m, press) || a.handleSettingsOverlayMouse(m, press) || a.handleTrajectoryMouse(m, press) || a.handleMsgMenuMouse(m, press) || a.handleStatusPopupMouse(m, press) {
				// A menu row picked by click arms its action under the lock;
				// it runs here, unlocked.
				a.runPendingMsgAction()
				return // the UI loop repaints after handleKey
			}
			// The tab strip is chrome under the top bar: a click there names a
			// session, never a transcript row, so it is asked after every
			// modal (a picker owns its own screen) and before the queue and
			// the transcript selection below.
			if a.handleTabStripMouse(m, press) {
				return
			}
			// The mid-turn queue (#157) sits above the composer, over the
			// transcript's tail. It is a surface, not content, so a click on a
			// row spends itself on the row instead of anchoring a selection
			// over the transcript text it covers. Checked after the modals,
			// which are modal and keep first claim on a click.
			if a.queueMouse(m, press) {
				a.runPendingQueueAction()
				return // the UI loop repaints after handleKey
			}
			switch m.Buttons() {
			case tcell.WheelUp:
				// Diff overlay owns the wheel while open — same as a modal —
				// otherwise the notch scrolls the transcript underneath it.
				if a.diffOverlayOpen() {
					a.dockOverlayScroll(3, false)
				} else if a.MsgViewOpen() {
					a.msgViewScroll(3, false)
				} else if !a.scrollThinkBox(m, false) {
					a.scroll(3, false)
				}
			case tcell.WheelDown:
				if a.diffOverlayOpen() {
					a.dockOverlayScroll(3, true)
				} else if a.MsgViewOpen() {
					a.msgViewScroll(3, true)
				} else if !a.scrollThinkBox(m, true) {
					a.scroll(3, true)
				}
			default:
				a.mu.Lock()
				a.handleMouse(m, press)
				a.mu.Unlock()
				a.runPendingMsgAction()
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
	// The trajectory ledger is modal on the same terms as the tree selector:
	// it owns every key while open, so nothing underneath it navigates.
	// The settings overlay is modal: it owns every key while open.
	if a.handleSettingsOverlayKey(key) {
		return
	}
	if a.handleTrajectoryKey(key) {
		return
	}
	// Diff overlay is modal for navigation: ↑↓/PgUp/PgDn/Home/End scroll
	// its body, and Esc closes it — the same modal contract the tree
	// selector and trajectory ledger use, so the double-Esc rewind block
	// below never sees an Esc while the overlay is up.
	if a.handleDiffOverlayKey(key) {
		return
	}
	// The user-message menu and its read-only surface are modal on the same
	// terms, and they sit ABOVE the diff overlay: the message view is what
	// "jump" opens, and a menu can be opened from a click that closed nothing
	// else. Their Esc case matters for the same reason the diff overlay's does
	// — without it, Esc falls through to the double-Esc rewind block below.
	if a.handleMsgMenuKey(key) {
		return
	}

	// The pill popup takes Esc before the double-Esc rewind ladder: a panel
	// over the transcript is dismissed by the same chord every other panel
	// answers, and Esc must not instead pull the draft out of the composer
	// while a metrics question is still on screen.
	if key.Key() == tcell.KeyEsc && a.StatusPopupOpen() {
		a.mu.Lock()
		a.closeStatusPopupLocked()
		a.mu.Unlock()
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
		// The diff overlay (opened from a dock click) is dismissed first.
		a.mu.Lock()
		hasOverlay := a.diffOv != nil
		a.mu.Unlock()
		if hasOverlay {
			a.closeDiffOverlay()
			return
		}
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
		a.quitOrCancel(running)
		return
	case "scroll-up":
		if a.diffOverlayOpen() {
			a.dockOverlayScroll(1, false)
		} else {
			a.scroll(1, false)
		}
		return
	case "scroll-down":
		if a.diffOverlayOpen() {
			a.dockOverlayScroll(1, true)
		} else {
			a.scroll(1, true)
		}
		return
	case "scroll-page-up":
		if a.diffOverlayOpen() {
			// handleDiffOverlayKey already owns bare PgUp; this covers a
			// remapped chord that resolves to the scroll-page action.
			a.mu.Lock()
			vp := 1
			if a.diffOv != nil {
				vp = max(1, a.diffOv.scrollVp-1)
			}
			a.mu.Unlock()
			a.dockOverlayScroll(vp, false)
		} else {
			a.scrollPage(false)
		}
		return
	case "scroll-page-down":
		if a.diffOverlayOpen() {
			a.mu.Lock()
			vp := 1
			if a.diffOv != nil {
				vp = max(1, a.diffOv.scrollVp-1)
			}
			a.mu.Unlock()
			a.dockOverlayScroll(vp, true)
		} else {
			a.scrollPage(true)
		}
		return
	case "scroll-top":
		if a.diffOverlayOpen() {
			a.mu.Lock()
			n := 0
			if a.diffOv != nil {
				n = len(a.diffOv.lines)
			}
			a.mu.Unlock()
			a.dockOverlayScroll(n, false)
		} else {
			a.scrollTo(false)
		}
		return
	case "scroll-bottom":
		if a.diffOverlayOpen() {
			a.mu.Lock()
			n := 0
			if a.diffOv != nil {
				n = len(a.diffOv.lines)
			}
			a.mu.Unlock()
			a.dockOverlayScroll(n, true)
		} else {
			a.scrollTo(true)
		}
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
	case "send-now":
		// F6 / the queued row's button (#157). The host owns the turn, so
		// the chord only names the intent; what "now" means — interrupt the
		// live run, acknowledge it, start a fresh turn with the oldest
		// pending message — is decided where the turn lives (cmd/xdev).
		// With nothing queued it is a notice, never a silent no-op: a chord
		// that does nothing when pressed is a bug report waiting to happen.
		if a.onSendNow != nil {
			text := a.oldestQueued()
			if text == "" {
				a.AddSystemBlock("nothing queued — type a prompt and Enter while a turn runs, or click a queued row")
				a.poke()
				return
			}
			a.onSendNow(text)
			a.poke()
			return
		}
		a.AddSystemBlock("send now is not wired in this build")
		return
	case "session.tab.next":
		if a.onTabCycle != nil {
			a.onTabCycle(1, false)
		} else {
			a.AddSystemBlock("session tabs are not wired in this build")
		}
		return
	case "session.tab.previous":
		if a.onTabCycle != nil {
			a.onTabCycle(-1, false)
		} else {
			a.AddSystemBlock("session tabs are not wired in this build")
		}
		return
	case "session.tab.next_unread":
		if a.onTabCycle != nil {
			a.onTabCycle(1, true)
		} else {
			a.AddSystemBlock("session tabs are not wired in this build")
		}
		return
	case "session.tab.previous_unread":
		if a.onTabCycle != nil {
			a.onTabCycle(-1, true)
		} else {
			a.AddSystemBlock("session tabs are not wired in this build")
		}
		return
	case "session.delete":
		// C-d / A-w / <leader>w. Close the CURRENT tab — or, on the last one,
		// quit: "close this" with nothing left to close has to mean "exit",
		// or a user who learned C-d quits from xdev still has to learn C-c.
		a.mu.Lock()
		tabs := append([]TabInfo(nil), a.tabs...)
		a.mu.Unlock()
		if len(tabs) <= 1 {
			// Nothing left to close, so the chord still exits — and it is
			// checked BEFORE the wiring test, so C-d keeps meaning "exit"
			// on a host with no tabset to close into.
			a.quitOrCancel(running)
			return
		}
		if a.onTabClose == nil {
			a.AddSystemBlock("session tabs are not wired in this build")
			return
		}
		a.closeTab(currentTabID(tabs))
		return
	case "session.list":
		// <leader>l — the open-session set as a modal list (opencode's
		// session_list). It reads the snapshot the status row already has.
		if err := a.TabsPicker(); err != nil {
			a.AddSystemBlock(err.Error())
			a.poke()
		}
		return
	case "session.new":
		// <leader>n — /new from the keyboard: a fresh session file, the
		// previous one parked in the tabset rather than dropped.
		if err := a.NewSession(); err != nil {
			a.AddSystemBlock(err.Error())
			a.poke()
		}
		return
	case "thinking-toggle":
		// Shift-Tab. Like the dock chords this runs after every modal
		// handler, so an open picker keeps first claim on the key (the model
		// picker binds Shift-Tab to "previous tab").
		a.ToggleThinking()
		return
	case "app.settings":
		if a.SettingsOverlayOpen() {
			a.CloseSettingsOverlay()
		} else {
			a.OpenSettingsOverlay()
		}
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
	//
	// `action` is the value Resolve already computed above — this switch does
	// NOT call Resolve again. A two-chord prefix is consumed by the first
	// call, so a second call on the same event disarms what it just armed:
	// Ctrl+X then L resolved as "l" alone and the pair never fired.
	if action != "" {
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
		case "history-prev":
			// The chord /hotkeys advertises. Editor.HandleKey has no
			// KeyCtrlR case, so without this the key resolved to an action
			// nothing dispatched and recall was Up-only.
			a.ed.HistoryPrev()
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
			a.quitOrCancel(running)
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
		// Mid-turn submit (#157). With a turn running, a text prompt is not a
		// lost keystroke: it becomes a pending entry above the composer and is
		// handed to the live agent's steer channel, so the model reads it at
		// its next step boundary. This branch runs BEFORE the transcript row is
		// appended, because a queued prompt is not yet part of the
		// conversation — the row belongs to the entry's list, and adding it
		// here too would print it twice the moment it is delivered.
		//
		// Attachments keep the old contract: a queued image has no honest
		// delivery story (the pixels are not in the steer channel), so it
		// falls through to the multimodal path and comes back to the composer
		// with its reason. An unwired onQueue falls through to the plain send
		// for the same reason — the host still owns the turn claim and still
		// prints its own refusal.
		if running && len(imgs) == 0 && a.onQueue != nil {
			if a.queuePrompt(text) {
				a.poke()
				return
			}
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
// says why in a toast. It exists because an attached image has no text
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
	a.setError(fmt.Sprintf("not sent: %d image(s) — %s", len(imgs), why))
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
		lines = a.thinkBoxLines(i, b, w)
	case KindTool:
		// omp's call row: state bullet, bold tool name, and the naming
		// argument as a phrase — never the raw JSON the model sent. While the
		// call is in flight the bullet spins and the elapsed ticks; the
		// settled wall time belongs to the result frame's footer.
		//
		// A phrase too long for the row WRAPS onto continuation rows instead
		// of being cut with an ellipsis. It used to be clipped to the width:
		// the one thing a user opens the transcript to read — the command —
		// was the one thing it silently shortened, at an ellipsis in the
		// middle of a long pipeline. The continuations are indented to where
		// the phrase starts, so the command reads as one block hanging off
		// the call, and each rendered row is its own selRow, so a drag over
		// them copies the rows as painted (the same contract the wrapped
		// user prompt has). Nothing here clips: a tool that names itself with
		// megabytes of arguments is the model's bug, and it should be
		// readable rather than quietly shortened.
		name, detail := toolSummary(b)
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
		bulletSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(fg)))
		nameSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.TextSecondary))).Bold(true)
		detailSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.Gray)))
		// The phrase starts after the bullet, the name and its " · ", so that
		// column is both where every continuation row indents to and the
		// width the phrase is wrapped at. A name longer than the row itself
		// pushes the indent past what fits — the max(10, …) floor below is
		// the same idiom every other wrapping row here uses (a user prompt, a
		// system notice): a terminal that narrow has no layout to keep.
		indent := width(bullet+" ") + width(name) + width(" · ")
		// The live elapsed rides the head row, so the wrap budget shrinks by
		// its width: a row that runs one cell past the content width paints
		// into the right edge (the dock's border, the scrollbar's track).
		elapsed := ""
		if b.Status == "running" && !b.Ts.IsZero() {
			elapsed = "  " + humanDur(time.Since(b.Ts))
		}
		// A call with no arguments at all names nothing, so it renders as the
		// name alone — segs stays empty and there is no continuation to walk.
		var segs []string
		if detail != "" {
			segs = wrap(detail, max(10, w-indent-width(elapsed)))
		}
		ln := textline(bullet+" ", bulletSt)
		ln.runs = append(ln.runs, cell{text: name, style: nameSt})
		if len(segs) > 0 {
			ln.runs = append(ln.runs, cell{text: " · " + segs[0], style: detailSt})
		}
		if elapsed != "" {
			ln.runs = append(ln.runs, cell{
				text:  elapsed,
				style: tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim))),
			})
		}
		lines = append(lines, ln)
		for i, seg := range segs {
			if i == 0 {
				continue // the head already sits after the name
			}
			cont := textline(strings.Repeat(" ", indent), nameSt)
			cont.runs = append(cont.runs, cell{text: seg, style: detailSt})
			lines = append(lines, cont)
		}
		// A `task` call's children, one dim row each. They read as
		// continuations of the row above (a `⎿` tick and an indent), not
		// as sibling tool calls, and they are the only place a user can
		// see what a subagent is doing while its call blocks — the model
		// sees only the yield (TestSubagentYieldOnlyIsolation).
		lines = append(lines, a.subLines(b, w)...)
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
// on where the box stops. The collapsed box is one row, so its window is one
// row and ThinkOff is pinned at the tail while it holds no wheel.
func thinkMaxOff(n, rows int) int { return max(0, n-rows) }

// thinkWindow slices a block's wrapped reasoning to the box's window: `off`
// rows above the newest thought, `rows` tall. The window is tail-anchored, the
// same way the transcript counts its own offset, so the newest thought is what
// a reader following the turn sees. An offset past either end reads as that
// end, never as an empty frame.
func thinkWindow(n, off, rows int) (start, end int) {
	end = n - clamp(off, 0, thinkMaxOff(n, rows))
	return max(0, end-rows), end
}

// focusFadeStep is one tick's worth of the focus tween. The UI runs at ~30fps
// (app.go tick), so 1/6 lands in ~200ms — an eased border, not a slide. The
// step is a constant rather than a duration so the ease has a fixed cost per
// frame whatever the tick happens to be doing.
const focusFadeStep = 1.0 / 6.0

// thinkBoxLines renders one reasoning block in the same rounded frame a result
// gets: the top border carries the state ("⠹ Thinking…" while it streams,
// "Thought for Xs" once it settles) and the body shows a fixed window of it.
//
// The window's height IS the focus. An unfocused box is one row — the newest
// thought — so a turn reads as a list of one-liners with its reasoning out of
// the way, and a click (App.thinkFocus, set in selection.go) grows it to
// thinkBoxRows, which the wheel then scrolls (Block.ThinkOff) and Ctrl+O drops
// entirely. A second click on it, or a click anywhere else, gives the wheel
// back and the box returns to one row. The focused box draws a bold rule: no
// other box takes the wheel, so the frame has to say which one has it. The
// full reasoning always stays in the session JSONL, so every height here is a
// view, never the record.
func (a *App) thinkBoxLines(i int, b *Block, w int) []line {
	box := a.th.Box()
	focused := i == a.thinkFocus
	border := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentThinking)))
	if fade := a.focusFade; fade >= 0 {
		// During the tween the border sits BETWEEN the dim and the focused
		// ink rather than between two identical ones: the base is already the
		// thinking accent, so the eased colour is that accent walked back
		// toward the body gray and forward again. At fade=1 it lands exactly on
		// AccentThinking, so a settled box is the same cell it always was.
		base := a.th.Get(theme.GrayDim)
		if fade < 1 {
			border = tcell.StyleDefault.Foreground(
				a.cellColor(theme.Lerp(base, a.th.Get(theme.AccentThinking), fade)))
		}
	}
	if focused {
		// Bold is the terminal's own bright variant: the aim reads as the same
		// hue turned up, not as a second colour with its own meaning. Applied
		// on top of the tween so the settled focused box is byte-for-byte the
		// style it always was.
		border = border.Bold(true)
	}
	bodySt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim)))
	inner := max(1, w-4) // side borders + one pad cell each

	hdr := "Thought"
	switch {
	case b.stream:
		hdr = "⠹ Thinking…"
	case b.thinkDur > 0:
		hdr = fmt.Sprintf("Thought for %.1fs", b.thinkDur.Seconds())
	}

	body := strings.TrimRight(b.Text, "\n")
	// A body the model never wrote is not rendered: the box keeps its frame
	// and its state header, and says what happened in one dim row. The full
	// text stays in the session JSONL — the box is a view, never the record —
	// so nothing is lost, and the transcript no longer paints a wall of
	// mojibake that hides the turn's real content.
	if junkThinking(body) {
		return []line{
			boxTop(box, border, hdr, w),
			boxRow(box, border, bodySt, junkReasoningNotice, inner),
			boxBottom(box, border, w),
		}
	}
	rows := a.thinkRows(b, w)
	// Ctrl+O (Block.Expanded) is the one height the click does not own: it is
	// every row, and it survives a focus change.
	height := thinkBoxCollapsed
	if focused || b.Expanded {
		height = thinkBoxRows
	}
	start, end := 0, len(rows)
	if !b.Expanded {
		start, end = thinkWindow(len(rows), b.ThinkOff, height)
	}
	// The hidden-row notice leads the window the way it does in a result box:
	// following the tail, what is elided is the head. Scrolled up, the count
	// also covers the rows the wheel has yet to come back to — one notice
	// beats two at this height. Collapsed there is nothing to elide into a
	// window — the single row IS the newest thought — so the notice is the
	// focused box's alone: at one row it would cost the row the user came for.
	out := []line{boxTop(box, border, hdr, w)}
	if focused {
		if hidden := len(rows) - (end - start); hidden > 0 {
			out = append(out, boxRow(box, border, bodySt, fmt.Sprintf("… %d rows hidden (Ctrl+O to expand)", hidden), inner))
		}
	}
	for _, wl := range rows[start:end] {
		out = append(out, boxRow(box, border, bodySt, wl, inner))
	}
	return append(out, boxBottom(box, border, w))
}

// thinkBoxAt names the reasoning block painted on screen row y, or -1 when that
// row carries no reasoning box (another kind of block, or chrome). Both the
// click that focuses a box and the wheel that scrolls the focused one come
// through here, so focus and scroll can agree on what is under the pointer.
// Callers hold a.mu; selViewport syncs the layout this hit-test reads.
func (a *App) thinkBoxAt(y int) int {
	if len(a.blocks) == 0 {
		return -1
	}
	top, vp := a.selViewport()
	hdr := a.transcriptTop()
	if vp <= 0 || y < hdr || y >= hdr+vp {
		return -1
	}
	// A header row belongs to the pinned prompt, so a reasoning box is not
	// under the pointer there however the row index would resolve it.
	if y-hdr < a.stickyHdr && a.stickyBlock >= 0 {
		return -1
	}
	bi := a.rowIdx.blockAt(int32(top + y - hdr))
	if bi < 0 || a.blocks[bi].Kind != KindThinking {
		return -1
	}
	return bi
}

// scrollThinkBox routes a wheel notch to the reasoning box a click focused
// (App.thinkFocus); anywhere else the notch is the transcript's, which is the
// default. Focus is what makes the box's own scroll deliberate: the wheel used
// to belong to whatever box sat under the pointer, so a box merely passing
// under a stationary hand stole the notch. It reports whether the notch was
// consumed — at either end of the box's own content the wheel falls through to
// the transcript, which is the way back out.
func (a *App) scrollThinkBox(m *tcell.EventMouse, down bool) bool {
	_, y := m.Position()
	a.mu.Lock()
	bi := a.thinkBoxAt(y)
	if bi < 0 || bi != a.thinkFocus {
		// Not on the focused box: nothing is focused, or the notch sits over
		// another box, another kind of block, or chrome. Only a click moves the
		// focus (selection.go), so the notch leaves it where it is and this one
		// belongs to the transcript, which is the default.
		a.mu.Unlock()
		return false
	}
	b := a.blocks[bi]
	// ThinkOff counts rows above the newest thought, so the wheel's own sense
	// inverts here: rolling up walks back through the reasoning, rolling down
	// returns to the live edge. A notch that cannot move the window is not a
	// scroll, and reporting it unconsumed hands it back to the transcript.
	step := -1
	if !down {
		step = 1
	}
	// A notch only ever reaches a focused box (the gate above), which is the
	// full-height one — so the wheel's ceiling is thinkBoxRows, not the
	// collapsed height.
	off := clamp(b.ThinkOff+step, 0, thinkMaxOff(len(a.thinkRows(b, a.contentWidth())), thinkBoxRows))
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

	// A live box shows the newest rows and nothing else. Its head is not
	// history yet — the settled result will print that head in full when the
	// command exits — and a head window over a command still running is a
	// window the reader watches scroll away from the line that just arrived.
	if b.Live && len(rows) > liveRows {
		rows = rows[len(rows)-liveRows:]
	}

	var out []line
	out = append(out, top)
	if header != "" {
		out = append(out, boxRow(box, border, bodySt, header, inner))
	}
	switch {
	case len(rows) == 0 && !b.Err && !b.Live:
		out = append(out, boxRow(box, border, mutedSt, "(no output)", inner))
	case len(rows) == 0:
		// An error result with nothing to say: the frame and footer carry it.
		// A live box with nothing yet has not said anything either — no
		// placeholder, because "(no output)" under a command that is two
		// seconds old is a verdict the transcript has not earned.
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
	// dressed up as a status. A live box has no facts yet: the running call
	// row above it is the "still going" statement, and a footer printing
	// nothing for the next ten seconds is noise.
	if !b.Live {
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
// the agent goroutine is never blocked by it. Since tty_deadline.go the flush
// is also bounded, so a frozen tty costs one dropped frame instead of the
// whole session.
func (a *App) draw() {
	a.paint()
	a.scr.Show()
	// A frame the terminal never took is not self-healing: tcell marks each
	// cell clean before the write, so a dropped frame leaves a partial screen
	// with nothing left to redraw. Sync forces every cell dirty, which costs a
	// full repaint exactly once per dropped frame — the price of not losing
	// the session to a pane that stopped reading.
	if a.frameDropped != nil && a.frameDropped.Swap(false) {
		a.scr.Sync()
	}
}

// logFrameAfterDraw is called once per frame after draw()
// to snapshot the screen buffer to the log file when --log is active.
func (a *App) logFrameAfterDraw() { a.logFrame() }

// paint renders the current state into the screen buffer. a.mu guards the
// state reads; it must NEVER be held across the Show() flush (see draw).
func (a *App) paint() {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.scr
	w, h := a.width, a.height
	s.Clear()
	// Per-frame facts about the viewport: a frame that draws no transcript
	a.scrollHint, a.selRows, a.selBarOn, a.selDockRows, a.linkHits = "", nil, false, nil, nil
	a.stickyHdr, a.stickyVis, a.stickyBlock, a.stickyDoc = 0, 0, -1, 0
	// The pills' hit table is a per-frame fact for the same reason the
	// scrollbar's is: a frame that drops a pill for width must not leave
	// last frame's rectangle live, or a click would open a popup for a
	// reading that is no longer on screen.
	a.statusHits = nil
	// The tab strip's rectangles are a per-frame fact for the same reason.
	a.tabHits = nil
	// selection capture — rows recorded before /clear would copy text that is
	// no longer on screen.
	// The scrollbar's geometry is the same per-frame fact: a welcome frame that
	// draws no bar must not leave last frame's grab live on the last column.
	a.scrollHint, a.selRows, a.selBarOn, a.selDockRows, a.linkHits = "", nil, false, nil, nil

	// Empty transcript: the welcome screen (grok welcome/mod.rs — logo,
	// menu, shortcuts) instead of a blank void.
	if len(a.blocks) == 0 {
		composerTop := h - 1 - a.composerRows()
		// Top bar, then the session strip under it, then the welcome body —
		// the strip owns its row whether or not the transcript exists, so the
		// two screens cannot disagree about what is open.
		a.drawTopBar(s, w, false)
		a.drawTabStrip(s, w)
		a.drawWelcome(s, w, h)
		a.drawSessionPicker(composerTop)
		a.drawHubRoster(composerTop)
		a.drawTreeSelector(composerTop)
		a.drawTrajectory(composerTop)
		a.drawPicker(composerTop)
		a.drawSlashDropdown(composerTop)
		a.drawAskCard(composerTop)
		a.drawSettingsOverlay(composerTop)
		a.drawQueue(composerTop)
		a.drawComposer(composerTop)
		a.drawStatusRow(h - 1)
		a.drawStatusPopup()
		// A gesture made before the first block exists — the composer is the
		// only selectable surface there — is highlighted here too; the branch
		// returns, so it never reaches the call at the end of paint().
		a.drawSelection()
		// The welcome screen has no transcript to sit over, but a toast
		// arriving before the first turn is exactly when one shows up (the
		// MCP connect finishes last), so the corner is taken here too.
		a.drawToasts(s)
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
	// The top bar belongs to the main pane: its prompts get the pane's width,
	// and a bar running the terminal's full width would print them under the
	// panel's own surface.
	a.drawTopBar(s, a.rightEdge(), true)
	// The session strip owns the row under the top bar (opencode's tab row),
	// so the transcript's first row shifts with it — transcriptTop() is the
	// single place that offset is computed, so the strip cannot desync the
	// scroll math by being drawn without being counted.
	a.drawTabStrip(s, a.rightEdge())
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
	end := min(start+vp, total)

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
	// The sticky header (grok scrollback/sticky.rs): the prompt the viewport has
	// scrolled past pins at the top of the transcript, IN FRONT of the rows it
	// re-renders — it replaces the viewport's first rows rather than pushing
	// the stream down. One thing follows from that, and it is the reason the
	// header is cheap: the document row a screen row shows does NOT move. Below
	// the header, screen row y is still document row start+(y-top), so every
	// hit-test and the selRows capture below keep their arithmetic untouched;
	// only the header's OWN rows resolve elsewhere, and they are published
	// (stickyHdr/stickyVis/stickyBlock/stickyDoc) for exactly that.
	sticky := computeSticky(int32(start), vp, a.stickyPrompts())
	header := a.stickyHeaderRows(sticky, max(10, contentW-2))
	gapRow := sticky.rows - len(header) // 1 while pinned, 0 while being pushed off
	// The header rows first (they overwrite the viewport's own first rows),
	// then the stream from the row the header ends at. Both are rowViews, so
	// the paint loop below cannot tell them apart.
	view := make([]rowView, 0, max(len(header)+gapRow, end-start))
	view = append(view, header...)
	for range gapRow {
		view = append(view, rowView{})
	}
	view = append(view, a.viewRows(int32(start+sticky.rows), int32(end))...)
	selRows := make([]selRow, 0, len(view))
	linkHits := make([]linkHit, 0)
	for row, r := range view {
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
		rowLinkHits := make([]linkHit, 0)
		for _, run := range r.ln.runs {
			st := run.style
			if banded {
				// Runs carry the band too, or the row fill survives only
				// in the gaps between the glyphs.
				st = st.Background(r.ln.bg)
			}
			drawText(s, x, y, run.text, st)
			if run.link != "" {
				rowLinkHits = append(rowLinkHits, linkHit{x0: x, x1: x + paintedWidth(run.text) - 1, y: y, target: run.link})
			}
			if !run.chrome {
				if startX < 0 {
					startX = x
				}
				content.WriteString(run.text)
			}
			x += paintedWidth(run.text)
		}
		if startX < 0 {
			startX = 0
		}
		// Selection hit-testing works off this text (the streaming cursor is
		// decoration, not content). The header's rows are captured like any
		// other: what is painted is what is copied, and the copy of a pinned
		// prompt is the text the header shows.
		selRows = append(selRows, selRow{text: strings.TrimSuffix(content.String(), "▍"), x0: startX})
		// The timestamp is painted over the runs after them, so its columns are
		// not clickable even where an earlier URL run extended underneath it.
		if r.ts != "" {
			last := edge - width(r.ts) - 3
			for i := range rowLinkHits {
				rowLinkHits[i].x1 = min(rowLinkHits[i].x1, last)
			}
		}
		for _, hit := range rowLinkHits {
			if hit.x0 <= hit.x1 {
				linkHits = append(linkHits, hit)
			}
		}
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
	// rows: a filled groove with the thumb riding it (opencode's ScrollBar —
	// a track background rather than a hairline glyph, and a thumb that can
	// land between two rows). The half-row span says which cells are thumb: a
	// full block, the upper or lower half block, or the bare groove.
	if sbOk {
		// The groove is the page's own raised surface (bg_highlight), one step
		// off the background: enough to read as a rail, quiet enough not to
		// compete with the transcript. The thumb is the palette's own bright
		// gray — Get's documented fallback chain resolves it for a custom
		// theme (gray_bright → gray → muted) without a new slot.
		groove := a.cellColor(a.th.Get(theme.BgHighlight))
		grooveSt := tcell.StyleDefault.Background(groove)
		thumbSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayBright))).Background(groove)
		for y := range end - start {
			ch := sbGlyph(y, sbStart, sbEnd)
			st := grooveSt
			if ch != ' ' {
				st = thumbSt
			}
			s.SetContent(edge-1, y+top, ch, nil, st)
		}
	}
	// Publish the bar's geometry for the mouse hit-test (selection.go): the
	// press that grabs the thumb and the drag that moves it act on exactly the
	// bar painted here — and on no bar at all when the transcript fits, since
	// sbOk false is what keeps grab off a column that carries content. The
	// column travels with it: with the context dock open the transcript's last
	// column is not the terminal's last, and a grab keyed to width-1 answers a
	// press on the panel's border instead of the bar under the pointer. The
	// thumb is published in half rows, the unit the bar was drawn in.
	a.selBarOn, a.selBarVP, a.selBarPos, a.selBarEnd = sbOk, vp, sbStart, sbEnd
	a.selBarX, a.selBarTotal = 0, total
	if sbOk {
		a.selBarX = edge - 1
	}
	a.selRows, a.selTop, a.linkHits = selRows, start, linkHits
	// The sticky header's geometry, published like the scrollbar's. Only the
	// header's OWN rows are a special case (a corner on one takes the pinned
	// prompt's rows); every row below it keeps the viewport's own mapping,
	// because the header re-renders rows the viewport already owned.
	a.stickyHdr, a.stickyVis, a.stickyBlock = sticky.rows, len(header), sticky.block
	a.stickyDoc = 0
	if sticky.block >= 0 {
		// The header's first painted row IS the prompt's row clipTop, so this is
		// the document row every hit-test reads instead of re-deriving it.
		a.stickyDoc = sticky.row + int32(sticky.clipTop)
	}
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
	up, down := a.sm.Indicator(total, vp)
	if up > 0 || down > 0 {
		a.scrollHint = fmt.Sprintf("▲ %d ▼ %d", up, down)
	} else {
		a.scrollHint = ""
	}
	// "↓ n new": the scroll hint says a jump is possible, this is the jump.
	// Painted over the transcript's own rows, before the dock and every
	// overlay, so a panel that covers the chip wins the click.
	a.drawJumpChip(s, edge, top, vp, down)
	// The composer's first input row sits below the transcript; it occupies
	// composerRows() rows above the status line.
	composerTop := h - 1 - cRows
	// The panel first, so every overlay below paints over it: the dock is chrome
	// beside the transcript, never a surface a modal has to negotiate with.
	if a.dockOn() {
		dtop, dh := a.dockGrid()
		a.drawDock(s, w-dockCols, dtop, dh)
		a.selDockRows = a.selDockRowsForPaint()
	}
	a.drawSessionPicker(composerTop)
	a.drawDiffOverlay(composerTop)

	a.drawHubRoster(composerTop)
	a.drawTrajectory(composerTop)
	a.drawTreeSelector(composerTop)
	a.drawPicker(composerTop)
	a.drawSlashDropdown(composerTop)
	a.drawAskCard(composerTop)
	a.drawSettingsOverlay(composerTop)
	// The mid-turn queue (#157) paints between the transcript and the
	// composer, so it goes after every overlay (an overlay is modal and owns
	// its rows) and before the composer, whose top border must stay the edge
	// of the box. It is chrome, not content, and publishes its own hit table.
	a.drawQueue(composerTop)
	a.drawComposer(composerTop)
	a.drawStatusRow(h - 1)
	// The pill popup paints after the status row (it is anchored to it) and
	// before the selection highlight, so a drag that ends over the panel does
	// not shine through it.
	a.drawStatusPopup()
	// The toasts paint last, over everything, so the corner is theirs: a
	// notice that scrolled under a selection highlight or a picker frame is
	// a notice the user never saw. They live in the transcript's rows, above
	// the composer, so nothing here can cover the draft.
	a.drawToasts(s)
	// Last, so it paints over every surface the frame just drew: see the note
	// where the selection geometry is published above.
	a.drawSelection()
	// The user-message surfaces paint after even the selection: they are
	// transient popups anchored to a click, and a highlight left over from an
	// earlier drag must not shine through the menu. The menu's own geometry
	// clamps it above the composer, so painting last cannot cover the prompt
	// the user is about to type into.
	a.drawMsgView(composerTop)
	a.drawMsgMenu()
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
	// A row is 6 cells of marker/dot/indent, the name, then the detail cell.
	// The name takes what the names need, up to the whole row: the old fixed
	// 28-cell detail reserve is what cut a name to twelve cells plus an
	// ellipsis on a narrow terminal, losing the tail of the title — and the
	// detail is the part that goes when the row cannot hold both, not the
	// thing being picked. The paint loop's room>4 guard drops it.
	//
	// It is measured BEFORE the window is taken, because a name wider than
	// the column wraps onto continuation rows — windowing first would
	// re-flow the rows the column was measured from. lines() reads labelW, so
	// the wrap width and the painted width are one number.
	p.labelW = min(p.widestLabel()+2, inner-6)
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

	// Rows. A label wider than the name column has already been wrapped into
	// continuation rows by lines(); each paints its own line of the label at
	// the column the label starts at, and only the row's first line carries
	// the marker, the current dot and the detail cell. The name column is
	// p.labelW, measured above the window for exactly this reason.
	labelW := p.labelW
	// Publish the row map the mouse router hit-tests against, so a click lands
	// on exactly the row the user saw — a continuation row included: it is the
	// same item, and clicking it must select the row it belongs to.
	p.hitY0, p.hitItem = y, make([]int, len(lines))
	// The selection is an ITEM, not a line: a wrapped label's continuation
	// rows are the highlighted row's, so the highlight follows the item and
	// moving down past a three-line title never looks like selecting a blank.
	selItem := -1
	if i := selLine - start; i >= 0 && i < len(lines) {
		selItem = lines[i].itemIdx
	}
	for i, ln := range lines {
		p.hitItem[i] = -1 // a section header is not a target
		if !ln.header {
			p.hitItem[i] = ln.itemIdx
		}
		sel := !ln.header && ln.itemIdx == selItem
		st := rowBg
		if sel {
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
		if !ln.cont {
			marker := "  "
			if sel {
				marker = "▶ "
			}
			drawText(a.scr, x0+2, y, marker, st.Foreground(a.cellColor(a.th.Get(theme.AccentAssistant))))
			if ln.item.Current {
				drawText(a.scr, x0+4, y, "●", st.Foreground(a.cellColor(a.th.Get(theme.AccentSuccess))))
			}
		}
		// The label cell is already the wrapped line: wrapping it here would
		// double the cut, and clipping it would put a second ellipsis on a
		// line that fits. lines() owns the wrap width for this reason.
		drawText(a.scr, x0+6, y, ln.text, st.Foreground(a.cellColor(a.th.Get(theme.AccentUser))))
		// The detail rides the row's FIRST line, in the column the name
		// column left, and is clipped by the room that is actually left —
		// the same cell it always had. The wrapped rows below carry title
		// text only, so a name never runs into it.
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
// (border+pad+prefix+right pad+border). It is measured against rightEdge, not
// the terminal: with the sidebar open the box sits inside the main pane, so a
// draft must wrap where the box ends — text the box cannot show is a wrap the
// editor has to know about, or the prompt grows rows the box will not paint.
func (a *App) composerAvail() int {
	avail := a.rightEdge() - 7
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
// info divider, tinted with the statusLine tokens. The box is as wide as the
// main pane, so the sidebar's columns are its right edge — the prompt is a
// window of its own now, not a row that runs the terminal's full width under
// the panel.
func (a *App) drawComposer(yTop int) {
	w := a.rightEdge()
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
	// The reasoning level beside the model it applies to: the two are one
	// request, and "which model" alone left the other half of it invisible.
	// The bare rung, not "thinking <level>" — the model it sits beside says
	// what the pair is, and the word only added width.
	if l := a.thinkingLevel(); l != "" {
		info += " · " + l
	}
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
	// takes the pixels instead; when the divider is too narrow for the hint,
	// the hint is dropped rather than eating the model name. The copy
	// confirmation that used to lead the queue is a toast now (toast.go), so
	// two hints want the slot: the draft's own hidden rows — text the user is
	// composing right now beats scrollback they already read — then the
	// transcript's ▲n▼n.
	var hint string
	if hint = draftHint(above, below); hint == "" {
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

// drawJumpChip paints the "↓ n new" jump-to-latest button over the
// transcript's own bottom-right corner while rows are hidden below the
// viewport, and publishes the rectangle it drew so the mouse can hit it
// (App.jump). The scroll hint on the composer divider says a jump is
// possible; this is the jump. Caller holds a.mu.
//
// It takes the transcript's edge, not the terminal's, so the chip stays with
// the content when the context dock is open. down is the hidden-row count
// from the scroll model: zero means the tail is on screen and the button has
// nothing to say.
func (a *App) drawJumpChip(s tcell.Screen, edge, top, vp, down int) {
	a.jump = panelRect{}
	if down <= 0 || vp < 2 {
		return
	}
	// A pill, not a bare line of text: the row behind it is transcript
	// content, so the button needs its own background (and a cell of padding
	// on each side) to read as a control rather than as a scrolled line.
	label := "↓ " + strconv.Itoa(down) + " new"
	w := width(label) + 2
	if w > edge-6 {
		label, w = "↓", 3
	}
	// Two rows above the bottom edge, and clear of the scrollbar's column: the
	// bar reports position continuously and must stay readable behind the
	// chip, and a chip on the last row would sit on the newest line.
	x, y := edge-1-w, top+vp-2
	bg := tcell.StyleDefault.
		Background(a.cellColor(a.th.Get(theme.BgHighlight))).
		Foreground(a.cellColor(a.th.Get(theme.TextPrimary)))
	fillPanelRows(s, y, y, x, edge-1, bg)
	drawText(s, x+1, y, label, bg)
	a.jump = panelRect{x: x, y: y, w: w, h: 1}
}

// drawStatusRow renders the bottom row: the working directory on the left,
// the configured HUD segments (settings statusLine.segments) right-aligned
// (caller holds a.mu). The keyboard chords used to live on the left; /hotkeys
// and the welcome menu carry them now, which frees the room the metrics need
// on a small terminal. The row belongs to the main pane: its budget and its
// right edge are the pane's, not the terminal's, so the metrics never paint
// into the sidebar's columns.
func (a *App) drawStatusRow(y int) {
	parts := a.hudParts()
	w := a.rightEdge()
	// The running tool call leads the row: "● <name> · cd <cwd>" on
	// the left, the configured segments right-aligned.
	cmdLabel := a.hudCommand()
	if cmdLabel != "" {
		pathLbl := pathDisplay(a.cwd, w-2-width(cmdLabel)-2-hudEssentialWidth(parts)-1)
		if pathLbl != "" {
			pathSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.Gray)))
			drawText(a.scr, 2, y, pathLbl, pathSt)
		}
		a.drawHUD(y, w, 2+width(cmdLabel)+width(pathLbl)+2, parts)
		return
	}
	// The work timer and the decode rate are what the row is for during a
	// run, so they claim the space first: the path is what shrinks.
	budget := w - 2 - hudEssentialWidth(parts) - 1
	lbl := pathDisplay(a.cwd, budget-2)
	if lbl != "" {
		pathSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.Gray)))
		drawText(a.scr, 2, y, lbl, pathSt)
	}
	a.drawHUD(y, w, 2+width(lbl), parts)
}

// drawHUD renders the configured status segments right-aligned on the
// status row (caller holds a.mu), within the main pane's own width — w is the
// pane's right edge, not the terminal's, so the metrics stop at the sidebar
// instead of running under it. Segment colors come from the statusLine*
// tokens, the separators from statusLineSep, and statusLineBg fills the row
// when the theme sets one. The metrics win a narrow row: the path is drawn
// first against the space the essential segments need, and any segment that
// still does not fit is dropped by keep-rank (theme and model first, the work
// timer and the rate last).
func (a *App) drawHUD(y, w, leftEnd int, parts []hudPart) {
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
	end := w - 2
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
		// Publish a pill's rectangle as it is painted, so the click that
		// opens its popup is tested against the pixels the frame actually
		// put there. A pill dropped for width never publishes one, so a
		// reading that is not on screen cannot be clicked.
		if p.popup {
			a.statusHits = append(a.statusHits, statusHit{
				name: p.name,
				rect: panelRect{x: x, y: y, w: width(p.text), h: 1},
			})
		}
		x += width(p.text)
	}
}

// statusSegments is the HUD segment vocabulary (settings
// statusLine.segments): model, tokens, context, cost, rate, theme, time.
// `tokens` and `time` are the two dsh PILLS (statuspill.go) — clickable, and
// carrying the headline reading only.
var statusSegments = map[string]bool{
	"model":   true,
	"tokens":  true,
	"context": true,
	"cost":    true,
	"rate":    true,
	"ttft":    true,
	"theme":   true,
	"time":    true,
	"command": true,
	// split is the pre-pill `tokens` reading: the ↑⇢↓ glyph split, kept
	// reachable under a name of its own so a session that wants the
	// buckets inline can still ask for them
	// (`statusLine.segments: time,split,context`).
	"split": true,
	// cache is the dsh "Cache hit N%" reading (deepseek-harness
	// StatsPills.tsx UsagePill): the session's total with the hit rate
	// beside it, so the one number that says whether the prefix cache is
	// working needs no subtraction. dsh is the donor here — the pill is
	// exactly "total · Cache hit N%", and xdev's tokens segment already
	// carries the split the pill's dialog breaks out.
	"cache": true,
	// toolcalls is dsh's "{turns} turns {steps} steps" TimePill count
	// reduced to the one figure xdev has no other home for: how many tool
	// calls this session spent, with failures beside it.
	"toolcalls": true,
	// debugMouse renders the last mouse event on the status bar
	// (settings `tui.debugMouse`, off by default). It always shows
	// when enabled: the segment never hides, so the log is visible
	// the moment it is opted in.
	"debugMouse": true,
	// sessions is the open-tab count with busy/unread marks — the whole
	// signal the multi-session TUI needs without a painted tab strip.
	"sessions": true,
}

// defaultStatusSegments is the shipped layout: the two dsh composer pills,
// right-aligned, and nothing else. dsh solved the same problem — a metrics
// row competing with the rest of the composer dock — by keeping two pills
// whose dialogs carry the breakdown, and that is the shape here: the time
// pill reads the work timer, the turn/step counts and the decode speed; the
// token pill reads the session total and the cache hit rate. Every other
// figure (the context meter, the spend, the call count, the token split)
// is one click away, which is where /usage's long form used to be the only
// place to read it — a report is not where a glance should have to go.
// The model keeps its composer divider slot, which is chrome rather than a
// segment.
var defaultStatusSegments = []string{"sessions", "command", pillTime, pillToken}

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
	case pillToken:
		// The token PILL, not the old ↑⇢↓ split: dsh's UsagePill button
		// reads "{total} · Cache hit N%" and its dialog breaks out the
		// buckets, which is the same division of labour this row now has
		// (pillLabel draws the button, drawStatusPopup the dialog). The
		// split is still one settings entry away (`tokens` keeps the old
		// meaning below) for a session that wants it inline.
		//
		// The total counts the cache write as well as the read: it is a
		// billed bucket, and dropping it would make the pill's own total
		// disagree with the popup it opens.
		if lbl := a.pillLabel(pillToken); lbl == "" {
			return "", ""
		} else {
			return lbl, theme.StatusLineSpend
		}
	case "split":
		// The pre-pill token reading, kept reachable under its own name:
		// the two glyphs each claim one half of a split the provider
		// reports three ways, so the row shows the split: ↑ is fresh
		// input, ⇢ the cache read beside it, and ↓ output with the
		// reasoning already inside it broken out. cache and think hide
		// when the provider reports none, so a provider that bills no
		// cache and reasons for nothing keeps the plain two-glyph row.
		if a.st.TokensIn == 0 && a.st.TokensOut == 0 {
			return "", ""
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s↑%s", a.th.HUDIcon(theme.HUDIconDatabase), HumanTokens(a.st.TokensIn))
		if a.st.TokensCache > 0 {
			fmt.Fprintf(&b, " ⇢%s", HumanTokens(a.st.TokensCache))
		}
		fmt.Fprintf(&b, " │ ↓%s", HumanTokens(a.st.TokensOut))
		if a.st.TokensThink > 0 {
			fmt.Fprintf(&b, " ˟%s", HumanTokens(a.st.TokensThink))
		}
		return b.String(), theme.StatusLineSpend
	case "cache":
		// dsh's cache-hit RATE (deepseek-harness StatsPills.tsx), and only
		// the rate. dsh's pill reads "{total} · Cache hit N%" because that
		// pill is its ONLY token reading; xdev's row already carries the
		// split, and the total beside it was the same 66.3k drawn twice.
		//
		// No leading icon either: the database icon is already on the token
		// segment this one refines, and a second ▤ three cells later is the
		// same duplication in another dress. The rate is cacheRead over the
		// BILLED prompt side (fresh + cached) — dsh's three disjoint
		// buckets minus the cache write xdev does not bill separately —
		// because output was never cacheable and counting it would read
		// low for the wrong reason. Hidden until something was actually
		// served from the cache: 0% on a provider that reports no cache is
		// a claim about a measurement nobody made.
		if a.st.TokensCache <= 0 || a.st.TokensIn+a.st.TokensCache <= 0 {
			return "", ""
		}
		return fmt.Sprintf("cache %d%%",
			100*a.st.TokensCache/(a.st.TokensIn+a.st.TokensCache)), theme.StatusLineSpend
	case "toolcalls":
		// dsh's TimePill counts turns and steps; the one figure xdev has
		// no other home for is the call count, and a failed call is worth
		// seeing beside the total rather than only in /usage. Hides at
		// zero so a session that has not called a tool keeps a clean row.
		if a.st.ToolCalls == 0 {
			return "", ""
		}
		if a.st.ToolErrors > 0 {
			return fmt.Sprintf("%s%d calls (%d failed)", a.th.HUDIcon(theme.HUDIconTool),
				a.st.ToolCalls, a.st.ToolErrors), theme.StatusLineSpend
		}
		return fmt.Sprintf("%s%d calls", a.th.HUDIcon(theme.HUDIconTool), a.st.ToolCalls), theme.StatusLineSpend
	case "context":
		// used/total of the LIVE context: what the next request costs against
		// the model's window. Hidden until both halves are known — an
		// undiscovered window (0) or a session that never ran makes no claim.
		if a.st.CtxWindow <= 0 || a.st.CtxUsed == 0 {
			return "", ""
		}
		return fmt.Sprintf("ctx %s/%s", HumanTokens(a.st.CtxUsed), HumanTokens(a.st.CtxWindow)), theme.StatusLineContext
	case "cost":
		if a.st.Cost <= 0 {
			return "", ""
		}
		return fmt.Sprintf("$%.4f", a.st.Cost), theme.StatusLineCost
	case pillTime:
		// The time PILL (dsh's TimePill): the work timer, then the
		// turn/step counts, then the decode speed. Total time spent
		// WORKING — the banked spans plus the live one; an idle agent, and
		// an agent parked on a question card, shows a frozen number,
		// because the wall clock between turns belongs to the user. "0s"
		// is a reading, so the pill never hides.
		return a.pillLabel(pillTime), theme.StatusLineSpend
	case "rate":
		// omp's ⚡ tok/s: the decode speed of the last message the provider
		// gave a token count for. ONE formula, one source — a live estimate
		// from rune counts reads up to 5x off the measured value, and swapping
		// between the two mid-turn is what made the number look like it was
		// drifting. Unmeasured hides; a message too short to measure clears
		// the rate (AddUsage), so a stale reading never stands in for one.
		if a.st.Rate <= 0 {
			return "", ""
		}
		return fmt.Sprintf("%s %.1f t/s", a.th.HUDIcon(theme.HUDIconGauge), a.st.Rate), theme.StatusLineSpend
	case "ttft":
		// ⌚ ttft: per-turn time-to-first-token. SetTTFT writes it;
		// the segment stays hidden until a turn has actually finished.
		if a.st.TTFT <= 0 {
			return "", ""
		}
		return a.th.HUDIcon(theme.HUDIconGauge) + " " + humanDur(time.Duration(a.st.TTFT)*time.Millisecond) + " ", ""
	case "theme":
		return a.th.Name, theme.StatusLineSep
	case "sessions":
		// Open-session summary: "2 tabs" idle, "2 tabs ·1" with one busy,
		// "2 tabs ·1 ✦1" with unread. Hidden on a single idle session so a
		// normal one-chat TUI keeps a clean row.
		n := len(a.tabs)
		if n == 0 {
			return "", ""
		}
		busy, unread := 0, 0
		for _, t := range a.tabs {
			if t.Running {
				busy++
			}
			if t.Unread {
				unread++
			}
		}
		if n == 1 && busy == 0 && unread == 0 {
			return "", ""
		}
		label := "tabs"
		if n == 1 {
			label = "tab"
		}
		text := fmt.Sprintf("%d %s", n, label)
		if busy > 0 {
			text += fmt.Sprintf(" ·%d", busy)
		}
		if unread > 0 {
			text += fmt.Sprintf(" ✦%d", unread)
		}
		return text, theme.StatusLineSep
	case "debugMouse":
		return a.debugMouseLine, theme.StatusLineSep
	}
	return "", ""
}

// mouseDebugLine formats one mouse event for the status bar: the button,
// the press/drag/release edge, the wheel direction and the coordinates.
// It is the only way to see what the terminal is actually sending —
// tcell strips the SGR motion bit, so a held drag looks like a press at
// every report, and the gesture a user thinks they made is not always
// the one that arrives.
func mouseDebugLine(m *tcell.EventMouse, press bool) string {
	x, y := m.Position()
	var btn string
	switch {
	case m.Buttons()&(tcell.WheelUp|tcell.WheelDown|tcell.WheelLeft|tcell.WheelRight) != 0:
		switch {
		case m.Buttons()&tcell.WheelUp != 0:
			btn = "wheel↑"
		case m.Buttons()&tcell.WheelDown != 0:
			btn = "wheel↓"
		case m.Buttons()&tcell.WheelLeft != 0:
			btn = "wheel←"
		default:
			btn = "wheel→"
		}
	case m.Buttons()&tcell.Button1 != 0:
		btn = "btn1"
	case m.Buttons()&tcell.Button2 != 0:
		btn = "btn2"
	case m.Buttons()&tcell.Button3 != 0:
		btn = "btn3"
	default:
		btn = "up"
	}
	edge := "move"
	if press {
		edge = "press"
	} else if m.Buttons() == 0 {
		edge = "release"
	}
	mod := m.Modifiers()
	var mods string
	if mod&tcell.ModShift != 0 {
		mods += "S"
	}
	if mod&tcell.ModAlt != 0 {
		mods += "A"
	}
	if mod&tcell.ModCtrl != 0 {
		mods += "C"
	}
	if mods != "" {
		mods = "+" + mods
	}
	return fmt.Sprintf("%s%s %s @%d,%d", btn, mods, edge, x, y)
}

// hudPart is one rendered HUD segment, carrying the segment name the
// keep-rank drop loop keys on. popup marks the two segments that are
// clickable: their text is the dsh pill's own label, and a click on one
// opens the panel that breaks that label out (statuspill.go).
type hudPart struct {
	name, text, token string
	popup             bool
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
			parts = append(parts, hudPart{name, text, token, statusPills[name]})
		}
	}
	return parts
}

// hudEssentialWidth is the space the metrics that must always survive a
// narrow row take: the two dsh pills (and the rate, when it is configured
// on its own), separated by a HUD separator. The rest of the HUD (and the
// hotkeys) give way to them.
//
// hudEssentialRank is that threshold by name rather than by number: the
// token split moved rank when the dsh segments landed, and a magic 5 would
// have silently promoted the wrong segment to "always survives" — which is
// exactly how the token counter stopped dropping on a narrow row. The two
// pills both sit at or above it now (statusKeepRank: tokens 7, time 8), so
// the path is what gives way on a narrow row, never a headline.
const hudEssentialRank = 7 // statusKeepRank: the pills and the rate

func hudEssentialWidth(parts []hudPart) int {
	essential := make([]hudPart, 0, 2)
	for _, p := range parts {
		if statusKeepRank[p.name] >= hudEssentialRank {
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
// runs out of width — the drop loop sheds the LOWEST rank first. The two dsh
// pills are what the user reads during a run, so they are dropped last and
// rank at or above hudEssentialRank; the theme name and the model label go
// first, and every opt-in refinement (the cache rate, the call count, the
// context meter, the token split) gives way before the two headlines it
// refines.
var statusKeepRank = map[string]int{
	"theme":     0,
	"model":     1,
	"cost":      2,
	"toolcalls": 3,
	"cache":     4,
	"split":     5,
	"context":   6,
	"tokens":    7,
	"time":      8,
	"rate":      9,
	"sessions":  6,
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
		x += runeWidth(r)
	}
}

// paintedWidth is the number of terminal cells drawText advances through.
// runewidth.StringWidth can disagree for a multi-rune grapheme, while the
// painter advances once per rune; hit rectangles must follow the painter.
func paintedWidth(s string) int {
	n := 0
	for _, r := range s {
		n += runeWidth(r)
	}
	return n
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

// HumanTokens renders 1234 as "1.2k" and a round 200000 as "200k": the HUD
// meter shows round windows and round spend, where ".0" is pure noise in a
// row that is always competing with the working-directory path for width.
func HumanTokens(n int64) string {
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
