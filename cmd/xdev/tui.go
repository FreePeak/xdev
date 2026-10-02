package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/collab"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/dist"
	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/memory"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/theme"
	"github.com/FreePeak/xdev/internal/tool"
	"github.com/FreePeak/xdev/internal/tui"
)

// runTUI drives the interactive TUI mode (M4).

func tabInfos(ts *tabset) []tui.TabInfo {
	snap := ts.snapshot()
	o := make([]tui.TabInfo, len(snap))
	for i, t := range snap {
		o[i] = tui.TabInfo{ID: t.ID, Title: t.Title, Running: t.Running, Unread: t.Unread, Current: t.Current}
	}
	return o
}

func runTUI(opts printOptions, themeName string) (exitCode int, err error) {
	cwd, err := os.Getwd()
	if err != nil {
		return 2, err
	}

	// Config & model (same path as print mode).
	cfg, err := config2Load()
	if err != nil {
		return 2, err
	}
	// Warm discovery-enabled providers off the UI thread: /model's picker
	// reads every provider's catalog on open, and a cold discovery probe
	// (4s timeout, dead-server worst case) would freeze the key thread on
	// the first open. Pinned-only providers need no warm-up — providerModels
	// answers those from memory.
	for name, pc := range cfg.Providers {
		if pc != nil && pc.Discovery != nil {
			go providerModels(name, pc)
		}
	}
	modelRef, effortRef, err := resolveModel(opts.Model, cfg, lastSettings())
	if err != nil {
		return 2, err
	}
	provName, modelName, err := config.ParseModelRef(modelRef)
	if err != nil {
		return 2, err
	}
	// The request-side thinking level: --thinking wins, else the persisted
	// `thinking` key, else the model's own ":effort" (applyThinkingFlag's "auto"
	// branch). roleEffort keeps the UN-folded model effort, so a later
	// /thinking auto (or the Shift-Tab toggle) re-binds to the model instead of
	// freezing whatever level an earlier call pinned. startLevel is the level
	// the user chose, BEFORE the model check: a model that cannot reason runs
	// at "auto" while the choice stays sticky, so switching back to a
	// reasoning model restores it.
	roleEffort := effortRef
	startLevel := thinkingLevel(lastSettings(), launch.Thinking)
	appliedLevel := thinkingForModel(startLevel, provName, modelName, cfg)
	if effortRef, err = applyThinkingFlag(appliedLevel, roleEffort); err != nil {
		return 2, err
	}
	pc, ok := cfg.Providers[provName]
	if !ok {
		return 2, fmt.Errorf("unknown provider %q (have: %v)", provName, providerKeys(cfg))
	}
	prov, err := buildProvider(provName, pc, modelName, cfg)
	if err != nil {
		return 2, err
	}
	// /model live switch: the submit loop reads this holder instead of the
	// startup constants, so switching the active model applies to the next
	// turn. Subagent task tools in `reg` are built once at startup and keep
	// the initial model (documented ceiling below).
	// Plan mode (M11): shared state across submits; the propose reviewer
	// surfaces the plan in the transcript and stays in read-only until the
	// user resolves (/plan off to approve, feedback to revise).
	planMode := &agent.PlanMode{}
	// Vibe mode (M14 #58): the director scope. Declared here because the
	// per-submit agent, the session swaps, and the status line all read it;
	// it is built below, once the registry's hub and task tools are known.
	var vibeScope *agent.VibeScope
	// vibeSys is the director's system prompt (built with the scope, below).
	var vibeSys func() string
	vibeActive := func() bool { return vibeScope != nil && vibeScope.Active() }
	// /prewalk live toggle: the holder is what each submit reads; Set
	// resolves the ref through the same precedence as --prewalk-into.
	var prewalkMu sync.Mutex
	prewalkTarget := &agent.FailoverTarget{}
	prewalkOn := false
	// Advisor (M11 #12): a background reviewer when settings.advisor is on
	// AND settings.advisorModel resolves. It watches transcript snapshots and
	// steers into the live run.
	adv := buildAdvisor(cfg, lastSettings())
	var advisorOn atomic.Bool
	advisorOn.Store(adv != nil)
	modelMu := &sync.Mutex{}
	live := &struct {
		prov       ai.Provider
		model      string
		provName   string
		effort     string
		roleEffort string
		level      string
	}{prov, modelName, provName, effortRef, roleEffort, startLevel}
	// thinkLevel reads the level in force (modelMu-guarded, like the rest of
	// the live holder): "auto" lets live.roleEffort decide at fold time.
	thinkLevel := func() string {
		modelMu.Lock()
		defer modelMu.Unlock()
		return live.level
	}

	// Tools + system prompt (shared with print mode).
	// The propose reviewer (TUI): surface the plan and hold the decision
	// for the user — /plan off resolves accept, any next prompt is the
	// revision note. Headless runs auto-accept (nil reviewer).
	// ponytail: the child (task-tool) default budget is stamped here at
	// startup, so a later /thinking flip reaches the parent turn and vibe
	// workers but not already-built task children — they follow --thinking and
	// the persisted key only. Rebuild the registry if that ever matters.
	reg := newToolRegistry(cwd, prov, provName, modelName, lastSettings(), effortBudget(effortRef), planMode)
	defer closeSharedHub() // hub-started children are session-scoped (T3 #8)
	// Rebuild-time interruption notice names the calls that are free to
	// repeat (tool.Replayer). Set against reg, which MCP/extension tools join
	// as they register — the same registry every turn reads.
	session.ReplaySafety = reg.ReplaySafe
	// toolsForTurn routes each turn at the vibe director's restricted view
	// while the mode is on. The parent registry is never mutated, so exiting
	// the mode restores the full toolset by construction.
	toolsForTurn := func() *tool.Registry {
		if vibeActive() {
			return vibeScope.Registry()
		}
		return reg
	}
	// Recomputed per submit: MCP and extension processes register tools
	// after startup, and a boot-frozen prompt would never mention them.
	// Extension processes: loaded ONCE (handshakes are expensive), their
	// tools join the live registry so the lazy prompt picks them up, and
	// each per-submit agent gets the same fail-closed Interceptor.
	var (
		agentMu  sync.Mutex
		curAgent *agent.Agent
		// lastTurnFailed marks whether the previous turn ended badly
		// (aborted or provider error): the instruction that follows a failure
		// is a friction signal for the sharpshooter backend (#89).
		lastTurnFailed atomic.Bool
	)
	routeToLiveAgent := func(call func(*agent.Agent, string)) func(text string) {
		return func(text string) {
			agentMu.Lock()
			target := curAgent
			agentMu.Unlock()
			if target != nil {
				call(target, text)
			}
		}
	}
	// #90: the mailbox push path. The TUI rebuilds its agent per submit, so
	// delivery rides the same live-agent indirection extensions steer through.
	// With no turn running the sink delivers nothing, and the poller leaves the
	// message UNREAD — the `inbox` tool and the next turn still see it.
	setInboxSink(func(m agent.Message) bool {
		agentMu.Lock()
		a := curAgent
		agentMu.Unlock()
		if a == nil {
			return false // idle: the message stays unread for the next turn
		}
		a.FollowUp(inboxFollowUp(m))
		return true
	})
	defer func() {
		setInboxSink(nil)
		if stopInbox != nil {
			stopInbox()
		}
	}()
	exts := attachExtensions(context.Background(), reg,
		routeToLiveAgent(func(a *agent.Agent, s string) { a.Steer(s) }),
		routeToLiveAgent(func(a *agent.Agent, s string) { a.FollowUp(s) }), cfg)
	if exts != nil {
		defer exts.Close()
	}

	overrides := agent.LoadSystemPromptOverrides(cwd)
	// ONE memory backend for the whole session. buildMemory was called at
	// each of the three sites below, so a cadence-tracking backend (Hindsight
	// retains every N turns; mnemopi counts turns) reset on every call and
	// never reached its threshold (#86).
	sessionMemory := buildMemory(lastSettings())
	buildSys := promptFnWithMemory(basePrompt(opts, cwd), cwd, reg,
		tailSystemPrompt(overrides, opts.AppendSystem), sessionMemory)

	// Session.
	store, err := openStartupSession(cwd, opts)
	if err != nil {
		return 2, fmt.Errorf("session: %w", err)
	}
	// tabs is the live session set. `store` below is a convenience that
	// always names the CURRENT session: every closure that used to read
	// the one `store` variable now reads through tabs, so a switch re-
	// points them without rewriting every call site. A turn claims its
	// own session id at start and keeps that store even after a switch.
	tabs := newTabset(store)
	storeOf := func() *session.Store { return tabs.store() }
	// The breadcrumb keys --continue for this pane. A fresh session is
	// memory-only until its first assistant message, so record the
	// AUTO-PERSIST path: --continue already guards with os.Stat, and a
	// breadcrumb naming the live session beats silently reopening the
	// previous one (the /new, /drop defect).
	saveBreadcrumb(breadcrumbPath(store))
	wireTaskParent(reg, storeOf())
	// The resume line on the way out. Registered BEFORE the defers that flush
	// and close the store and release the screen, so LIFO order prints it last
	// of the three: tcell has left the alt screen (anything written before Fini
	// is wiped) and the session file is closed. It names the store the close
	// defer below captured — closeAll empties the tabset, so storeOf() reads
	// nil by then and the line printed nothing at all (#519). That capture is
	// what lets /new, /fork and /resume change what the line names.
	var exitStore *session.Store
	defer func() {
		if id := lastDetachID(); id != "" {
			fmt.Printf("─── detached ──────────────────────────────────────\n")
			fmt.Printf("  turn kept running as background job %s\n", id)
			fmt.Printf("  xdev bg logs %s\n", id)
			fmt.Printf("  xdev bg stop %s\n", id)
			fmt.Printf("  xdev config set tui.exitDetach false   # kill on quit\n")
			fmt.Printf("──────────────────────────────────────────────────\n")
		}
		if exitStore != nil {
			if text := exitMenuText(exitStore, cwd); text != "" {
				fmt.Print(text)
			}
		}
	}()
	defer func() {
		// Cancel every in-flight turn before the stores close under them.
		tabs.abortAll()
		modelMu.Lock()
		lm := live.provName + "/" + live.model
		modelMu.Unlock()
		if s := storeOf(); s != nil {
			_ = s.Append(&session.ModelChangeEntry{Model: lm})
			_ = s.Append(&session.CustomEntry{CustomType: "session_exit", Data: map[string]any{"mode": "tui", "code": exitCode}})
			exitStore = s // the exit line's store, read before closeAll drops it
		}
		tabs.closeAll()
	}()

	// Screen.
	th := theme.LoadNamed(themeName, theme.CustomDir())
	if lastSettings().ColorBlindMode {
		th = theme.ApplyColorBlindMode(th)
	}
	scr, err := tcell.NewScreen()
	if err != nil {
		return 2, fmt.Errorf("tui: screen: %w", err)
	}
	setCursorColor(th.Get(theme.AccentUser)) // OSC 12 (survives into raw mode)
	if err := scr.Init(); err != nil {
		return 2, fmt.Errorf("tui: init: %w", err)
	}
	// Wheel events drive the in-app transcript scroll, not the host
	// terminal's own scrollback (#17 follow-up, user-reported 2026-09-10).
	scr.EnableMouse()
	// Bracketed paste: without it a multi-line paste arrives as keys with CR
	// between the lines, and CR is Enter — one message per line (paste.go).
	// A terminal that ignores the mode keeps the old behaviour; nothing hangs.
	scr.EnablePaste()
	defer scr.Fini()
	defer setCursorReset()

	// A stop, a hangup or a SIGTERM must not leave the shell in a raw-mode
	// alt screen: the two defers above only run when runTUI returns. The
	// guard restores first, then dies on the signal it was sent
	// (tui_signal.go).
	if stopSignals := watchTerminalRoutes(scr.Fini); stopSignals != nil {
		defer stopSignals()
	}

	// A panic on a background goroutine runs no defer, so the two Fini
	// defers above never fire and the shell comes back to a raw-mode alt
	// screen — the panic-path twin of what the signal guard above just fixed.
	// Armed here because this is where tcell starts owning the tty
	// (tui_panic.go).
	prevRestore := terminalRestore
	terminalRestore = scr.Fini
	defer func() { terminalRestore = prevRestore }()

	app := tui.New(scr, th, modelRef, store.ID())
	// --log: write a TUI screen transcript to <path> after each
	// paint frame (off by default). Relative paths resolve under
	// config.DataDir(); the file is opened truncated and closed on exit.
	if launch.LogFile != "" {
		logPath := launch.LogFile
		if !filepath.IsAbs(logPath) {
			logPath = filepath.Join(config.DataDir(), logPath)
		}
		if dir := filepath.Dir(logPath); dir != "" {
			_ = os.MkdirAll(dir, 0o700)
		}
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return 2, fmt.Errorf("tui: log file: %w", err)
		}
		defer f.Close()
		app.SetLogFile(f)
	}
	// A frozen TUI is otherwise undiagnosable after the fact: the UI loop is
	// single-goroutine, so anything that fails to return there kills keys,
	// Ctrl+C and output together while the process stays alive. If one loop
	// iteration stalls, xdev now writes the goroutine stacks where `xdev gc`
	// already collects them.
	app.SetStallDumpDir(filepath.Join(config.DataDir(), "dumps"))
	// The last resort for a UI loop that never comes back: restore the
	// terminal and end the session, because nothing inside the process can
	// (the quit chord is applied by the loop, which is what is stuck). Ninety
	// seconds, not minutes: the stalls this exists for are the process
	// getting no CPU, and the one thing a starved process cannot do is wait
	// patiently — every extra second is a second the user spends killing the
	// pane from outside. Long enough that a loop recovering on its own (a
	// paste the user still holds, a burst of output) is never taken away.
	// Same restore as the signal and panic guards.
	app.SetStallExitAfter(90 * time.Second)
	app.SetStallRestore(scr.Fini)
	// MCP servers (optional; absent config = nothing happens). Attached once
	// the app exists, because a failed server is a startup fact the user has
	// to read — and stderr is not readable under the alt screen (#272). The
	// toast stack carries it on the error grace every other failure gets: it
	// had its own two minutes, which is not a toast but a status line, and a
	// corner that holds its text from launch reads as broken. Nothing is lost
	// by the shorter life — the dock's MCP section keeps the server listed
	// with a ○ for as long as it stays down. The last argument is the dock's:
	// the MCP rows are painted ○ until the connect lands, and nothing else
	// would repaint them the moment it does.
	mgr := attachMCP(context.Background(), reg, false, func(msg string) {
		app.Toast(tui.ToastError, msg, 0) // 0 = the level's own error grace
	}, app.DockBump)
	if mgr != nil {
		defer mgr.Close()
	}
	// showThinking drives the reasoning display (issue #20): the layered
	// config is the source of truth, with --hide-thinking / --print-thoughts
	// overriding it for this run (display only — the model still thinks).
	app.SetShowThinking(showThinkingOn(lastSettings()))
	// Mermaid fences render as diagrams (settings renderMermaid, default on).
	// Same shape as showThinking: display-only, and a diagram the renderer
	// cannot draw falls back to the code band, so turning it off changes how a
	// message looks and never what it says.
	app.SetRenderMermaid(lastSettings().RenderMermaidOn())
	// HUD segments (settings statusLine.segments): unknown names are
	// skipped with a warning, unset keeps the shipped layout.
	app.SetStatusSegments(lastSettings().StatusLineSegments())
	// debugMouse renders every mouse event on the status bar (settings
	// `tui.debugMouse`, off by default). It is opt-in so a normal
	// session does not scroll the HUD with pointer noise.
	app.SetDebugMouse(lastSettings().DebugMouse)
	// The context dock (#291 §1): the fixed-width column right of the transcript.
	// Its sources are the state the session already keeps — the proposed plan and
	// the task list through PlanMode, the hub roster through the same snapshot
	// /hub reads — and the files it lists are read from the transcript's own diff
	// blocks inside the panel, so no second change-tracker exists. Alt+s is the
	// only key it takes, and it persists the display policy the way showThinking
	// does: the global settings layer, the in-memory copy, and nothing else.
	app.SetDockMode(lastSettings().SidebarModeOn())
	app.SetDockModeFunc(func(mode string) {
		if err := config.Set(config.GlobalSettingsPath(), "sidebarMode", mode); err != nil {
			logx.Errorf("sidebarMode: %v", err)
			return
		}
		lastSettings().SidebarMode = mode
	})
	// The build this process is, in the dock's footer: the first question
	// about a session that behaves strangely is which build it was.
	app.SetVersion(version)
	// /settings lists the resolved config; toggles persist to the global
	// layer (the same file `xdev config set` edits) and update the
	// in-memory settings so a later /settings sees them.
	app.SetSettingsOps(&tui.SettingsOps{
		Path: config.GlobalSettingsPath(),
		List: func() []string { return config.List(lastSettings(), config.GlobalSettingsPath()) },
		SetThinking: func(on bool) error {
			if err := config.Set(config.GlobalSettingsPath(), "showThinking", fmt.Sprint(on)); err != nil {
				return err
			}
			v := on
			lastSettings().ShowThinking = &v
			return nil
		},
		SetSidebar: func(mode string) error {
			if err := config.Set(config.GlobalSettingsPath(), "sidebarMode", mode); err != nil {
				return err
			}
			lastSettings().SidebarMode = mode
			return nil
		},
		SetMermaid: func(on bool) error {
			if err := config.Set(config.GlobalSettingsPath(), "renderMermaid", fmt.Sprint(on)); err != nil {
				return err
			}
			v := on
			lastSettings().RenderMermaid = &v
			return nil
		},
		SetExitDetach: func(on bool) error {
			if err := config.Set(config.GlobalSettingsPath(), "tui.exitDetach", fmt.Sprint(on)); err != nil {
				return err
			}
			v := on
			lastSettings().Tui.ExitDetach = &v
			return nil
		},
	})

	// Settings overlay (Alt+,): the settings this session already has a live
	// seam for. The panel owns its key handling and rendering; what is wired
	// here is the two things only cmd can do — read the layered values the
	// session actually resolved, and persist a change to the global layer the
	// way `xdev config set` does. Rows are declared once, with the live apply
	// each one performs, so the three callbacks cannot drift apart: a setting
	// with no seam here has no row, which is why the overlay shows the
	// resolved state rather than a generic editor of the config file.
	app.SetSettingsOverlayOps(&tui.SettingsOverlayOps{
		Path: config.GlobalSettingsPath(),
		Read: func() []tui.SettingsRow {
			s := lastSettings()
			rows := []tui.SettingsRow{
				{Key: "showThinking", Label: "Show thinking", Value: fmt.Sprint(s.ShowThinkingOn()),
					Editable: true, Kind: "toggle"},
				{Key: "renderMermaid", Label: "Render mermaid", Value: fmt.Sprint(s.RenderMermaidOn()),
					Editable: true, Kind: "toggle"},
				{Key: "tui.exitDetach", Label: "Detach on quit", Value: fmt.Sprint(s.TuiExitDetachOn()),
					Editable: true, Kind: "toggle"},
				// thinking is a select, not a toggle: the vocabulary is the
				// level ladder, and it lives behind one door (ThinkingOps.Set)
				// so a level set here and a level set by /thinking cannot
				// disagree about the wire.
				{Key: "thinking", Label: "Thinking level", Value: s.ThinkingLevel(),
					Editable: true, Kind: "select", Options: append([]string(nil), config.ThinkingLevels...)},
				{Key: "sidebarMode", Label: "Sidebar", Value: s.SidebarModeOn(),
					Editable: true, Kind: "select", Options: []string{"auto", "show", "hide"}},
				{Key: "theme", Label: "Theme", Value: s.Theme, Editable: false, Kind: "text"},
				{Key: "approvalMode", Label: "Approval mode", Value: s.ApprovalMode, Editable: false, Kind: "text"},
				{Key: "defaultModel", Label: "Model", Value: s.DefaultModel, Editable: false, Kind: "text"},
				{Key: "memory", Label: "Memory", Value: s.Memory, Editable: false, Kind: "text"},
				{Key: "advisor", Label: "Advisor", Value: fmt.Sprint(s.Advisor), Editable: false, Kind: "text"},
				{Key: "colorBlindMode", Label: "Color-blind mode", Value: fmt.Sprint(s.ColorBlindMode), Editable: false, Kind: "text"},
				{Key: "debugMouse", Label: "Debug mouse", Value: fmt.Sprint(s.DebugMouse),
					Editable: true, Kind: "toggle"},
			}
			return rows
		},
		Write: func(key, value string) error {
			// The one write path: the global layer, exactly what
			// `xdev config set` edits. config.Set validates the key against
			// the schema and round-trips the file before it lands, so a bad
			// value fails here instead of quarantining the user's config on
			// the next start.
			if err := config.Set(config.GlobalSettingsPath(), key, value); err != nil {
				return err
			}
			// Fold the new value into the in-memory layer as well: a later
			// /settings (or a refresh of this panel) reads the session's
			// resolved settings, so without this it would show the old value
			// until the next process.
			switch key {
			case "showThinking":
				v := value == "true"
				lastSettings().ShowThinking = &v
			case "renderMermaid":
				v := value == "true"
				lastSettings().RenderMermaid = &v
			case "tui.exitDetach":
				v := value == "true"
				lastSettings().Tui.ExitDetach = &v
			case "thinking":
				lastSettings().Thinking = value
			case "sidebarMode":
				lastSettings().SidebarMode = value
			case "debugMouse":
				lastSettings().DebugMouse = value == "true"
			}
			return nil
		},
	})
	// /thinking and the Shift-Tab toggle (#20's other half): the request-side
	// level, not the display. Persisting and the live holder both live here,
	// because cmd owns the settings file and the provider holder. A flip takes
	// effect on the next turn — the same latency /model has, and the same
	// reason: the turn reads live.effort once, at agent construction.
	app.SetThinkingOps(&tui.ThinkingOps{
		Current: thinkLevel,
		Set: func(level string) error {
			// Validate before writing: the fold is the only place the
			// vocabulary is enforced, so a typo must not reach the file.
			if _, err := applyThinkingFlag(level, ""); err != nil {
				return err
			}
			if err := config.Set(config.GlobalSettingsPath(), "thinking", level); err != nil {
				return err
			}
			modelMu.Lock()
			// A level pinned while a non-reasoning model is live still lands
			// in live.level (the sticky choice), but live.effort follows what
			// this model can actually take — the same fallback a /model switch
			// makes, so the two paths cannot disagree about the wire.
			applied := thinkingForModel(level, live.provName, live.model, cfg)
			le, err := applyThinkingFlag(applied, live.roleEffort)
			if err != nil {
				modelMu.Unlock()
				return err
			}
			live.effort, live.level = le, level
			modelMu.Unlock()
			lastSettings().Thinking = level
			return nil
		},
	})
	if exts != nil {
		// The UI callback carries no context: the extension's per-event
		// timeout is the bound here.
		app.SetExtensionCommands(exts.Commands(), func(name, args string) (string, error) {
			return exts.RunCommand(context.Background(), name, args)
		})
		// Declarative render specs (card/table/tree) for extension tools;
		// anything that fails to parse degrades to plain text in the view.
		specs := map[string]tui.RenderSpec{}
		for toolName, r := range exts.Renderers() {
			specs[toolName] = tui.RenderSpec{Kind: r.Kind, Spec: r.Spec}
		}
		app.SetRenderers(specs)
	}

	// Replay an opened session's history as read-only blocks (thinking
	// blocks replay too, gated by the showThinking display toggle). The
	// store, not the flag, decides: --continue and --resume both land a
	// populated store here, a fresh session has none.
	if len(storeOf().Entries()) > 0 {
		if res, err := session.BuildContext(storeOf().Entries(), storeOf().LeafID(), session.SystemPrompt{}); err == nil {
			replaySession(app, res.Messages)
		}
	}
	// Live conversation is the store: user/assistant/toolResult messages
	// are persisted by the hooks and Agent.persist, so each submit rebuilds
	// history from the store (compaction entries replay correctly through
	// BuildContext). Seeding from a separate slice went stale and dropped
	// assistant turns (conversation amnesia).
	rebuildHistory := func() []ai.Message {
		s := storeOf()
		if s == nil {
			return nil
		}
		res, err := session.BuildContext(s.Entries(), s.LeafID(), session.SystemPrompt{})
		if err != nil {
			return nil
		}
		return res.Messages
	}

	// --max-time bounds the session: every turn shares baseCtx, so the
	// deadline releases the whole tree. Esc must NOT: baseCancel kills every
	// future turn too (see liveTurn).
	baseCtx, baseCancel := withMaxTime(context.Background(), launch.MaxTime)
	defer baseCancel()

	// Serializes conversation accumulation across turns of ONE session
	// (the store's own lock covers appends; this is for rebuildHistory
	// and the rare multi-step mutation that spans two store reads).
	var sessMu sync.Mutex
	// running/turn used to be process-wide. They live on the tabset now:
	// each session claims its own turn slot, and Esc aborts only the
	// session on screen. See tabset.go.

	// Handoff (M5 #23): /handoff and -handoff replace the live context with
	// a handoff document committed as a normal compaction entry, so the
	// next turn continues from the document. The side request mirrors a
	// live turn's transform (system + history + a trailing instruction, no
	// tools) on the session model; the reset closure rewinds the advisor feed
	// cursor — the todo list and plan mode are the agent's own seams — and
	// settings handoff.saveToDisk mirrors the document to disk.
	handoffSettings := func() agent.HandoffSettings {
		hs := agent.HandoffSettings{SaveDir: handoffSaveDir(lastSettings())}
		if t := resolveInto("", cfg, lastSettings(), "handoff"); t != nil {
			hs.Target = *t
		}
		if adv != nil {
			hs.Reset = func(kept []ai.Message) { adv.Reset(kept) }
		}
		return hs
	}
	runHandoff := func(instruction string) (string, error) {
		s := storeOf()
		if s == nil {
			return "", fmt.Errorf("no session")
		}
		if tabs.isRunning(s.ID()) {
			return "", fmt.Errorf("a turn is running — Esc cancels it first")
		}
		modelMu.Lock()
		lp, lm, lpn := live.prov, live.model, live.provName
		modelMu.Unlock()
		ag := &agent.Agent{
			Provider:   lp,
			Tools:      reg,
			Model:      lm,
			Store:      s,
			Compaction: agent.CompactionConfig{ContextWindow: modelWindow(cfg, lpn, lm), Methods: agent.HandoffOrder(lastSettings().CompactionMethodOrder())},
			PlanMode:   planMode,
			Handoff:    handoffSettings(),
		}
		wireAgentMode(ag, reg, cfg, lastSettings(), lpn, lm, cwd, true)
		return ag.HandoffDoc(baseCtx, buildSys(), instruction)
	}
	// #272: the alt screen swallows stderr, which is where discovery
	// warnings used to go — so an empty or half-broken agent set looked
	// exactly like a working one. Say what loaded, before the first turn.
	notice, _, _ := taskAgentsAtStartup(cwd)
	if n := dist.Notice(version); n != "" {
		// A newer release is reported where the user reads, not on stderr
		// under the alt screen: the welcome screen's notice slot, on its
		// own line. The agents line keeps that slot when it is the only
		// one; the update line joins it rather than replacing it, because
		// both are startup facts.
		notice = strings.TrimSuffix(notice, "\n") + "\n" + n
	}
	if notice != "" {
		app.SetStartupNotice(notice)
	}

	// Check for a newer release in the background, at most twice a day. The
	// record the previous run wrote is what this session just read, so the
	// check pays for the next launch, not this one; dist.MaybeCheck is a
	// no-op while a scheduled job owns the checking.
	go dist.MaybeCheck(version)

	// -handoff: document the resumed session before the first turn.
	if handoffMode && len(storeOf().Entries()) > 0 {
		if doc, err := runHandoff(""); err != nil {
			logx.Errorf("handoff: %v", err)
		} else {
			app.AddSystemBlock(doc)
		}
	}

	ts := &tuiSession{store: store, app: app, tabs: tabs, id: store.ID(), focused: true}

	var swapStoreTo func(*session.Store) error

	// focusTab rebuilds the App view around a tab that is already open.
	// The previous session is PARKED, not closed: its turn keeps running
	// and its store stays open. The App still owns one transcript, so the
	// rebuild is a Reset + replay — the same path /resume already took.
	focusTab := func(t *tab) {
		if t == nil {
			return
		}
		ts.setFocused(false) // freeze any in-flight paint from the old session
		ts.store = t.store
		ts.id = t.id
		ts.setFocused(true)
		wireTaskParent(reg, t.store)
		if vibeScope != nil {
			workers, on := agent.LoadVibe(t.store.Entries())
			vibeScope.Restore(workers, on)
		}
		app.Reset()
		app.SetLocation(t.store.CWD())
		app.SetSessionID(t.store.ID())
		saveBreadcrumb(breadcrumbPath(t.store))
		if res, err := session.BuildContext(t.store.Entries(), t.store.LeafID(), session.SystemPrompt{}); err == nil {
			replaySession(app, res.Messages)
		}
		// The HUD spinner follows the FOREGROUND session only.
		app.SetRunning(t.running)
		app.SetTabs(tabInfos(tabs))
	}

	// swapStore opens a fresh session and parks (or drops) the previous
	// one. drop=true deletes the previous file — /drop — and aborts its
	// turn; otherwise the previous session stays open in the tabset so a
	// Ctrl+] can cycle back to it while its turn finishes.
	swapStore := func(drop bool) error {
		sessMu.Lock()
		defer sessMu.Unlock()
		if vibeActive() {
			return fmt.Errorf("vibe mode is active — /vibe off first")
		}
		old := storeOf()
		ns, err := openSession(cwd, false, "")
		if err != nil {
			return err
		}
		bus := buildHookBus(cwd, opts, app.AddSystemBlock)
		emitSwitchEvents(bus, true, shortSessionID(ns.ID()), ns.Title())
		if drop && old != nil {
			path := old.Path()
			_ = tabs.close(old.ID()) // aborts turn + Close
			if path != "" {
				if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
					logx.Errorf("drop session file: %v", rmErr)
				}
			}
		}
		i, err := tabs.open(ns)
		if err != nil {
			_ = ns.Close()
			return err
		}
		t := tabs.activate(i)
		focusTab(t)
		app.AddSystemBlock("· new session " + shortSessionID(ns.ID()))
		emitSwitchEvents(bus, false, shortSessionID(ns.ID()), ns.Title())
		return nil
	}

	// swapStoreTo adopts an already-open store (fork/resume): parks the
	// previous session and makes this one current. The previous turn keeps
	// running; only the view moves.
	swapStoreTo = func(ns *session.Store) error {
		sessMu.Lock()
		defer sessMu.Unlock()
		if vibeActive() {
			return fmt.Errorf("vibe mode is active — /vibe off first")
		}
		// A session interrupted mid-write is repaired on open (#122); in the
		// TUI the transcript is the only surface the user can actually read, so
		// the notice goes there rather than to stderr under the alt screen.
		if n := sessionRepairNotice(ns); n != "" {
			app.AddSystemBlock(n)
		}
		bus := buildHookBus(cwd, opts, app.AddSystemBlock) // resolved per switch: /settings edits land
		emitSwitchEvents(bus, true, shortSessionID(ns.ID()), ns.Title())
		i, err := tabs.open(ns)
		if err != nil {
			return err
		}
		t := tabs.activate(i)
		focusTab(t)
		app.AddSystemBlock("· session " + shortSessionID(ns.ID()) + " — " + ns.Title())
		emitSwitchEvents(bus, false, shortSessionID(ns.ID()), ns.Title())
		return nil
	}

	// cycleTab is the Alt+] / Alt+[ chord: park the current session and
	// focus the next (or previous) one. A single open session is a notice,
	// never a silent no-op.
	cycleTab := func(dir int, onlyUnread bool) {
		t := tabs.cycle(dir, onlyUnread)
		if t == nil {
			open, _, _ := tabs.summary()
			if open < 2 {
				app.AddSystemBlock("· one session open — /new or /resume to open another")
			} else {
				app.AddSystemBlock("· no unread sessions")
			}
			return
		}
		sessMu.Lock()
		focusTab(t)
		sessMu.Unlock()
		label := shortSessionID(t.id)
		if t.title != "" {
			label = t.title
		}
		app.AddSystemBlock("· session " + label)
	}

	// focusTabByID is the /tabs row path: Enter names the session to switch
	// to, so the cycle's dir/onlyUnread arguments have no meaning here.
	focusTabByID := func(id string) error {
		t := tabs.focus(id)
		if t == nil {
			return fmt.Errorf("that session is no longer open")
		}
		sessMu.Lock()
		focusTab(t)
		sessMu.Unlock()
		label := t.title
		if label == "" {
			label = shortSessionID(t.id)
		}
		app.AddSystemBlock("· session " + label)
		return nil
	}

	// closeTabByID is what the session.delete chord and the tab strip's ×
	// both call. Closing the CURRENT tab focuses the neighbour tabs.close
	// already picked; closing a parked one leaves the view where it is, so
	// nothing has to be rebuilt. The session FILE survives — closing a tab is
	// closing a tab, and /drop is still how one is deleted.
	closeTabByID := func(id string) error {
		sessMu.Lock()
		defer sessMu.Unlock()
		cur := tabs.current()
		if cur == nil || cur.id != id {
			if tabs.close(id) == nil && len(tabs.snapshot()) == 0 {
				return fmt.Errorf("that session is no longer open")
			}
			app.SetTabs(tabInfos(tabs))
			app.AddSystemBlock("· closed session " + shortSessionID(id))
			return nil
		}
		next := tabs.close(id) // aborts a live turn, closes the store
		if next == nil {
			return nil // the last tab went: the App turns that into a quit
		}
		focusTab(next)
		label := next.title
		if label == "" {
			label = shortSessionID(next.id)
		}
		app.AddSystemBlock("· closed a session · now " + label)
		return nil
	}

	// Session lifecycle (issue #11): /new swaps in a fresh session file,
	// /clear resets in place (durable reset_boundary, history kept on
	// disk), /drop deletes the file and starts fresh.
	//
	// @-file completion (M7 #8, PRD §IV.6) has two sources, and they answer
	// different tokens. The typed token names ONE directory plus a prefix, so
	// a keystroke costs one readdir of that directory — which is what makes
	// hidden and gitignored paths affordable to offer: walking the repo to
	// filter them out was the whole cost, and there is no walk here. A bare
	// `@` names no directory, so it lists this same root (the cwd).
	//
	// A bare PREFIX (`@pa`) is the token one readdir cannot answer, and it is
	// the common one: you know the name, not where it lives. So a background
	// index of every file under the cwd answers it from memory — the walk
	// #329 took off the keystroke path (5.7s per keystroke on a 226k-file
	// polyrepo) run ONCE, off-thread, while the welcome screen is still up.
	// The dropdown is usable before the walk lands (it shows the readdir) and
	// fills in when it does; no keystroke ever waits on the filesystem.
	listDir := func(dir string) []tui.PathEntry {
		full := filepath.Join(cwd, filepath.FromSlash(dir))
		ents, err := os.ReadDir(full)
		if err != nil {
			return nil // an unreadable or absent directory offers nothing
		}
		out := make([]tui.PathEntry, 0, len(ents))
		for _, e := range ents {
			isDir := e.IsDir()
			// A symlink to a directory must complete AS one, or the menu offers
			// a file that walking into cannot open.
			if e.Type()&fs.ModeSymlink != 0 {
				if info, serr := os.Stat(filepath.Join(full, e.Name())); serr == nil {
					isDir = info.IsDir()
				}
			}
			out = append(out, tui.PathEntry{Name: e.Name(), IsDir: isDir})
		}
		return out // os.ReadDir sorts lexically, which the menu shows as-is
	}
	app.SetPathCompletion(cwd, listDir)
	app.StartPathIndex(cwd)
	app.SetPickerResume(func(id string) {
		if id == "" {
			return
		}
		path, err := resolveResumeID(cwd, id)
		if err != nil {
			app.AddSystemBlock("resume: " + err.Error())
			return
		}
		resumed, err := session.Open(path)
		if err != nil {
			app.AddSystemBlock("resume: " + err.Error())
			return
		}
		if err := swapStoreTo(resumed); err != nil {
			app.AddSystemBlock("resume: " + err.Error())
		}
	})
	// Picker extras (issue #26): prompt-text search, pin sidecar, delete.
	app.SetPickerSearch(func(query string) []tui.SessionPickerItem {
		return searchPickerItems(cwd, query)
	})
	app.SetPickerPinToggle(func(id string) {
		if err := toggleSessionPin(id); err != nil {
			app.AddSystemBlock("pin: " + err.Error())
		}
	})
	app.SetPickerDelete(func(id string) error {
		return deleteSessionByShortID(id, storeOf().Path())
	})
	app.SetResumeList(func(cwd string) error {
		// The rows here are the session picker's; the closure used to also
		// build a []tui.ResumeOption that nothing read, at the price of a
		// second full session.List on every call.
		items := resumePickerItems(cwd)
		if len(items) == 0 {
			app.AddSystemBlock("no other sessions in this directory")
			return nil
		}
		app.OpenSessionPicker(items)
		return nil
	})
	// branchReplay rebuilds the transcript from the live leaf — the
	// TUI-side twin of the re-render omp performs after every tree
	// navigation. Silent: callers own their status notice.
	branchReplay := func() {
		res, err := session.BuildContext(storeOf().Entries(), storeOf().LeafID(), session.SystemPrompt{})
		if err != nil {
			return
		}
		app.Reset()
		replaySession(app, res.Messages)
	}
	// navigateTree is the port of omp's session.navigateTree (the tree
	// selector's Enter / Shift+Enter / Alt+S): the leaf lands on the
	// selected entry, EXCEPT for user messages — those rewind to their
	// PARENT (before the very first message: a fresh reset-boundary root)
	// and the prompt comes back as the composer draft, so edit-and-resend
	// never duplicates the entry. summarize first condenses the abandoned
	// branch into a branch_summary entry hung off the target, where the
	// new branch's model actually reads it. The transcript is restored
	// from the new leaf either way.
	navigateTree := func(entryID string, summarize bool) (string, error) {
		e := storeOf().Entry(entryID)
		if e == nil {
			return "", fmt.Errorf("no entry %q in this session", entryID)
		}
		target, draft := treeRewindTarget(e)
		if summarize {
			if err := summarizeAndBranch(store, target); err != nil {
				return "", err
			}
		} else if target == "" {
			if err := storeOf().ResetLeaf(); err != nil {
				return "", err
			}
		} else if err := storeOf().Branch(target); err != nil {
			return "", err
		}
		if st := agent.ScheduleStateOf(reg); st != nil {
			st.RefoldActive()
		}
		branchReplay()
		return draft, nil
	}
	// userEntryID maps the i-th ❯ row of the live transcript back to the store
	// entry that holds it. ContextResult.EntryIDs runs parallel to Messages,
	// and harnessUserAttribution is the same filter replayTranscript counted
	// the rows with, so the ordinal the TUI sends is the ordinal this answers
	// for. A row with no entry (a compaction summary, a turn the store refused)
	// reads as "", and the menu says so rather than rewinding somewhere random.
	userEntryID := func(i int) string {
		res, err := session.BuildContext(storeOf().Entries(), storeOf().LeafID(), session.SystemPrompt{})
		if err != nil {
			return ""
		}
		n := 0
		for j, m := range res.Messages {
			if m.Role != ai.RoleUser || harnessUserAttribution(m) || m.Text() == "" {
				continue
			}
			if n == i {
				if j < len(res.EntryIDs) {
					return res.EntryIDs[j]
				}
				return ""
			}
			n++
		}
		return ""
	}
	// branchToEntry moves the live leaf to an entry and replays the new
	// branch's transcript into the TUI (/branch <id-prefix>).
	branchToEntry := func(entryID string) error {
		if err := storeOf().Branch(entryID); err != nil {
			return fmt.Errorf("branch: %v", err)
		}
		branchReplay()
		app.AddSystemBlock("· branched to " + entryID[:min(8, len(entryID))] + " — replayed")
		if st := agent.ScheduleStateOf(reg); st != nil {
			st.RefoldActive()
		}
		return nil
	}
	app.SetSessionBranch(func(args string) error {
		sid, ok := tabs.claimCurrent()
		if !ok {
			return fmt.Errorf("a turn is running — Esc cancels it first")
		}
		defer tabs.release(sid)
		query := strings.TrimSpace(args)
		if query == "" {
			return fmt.Errorf("branch: entry-id prefix required (ids are listed by /tree)")
		}
		// First prefix match wins (omp addresses entries by full id; the
		// selector hands Enter the full id — the prefix form is a typed
		// convenience).
		for _, e := range storeOf().Entries() {
			if env := e.Envelope(); strings.HasPrefix(env.ID, query) {
				return branchToEntry(env.ID)
			}
		}
		return fmt.Errorf("branch: no entry matching %q (entries before the last /clear or compaction are not addressable)", query)
	})
	// /tree selector: entry rows built from the live store, labels from
	// the dataDir sidecar (UI state — the session package stays label-free).
	app.SetTreeData(func() []tui.TreeEntry { return treeEntries(storeOf()) })
	app.SetTreeLabels(loadSessionLabels, saveSessionLabel)
	app.SetLocation(cwd)
	// Agent Hub roster (/hub, issue #37): the registry owns this session's
	// hub (registered alongside the hub tool); read it back out.
	var sessionHub *agent.Hub
	if ht, ok := reg.Get(agent.HubToolName); ok {
		if hb, ok := ht.(*agent.HubTool); ok {
			sessionHub = hb.Hub
		}
	}
	if sessionHub != nil {
		// The dock's roster must move when a child settles after the turn that
		// started it is long over — the only other thing that would repaint it is
		// polling the panel at frame rate, which is the exact cost #283/#284 closed.
		sessionHub.SetNotify(app.DockBump)
		app.SetHubOps(&tui.HubOps{
			Roster: func() []tui.HubAgent {
				rows := sessionHub.Roster()
				out := make([]tui.HubAgent, len(rows))
				for i, r := range rows {
					out[i] = tui.HubAgent{
						ID: r.ID, Name: r.Name, Status: r.Status, Model: r.Model,
						Activity: r.Activity, Cost: hubCostLabel(r),
					}
				}
				return out
			},
			Transcript: func(id string, fromSeq int) ([]tui.HubTranscriptLine, int, bool) {
				entries, total, ok := sessionHub.Transcript(id, fromSeq)
				if !ok {
					return nil, 0, false
				}
				lines := make([]tui.HubTranscriptLine, len(entries))
				for i, e := range entries {
					lines[i] = tui.HubTranscriptLine{Role: e.Role, Text: e.Text}
				}
				return lines, total, true
			},
			Park:   sessionHub.Park,
			Kill:   sessionHub.Cancel,
			Revive: func(id string) bool { return sessionHub.Revive(id, "") == nil },
		})
	}
	// The dock's sources (#291 §1). Each is one block whose first line is its
	// heading, and each reads state this session already keeps: the plan and its
	// task list through PlanMode, the roster through the hub's own snapshot.
	// Nothing here caches a second copy of anything — a source that is nil is a
	// section that renders nothing, so a session without a hub loses the Agents
	// section and keeps the rest.
	var todoTool *tool.TodoTool
	if tt, ok := reg.Get("todo"); ok {
		todoTool, _ = tt.(*tool.TodoTool)
	}
	if todoTool != nil { // a typed nil would satisfy the interface and panic on read
		planMode.SetTodo(todoTool) // the plan view's phase list, same snapshot
	}
	ops := tui.DockOps{
		Plan:  planMode.View,
		Tasks: planMode.Todo,
		// The panel's title slot. It reads through the `store` variable rather
		// than capturing one store: /new, /resume and /fork all swap it, and a
		// panel pinned to the session the process started with would show the
		// old title — and the old id — for the rest of the run.
		Session: func() (string, string) { return storeOf().Title(), shortSessionID(storeOf().ID()) },
		// MCP: attachMCP assigned reg.MCPBlock from the mcp.yml it loaded
		// before this closure existed, so the section is the ENABLED server
		// list — one row per name, ○ on the ones with no live session. The nil
		// check stays: a registry with no MCP config at all (MCP off) never got
		// the field, and the panel is built on the first paint.
		MCP: func() string {
			if reg.MCPBlock == nil {
				return ""
			}
			return reg.MCPBlock()
		},
		// The panel's one button row: the heading is the ledger's own count,
		// read through `store` so /new, /resume and /fork move it with the
		// session. The click opens the same ledger /trajectory opens.
		Trajectory: func() string { return trajectoryHeading(storeOf()) },
	}
	// The ledger's records come from the same store, read on open rather than
	// held: a snapshot kept live would walk the session on every rebuild.
	app.SetTrajectoryOps(&tui.TrajectoryOps{
		Records: func() []tui.TrajectoryRecord { return trajectoryRows(storeOf()) },
	})
	if sessionHub != nil {
		ops.Agents = func() string { return dockAgentsLabel(sessionHub.Roster()) }
	}
	app.SetDockOps(ops)
	// The proposal moving is the one event the panel must show without waiting to
	// be asked: a plan published from the agent goroutine repaints, and a
	// consumed one closes its section. Every other section rides the tool and
	// message hooks below.
	planMode.SetInvalidate(app.DockBump)
	// Vibe mode (M14 #58): the director scope over the session hub and the
	// task tool's subagent machinery. Workers inherit the task tool's tool
	// surface and approval posture; the tier selects the bundled agent
	// prompt, and every worker runs on the session's live model.
	if sessionHub != nil {
		var taskTool *agent.TaskTool
		if tt, ok := reg.Get(agent.TaskToolName); ok {
			taskTool, _ = tt.(*agent.TaskTool)
		}
		if taskTool != nil {
			vibeScope = agent.NewVibeScope(agent.VibeConfig{
				Hub:   sessionHub,
				Task:  taskTool,
				Tools: reg,
				Resolve: func() (*agent.VibeModel, error) {
					// A worker runs on the session's live model (the same
					// routing the task tool gives its children).
					modelMu.Lock()
					lp, lm, le := live.prov, live.model, live.effort
					modelMu.Unlock()
					return &agent.VibeModel{Provider: lp, Model: lm, Thinking: effortBudget(le)}, nil
				},
				Persist: func(customType string, data map[string]any) {
					if err := storeOf().Append(&session.CustomEntry{CustomType: customType, Data: data}); err != nil {
						logx.Errorf("vibe: persist %s: %v", customType, err)
					}
				},
				ParentID: func() string { return storeOf().ID() },
				Conflicts: func() []string {
					var out []string
					if planMode.Active() {
						out = append(out, "plan")
					}
					if gs := agent.GoalStateOf(reg); gs != nil {
						if gv, ok := gs.View(); ok && gv.Status == agent.GoalActive {
							out = append(out, "goal")
						}
					}
					return out
				},
				OnSettle: func(w agent.VibeWorker) {
					app.AddSystemBlock("· vibe " + w.ID + " (" + w.Tier + ") " + w.Status + ": " + agent.VibePreview(w.Output))
				},
			})
			vibeSys = promptFnWithMemory(basePrompt(opts, cwd), cwd, vibeScope.Registry(),
				tailSystemPrompt(overrides, opts.AppendSystem)+"\n\n"+agent.VibeDirectorPrompt, sessionMemory)
			// The startup session may itself be a resume: adopt its mode.
			workers, on := agent.LoadVibe(storeOf().Entries())
			vibeScope.Restore(workers, on)
		}
	}
	app.SetVibeOps(&tui.VibeOps{
		Active: vibeActive,
		Set: func(on bool) error {
			if vibeScope == nil {
				return fmt.Errorf("vibe mode not wired (this build has no agent hub/task tool)")
			}
			if on {
				return vibeScope.Enter()
			}
			vibeScope.Exit()
			return nil
		},
		Status: func() string {
			if vibeScope == nil {
				return "vibe: not wired"
			}
			return vibeScope.Status()
		},
	})
	// The active model ref, as the status line and /model see it.
	modelNow := func() string {
		modelMu.Lock()
		defer modelMu.Unlock()
		return live.provName + "/" + live.model
	}
	// turn is in flight.
	app.SetSessionOps(&tui.SessionOps{
		Fork: func() error {
			// Fork parks the source and opens the child as current.
			// A fresh session lives memory-only until its first
			// assistant message — materialize it so the fork has a
			// source file to copy.
			if storeOf().Path() == "" {
				if _, err := storeOf().EnsureOnDisk(
					session.SessionFilePath(sessionDataDir(), cwd, time.Now(), storeOf().ID()), session.Options{}); err != nil {
					return err
				}
			}
			fork, err := session.ForkSession(storeOf().Path(),
				session.SessionFilePath(sessionDataDir(), cwd, time.Now(), session.NewSessionID()), "")
			if err != nil {
				return err
			}
			return swapStoreTo(fork)
		},
		Dump: func() (string, error) {
			return dumpSession(storeOf())
		},
		// /export: the system prompt and the active model are read from
		// the live session here rather than stored, so a mid-session
		// switch or context change is reflected in the export.
		Export: func(path string) (string, error) {
			return exportSession(store, buildSys(), modelNow(), path)
		},
		// /share: seal the transcript and serve it on loopback; the link
		// (key in the fragment) is view-only and lives as long as this
		// process does.
		Share: func() (string, error) {
			return shareLive(storeOf(), buildSys(), modelNow())
		},
		Resume: func(query string) error {
			// /resume parks the current session and focuses another. A turn
			// in the parked session keeps running.
			if query == "" {
				// Interactive picker (omp/Claude Code /resume): rows
				// span all projects (Tab toggles scope; the picker
				// defaults to this folder and shows the Tab hint when
				// the folder has no sessions), newest first;
				// Up/Down + Enter resumes, Esc closes. No candidates
				// at all → text listing.
				items := resumePickerItems(cwd)
				if len(items) == 0 {
					return app.ListSessions(cwd)
				}
				app.OpenSessionPicker(items)
				return nil
			}
			// Foreign-session import (issue #28): /resume @claude|@codex
			// lists foreign transcripts; with an id/path prefix it imports
			// one (read-only source) and switches to the new session.
			if kind, ref, ok := splitForeignQuery(query); ok {
				if ref == "" {
					list, err := listForeignTranscripts(kind, cwd)
					if err != nil {
						return err
					}
					if len(list) == 0 {
						app.AddSystemBlock("no " + kind + " transcripts found for " + cwd)
						return nil
					}
					lines := make([]string, 0, len(list))
					for i, ft := range list {
						if i == 12 {
							break
						}
						lines = append(lines, fmt.Sprintf("%-8s  %s  (last %s)",
							shortSessionID(ft.ID), ft.Path, ft.ModTime.Format("Jan 02 15:04")))
					}
					app.AddSystemBlock(kind + " transcripts (import with /resume @" + kind + " <id-prefix>):\n" +
						strings.Join(lines, "\n"))
					return nil
				}
				imported, err := importForeignSession(kind, ref, cwd)
				if err != nil {
					return err
				}
				return swapStoreTo(imported)
			}
			path, err := resolveResumeID(cwd, query)
			if err != nil {
				return err
			}
			resumed, err := session.Open(path)
			if err != nil {
				return err
			}
			return swapStoreTo(resumed)
		},
		NavigateTree: func(entryID string, summarize bool) (string, error) {
			sid, ok := tabs.claimCurrent()
			if !ok {
				return "", fmt.Errorf("a turn is running — Esc cancels it first")
			}
			defer tabs.release(sid)
			return navigateTree(entryID, summarize)
		},
		UserEntryID: userEntryID,
		New: func() error {
			// /new parks the current session (turn keeps running) and opens a
			// fresh one — that is the whole point of the tab set.
			return swapStore(false)
		},
		Fresh: func() error {
			if tabs.currentRunning() {
				return fmt.Errorf("a turn is running — Esc cancels it first")
			}
			// /fresh (issue #11 §5): rotate PROVIDER-facing state only —
			// the session store, transcript file, title, and session id
			// are all kept (that is the difference from /new, which
			// mints a new identity). xdev providers are stateless across
			// Stream calls (each submit rebuilds history from the store),
			// so there is no provider session id / prompt-cache key to
			// drop today.
			// ponytail: when provider-session or prompt-cache state lands
			// (openSession / swapStoreTo), clear it here.
			return swapStoreTo(storeOf())
		},
		Clear: func() error {
			sid, ok := tabs.claimCurrent()
			if !ok {
				return fmt.Errorf("a turn is running — Esc cancels it first")
			}
			defer tabs.release(sid)
			if err := storeOf().ResetLeaf(); err != nil {
				return err
			}
			if st := agent.ScheduleStateOf(reg); st != nil {
				st.RefoldActive()
			}
			app.Reset()
			// The transcript must not go fully blank: draw() renders the
			// welcome screen whenever there are no blocks, and a command
			// that answers with nothing reads as if it were swallowed.
			app.AddSystemBlock("· context cleared — history kept on disk")
			return nil
		},
		Recent: func() []tui.ResumeOption { return recentResumeOptions(cwd, storeOf().ID()) },
		Drop: func() error {
			// /drop aborts + deletes the current session; a parked neighbour
			// becomes current. The tabset.close path cancels the turn.
			return swapStore(true)
		},
		// /handoff (M5 #23): replace the live context with a handoff
		// document committed as a compaction entry.
		Handoff: runHandoff,
		// /rename: a manual title, written into the fixed-width slot so
		// /resume and the breadcrumb show it (#107).
		Rename: func(title string) error { return storeOf().Rename(title, session.TitleSourceManual) },
	})

	// setRef applies a resolved model ref; shared by /model
	// and the Ctrl+P cycle so both paths keep the same
	// effect (entry, status, context window).
	setRef := func(ref string) error {
		nr, ne, err := resolveModel(ref, cfg, lastSettings())
		if err != nil {
			return err
		}
		nprovName, nmodelName, err := config.ParseModelRef(nr)
		if err != nil {
			return err
		}
		npc, ok := cfg.Providers[nprovName]
		if !ok {
			return fmt.Errorf("unknown provider %q", nprovName)
		}
		nprov, err := buildProvider(nprovName, npc, nmodelName, cfg)
		if err != nil {
			return err
		}
		modelMu.Lock()
		live.prov, live.model, live.provName = nprov, nmodelName, nprovName
		modelMu.Unlock()
		if err := storeOf().Append(&session.ModelChangeEntry{Model: nprovName + "/" + nmodelName}); err != nil {
			logx.Errorf("model change entry: %v", err)
		}
		// A /model (or role) switch changes the ":effort" the role pins, so
		// re-fold the level the user pinned on top of it: with "auto" the new
		// role's effort takes effect, with "off" reasoning stays off. The
		// pinned level stays sticky — live.level keeps what the user chose —
		// but a model the catalog marks as non-reasoning runs at "auto", and
		// the switch says so once instead of silently spending (or not
		// spending) a budget the user never sees change.
		roleNe := ne
		pinned := thinkLevel()
		applied := thinkingForModel(pinned, nprovName, nmodelName, cfg)
		if applied != pinned {
			app.AddSystemBlock("· " + nprovName + "/" + nmodelName + " cannot reason — thinking " + pinned + " → auto (still " + pinned + " on a model that can)")
		}
		if ne, err = applyThinkingFlag(applied, roleNe); err != nil {
			return err
		}
		app.SetStatusModel(nprovName + "/" + nmodelName)
		// The HUD context segment measures against the new window.
		app.SetContextWindow(int64(modelWindow(cfg, nprovName, nmodelName)))
		persistDefaultModel(nprovName + "/" + nmodelName)
		return nil
	}

	// /model: the interactive selector. Roles tab first (each row sets that
	// slot), then the concrete model catalog — all models plus one view per
	// provider, which is what omp's /model shows.
	app.SetModelOps(&tui.ModelOps{
		Current: func() string {
			modelMu.Lock()
			defer modelMu.Unlock()
			return live.provName + "/" + live.model
		},
		Views: func() []tui.PickerView {
			modelMu.Lock()
			cur := live.provName + "/" + live.model
			modelMu.Unlock()
			return modelPickerViews(cfg, lastSettings(), cur, app)
		},
		Models: func() []tui.PickerItem {
			modelMu.Lock()
			cur := live.provName + "/" + live.model
			modelMu.Unlock()
			return modelPickerItems(cfg, lastSettings(), cur)
		},
		Set: setRef,
		// Cycle advances the active model through the --models
		// patterns (omp's Ctrl+P): each pattern matches the first
		// catalog entry whose provider/model/label contains it.
		Cycle: func() (string, bool) {
			if len(lastSettings().Models.Cycle) == 0 {
				return "", false
			}
			cur := ""
			modelMu.Lock()
			if live.provName != "" && live.model != "" {
				cur = live.provName + "/" + live.model
			}
			modelMu.Unlock()
			items := modelPickerItems(cfg, lastSettings(), cur)
			if len(items) == 0 {
				return "", false
			}
			next := ""
			for _, pat := range lastSettings().Models.Cycle {
				for _, it := range items {
					if strings.Contains(strings.ToLower(it.Value), strings.ToLower(pat)) ||
						strings.Contains(strings.ToLower(it.Label), strings.ToLower(pat)) {
						if it.Value == cur {
							continue
						}
						next = it.Value
						break
					}
				}
				if next != "" {
					break
				}
			}
			if next == "" {
				return "", false
			}
			if err := setRef(next); err != nil {
				return "", false
			}
			return next, true
		},
	})
	// In the TUI, propose HOLDS the decision: the plan shows in the tool
	// block, the model is told to wait, and the user resolves with /plan
	// off (approve) or feedback (revise).
	planMode.SetPropose(agent.NewProposeTool(planMode, func(context.Context, string) (bool, string) {
		return false, "awaiting user review — the user will /plan off to approve or send revision feedback"
	}))
	// ask (#36): the option card is the answer path, so its wait is the
	// policy's wait: with ask.autoAnswer off there is no timeout, the card
	// waits for the human until they answer, skip, or the turn ends. Both
	// halves read the policy PER CALL rather than capturing it, because
	// /auto-answer flips it mid-session and the next card must already obey
	// the new answer.
	if at, ok := reg.Get(tool.AskToolName); ok {
		if at2, isAsk := at.(*tool.AskTool); isAsk {
			askAuto := func() bool { return lastSettings().AskAutoAnswerOn() }
			at2.Sink = &askCardSink{
				ops: app.NewAskOps(func() time.Duration {
					if !askAuto() {
						return 0 // no timer: only a pick or a canceled turn ends the wait
					}
					return lastSettings().AskTimeout()
				}),
				auto:     askAuto,
				fallback: func() tool.AskSink { return tool.NewHeadlessAskSink(lastSettings().AskTimeout(), askAuto()) },
			}
		}
	}
	// A foreground `task` spawn is invisible by default: the call blocks on
	// the child, and the child speaks only to the model. These events are
	// the human's half — the transcript shows what each child is doing —
	// and they never reach the parent's context.
	if tt, ok := reg.Get(agent.TaskToolName); ok {
		if taskTool, isTask := tt.(*agent.TaskTool); isTask {
			taskTool.OnEvent = (&taskChildSink{app: app}).onEvent
		}
	}
	// /auto-answer: the ask card's answer policy, flipped live and persisted
	// to the same global layer `xdev config set ask.autoAnswer` edits. The
	// in-memory fold is what the sink above reads, so the very next question
	// obeys the flip — one write covers the file and this session.
	app.SetAutoAnswerOps(&tui.AutoAnswerOps{
		Path:    config.GlobalSettingsPath(),
		Current: func() bool { return lastSettings().AskAutoAnswerOn() },
		Set: func(on bool) error {
			if err := config.Set(config.GlobalSettingsPath(), "ask.autoAnswer", fmt.Sprint(on)); err != nil {
				return err
			}
			lastSettings().Ask.AutoAnswer = on
			if at, ok := reg.Get(tool.AskToolName); ok {
				if askTool, isAsk := at.(*tool.AskTool); isAsk {
					// The tool reads AutoAnswer for the result text it hands the
					// model ("no answer within …"), so it must move with the
					// policy or the model is told a wait that is not happening.
					askTool.AutoAnswer = on
				}
			}
			return nil
		},
	})
	app.SetMemoryOps(memoryOps(sessionMemory))
	app.SetAdvisorOps(&tui.AdvisorOps{
		Enabled: func() bool { return adv != nil },
		Set: func(on bool) error {
			if on && adv == nil {
				return fmt.Errorf("advisor unavailable: set advisorModel and advisor: true in settings")
			}
			advisorOn.Store(on)
			return nil
		},
		Status: func() string {
			if adv == nil {
				return "off (no advisorModel configured)"
			}
			if adv.Halted() {
				return "halted after repeated failures"
			}
			if advisorOn.Load() {
				return "on"
			}
			return "off"
		},
		Dump: func() string {
			if adv == nil {
				return "advisor: none"
			}
			notes := adv.Dump()
			if len(notes) == 0 {
				return "advisor: no notes from the last review"
			}
			var b strings.Builder
			for _, n := range notes {
				fmt.Fprintf(&b, "[%s] %s\n", n.Severity, n.Text)
			}
			return strings.TrimRight(b.String(), "\n")
		},
	})
	app.SetPrewalkOps(&tui.PrewalkOps{
		Enabled: func() bool { return true },
		Status: func() string {
			prewalkMu.Lock()
			defer prewalkMu.Unlock()
			if !prewalkOn {
				return "prewalk off (flags still apply: --prewalk)"
			}
			return fmt.Sprintf("prewalk on — hands off to %s/%s after the first edit/write once a plan todo list exists", prewalkTarget.Provider.Name(), prewalkTarget.Model)
		},
		Set: func(on bool, into string) error {
			if on {
				// An empty target is the session model: resolveModel
				// falls through to defaultModel / models.yml.
				nref, _, err := resolveModel(strings.TrimSpace(into), cfg, lastSettings())
				if err != nil {
					return err
				}
				pn, mn, err := config.ParseModelRef(nref)
				if err != nil {
					return err
				}
				pc, ok := cfg.Providers[pn]
				if !ok {
					return fmt.Errorf("unknown provider %q", pn)
				}
				prov, err := buildProvider(pn, pc, mn, cfg)
				if err != nil {
					return err
				}
				prewalkMu.Lock()
				prewalkOn, *prewalkTarget = true, agent.FailoverTarget{Provider: prov, Model: mn}
				prewalkMu.Unlock()
				return nil
			}
			prewalkMu.Lock()
			prewalkOn = false
			prewalkMu.Unlock()
			return nil
		},
	})
	app.SetPlanOps(&tui.PlanOps{
		Show: func() string { return planShowText(planMode) },
		Get:  func() bool { return planMode.Active() },
		Set: func(on bool) error {
			// The director's reduced toolset and read-only planning
			// contradict each other: one mode at a time.
			if on && vibeActive() {
				return fmt.Errorf("vibe mode is active — /vibe off first")
			}
			planMode.SetActive(on)
			return nil
		},
	})

	// goalKick runs the goal's first turn. Setting a goal must actually start
	// it: the goal state on its own only decorates the next user-driven turn,
	// so `/goal <objective>` printed "goal created" and then nothing ran. The
	// run continues from there (agent.Agent.GoalContinuation), and the
	// objective becomes the session title and the first prompt the user sees.
	// Safe before SetHandlers: SendPrompt is a no-op while onSend is unwired.
	goalKick := func(objective string) { app.SendPrompt(objective) }

	// /goal drives the same GoalState the goal tool owns: the ops mutate
	// through the tool's own seam (so the session entry + the per-turn
	// reminder stay consistent) and echo the resulting state.
	app.SetGoalOps(&tui.GoalOps{
		View: func() string {
			gs := agent.GoalStateOf(reg)
			if gs == nil {
				return "goal: not wired"
			}
			return gs.Describe()
		},
		Set: func(objective string) (string, error) {
			gs := agent.GoalStateOf(reg)
			if gs == nil {
				return "", fmt.Errorf("goal not wired")
			}
			// One active goal at a time: a goal the model already created
			// (or one the user is still working) is not silently replaced —
			// Drop closes it, and /goal drop is the command for that.
			if _, err := gs.Create(objective, 0); err != nil {
				return "", err
			}
			goalKick(objective)
			return "goal created\n" + gs.Describe(), nil
		},
		Continue: func(objective string) (string, error) {
			gs := agent.GoalStateOf(reg)
			if gs == nil {
				return "", fmt.Errorf("goal not wired")
			}
			g, err := gs.Resume(objective)
			if err != nil {
				return "", err
			}
			goalKick(g.Objective)
			return "goal resumed\n" + gs.Describe(), nil
		},
		Complete: func(notes []string) (string, error) {
			gs := agent.GoalStateOf(reg)
			if gs == nil {
				return "", fmt.Errorf("goal not wired")
			}
			if _, err := gs.Complete(notes); err != nil {
				return "", err
			}
			return gs.Describe(), nil
		},
		Drop: func() (string, error) {
			gs := agent.GoalStateOf(reg)
			if gs == nil {
				return "", fmt.Errorf("goal not wired")
			}
			if _, err := gs.Drop(); err != nil {
				return "", err
			}
			return "goal dropped\n" + gs.Describe(), nil
		},
	})
	// The goal tool, registered HERE rather than in newToolRegistry: /goal and
	// the loop both reach it through the registry (agent.GoalStateOf), and a
	// builder-shared registration made every registry in the tree carry a
	// goal while the TUI — which registers its own agent — had none, so /goal
	// answered "goal not wired" in every session. It goes in before
	// wireTaskParent binds it, so an early call cannot read unbound state.
	reg.Register(&agent.GoalTool{Goals: agent.NewGoalState(nil)})
	wireTaskParent(reg, storeOf())
	app.SetScheduleOps(&tui.ScheduleOps{
		List: func() string {
			st := agent.ScheduleStateOf(reg)
			if st == nil {
				return "schedule: not wired"
			}
			rows := st.List()
			if len(rows) == 0 {
				return "no schedules"
			}
			return agent.FormatScheduleList(rows, time.Now().UTC())
		},
		Create: func(prompt, selector string) (string, error) {
			st := agent.ScheduleStateOf(reg)
			if st == nil {
				return "", fmt.Errorf("schedule not wired")
			}
			kind, seconds, at, err := tui.ScheduleSelector(selector)
			if err != nil {
				return "", err
			}
			in := agent.ScheduleInput{Prompt: prompt, At: at}
			switch kind {
			case "after":
				in.AfterSeconds = seconds
			case "every":
				in.EverySeconds = seconds
			}
			rec, err := st.Create(in)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("scheduled %s for %s", rec.ID, rec.ScheduledAt.Format(time.RFC3339)), nil
		},
		Delete: func(id string) error {
			st := agent.ScheduleStateOf(reg)
			if st == nil {
				return fmt.Errorf("schedule not wired")
			}
			return st.Delete(id)
		},
	})

	// No auto-created goal here, unlike print mode (#387): an active goal is
	// what /vibe reads as a conflict, so a placeholder would refuse to enter
	// director mode in every fresh session. The objective is the user's to
	// name — `/goal <objective>` — and the state stays empty until then.
	// applyTheme runs every resolved palette through the color-blind remap
	// (settings colorBlindMode) so startup, /theme and live reload agree.
	applyTheme := func(t *theme.Theme) *theme.Theme {
		if lastSettings().ColorBlindMode {
			return theme.ApplyColorBlindMode(t)
		}
		return t
	}
	if themeName != "" {
		stopWatch := theme.Watch(theme.CustomDir(), themeName, func(nt *theme.Theme) {
			app.SetTheme(applyTheme(nt))
		})
		defer stopWatch()
	}
	// themeLabel is what the user configured — "auto" (the polarity-detected
	// palette) or a concrete theme. /theme reports and writes it, so the
	// value /settings shows ("theme auto") is restorable instead of dead and
	// a switch survives a restart instead of evaporating at exit.
	themeLabel := themeName
	if themeLabel == "" {
		themeLabel = "auto"
	}
	app.SetThemeOps(&tui.ThemeOps{
		Current: func() string {
			if themeLabel == "auto" {
				return "auto (" + th.Name + ")"
			}
			return th.Name
		},
		List: func() []string { return theme.AvailableThemes(theme.CustomDir()) },
		Set: func(name string) error {
			nt := theme.LoadNamed(name, theme.CustomDir())
			if nt == nil {
				return fmt.Errorf("unknown theme %q", name)
			}
			// LoadNamed falls back on failure, so only an exact hit (or the
			// "auto" polarity default, whose resolved name differs by
			// design) counts: a typo is reported, never silently applied.
			if nt.Name != name && name != "auto" {
				known := false
				for _, avail := range theme.AvailableThemes(theme.CustomDir()) {
					if avail == name {
						app.SetTheme(applyTheme(nt))
						th = nt
						return nil
					}
				}
				if !known {
					return fmt.Errorf("unknown theme %q", name)
				}
			}
			app.SetTheme(applyTheme(nt))
			th = nt
			themeLabel = name
			if err := config.Set(config.GlobalSettingsPath(), "theme", name); err != nil {
				// The palette switched; only saving failed. Say which, rather
				// than reporting the switch itself as broken.
				return fmt.Errorf("theme %s applied for this session, but saving failed: %v", name, err)
			}
			lastSettings().Theme = name
			return nil
		},
	})
	// --- /connect: the provider catalog. The listing and the write live in
	// internal/config; what is wired here is TUI state — the picker rows, and
	// folding a newly connected provider into the running config so /model can
	// reach it without a restart.
	app.SetConnectOps(&tui.ConnectOps{
		Items: connectPickerItems(cfg),
		Connect: func(name string) error {
			if _, err := config.Connect(name, ""); err != nil {
				return err
			}
			// Fold the new provider into the running config so /model lists it
			// without a restart, key by key rather than by replacing the struct
			// (the non-yaml fields — the repo-trust notices — stay as they
			// were). ponytail: this mutates shared config from the key thread,
			// the way /theme already mutates lastSettings(); the upgrade path
			// is one mutex on Config with every read site behind it, not a
			// lock bolted onto this one writer.
			if fresh, err := config2Load(); err == nil {
				for k, v := range fresh.Providers {
					cfg.Providers[k] = v
				}
				if fresh.DefaultModel != "" {
					cfg.DefaultModel = fresh.DefaultModel
				}
			}
			return nil
		},
		DefaultRef: config.ConnectDefaultRef,
	})

	// --- collab (M14 #59): E2E-encrypted live session sharing -----------
	// Hosting serves this session over an in-process WebSocket relay
	// (internal/collab): guests receive the sealed transcript and, with a
	// full link, prompt through this process — the host stays authoritative
	// and runs every tool. A joined guest renders a native replica of the
	// host transcript in this TUI and forwards typed prompts to the host.
	// ponytail: entry fan-out polls the store (internal/session has no
	// append observer), so an entry reaches guests within one poll tick;
	// streaming deltas, ui-request, bus, and agents frames have working
	// protocol support but no producers wired yet.
	tui.Collab = &tui.CollabOps{
		Start: func(mode tui.CollabMode) (string, error) {
			collabMu.Lock()
			defer collabMu.Unlock()
			if collabHost != nil {
				return collabShareText(collabHost, mode.View), nil
			}
			addr := mode.Addr
			if addr == "" {
				addr = os.Getenv("XDEV_COLLAB_ADDR")
			}
			h, err := collab.NewHost(collab.HostConfig{
				Addr: addr,
				// Binding beyond loopback is the explicit opt-in
				// (issue #59): /collab remote or XDEV_COLLAB_ADDR.
				AllowRemote: mode.Remote || addr != "",
				Name:        collab.DefaultName(),
				Backend: collab.Backend{
					Snapshot: func() []byte { return collabSnapshot(storeOf()) },
					Prompt: func(name, text string) {
						app.AddSystemBlock("· collab " + name + ": " + text)
						app.SendPrompt(text)
					},
					// Cancel the live turn, never baseCtx (see liveTurn).
					Interrupt: func() { tabs.abortCurrent() },
				},
				Entries: func() [][]byte { return collabEntries(storeOf()) },
				Logf:    func(f string, a ...any) { logx.Debugf("collab: "+f, a...) },
			})
			if err != nil {
				return "", err
			}
			if _, err := h.ListenAndServe(); err != nil {
				return "", err
			}
			collabHost = h
			return collabShareText(h, mode.View), nil
		},
		Status: func() string {
			collabMu.Lock()
			h, g := collabHost, collabGuest
			collabMu.Unlock()
			switch {
			case h != nil:
				return collabStatusText(h)
			case g != nil:
				return "· collab: guest in room " + g.RoomID() + " — replica " + collab.ReplicaPath(g.RoomID())
			default:
				return "· collab: not sharing (run /collab to share, /join <link> to mirror someone else)"
			}
		},
		Stop: func() error {
			collabMu.Lock()
			h, g := collabHost, collabGuest
			collabHost, collabGuest = nil, nil
			collabMu.Unlock()
			if g != nil {
				_ = g.Close()
			}
			if h != nil {
				return h.Stop()
			}
			return nil
		},
		Join: func(link string) (string, error) {
			l, err := collab.ParseLink(link)
			if err != nil {
				return "", err
			}
			collabMu.Lock()
			if collabGuest != nil {
				collabMu.Unlock()
				return "", fmt.Errorf("already joined room %s — /collab stop leaves first", collabGuest.RoomID())
			}
			collabMu.Unlock()
			var g *collab.Guest
			g, err = collab.Join(baseCtx, collab.GuestConfig{
				Link: l,
				Name: collab.DefaultName(),
				// The replica renders through the same context builder
				// /resume uses, so the guest transcript is native.
				OnSnapshot: func(data []byte) {
					app.AddSystemBlock("· collab room " + l.RoomID + " — replica " + collab.ReplicaPath(l.RoomID))
					replayTranscript(app, collab.Messages(data))
				},
				OnEntry: func(line []byte) { replayTranscript(app, collab.Messages(line)) },
				OnEvent: func(raw json.RawMessage) {
					if txt := collab.NoticeText(raw); txt != "" {
						app.AddSystemBlock("· collab: " + txt)
					}
				},
				OnClose: func(cerr error) {
					collabMu.Lock()
					if collabGuest == g {
						collabGuest = nil
					}
					collabMu.Unlock()
					if cerr != nil {
						app.AddSystemBlock("· collab disconnected: " + cerr.Error())
					}
				},
			})
			if err != nil {
				return "", err
			}
			collabMu.Lock()
			collabGuest = g
			collabMu.Unlock()
			go func() { _ = g.Run(baseCtx) }()
			perm := "view-only"
			if g.Writable() {
				perm = "full control"
			}
			return "· joining collab room " + l.RoomID + " (" + perm + ")", nil
		},
		Forward: func(text string) bool {
			collabMu.Lock()
			g := collabGuest
			collabMu.Unlock()
			if g == nil {
				return false
			}
			// A guest's typed prompt belongs to the host session: send it
			// over the room instead of starting a local turn.
			if err := g.Prompt(text); err != nil {
				app.AddSystemBlock("· collab: " + err.Error())
			}
			return true
		},
	}
	app.SetCommandDir(cwd)

	// startTurn launches one agent run against the CURRENT store and returns
	// immediately; the caller owns the `running` claim (CompareAndSwap) and any
	// entry it committed first. runTurn calls it after persisting the user
	// prompt; the F5 retry calls it directly to resume a session whose stream
	// dropped mid-turn — the same history drives another turn, no new prompt.
	//
	// flushPendingQueue is the mid-turn queue's run-end flush (#157), declared
	// further down (it needs runTurn) and deferred at the top of the goroutine
	// so it runs LAST: a pending row the model never saw becomes the next turn
	// rather than a row that lies about being sent. A turn can only start
	// after the whole wiring below is in place, so the var is never nil here.
	var flushPendingQueue = func() {}
	startTurn := func() {
		// Capture the session this turn belongs to UP FRONT. A switch mid-turn
		// re-points storeOf(), and the turn must keep writing to the store it
		// started on — never the newly focused one.
		turnStore := storeOf()
		if turnStore == nil {
			return
		}
		sid := turnStore.ID()
		ctx, cancel := context.WithCancel(baseCtx)
		tabs.publish(sid, cancel)
		goGuarded(func() {
			// The mid-turn queue's run-end flush (#157), registered FIRST so it
			// runs LAST: the deferred release below frees the turn claim, and
			// this flush claims again to start the next turn. A pending row the
			// model never saw becomes an ordinary turn rather than a row that
			// lies about being sent. Only the FOREGROUND session flushes: a
			// parked turn finishing must not steal the screen's composer queue.
			defer func() {
				if t := tabs.current(); t != nil && t.id == sid {
					flushPendingQueue()
				}
			}()
			// LIFO: clear runs FIRST so this turn can never nil a slot that a
			// newer turn already claimed.
			defer cancel()
			defer tabs.release(sid)
			defer tabs.clear(sid)
			// Spinner follows the foreground session only.
			if t := tabs.current(); t != nil && t.id == sid {
				app.SetRunning(true)
			}
			feedAdvisor := func() {}
			modelMu.Lock()
			lp, lm, lpn, le := live.prov, live.model, live.provName, live.effort
			modelMu.Unlock()
			// Per-turn hooks pin the store and the focused predicate so a
			// parked turn keeps persisting and never paints over the live view.
			turnTS := &tuiSession{store: turnStore, app: app, tabs: tabs, id: sid}
			ag := &agent.Agent{
				Provider: lp,
				Tools:    toolsForTurn(),
				// feedAdvisor is assigned after the agent exists, so go
				// through an indirection: a direct field copy would
				// capture the nil func at literal time.
				Hooks:      memoryTurnHooks(&tuiHooks{ts: turnTS, feed: func() { feedAdvisor() }}, lastSettings()),
				TTSR:       agent.NewTTSR(ttsrConfig(lastSettings())),
				MaxTokens:  opts.MaxTokens,
				MaxTurns:   opts.MaxTurns,
				Model:      lm,
				Store:      turnStore,
				Compaction: agent.CompactionConfig{ContextWindow: modelWindow(cfg, lpn, lm), Methods: agent.HandoffOrder(lastSettings().CompactionMethodOrder())},
				Failovers:  failoverChain(cfg, lastSettings(), lpn, lm),
				Thinking:   effortBudget(le),
				// Intercept set below from exts (only when non-nil).
				Policy:  agentPolicy(),
				Handoff: handoffSettings(),
				// Live tool output: a running tool's chunks paint as they
				// arrive, into the box the first chunk opens under that call's
				// own row. A tool that streams nothing opens no box. Gated the
				// same way every other paint is: a parked turn is silent.
				OnOutput: func(callID, name, chunk string) {
					if turnTS.isFocused() {
						app.AppendToolOutput(callID, name, chunk)
					}
				},
			}
			// Shared per-mode seams: catalog bridge + secrets redactor
			// (#79/#80). The TUI is the daily driver; an unredacted tool
			// result here is the case that mattered.
			if st := wireAgentMode(ag, reg, cfg, lastSettings(), lpn, lm, cwd, true); st != nil {
				// The TUI has a console: a silent provider swap or a
				// cooldown revert is otherwise invisible to the user.
				st.Notify = func(msg string) {
					if turnTS.isFocused() {
						app.AddSystemBlock("· " + msg)
					} else {
						tabs.note(sid)
					}
				}
			}
			// The HUD context segment measures against this window.
			if turnTS.isFocused() {
				app.SetContextWindow(int64(modelWindow(cfg, lpn, lm)))
			}
			prewalkMu.Lock()
			pwOn, pwT := prewalkOn, *prewalkTarget
			prewalkMu.Unlock()
			if pwOn {
				ag.Prewalk = &agent.Prewalk{Target: pwT}
			} else if t := resolvePrewalk(opts, cfg, lastSettings()); t != nil {
				ag.Prewalk = &agent.Prewalk{Target: *t}
			}
			ag.PlanMode = planMode
			// feedAdvisor hands the reviewer a fresh snapshot after each
			// assistant message; it never blocks the primary turn.
			if adv != nil {
				adv.Primary = ag
				feedAdvisor = func() {
					if !advisorOn.Load() {
						return
					}
					sessMu.Lock()
					snap := rebuildHistory()
					sessMu.Unlock()
					go adv.Feed(baseCtx, append([]ai.Message(nil), snap...))
				}
			}
			// Extension actions steer the live run: this agent is the
			// target until the next submit replaces it.
			// Hook bus per submit: --hook specs, settings `hooks`, discovered
			// files. Extensions compose into the same fail-closed chain.
			hookBus := buildHookBus(cwd, opts, app.AddSystemBlock)
			agentMu.Lock()
			curAgent = ag
			if c := agent.NewChain(hookBus, exts); c != nil {
				ag.Intercept = c
			}
			ag.Hooks = agent.WithCompactionEvent(ag.Hooks, ag.Intercept)
			// The mid-turn queue's retirement point (#157). The loop's steering
			// drain is where a queued prompt actually becomes part of the
			// conversation, so that is where the TUI's pending row retires — not
			// when the steer call returned, which only says the text is sitting in
			// a channel. The retirement is a hit, not a swap: two prompts with the
			// same text are two entries, and the oldest undelivered one is the one
			// that was sent.
			ag.SteeringDelivered = app.RetireDelivered
			agentMu.Unlock()
			defer func() {
				agentMu.Lock()
				if curAgent == ag {
					curAgent = nil
				}
				agentMu.Unlock()
				// Spinner off only if we still own the foreground.
				if t := tabs.current(); t != nil && t.id == sid {
					app.SetRunning(false)
				}
				app.SetTabs(tabInfos(tabs))
			}()
			sessMu.Lock()
			hist, errH := session.BuildContext(turnStore.Entries(), turnStore.LeafID(), session.SystemPrompt{})
			var history []ai.Message
			if errH == nil {
				history = hist.Messages
			}
			sessMu.Unlock()
			sys := buildSys()
			if vibeActive() {
				sys = vibeSys() // director prompt for the restricted toolset
			}
			finalMsg, err := ag.Run(ctx, hookBus.Context(ctx, sys), history)
			if turnTS.isFocused() {
				app.EndAssistant()
				app.FinishRun()
				// One finished run is one turn — the count dsh's TimePill
				// reads beside its steps, whatever the turn ended with.
				app.AddTurn()
			} else {
				tabs.note(sid)
			}
			// Ai-title cascade (#107): the TUI sessions are the ones the
			// picker lists, and they are the ones stuck with "tui
			// <timestamp>". Async on purpose: the user's next keystroke
			// must not wait on a title request.
			lastTurnFailed.Store(err != nil)
			if err == nil && finalMsg != nil && !launch.NoTitle && !launch.NoSession {
				// The bump after the title lands is what repaints the panel's
				// title slot: the cascade rewrites the store's title on a
				// goroutine long after this frame, so nothing else would.
				goGuarded(func() {
					generateTitle(cfg, lastSettings(), cwd, lpn, lm, turnStore,
						append(append([]ai.Message(nil), history...), *finalMsg))
					tabs.setTitle(sid, turnStore.Title())
					if turnTS.isFocused() {
						app.DockBump()
						app.SetTabs(tabInfos(tabs))
					}
				})
			}
			if err != nil {
				if ctx.Err() != nil {
					if turnTS.isFocused() {
						app.AddSystemBlock("· turn canceled")
					}
				} else if errors.Is(err, agent.ErrEmptyTurn) {
					// The model answered nothing after every nudge was spent
					// (a thinking-mode upstream leaving only a reasoning block,
					// or nothing at all). Rebuild context from the persisted
					// history and re-run the agent so the session auto-resumes
					// instead of dying with a dead-end error (#331). Same
					// recovery print and rpc modes already had — the TUI is
					// the daily driver and was the one gap.
					if turnTS.isFocused() {
						app.AddSystemBlock("· the model answered with nothing — retrying from history")
					}
					ctxRes, rerr := session.BuildContext(turnStore.Entries(), turnStore.LeafID(), session.SystemPrompt{})
					if rerr == nil {
						finalMsg, err = ag.Run(ctx, hookBus.Context(ctx, sys), ctxRes.Messages)
					}
					if err != nil && turnTS.isFocused() {
						app.AddSystemBlock("error: " + err.Error())
					} else if err != nil {
						tabs.note(sid)
					}
				} else if turnTS.isFocused() {
					app.AddSystemBlock("error: " + err.Error())
				} else {
					tabs.note(sid)
				}
			}
		})
	}

	// runTurn owns one submit: persist the user message, then drive the agent.
	// imgs is the pasted images riding with it (nil for a plain prompt). It
	// reports whether the turn was taken, so a draft carrying attachments can go
	// back to the composer instead of being sent without them — see
	// tui.App.SetImageSend.
	// runTurnNow is runTurn for a caller that ALREADY holds the turn claim.
	// The send-now path claims the slot itself (cmd/xdev/tui.go), because
	// between "the interrupted run released" and "the next turn starts" there
	// is a window another submit could take, and a send-now that lost it must
	// leave the message queued rather than race. Everything below the claim —
	// the guest forward, the naming, the persist, the memory/friction notes,
	// the start — is identical to runTurn by construction: one function.
	persistAndStart := func(text string, imgs []tui.PasteImage) bool {
		s := storeOf()
		if s == nil {
			return false
		}
		// Name the session after its first prompt: /resume and the
		// breadcrumb read the title slot, and "print <timestamp>" hides
		// everything about the conversation. Called before the first
		// assistant message materializes the file, so the title lands in
		// the slot without needing a rewrite pass; later prompts keep
		// the first one's title (omp's first-prompt cascade).
		if s.Path() == "" {
			if t := titleFromPrompt(text); t != "" {
				s.SetTitle(t)
				tabs.setTitle(s.ID(), t)
			}
		}
		sessMu.Lock()
		msg := ai.Message{
			Role:        ai.RoleUser,
			Attribution: "user",
			UserTS:      time.Now().UnixMilli(),
		}
		sessMu.Unlock()
		// A pasted image is a block, not a word in the text: the chip the
		// composer showed has already been stripped (tui.App.expandPastes),
		// and what is left of the draft goes out beside the payloads in the

		// order they sit in the prompt.
		if text != "" {
			msg.Content = append(msg.Content, ai.TextBlock{Text: text})
		}
		for _, im := range imgs {
			msg.Content = append(msg.Content, ai.ImageBlock{Source: ai.ImageSource{
				Type:      "base64",
				MediaType: im.MediaType,
				Data:      base64.StdEncoding.EncodeToString(im.Data),
			}})
		}
		if err := s.Append(&session.MessageEntry{Message: msg}); err != nil {
			logx.Errorf("persist user message: %v", err)
		}
		// #86: the memory turn boundary. print mode counted turns for the
		// remote backend; the TUI — where sessions are actually long —
		// never fed it, so retainEveryNTurns could not fire and queued
		// retains sat until exit.
		noteMemoryTurn(sessionMemory, []ai.Message{msg})
		// #89: the friction detector had no TUI feed at all, so decision
		// files only ever accumulated in print runs.
		observeFriction(sessionMemory, text, lastTurnFailed.Swap(false))
		startTurn()
		return true
	}
	// runTurn is persistAndStart behind the turn claim: a submit that finds
	// the slot taken is refused (the composer's draft comes back with the
	// reason), and one that claims it owns the turn from there on. The guest
	// forward comes FIRST, before the claim, because a joined room has no
	// local turn to take — the host runs it.
	runTurn := func(text string, imgs []tui.PasteImage) bool {
		if collabGuestJoined() {
			if imgs != nil {
				return false
			}
			if tui.Collab.Forward(text) {
				return true
			}
		}
		sid, ok := tabs.claimCurrent()
		if !ok {
			app.AddSystemBlock("a turn is already running — Esc cancels it")
			return false
		}
		if !persistAndStart(text, imgs) {
			tabs.release(sid)
		}
		return true
	}
	// runTurnNow is persistAndStart for a caller that ALREADY holds the turn
	// claim: the send-now path claims the slot itself, so it can lose the race
	// and leave the message queued rather than start a second turn. A false
	// return means the message was not persisted, so the caller must release
	// the claim it took.
	runTurnNow := func(text string) bool { return persistAndStart(text, nil) }
	// The mid-turn queue's run-end flush (#157), the var startTurn defers.
	flushPendingQueue = func() {
		if collabGuestJoined() {
			return
		}
		// Anything still queued when a run ends is a message the model never
		// saw: typed in the last moments of a turn, or one the run could not
		// steer. Starting it as an ordinary turn is the honest outcome — a row
		// that outlives its run is a prompt the user believes was sent and was
		// not. Drained oldest-first, one per run, so a burst of prompts does
		// not collapse into a single fused message.
		text := app.TakeOldestQueued()
		if text == "" {
			return
		}
		// runTurn takes the claim itself, so the row was taken back if the
		// claim is gone (a turn that started between the pop and here owns the
		// slot, and its own flush will drain the rest).
		if !runTurn(text, nil) {
			app.QueueAgain(text)
		}
	}
	// Background subagent completion notice (#296). A job started with
	// background:true used to end silently: the model asked for work in
	// parallel, then had to guess when it was done, because a result it never
	// learns about is one it never reads. The same three-phase shape as the
	// schedule reminder below, and for the same reason it cannot be an in-turn
	// injection: Run takes history by value, and an idle session has no Agent
	// and no turn goroutine to drain a queue. So — gate on idle, claim the
	// turn, persist into the store the next run rebuilds from, start the turn.
	if sessionHub != nil {
		hubCtx, hubCancel := context.WithCancel(baseCtx)
		hubDone := make(chan struct{})
		goGuarded(func() {
			defer close(hubDone)
			agent.StartHubNoticeDelivery(hubCtx, sessionHub,
				func() bool { return !collabGuestJoined() && !tabs.currentRunning() },
				func(notice string) error {
					target := storeOf()
					sid, ok := tabs.claimCurrent()
					if target == nil || !ok {
						return agent.ErrHubNoticeDelivery
					}
					msg := ai.Message{
						Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: notice}},
						Attribution: agent.HubNoticeAttribution, UserTS: time.Now().UnixMilli(),
					}
					if err := target.Append(&session.MessageEntry{Message: msg}); err != nil {
						tabs.release(sid)
						return fmt.Errorf("%w: %v", agent.ErrHubNoticeDelivery, err)
					}
					return nil
				},
				func(noticeErr error) {
					if noticeErr != nil {
						// Not lost: the job is on the roster and
						// `hub result` still answers.
						app.AddSystemBlock("· background subagent finished; its notice could not be delivered, read it with hub result")
					}
					startTurn()
				})
		})
		defer func() {
			hubCancel()
			<-hubDone
		}()
	}
	// Schedule delivery is an ordinary later turn, never steering. It waits
	// for an idle interactive session, persists the reminder, then uses the
	// same startTurn path as a user prompt.
	scheduleCtx, scheduleCancel := context.WithCancel(baseCtx)
	scheduleDone := make(chan struct{})
	goGuarded(func() {
		defer close(scheduleDone)
		agent.StartScheduleDelivery(scheduleCtx, agent.ScheduleStateOf(reg),
			func([]agent.Schedule) bool { return !collabGuestJoined() && !tabs.currentRunning() },
			func(batch []agent.Schedule) error {
				claimed := agent.ScheduleStateOf(reg)
				if claimed == nil {
					return agent.ErrScheduleDelivery
				}
				// The state is delivery-locked for this callback, so its
				// bound store is stable; a swap waits for BindDelivery.
				storeForDelivery := claimed.CurrentStore()
				sid, ok := tabs.claimCurrent()
				if storeForDelivery == nil || !ok {
					return agent.ErrScheduleDelivery
				}
				msg := ai.Message{
					Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: agent.SchedulePrompt(batch)}},
					Attribution: "schedule", UserTS: time.Now().UnixMilli(),
				}
				if err := storeForDelivery.Append(&session.MessageEntry{Message: msg}); err != nil {
					tabs.release(sid)
					return fmt.Errorf("%w: %v", agent.ErrScheduleDelivery, err)
				}
				return nil
			},
			func(_ []agent.Schedule, claimErr error) {
				if claimErr != nil {
					app.AddSystemBlock("· scheduled reminder dispatch deferred; running the due turn")
				} else {
					app.AddSystemBlock("· scheduled reminder due")
				}
				startTurn()
			})
	})
	defer func() {
		scheduleCancel()
		<-scheduleDone
	}()
	app.SetHandlers(
		func(text string) { runTurn(text, nil) },
		// Esc / Ctrl+C aborts the live turn only; see liveTurn. This handler
		// used to call baseCancel(), which bricked every future turn after
		// the first cancel while the TUI still looked alive. The quit chord
		// also lands here (tui.App.quitOrCancel cancels first, then quits),
		// so a turn that outlives the UI unwinds against this turn's own
		// cancel — never baseCancel, which the defers below still own.
		func() { tabs.abortCurrent() },

		func() { app.Quit() },
	)
	// Detach-on-quit (settings tui.exitDetach, default ON — opencode parity).
	// A live turn is handed to a --bg worker so killing the TUI does not
	// kill the work. The worker resumes the same session id; hang caps are
	// the same as an explicit --bg run (default --max-time 2h).
	//
	// ponytail: we cannot transplant the in-process turn goroutine across a
	// process boundary, so "detach" means "cancel the TUI turn and spawn a
	// print-mode child that continues the same session from disk". The model
	// may re-do the last unfinished step; the session file is the source of
	// truth either way. Upgrade path: a long-lived supervisor that owns the
	// turn process from the start (#131 residual / attach).
	var detachedOnQuit atomic.Bool
	app.SetQuitRunning(func() bool {
		if !lastSettings().TuiExitDetachOn() {
			return false
		}
		// The FOREGROUND session is the one the user is looking at, so it is
		// the one handed off. A parked session mid-turn stays in this
		// process, and abortAll() cancels it when the TUI actually exits —
		// detaching only the screen's own session is the honest reading.
		// ponytail: one hand-off, not one per running tab. A PARKED session
		// mid-turn is still cancelled by abortAll() on exit, so quitting with
		// two busy sessions keeps only the foreground one. Upgrade path:
		// spawn one child per running tab id and print one block per id.
		s := storeOf()
		if s == nil || !tabs.isRunning(s.ID()) {
			return false
		}
		// Need a durable session file for the child to resume.
		if s.Path() == "" {
			if _, err := s.EnsureOnDisk(
				session.SessionFilePath(sessionDataDir(), cwd, time.Now(), s.ID()),
				session.Options{},
			); err != nil {
				app.AddSystemBlock("· detach failed — session not on disk: " + err.Error())
				return false
			}
		}
		// Stop the in-process turn first so the store is quiet for the child.
		tabs.abort(s.ID())
		// Wait briefly for the turn to release; do not block quit forever.
		deadline := time.Now().Add(2 * time.Second)
		for tabs.isRunning(s.ID()) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		childArgv := []string{
			"--print",
			"--resume", s.ID(),
			"--max-time", "2h",
		}
		// Carry the live model so the child does not fall back to defaultModel.
		modelMu.Lock()
		ref := live.provName + "/" + live.model
		modelMu.Unlock()
		if ref != "/" && !strings.HasPrefix(ref, "/") && !strings.HasSuffix(ref, "/") {
			childArgv = append(childArgv, "--model", ref)
		}
		// Empty prompt: print mode with --resume and no prompt still drives a
		// turn from the session's unfinished state only when a prompt is
		// supplied. Give the child a continuation nudge so Run has a user turn.
		id, err := spawnBg("continue the unfinished work from this session", childArgv)
		if err != nil {
			app.AddSystemBlock("· detach failed: " + err.Error())
			return false
		}
		detachedOnQuit.Store(true)
		// Stash the id on the store via a side channel the exit banner reads.
		setLastDetachID(id)
		return true
	})
	app.SetImageSend(func(text string, imgs []tui.PasteImage) bool { return runTurn(text, imgs) })
	// The mid-turn submit queue (#157). Two paths, both about the live turn:
	//
	//   - onQueue: a prompt typed while a turn is running joins that run. The
	//     agent already accepts a queued message and injects it as a user
	//     message at its next step boundary (Agent.Steer → the loop's steering
	//     drain), so delivery is one call and the message is persisted by the
	//     loop itself. False means "no live agent to steer" — the TUI shows
	//     the row, cmd says no, and the row is withdrawn rather than left
	//     promising a delivery.
	//   - onSendNow: interrupt the live turn and run this message as a fresh
	//     turn. Interrupting rather than injecting is the honest reading of
	//     "now": the in-flight provider request cannot be pre-empted, and a
	//     message the user asked to have sent NOW should not wait behind a
	//     round trip. The acknowledgement is a system block, so the user can
	//     see the run was stopped on purpose and not by a network failure.
	//
	// The delivery order is the queue's own: the oldest pending message goes
	// first and the rest keep their place behind it.
	app.SetQueueHandlers(
		func(text string) bool {
			// A guest's prompt belongs to the host session; there is no local
			// run to steer, so the room answers the question the same way the
			// forward path always has.
			if collabGuestJoined() {
				return tui.Collab.Forward(text)
			}
			agentMu.Lock()
			target := curAgent
			agentMu.Unlock()
			if target == nil {
				return false // no live turn: the submit falls back to a real one
			}
			target.Steer(text)
			return true
		},
		func(text string) {
			if !tabs.currentRunning() {
				// Nothing to interrupt: the message is an ordinary submit now.
				runTurn(text, nil)
				return
			}
			if collabGuestJoined() {
				app.AddSystemBlock("joined as a guest — the host runs the turn")
				return
			}
			// Cancel first, then wait for the run to release its claim. The
			// turn goroutine clears the tab's running flag on its way out;
			// taking the claim before it does would either lose the race or
			// steal the turn from a run that is still unwinding.
			stopped := tabs.abortCurrent()
			if !stopped {
				app.AddSystemBlock("no turn is running — sending the message now")
				runTurn(text, nil)
				return
			}
			// The abort is asynchronous by nature: the run is mid-stream and
			// unwinds on its own goroutine. Wait for the claim to come back
			// before starting the next turn, bounded so a provider that ignores
			// the cancel cannot wedge the composer.
			//
			// The row leaves the queue ONLY when this actually delivers it, and
			// a run that is still unwinding at the deadline will flush the queue
			// at ITS end — so keeping the row here is both safe and necessary.
			// Dropping it early would lose the message; dropping it after a
			// successful runTurn is what prevents a double delivery, because
			// runTurn IS the delivery.
			app.AddSystemBlock("· interrupted — delivering now")
			deadline := time.Now().Add(2 * time.Second)
			for tabs.currentRunning() && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if tabs.currentRunning() {
				app.AddSystemBlock("the interrupted turn has not released yet — this message runs as soon as it does")
				return
			}
			// Claim the slot before runTurnNow, so two racing send-nows cannot
			// both believe they own it. A failed claim means another turn got
			// there first, and the row stays queued for that turn's flush.
			sid, ok := tabs.claimCurrent()
			if !ok {
				app.AddSystemBlock("another turn started first — this message is still queued")
				return
			}
			app.SetRunning(false)
			// runTurnNow persists the message and starts the turn; the row is
			// dropped only once that succeeded, because a message persisted
			// AND left queued would be delivered a second time by the flush.
			if runTurnNow(text) {
				app.DropQueued(text)
				return
			}
			tabs.release(sid)
		})
	// Shell mode (M10 #163): "!<command>" in the composer runs locally and
	// prints to the transcript. Nothing is sent to the model, so the draft
	// costs no tokens; the block is display-only — it is not persisted to the
	// session, so a resume replays the conversation without it.
	tui.Bang = newBangRunner(app, cwd, baseCtx)
	// F5 retry: re-run the agent over the CURRENT store with no new prompt —
	// the recovery for a turn a dropped stream cut short. The turn claim is
	// taken exactly as runTurn takes it, so an in-flight run is refused rather
	// than stolen; the empty store is refused too (nothing to resume yet).
	app.SetRetry(func() {
		if collabGuestJoined() {
			app.AddSystemBlock("joined as a guest — the host runs the turn")
			return
		}
		sid, ok := tabs.claimCurrent()
		if !ok {
			app.AddSystemBlock("a turn is already running — Esc cancels it")
			return
		}
		s := storeOf()
		if s == nil || len(s.Entries()) == 0 {
			tabs.release(sid)
			app.AddSystemBlock("nothing to retry yet — send a prompt first")
			return
		}
		startTurn()
	})
	// Vision is the live model's property, not the launch model's: /model
	// mid-session changes whether an attachment can be read at all.
	// Session tabs (opencode session.tab.next / .previous). Alt+] / Alt+[
	// cycle the open set without aborting a parked turn; Alt+Shift+] jumps
	// to the next unread one. Wired through the keybinding table so
	// keybindings.yml can move them.
	app.SetTabCycle(func(dir int, onlyUnread bool) { cycleTab(dir, onlyUnread) })
	app.SetTabPick(focusTabByID)
	app.SetTabClose(closeTabByID)
	app.SetTabs(tabInfos(tabs))
	app.SetVision(func() bool {
		modelMu.Lock()
		defer modelMu.Unlock()
		return modelVision(cfg, live.provName, live.model)
	})

	app.Run() // blocks until Quit
	// #92: the session is ending (quit, or the terminal closing): tell the
	// bus once, with whatever agent was last live.
	agentMu.Lock()
	if curAgent != nil {
		curAgent.EmitSessionShutdown()
	}
	agentMu.Unlock()
	// The memory session boundary (M12 #43/#44): the remote server flushes its
	// queued retains, the local store drains its queue inside a fixed budget.
	// Both are best-effort — the TUI is exiting either way.
	if h := hindsightFrom(lastSettings()); h != nil {
		if err := h.EndSession(); err != nil {
			logx.Debugf("memory: hindsight session end: %v", err)
		}
	}
	if mm := mnemopiFrom(lastSettings()); mm != nil {
		mm.Drain(context.Background())
	}
	return 0, nil
}

// memoryOps builds the /memory command wiring for one backend: the shared
// view/stats/clear trio over the Store seam, plus the backend-specific verbs
// (M12 #43/#44) — the queue-backed store answers queue|sync|enqueue, the
// remote server answers diagnose|enqueue. Each backend leaves the other's
// verbs nil and MemoryOps.Dispatch reports them as unavailable. nil (no
// backend configured) leaves /memory unwired.
func memoryOps(mem memoryBackend) *tui.MemoryOps {
	if mem == nil {
		return nil
	}
	ops := &tui.MemoryOps{
		View: func() string {
			summary, lessons := mem.Paths()
			out := mem.Stats()
			if s := mem.Summary(); s != "" {
				out += "\n\n" + s
			}
			return out + "\n\n  " + summary + "\n  " + lessons
		},
		Stats: mem.Stats,
		Clear: mem.Clear,
	}
	if mm, ok := mem.(*memory.Mnemopi); ok {
		ops.Queue = mm.QueueStats
		ops.Sync = func() (string, error) {
			res, err := mm.Sync(context.Background(), 0)
			if err != nil {
				return "", err
			}
			return mnemopiSyncReport(res), nil
		}
		ops.Enqueue = func(text string) (string, error) {
			if _, err := mm.Enqueue(text, "user"); err != nil {
				return "", err
			}
			// /memory enqueue is explicit: queue the retain and apply it now,
			// inside the same bounded drain the exit path uses.
			res, err := mm.Sync(context.Background(), 0)
			if err != nil {
				return "", err
			}
			return "queued: " + mnemopiSyncReport(res), nil
		}
	}
	if h, ok := mem.(*memory.Hindsight); ok {
		ops.Diagnose = h.Diagnose
		ops.Enqueue = func(string) (string, error) { return h.Enqueue() }
	}
	return ops
}

// tuiSession accumulates the live conversation and persists messages.
// titleFromPrompt derives a session title from the first user prompt: the
// first line, whitespace-collapsed, capped at 40 runes.
func titleFromPrompt(text string) string {
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(text), "\n", 2)[0])
	line = strings.Join(strings.Fields(line), " ")
	runes := []rune(line)
	if len(runes) > 40 {
		return string(runes[:40]) + "…"
	}
	return line
}

// breadcrumbPath prefers the materialized file path and falls back to the
// auto-persist target, so a session that has not written its first
// assistant message yet still owns the pane's --continue breadcrumb.
func breadcrumbPath(s *session.Store) string {
	if p := s.Path(); p != "" {
		return p
	}
	return s.AutoPath()
}

type tuiSession struct {
	store *session.Store
	app   *tui.App
	tabs  *tabset
	// id is the session this hooks instance belongs to. A turn captures
	// it at start so a mid-turn switch cannot re-point its paints.
	id string
	// focused is true while this session owns the App transcript. A parked
	// turn keeps writing to its store and only sets the unread badge.
	focused bool
	// mu guards ttftRequest and focused (tuiHooks writes on OnStart /
	// onTurnEnd, both on different goroutines) — same lock the
	// hooks use, lives on the session because the hooks reference
	// ts, not themselves.
	mu          sync.Mutex
	ttftRequest time.Time
}

func (s *tuiSession) setFocused(on bool) {
	s.mu.Lock()
	s.focused = on
	s.mu.Unlock()
}

// isFocused is true while this session owns the App transcript. A per-turn
// hooks instance carries the session id it started on and asks the tabset;
// the process-wide `ts` still uses the focused flag focusTab toggles.
func (s *tuiSession) isFocused() bool {
	// A turn pinned to a session id asks the tabset: still current?
	if s.tabs != nil && s.id != "" {
		cur := s.tabs.current()
		return cur != nil && cur.id == s.id
	}
	// No tabset (unit tests, single-session harnesses that build a bare
	// tuiSession{app: app}): always paint. The focused flag is only
	// meaningful when a tabset is driving focusTab.
	if s.tabs == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.focused
}

// paint runs fn against the App only while this session is focused; a parked
// turn just raises the unread badge so the top bar can show it.
func (s *tuiSession) paint(fn func()) {
	if s.isFocused() {
		fn()
		return
	}
	if s.tabs != nil && s.id != "" {
		s.tabs.note(s.id)
		// Bump the tab strip so the badge appears without a keystroke.
		s.app.SetTabs(tabInfos(s.tabs))
	}
}

// tuiHooks implements agent.TurnHooks for the TUI.
type tuiHooks struct {
	ts *tuiSession
	// feed (optional) hands the advisor a fresh transcript snapshot after
	// each assistant message — the reviewer steers into the live run.
	feed func()
	// retryCounter tracks consecutive transient stream errors in the
	// current turn, so repeated "stream error — retrying" lines collapse
	// into a single "· stream error — retrying (xN)".
	retryCounter int
	// ttftRequest is set by OnStart; OnMessageEnd computes the
	// turn's ttft from it and writes it via onTurnEnd (nil-safe).
	// OnStart fires on the agent goroutine, OnTurnEnd on the Run
	// goroutine — mu serializes the two writers.
	mu          sync.Mutex
	ttftRequest time.Time
}

// taskChildSink wires a `task` call's children into the transcript: the
// call row is the parent, the child rows hang under it, and the model's
// context is not involved (the parent still sees only the yield).
//
// The agent's child events carry no call id, and two `task` calls can be in
// flight in the same turn (MaxToolWorkers), so the call a child belongs to
// is the newest running `task` row at the moment the child STARTS — which
// is exactly when the spawn is the one that is blocking. Children are
// keyed by their label, which a batch makes unique (childLabel).
type taskChildSink struct {
	app *tui.App
	mu  sync.Mutex
	ids map[string]string // child label -> the `task` call id it belongs to
}

// onEvent is the TaskTool.OnEvent callback: one child moment, straight to
// the transcript.
func (s *taskChildSink) onEvent(ev agent.SubagentEvent) {
	callID := s.callID(ev.Label)
	switch ev.Kind {
	case agent.SubagentStart:
		s.mu.Lock()
		if callID == "" {
			callID = s.app.RunningTaskCallID()
			if callID != "" {
				if s.ids == nil {
					s.ids = map[string]string{}
				}
				s.ids[ev.Label] = callID
			}
		}
		s.mu.Unlock()
		s.app.AddTaskChild(callID, ev.Label, ev.Agent, ev.Model)
	case agent.SubagentTool:
		s.app.UpdateTaskChild(callID, ev.Label, ev.Tool, string(ev.Args), ev.Status)
	case agent.SubagentEnd:
		s.app.FinishTaskChild(callID, ev.Label, ev.Status, ev.Dur)
	}
}

// callID is the `task` call this child was started under ("" = unknown, and
// the transcript then keeps the event off the transcript rather than
// guessing which call it belonged to).
func (s *taskChildSink) callID(label string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ids[label]
}

func (h *tuiHooks) OnStart(req ai.StreamRequest) {
	h.ts.mu.Lock()
	h.ts.ttftRequest = time.Now()
	h.ts.mu.Unlock()
}

func (h *tuiHooks) OnEvent(ev ai.Event) {
	switch ev.Type {
	case ai.EventStart:
		// One message, one decode window: a turn that died into the retry
		// ladder never reached EventDone, so its window would otherwise be
		// inherited as the next turn's denominator (a t/s reading several
		// times too low, for the rest of the session).
		h.ts.paint(func() { h.ts.app.BeginMessage() })
		// One EventStart is one provider request, which is the "step"
		// dsh's TimePill counts beside its turns.
		h.ts.paint(func() { h.ts.app.AddStep() })
	case ai.EventTextStart:
		h.ts.paint(func() { h.ts.app.BeginAssistant() })
	case ai.EventTextDelta:
		h.ts.paint(func() { h.ts.app.AppendAssistant(ev.Delta) })
	case ai.EventTextEnd:
		h.ts.paint(func() { h.ts.app.EndAssistant() })
	case ai.EventThinkingStart:
		h.ts.paint(func() { h.ts.app.BeginThinking() })
	case ai.EventThinkingDelta:
		h.ts.paint(func() { h.ts.app.AppendThinking(ev.Delta) })
	case ai.EventThinkingEnd:
		h.ts.paint(func() { h.ts.app.EndThinking() })
	case ai.EventToolcallDelta:
		// output_tokens counts tool-argument JSON, so the decode window has
		// to span it — the numerator and the denominator must measure the
		// same message.
		h.ts.paint(func() { h.ts.app.NoteToolDelta() })
	case ai.EventDone:
		h.ts.paint(func() { h.ts.app.EndAssistant() })
		if ev.Usage != nil {
			// The ctx number is the whole request — cached input included
			// (Claude Code's used_tokens), which is Usage.TotalTokens, not
			// the sum the counters accumulate. The counters need the
			// split: Input is fresh-only, Output already contains the
			// reasoning, so without CacheRead and ReasoningTokens the row
			// claimed a 479-token prompt for a 65k one and 1770 tokens of
			// answer for 506.
			h.ts.paint(func() {
				h.ts.app.AddUsage(ev.Usage.Input, ev.Usage.Output,
					ev.Usage.CacheRead, ev.Usage.ReasoningTokens, ev.Usage.TotalTokens)
			})
			// The cache WRITE side of the same prompt: a separate bucket
			// on the wire, and one the token pill's total and /usage
			// both had nowhere to put.
			if ev.Usage.CacheWrite > 0 {
				h.ts.paint(func() { h.ts.app.AddCacheWrite(ev.Usage.CacheWrite) })
			}
			if ev.Usage.Cost != nil {
				h.ts.paint(func() { h.ts.app.AddCost(ev.Usage.Cost.Total) })
			}
			// The provider's own wall time and time-to-first-token, for
			// /usage's LLM-time and average-TTFT lines. Carried on the
			// message, not the usage: a turn that reported no usage still
			// spent the time it took.
			if ev.Message != nil {
				h.ts.paint(func() {
					h.ts.app.AddLLMTime(
						time.Duration(ev.Message.DurationMS)*time.Millisecond,
						ev.Message.TTFTMS)
				})
			}
		}
	case ai.EventError:
		// An unbounded-wait round (retry.infinite / retry.retryAllErrors)
		// says "still waiting" once per round — show it, not the blip notice.
		// The retain-and-continue round is the same shape: its partials are
		// already on screen, so without its own line the only thing the turn
		// shows is the collapsed blip below.
		var down *agent.AllTargetsDownError
		if errors.As(ev.Err, &down) {
			h.ts.paint(func() { h.ts.app.AddSystemBlock(ev.Err.Error()) })
			break
		}
		var cont *agent.ContinuationRetryError
		if errors.As(ev.Err, &cont) {
			h.ts.paint(func() { h.ts.app.AddSystemBlock("· " + ev.Err.Error()) })
			break
		}
		var empty *agent.EmptyTurnRetryError
		if errors.As(ev.Err, &empty) {
			h.ts.paint(func() { h.ts.app.AddSystemBlock(ev.Err.Error()) })
			break
		}
		// A transient blip is being retried by the recovery ladder: the
		// wire error would flash once per attempt, so it collapses to a
		// notice. Repeated transient errors in one turn count up:
		// "· stream error — retrying", then "· stream error — retrying (x2)",
		// "(x3)" — hard errors still print verbatim — the turn ends on them.
		if ai.Classify(ev.Err) == ai.ClassTransient {
			h.retryCounter++
			if h.retryCounter == 1 {
				h.ts.paint(func() { h.ts.app.AddSystemBlock("· stream error — retrying") })
			} else {
				h.ts.paint(func() { h.ts.app.AddSystemBlock(fmt.Sprintf("· stream error — retrying (x%d)", h.retryCounter)) })
			}
			break
		}
		h.ts.paint(func() { h.ts.app.AddSystemBlock("stream error: " + ev.Err.Error()) })
	}
}

// workOf is the work a rebuilt history already banked: the provider-request
// spans the assistant messages carry PLUS the tool spans the toolResult
// messages carry. A resumed, forked or rewound session starts with that
// number on the HUD's time segment instead of zero, so the active-work total
// survives restarts — and a tree navigation shows only the path that is on
// screen.
//
// Tool spans belong here because the live timer counts them: markRun banks
// the whole run span, thinking and streaming and tools alike. Measuring only
// the provider requests made a resumed session read at roughly half what it
// showed before it closed — 7,728s against 15,240s on one real session file,
// a 49% under-report — and /usage's "tool time · N% of active time" line was
// comparing two different denominators. Messages written before durations
// were recorded, or imported without one, simply add nothing.
//
// ponytail: two ceilings, both stated rather than faked. (1) A bang-mode
// (!bash) call never reaches the store, so its span is lost across a resume
// even though the live timer counted it — upgrading means persisting those
// calls as real toolResult entries instead of transcript-only rows. (2) Same-
// batch tool calls run CONCURRENTLY (MaxToolWorkers = 6), so summing their
// spans can exceed the wall time they actually took; the live timer measures
// the run's wall clock and cannot over-count. The alternative — persisting
// run spans rather than message spans — needs the run to be an entry of its
// own, which the store has no type for.
func workOf(msgs []ai.Message) time.Duration {
	var work time.Duration
	for _, m := range msgs {
		switch m.Role {
		case ai.RoleAssistant, ai.RoleToolResult:
			if m.DurationMS > 0 {
				work += time.Duration(m.DurationMS) * time.Millisecond
			}
		}
	}
	return work
}

// ttftOf sums a rebuilt history's per-turn time-to-first-token and counts the
// turns that carried one, so a resumed session's average TTFT is the average
// it really had rather than a zero. Average is taken at render time (sum /
// count), never stored pre-divided.
func ttftOf(msgs []ai.Message) (sum int64, count int64) {
	for _, m := range msgs {
		if m.Role == ai.RoleAssistant && m.TTFTMS > 0 {
			sum += m.TTFTMS
			count++
		}
	}
	return sum, count
}

// countsOf counts a rebuilt history's turns and steps for the status pill:
// a turn is one user prompt the person (or the harness, for a goal or a
// continuation) actually asked for, and a step is one provider request the
// session sent. harnessUserAttribution is the same filter replayTranscript
// used to decide what becomes a ❯ row, so the two agree on what a "turn"
// is; assistant messages are the steps, tool results the answers inside one.
func countsOf(msgs []ai.Message) (turns, steps int) {
	for _, m := range msgs {
		switch m.Role {
		case ai.RoleUser:
			if !harnessUserAttribution(m) && m.Text() != "" {
				turns++
			}
		case ai.RoleAssistant:
			steps++
		}
	}
	return turns, steps
}

// usageOf sums a rebuilt history's token buckets and spend the same way the
// live path banks them (tuiHooks.OnEvent → AddUsage/AddCost), so a resumed
// session's token pill, its cache-hit rate and /usage's cost line read the
// same numbers the session had before it was closed. Every figure comes off
// the persisted per-message Usage, which is the only record of the split —
// an older message written before a bucket was tracked simply adds zero.
func usageOf(msgs []ai.Message) (in, out, cache, think, cacheWrite int64, cost float64) {
	for _, m := range msgs {
		if m.Role != ai.RoleAssistant || m.Usage == nil {
			continue
		}
		u := m.Usage
		in += u.Input
		out += u.Output
		cache += u.CacheRead
		think += u.ReasoningTokens
		cacheWrite += u.CacheWrite
		if u.Cost != nil {
			cost += u.Cost.Total
		}
	}
	return in, out, cache, think, cacheWrite, cost
}

// replaySession is the one replay path every adoption shares: a startup
// --continue/--resume, a tab focus, a tree navigation or a branch. It draws
// the transcript and re-bases EVERY session metric off the rebuilt messages,
// so an adopted session shows the numbers it had before it was closed:
// token buckets and spend, the work timer, LLM time, average TTFT, and the
// turn/step counts.
//
// App.Reset is the per-session boundary that clears them; this is what puts
// them back, and having one function is what keeps a new metric from being
// wired at three sites and missed at the fourth.
func replaySession(app *tui.App, msgs []ai.Message) {
	replayTranscript(app, msgs)
	work := workOf(msgs)
	app.SetContextReplay(agent.ContextTokens(msgs))
	app.SetWork(work)
	ttftSum, ttftCount := ttftOf(msgs)
	app.SetLLMTime(work, ttftSum, ttftCount)
	in, out, cache, think, cacheWrite, cost := usageOf(msgs)
	app.SetSessionUsage(in, out, cache, think, cacheWrite, cost)
	turns, steps := countsOf(msgs)
	app.SetSessionCounts(turns, steps)
}

// shortSessionID renders the first 8 chars of a session id (matches the TUI
// status line convention).
func shortSessionID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// bangToolName names the transcript rows shell mode writes. A name of its own
// is load-bearing: a bang run must never match — or close — the box of an
// agent bash call that is in flight, and the name is what says so.
const bangToolName = "!bash"

// newBangRunner builds the executor behind composer shell mode (#163): it
// runs one command through the bash tool in the session cwd and writes the
// result as a tool box, with no model call anywhere on the path.
//
// Runs are serialized. Each run gets its own call id, so the result pairs
// with the row that opened it even if a later run starts first; the lock is
// what makes a burst of typed commands execute in the order they were sent.
//
// ponytail: display-only, i.e. the transcript is the sole sink — the output
// is not persisted to the session and never enters the model's context, which
// is exactly what "costs no tokens" means. Upgrading it means appending a
// session.CustomEntry here and replaying it as a user-role note next turn.
func newBangRunner(app *tui.App, cwd string, ctx context.Context) func(string) error {
	var mu sync.Mutex
	var seq atomic.Int64
	return func(command string) error {
		args, err := json.Marshal(map[string]string{"command": command})
		if err != nil {
			return err
		}
		goGuarded(func() {
			mu.Lock()
			defer mu.Unlock()
			started := time.Now()
			id := fmt.Sprintf("bang-%d", seq.Add(1))
			app.AddToolBlock(id, bangToolName, string(args))
			res, execErr := tool.NewBashTool(cwd).Execute(ctx, args)
			if execErr != nil {
				// The transcript can only show a Result; a hard error (a bad
				// cwd, a refused spawn) becomes an error box rather than a
				// row that stays "running" forever.
				res = tool.Result{Text: execErr.Error(), IsError: true}
			}
			out := tool.OutcomeOf(res.Details)
			elapsed := time.Since(started)
			app.FinishTool(id, bangToolName, res.IsError, res.Text, tui.ToolOutcome{
				Dur:       elapsed.Round(time.Millisecond).String(),
				Elapsed:   elapsed,
				Exit:      out.Exit,
				HasExit:   out.HasExit,
				Truncated: out.Truncated,
				Diff:      out.Diff,
			})
		})
		return nil
	}
}

// OnToolStart hands the transcript the call's raw arguments: the renderer
// reads the naming field out of them (omp's `name · detail` row), so nothing
// here pre-flattens the JSON into a preview the terminal then has to unpick.
func (h *tuiHooks) OnToolStart(call ai.ToolCallBlock) {
	h.ts.paint(func() { h.ts.app.BeginActiveCommand(call.Name) })
	h.ts.paint(func() { h.ts.app.AddToolBlock(call.ID, call.Name, string(call.Arguments)) })
}

// OnToolEnd passes the outcome facts the status footer shows — exit code,
// dropped output, and the change the tool made to a file — flattened from the
// tool's own structured details.
func (h *tuiHooks) OnToolEnd(call ai.ToolCallBlock, res tool.Result, dur time.Duration) {
	out := tool.OutcomeOf(res.Details)
	h.ts.paint(func() {
		h.ts.app.FinishTool(call.ID, call.Name, res.IsError, res.Text, tui.ToolOutcome{
			Dur:       dur.Round(time.Millisecond).String(),
			Elapsed:   dur,
			Exit:      out.Exit,
			HasExit:   out.HasExit,
			Truncated: out.Truncated,
			Diff:      out.Diff,
		})
	})
	h.ts.paint(func() { h.ts.app.EndActiveCommand() })
	// The dock's Files section is read from the transcript's diff blocks, and the
	// task list from the todo tool's state: both move here, and nowhere else in a
	// quiet session. One bump per finished call, no per-frame source read.
	h.ts.paint(func() { h.ts.app.DockBump() })
}

// OnMessageEnd persists the assistant message (persistence on message_end).
func (h *tuiHooks) OnMessageEnd(msg *ai.Message) {
	if msg == nil || msg.Role != ai.RoleAssistant {
		return
	}
	if err := h.ts.store.Append(&session.MessageEntry{Message: *msg}); err != nil {
		logx.Errorf("persist assistant message: %v", err)
	}
	if h.feed != nil {
		h.feed()
	}
	// A message's end is the last word on what it said about the plan, and the
	// transcript gained a block the panel's height budget has to account for.
	h.ts.paint(func() { h.ts.app.DockBump() })
	if msg.TTFTMS > 0 {
		h.ts.mu.Lock()
		req := h.ts.ttftRequest
		h.ts.mu.Unlock()
		if req.IsZero() {
			// OnStart hasn't fired for this turn (idle
			// resume, or the clock went backwards) — leave
			// the turn's ttft unwritten.
		} else {
			h.onTurnEnd(msg.TTFTMS)
		}
	}
}

// OnMessageEnd persists the assistant message and reports its
// ttft to OnTurnEnd (only when OnStart's clock is newer — i.e.
// this turn, not the session start). Called on the agent goroutine;
// OnTurnEnd fires on the Run goroutine, so the lock serializes
// the two writers and neither clobbers a real value.
func (h *tuiHooks) onTurnEnd(ttft int64) {
	h.ts.mu.Lock()
	defer h.ts.mu.Unlock()
	h.ts.ttftRequest = time.Time{} // consumed: a real OnTurnEnd is coming
	h.ts.paint(func() { h.ts.app.SetTTFT(ttft) })
}

func (h *tuiHooks) OnToolResultMessage(msg *ai.Message) {
	if err := h.ts.store.Append(&session.MessageEntry{Message: *msg}); err != nil {
		logx.Errorf("persist toolResult: %v", err)
	}
}

func (h *tuiHooks) OnTurnEnd(reason ai.StopReason, err error) {}

// OnContinuation surfaces the injected cut-off recovery turn live: a
// harness event, not a fake user prompt (#283).
func (h *tuiHooks) OnContinuation(text string) {
	h.ts.paint(func() {
		h.ts.app.AddSystemBlock("· provider cut off mid-message — partial retained, continuation injected")
	})
}

// OnEmptyTurn names the stall instead of letting the run end on a blank turn:
// the nudge prompt goes out as a hidden turn, so without this the transcript
// just stops (#331).
func (h *tuiHooks) OnEmptyTurn(text string) {
	h.ts.paint(func() { h.ts.app.AddSystemBlock("· the model answered with nothing — asked again") })
}

func (h *tuiHooks) OnCompaction(tokensBefore int64) {
	h.ts.paint(func() { h.ts.app.AddSystemBlock(fmt.Sprintf("· context compacted (~%d tokens)", tokensBefore)) })
}

// OnGoalUpdated implements agent.GoalHook: goal transitions land in the
// transcript.
func (h *tuiHooks) OnGoalUpdated(g agent.Goal) {
	h.ts.paint(func() { h.ts.app.AddSystemBlock("· goal " + g.Status + " — " + g.Objective) })
}

// config2Load is a tiny alias so tui.go shares print.go's loader.
func config2Load() (*config.Config, error) { return config.LoadModelsLayered() }

// setCursorColor writes the OSC 12 cursor-color sequence to the terminal
// before tcell takes over raw mode.
func setCursorColor(c theme.Color) {
	fmt.Fprintf(os.Stdout, "\033]12;#%02x%02x%02x\033\\", c.R, c.G, c.B)
}

// setCursorReset restores the terminal's default cursor color (OSC 112).
func setCursorReset() {
	fmt.Fprint(os.Stdout, "\033]112\033\\")
}

// replayTranscript replays built context as read-only transcript blocks.
// Thinking blocks ride along (BeginThinking is a no-op while showThinking
// is off), so resumed, forked, and branched sessions show past reasoning
// the same way fresh turns do instead of silently dropping it.
// harnessUserAttribution reports whether a user-role message is harness text
// the person never typed. Those never become ❯ rows: replaying one would
// invent a turn that never happened.
//
// This predicate is deliberately shared with userEntryID, which maps a ❯ row's
// ordinal back to its store entry. The two must agree exactly — a row counted
// differently on the way in and on the way out would point the message menu's
// revert at the wrong message, silently.
func harnessUserAttribution(m ai.Message) bool {
	switch m.Attribution {
	case agent.ContinuationAttribution,
		agent.GoalContinuationAttribution,
		agent.PromptContinuationAttribution,
		agent.TurnBudgetAttribution,
		agent.EmptyTurnAttribution,
		agent.HubNoticeAttribution:
		return true
	}
	return false
}

func replayTranscript(app *tui.App, msgs []ai.Message) {
	for _, m := range msgs {
		switch m.Role {
		case ai.RoleUser:
			// Two of the harness turns are narrated as system events rather
			// than dropped: a provider cut-off recovery marks the episode
			// where the stream died (#283), and the turn-budget wrap-up is
			// the only thing on screen explaining why the transcript stops
			// mid-task.
			if m.Attribution == agent.ContinuationAttribution {
				app.AddSystemBlock("· recovered provider cut-off — continuation injected")
				continue
			}
			if m.Attribution == agent.TurnBudgetAttribution {
				app.AddSystemBlock("· turn wrapped up — the session keeps going instead of asking you to say \"continue\"")
				continue
			}
			// The rest (goal continuations, prompt continuations, the
			// empty-turn nudge) are text nobody typed: replaying one as a ❯
			// block would invent a turn that never happened.
			if harnessUserAttribution(m) {
				continue
			}
			if txt := m.Text(); txt != "" {
				app.AddUserBlock(txt)
			}
		case ai.RoleAssistant:
			var pending strings.Builder
			for _, blk := range m.Content {
				switch b := blk.(type) {
				case ai.ThinkingBlock:
					if pending.Len() > 0 {
						app.AddAssistantBlock(pending.String())
						pending.Reset()
					}
					app.BeginThinking()
					app.AppendThinking(b.Thinking)
					app.EndThinking()
				case ai.TextBlock:
					pending.WriteString(b.Text)
				}
			}
			if pending.Len() > 0 {
				app.AddAssistantBlock(pending.String())
			}
		case ai.RoleToolResult:
			// A resumed session must show the tool calls that produced the
			// files it changed: the dock's FILES section is read from the
			// finished result blocks (App.dockChanges), so dropping these on
			// replay left the panel half empty on every reopen (#291).
			out := tool.OutcomeOf(m.Details)
			dur := ""
			var elapsed time.Duration
			if m.DurationMS > 0 {
				elapsed = time.Duration(m.DurationMS) * time.Millisecond
				dur = elapsed.Round(time.Millisecond).String()
			}
			app.AddToolBlock(m.ToolCallID, m.ToolName, "")
			app.FinishTool(m.ToolCallID, m.ToolName, m.IsError, m.Text(), tui.ToolOutcome{
				Dur:       dur,
				Elapsed:   elapsed,
				Exit:      out.Exit,
				HasExit:   out.HasExit,
				Truncated: out.Truncated,
				Diff:      out.Diff,
			})
		}
	}
}

// modelPickerViews builds the /model selector's tabs: "All models"
// (sectioned by provider) and one view per provider — the same shape omp's
// /model shows.
func modelPickerViews(cfg *config.Config, s *config.Settings, current string, app *tui.App) []tui.PickerView {
	items := modelPickerItems(cfg, s, current)
	_ = app
	var views []tui.PickerView
	if len(items) > 0 {
		all := make([]tui.PickerItem, len(items))
		copy(all, items)
		views = append(views, tui.PickerView{Name: "All models", Items: all, Action: "use"})
	}
	// One provider needs no per-provider tab: it would be byte-identical to
	// "All models" (live: /model showed onegw twice with a single provider).
	var perProvider []tui.PickerView
	for _, name := range providerKeys(cfg) {
		pc := cfg.Providers[name]
		if pc == nil || (s != nil && s.ProviderDisabled(name)) {
			continue
		}
		var mine []tui.PickerItem
		for _, it := range items {
			if strings.HasPrefix(it.Value, name+"/") {
				mine = append(mine, it)
			}
		}
		if len(mine) > 0 {
			perProvider = append(perProvider, tui.PickerView{Name: name, Items: mine, Action: "use"})
		}
	}
	if len(perProvider) > 1 {
		views = append(views, perProvider...)
	}
	return views
}

// modelPickerItems is the flat model catalog behind the selector: every
// provider's pinned models merged with its discovery results (cached once
// per provider per process by providerModels), sectioned by provider so the
// "All models" tab reads as a table.
func modelPickerItems(cfg *config.Config, s *config.Settings, current string) []tui.PickerItem {
	if cfg == nil {
		return nil
	}
	var out []tui.PickerItem
	seen := map[string]bool{}
	for _, name := range providerKeys(cfg) {
		pc := cfg.Providers[name]
		if pc == nil || (s != nil && s.ProviderDisabled(name)) {
			continue
		}
		for _, m := range providerModels(name, pc) {
			if m.ID == "" {
				continue
			}
			ref := name + "/" + m.ID
			if seen[ref] {
				continue
			}
			seen[ref] = true
			out = append(out, tui.PickerItem{
				Label:   ref,
				Detail:  modelDetail(m),
				Value:   ref,
				Section: name,
				Current: sameModelRef(ref, current),
			})
		}
	}
	return out
}

// modelDetail renders a model row's second column: its display name plus
// the context window when the config declares one.
func modelDetail(m config.ModelConfig) string {
	parts := []string{}
	if m.Name != "" && m.Name != m.ID {
		parts = append(parts, m.Name)
	}
	if m.ContextWindow > 0 {
		parts = append(parts, humanCtx(m.ContextWindow))
	}
	if m.Reasoning {
		parts = append(parts, "reasoning")
	}
	return strings.Join(parts, " · ")
}

func humanCtx(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%gM ctx", float64(n)/1_000_000)
	case n >= 1000:
		return fmt.Sprintf("%dk ctx", n/1000)
	default:
		return fmt.Sprintf("%d ctx", n)
	}
}

// sameModelRef compares two model refs ignoring an ":effort" suffix, so
// "onegw/dev:high" and the "onegw/dev" it resolves to mark the same session.
func sameModelRef(a, b string) bool {
	strip := func(s string) string {
		if i := strings.LastIndex(s, ":"); i >= 0 {
			return s[:i]
		}
		return s
	}
	return a != "" && strip(a) == strip(b)
}

// recentResumeOptions lists this folder's resumable sessions for the
// no-argument /resume picker: subagent children and the live session are
// filtered out (nothing to resume onto), newest first. No row cap — the
// picker windows and scrolls its own list, so a cap here was the list's
// real length (12 was the whole of it).
func recentResumeOptions(cwd, currentID string) []tui.ResumeOption {
	metas, err := session.List(sessionDataDir())
	if err != nil {
		return nil
	}
	out := make([]tui.ResumeOption, 0, len(metas))
	for _, m := range metas {
		if m.CWD != cwd || m.TitleSource == session.TitleSourceSubagent || m.ID == currentID {
			continue
		}
		// The status closes the detail so the row says what happened to
		// the session (#107) before the user resumes it.
		detail := m.ID[:8] + " · " + m.ModTime.Format("Jan 02 15:04") + " · " + m.CWD
		if m.Status != "" {
			detail += " · " + string(m.Status)
		}
		out = append(out, tui.ResumeOption{ID: m.ID, Title: m.Title, Detail: detail})
	}
	return out
}

// resumePickerItems lists resumable sessions as picker rows across ALL
// projects (session.List already scans every bucket): subagent children
// are filtered out (same rules as the text listing); session.List is
// newest-first. Rows carry InCwd so the TUI can window the
// current-folder scope without a second scan, and Pinned from the
// session-pins.json sidecar. Capped at 50 — enough for Tab-all-projects
// browsing while the picker windows to 8 visible rows.
func resumePickerItems(cwd string) []tui.SessionPickerItem {
	metas, err := session.List(sessionDataDir())
	if err != nil {
		return nil
	}
	pins := loadSessionPins()
	var out []tui.SessionPickerItem
	for _, m := range metas {
		if m.TitleSource == "subagent" || len(m.ID) < 8 {
			continue
		}
		out = append(out, tui.SessionPickerItem{
			ID:     m.ID[:8],
			Title:  m.Title,
			Mtime:  m.ModTime.Format("Jan 02 15:04"),
			Size:   humanSize(m.SizeBytes),
			Pinned: pins[m.ID[:8]],
			InCwd:  m.CWD == cwd,
			Status: string(m.Status),
			CWD:    m.CWD,
		})
		if len(out) >= 50 {
			break
		}
	}
	return out
}

// searchPickerItems ranks sessions for the picker query: every
// whitespace-separated token must match the id, the title, or the
// lifecycle status (the TUI re-applies scope itself), and
// sessions whose JSONL body contains the tokens count as prompt matches —
// matches rank by match count: id/title/status hits count double, body
// hits count occurrences. The body scan is capped (64 KiB per file, 50
// files) — the picker runs per keystroke.
func searchPickerItems(cwd, query string) []tui.SessionPickerItem {
	items := resumePickerItems(cwd)
	tokens := strings.Fields(strings.ToLower(query))
	if len(tokens) == 0 {
		return items
	}
	type ranked struct {
		item  tui.SessionPickerItem
		score int
	}
	var out []ranked
	for _, it := range items {
		hay := strings.ToLower(it.ID + " " + it.Title + " " + it.Status)
		score, ok := 0, true
		var body []string // tokens the id/title did not satisfy
		for _, t := range tokens {
			if strings.Contains(hay, t) {
				score += 2 // id/title match counts double
			} else {
				body = append(body, t)
			}
		}
		if len(body) > 0 {
			if n := countSessionFileTokens(it.ID, body); n > 0 {
				score += n // prompt matches rank by occurrence count
			} else {
				ok = false
			}
		}
		if ok {
			out = append(out, ranked{item: it, score: score})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	res := make([]tui.SessionPickerItem, 0, len(out))
	for _, r := range out {
		res = append(res, r.item)
	}
	return res
}

// countSessionFileTokens counts query-token occurrences in the first
// 64 KiB of a candidate session JSONL (case-insensitive); 0 when ANY
// token is absent. Files are named <timestamp>_<id>.jsonl, so the short
// id locates them under sessions/<bucket>/.
func countSessionFileTokens(shortID string, tokens []string) int {
	if len(shortID) < 8 {
		return 0
	}
	paths := make([]string, 0, 4)
	root := session.SessionsRoot(sessionDataDir())
	buckets, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	for _, b := range buckets {
		if !b.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, b.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if !f.IsDir() && strings.Contains(f.Name(), shortID) {
				paths = append(paths, filepath.Join(root, b.Name(), f.Name()))
			}
		}
	}
	if len(paths) == 0 {
		return 0
	}
	f, err := os.Open(paths[0])
	if err != nil {
		return 0
	}
	defer f.Close()
	buf := make([]byte, 64<<10)
	n, _ := f.Read(buf)
	if n == 0 {
		return 0
	}
	hay := strings.ToLower(string(buf[:n]))
	total := 0
	for _, t := range tokens {
		c := strings.Count(hay, t)
		if c == 0 {
			return 0
		}
		total += c
	}
	return total
}

// humanSize renders bytes the way the token counter renders numbers.
func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}

// treeRewindTarget maps the selected tree entry to where the leaf lands and
// what returns to the composer — omp's session.navigateTree target rule: a
// user message rewinds to its PARENT ("" for the very first message: a
// fresh root) and its prompt comes back as the draft to edit and resend;
// every other entry becomes the leaf itself with no draft. A user row the
// harness wrote (goal continuation, provider cut-off, turn-budget wrap-up)
// still rewinds, but offers no draft: the user never typed that text.
func treeRewindTarget(e session.Entry) (target, draft string) {
	env := e.Envelope()
	if msg, ok := e.(*session.MessageEntry); ok && msg.Message.Role == ai.RoleUser {
		// Harness turns — goal continuation, provider cut-off, the turn-budget
		// wrap-up — are messages the user never typed. The row still rewinds
		// to the parent, but its text must not come back as a draft to resend
		// (the same rule the stats counter and the replay use, #283).
		if a := msg.Message.Attribution; a != "" && a != "user" {
			return env.ParentID, ""
		}
		return env.ParentID, msg.Message.Text()
	}
	return env.ID, ""
}

// treeEntries snapshots the session entry graph as tree-selector rows: file
// order, active = current leaf. No depth — the selector paints rows flush left
// and tags each with its author, so the parent walk that used to compute an
// indentation level (O(n·depth) per snapshot) is gone with the gutter.
func treeEntries(store *session.Store) []tui.TreeEntry {
	entries := store.Entries()
	leaf := store.LeafID()
	out := make([]tui.TreeEntry, 0, len(entries))
	for _, e := range entries {
		env := e.Envelope()
		te := tui.TreeEntry{ID: env.ID, Type: env.Type, Active: env.ID == leaf}
		switch t := e.(type) {
		case *session.MessageEntry:
			te.Role = string(t.Message.Role)
			// MessageLabel, not Text(): most rows of a real session are
			// tool-call-only assistant turns, which carry no text block at all,
			// so Text() returned "" and the panel painted those ids over blank
			// lines — the "empty lines" at the end of the history.
			te.Summary = clipSummary(ai.MessageLabel(&t.Message), 60)
		case *session.CompactionEntry:
			te.Summary = "(compaction)"
		case *session.BranchSummaryEntry:
			te.Summary = "(branch summary)"
		case *session.ModelChangeEntry:
			te.Summary = t.Model
		case *session.CustomEntry:
			te.Summary = "(" + t.CustomType + ")"
		}
		out = append(out, te)
	}
	return out
}

// trajectoryRows snapshots the session as ledger rows, in file order: the
// ledger is the session's own record sequence, so /tree's branch layout is
// deliberately not reproduced here. Rows are numbered from 1 as they are read.
func trajectoryRows(store *session.Store) []tui.TrajectoryRecord {
	entries := store.Entries()
	out := make([]tui.TrajectoryRecord, 0, len(entries))
	for _, e := range entries {
		out = append(out, trajectoryRow(e, len(out)+1))
	}
	return out
}

// trajectoryRow is one ledger row: the entry's own label, its full body for the
// inspector, and the machine facts that ride beside it. A row is built once per
// open, never cached — the ledger reads the store when it opens and the panel
// only ever reads the count.
func trajectoryRow(e session.Entry, i int) tui.TrajectoryRecord {
	env := e.Envelope()
	rec := tui.TrajectoryRecord{Index: i, Kind: env.Type}
	// A harness-written user turn (goal continuation, provider cut-off, budget
	// wrap-up) is not a turn the human took, so it does not open one.
	turn := false
	switch t := e.(type) {
	case *session.MessageEntry:
		m := &t.Message
		rec.Kind = string(m.Role)
		if m.Role == ai.RoleToolResult {
			rec.Kind = "tool"
		}
		rec.Text = clipSummary(ai.MessageLabel(m), 80)
		rec.Detail = trajectoryDetail(m)
		rec.Meta = trajectoryMeta(m)
		if m.Role == ai.RoleUser {
			turn = m.Attribution == "" || m.Attribution == "user"
		}
	case *session.CompactionEntry:
		rec.Kind = "compacted"
		rec.Text = clipSummary(ai.MessageLabel(&t.Summary), 80)
		rec.Detail = trajectoryDetail(&t.Summary)
		if t.TokensBefore > 0 {
			rec.Meta = fmt.Sprintf("from %s tokens", tui.HumanTokens(t.TokensBefore))
		}
		if t.Method != "" {
			rec.Meta = strings.TrimSpace(rec.Meta + " · " + t.Method)
		}
	case *session.BranchSummaryEntry:
		rec.Kind = "branch"
		rec.Text = clipSummary(ai.MessageLabel(&t.Summary), 80)
		rec.Detail = trajectoryDetail(&t.Summary)
	case *session.ModelChangeEntry:
		rec.Kind = "model"
		rec.Text, rec.Detail = t.Model, t.Model
	case *session.ResetBoundaryEntry:
		rec.Kind, rec.Text = "reset", "(context cut here)"
	case *session.CustomEntry:
		rec.Kind, rec.Text = "custom", "("+t.CustomType+")"
		if len(t.Data) > 0 {
			if b, err := json.MarshalIndent(t.Data, "", "  "); err == nil {
				rec.Detail = string(b)
			}
		}
	}
	rec.Turn = turn
	return rec
}

// trajectoryDetail is the inspector body: full prompt text, tool-call payloads
// (pretty JSON), tool-result body, and streamed reasoning. The row preview is
// one line; this pane is where the full record is actually read.
func trajectoryDetail(m *ai.Message) string {
	parts := make([]string, 0, 4)
	if txt := strings.TrimRight(m.Text(), "\n"); strings.TrimSpace(txt) != "" {
		parts = append(parts, txt)
	}
	if calls := m.ToolCalls(); len(calls) > 0 {
		parts = append(parts, trajectoryToolPayloads(calls))
	}
	if think := trajectoryThinking(m); think != "" {
		parts = append(parts, "reasoning:\n"+think)
	}
	return strings.Join(parts, "\n\n")
}

// trajectoryToolPayloads pretty-prints every tool call the assistant issued so
// the inspector shows full args (opencode/dsh payload tab), not just the name.
func trajectoryToolPayloads(calls []ai.ToolCallBlock) string {
	var b strings.Builder
	b.WriteString("tool calls:")
	for _, c := range calls {
		b.WriteString("\n")
		b.WriteString(c.Name)
		args := c.Arguments
		if len(args) == 0 && c.PartialArgs != "" {
			args = json.RawMessage(c.PartialArgs)
		}
		if len(args) == 0 {
			b.WriteString("()")
			continue
		}
		var pretty bytes.Buffer
		if json.Indent(&pretty, args, "  ", "  ") == nil {
			b.WriteString("(\n  ")
			b.Write(pretty.Bytes())
			b.WriteString("\n)")
		} else {
			b.WriteString("(")
			b.Write(args)
			b.WriteString(")")
		}
	}
	return b.String()
}

// trajectoryThinking joins the message's reasoning blocks for the inspector;
// they never reach the preview because MessageLabel answers with text first.
func trajectoryThinking(m *ai.Message) string {
	var b strings.Builder
	for _, blk := range m.Content {
		t, ok := blk.(ai.ThinkingBlock)
		if !ok || strings.TrimSpace(t.Thinking) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(strings.TrimRight(t.Thinking, "\n"))
	}
	return b.String()
}

// trajectoryMeta is the machine-fact line the inspector (and ledger row tail)
// show: token split like opencode/dsh (new / cache / out / think / total),
// cost, wall duration, TTFT, and tool exit. Empty when the entry carries none.
func trajectoryMeta(m *ai.Message) string {
	var parts []string
	if u := m.Usage; u != nil {
		if u.Input > 0 {
			parts = append(parts, "↑"+tui.HumanTokens(u.Input)+" new")
		}
		if u.CacheRead > 0 {
			parts = append(parts, "⇢"+tui.HumanTokens(u.CacheRead)+" cache")
		}
		if u.CacheWrite > 0 {
			parts = append(parts, "⇢"+tui.HumanTokens(u.CacheWrite)+" cache+")
		}
		if u.Output > 0 {
			parts = append(parts, "↓"+tui.HumanTokens(u.Output))
		}
		if u.ReasoningTokens > 0 {
			parts = append(parts, "think "+tui.HumanTokens(u.ReasoningTokens))
		}
		if u.TotalTokens > 0 {
			parts = append(parts, "total "+tui.HumanTokens(u.TotalTokens))
		}
		if u.Cost != nil && u.Cost.Total > 0 {
			parts = append(parts, fmt.Sprintf("$%.4f", u.Cost.Total))
		}
	}
	durMS := m.DurationMS
	var toolOut tool.Outcome
	if m.Role == ai.RoleToolResult {
		toolOut = tool.OutcomeOf(m.Details)
		// Older tool results only carried wall time inside Details (bash
		// durationMs); prefer the message field when the loop stamped it.
		if durMS <= 0 {
			durMS = toolOut.DurationMS
		}
	}
	if durMS > 0 {
		parts = append(parts, (time.Duration(durMS) * time.Millisecond).Round(time.Millisecond).String())
	}
	if m.TTFTMS > 0 {
		parts = append(parts, "ttft "+(time.Duration(m.TTFTMS)*time.Millisecond).Round(time.Millisecond).String())
	}
	if m.Role == ai.RoleToolResult {
		if toolOut.HasExit {
			parts = append(parts, fmt.Sprintf("exit %d", toolOut.Exit))
		}
		if m.IsError {
			parts = append(parts, "error")
		}
	}
	return strings.Join(parts, " · ")
}

// trajectoryHeading is the dock row's title: the ledger's own count, read
// cheaply. It is deliberately not the record list — the panel rebuilds only
// when something moved, and walking a long session there is the per-frame cost
// the rebuild cap exists to avoid.
func trajectoryHeading(store *session.Store) string {
	n := len(store.Entries())
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("TRAJECTORY · %d records", n)
}

// clipSummary collapses whitespace and truncates a one-line entry preview.
func clipSummary(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// sessionLabelsPath is the tree-selector label sidecar:
// <dataDir>/session-labels.json, {"<entryId>": "label"}. UI state, so it
// lives here next to the other dataDir files, not in the session store.
func sessionLabelsPath() string {
	return filepath.Join(config.DataDir(), "session-labels.json")
}

// loadSessionLabels returns the id→label map; a missing or malformed file
// reads as empty (labels are advisory state, never worth failing over).
func loadSessionLabels() map[string]string {
	m := map[string]string{}
	b, err := os.ReadFile(sessionLabelsPath())
	if err != nil {
		return m
	}
	if json.Unmarshal(b, &m) != nil {
		return map[string]string{}
	}
	return m
}

// saveSessionLabel sets (or, with an empty label, clears) one entry's
// label and rewrites the sidecar.
func saveSessionLabel(id, label string) error {
	m := loadSessionLabels()
	if label == "" {
		delete(m, id)
	} else {
		m[id] = label
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(sessionLabelsPath(), b, 0o644)
}

// switchEmitter is the slice of the hook bus the session-switch events
// need. It is an interface (not *hooks.Bus) so tests can record emissions
// with a stub; a nil bus is safe (Bus methods are nil-tolerant).
type switchEmitter interface {
	Emit(ctx context.Context, event string, payload any)
}

// emitSwitchEvents notifies the hook bus around a session switch (omp
// `session_before_switch` / `session_switch`, research §4). before=true
// fires ahead of the store swap so a hook can observe the outgoing
// session; the second call fires once the new session is live. Emissions
// are best-effort: a broken hook never blocks a switch.
func emitSwitchEvents(bus switchEmitter, before bool, to, title string) {
	if bus == nil {
		return
	}
	event := "session_switch"
	if before {
		event = "session_before_switch"
	}
	bus.Emit(context.Background(), event, map[string]any{"session": to, "title": title})
}

// hubCostLabel renders the roster's cost column: USD when the provider
// priced the run, otherwise a token estimate, otherwise "-".
func hubCostLabel(r agent.RosterEntry) string {
	if r.Cost > 0 {
		return fmt.Sprintf("$%.4f", r.Cost)
	}
	if r.Tokens > 0 {
		return fmt.Sprintf("~%.1fk tok", float64(r.Tokens)/1000)
	}
	return "-"
}

// planShowText is /plan show (#291 §2): the proposed document read as a
// document, then the task list it was written against. The dock renders the same
// two reads as its top sections; this is the copy that survives in the
// transcript and the one that works in a terminal too narrow for the panel.
// Approval is not here — /plan off and typed feedback already resolve a
// proposal, and only PlanMode.Resolve gets to do that.
func planShowText(pm *agent.PlanMode) string {
	pending, active := pm.View()
	if pending == "" {
		if !active {
			return ""
		}
		return "plan mode is on — nothing proposed yet; propose submits the plan for review"
	}
	out := "PLAN — proposed, awaiting review\n\n" + strings.TrimRight(pending, "\n")
	if phases := pm.Todo(); phases != "" {
		out += "\n\n" + phases // the task list, heading included
	}
	return out + "\n\nresolve: /plan off approves · any prompt you type next is revision feedback"
}

// dockAgentsLabel renders the hub roster as the dock's Agents section: a heading
// carrying how many are still working, then one row per child with what it is
// doing. /hub reads the same Roster() snapshot, so the panel and the overlay
// cannot disagree about a status.
func dockAgentsLabel(rows []agent.RosterEntry) string {
	if len(rows) == 0 {
		return ""
	}
	run := 0
	for _, r := range rows {
		if r.Status == "running" {
			run++
		}
	}
	var b strings.Builder
	if run == 0 {
		fmt.Fprintf(&b, "AGENTS · %d settled", len(rows))
	} else {
		fmt.Fprintf(&b, "AGENTS · %d running / %d", run, len(rows))
	}
	for _, r := range rows {
		// No width work here: the panel clips each row to its interior, and a
		// byte cut here would split a rune the clip could not recover.
		name := r.Name
		if name == "" {
			name = r.ID
		}
		fmt.Fprintf(&b, "\n%s %s · %s", r.Status, name, r.Activity)
	}
	return b.String()
}

// askCardSink is the ask tool's TUI answer path (#36 → #106): the interactive
// option card answers when the user picks one. When nobody picks, the card has
// already spent the policy's one wait and put the question in the transcript,
// so the sink answers now — with the recommendation only when ask.autoAnswer
// asked for it. Falling through to the headless sink would wait a second time
// and then report "no answer within" a wait the user never saw; the headless
// sink stays the fallback only when no card can be shown at all.
//
// The policy is READ per call, not captured, for the same reason the card's
// wait is: /auto-answer changes it mid-session, and the next question must
// obey the answer the human gave one turn earlier. Two sources of truth here
// (a bool that was captured plus a settings file that moved) is how a card
// ends up answering a question the human was told it would not.
type askCardSink struct {
	ops      *tui.AskOps
	auto     func() bool         // the live ask.autoAnswer policy; nil = off
	fallback func() tool.AskSink // the headless path, built with the same policy
}

// autoAnswer reports the live policy; an unwired sink answers nobody.
func (s *askCardSink) autoAnswer() bool { return s.auto != nil && s.auto() }

func (s *askCardSink) Ask(ctx context.Context, req tool.AskRequest) (tool.AskResponse, error) {
	if s.ops == nil || s.ops.Show == nil {
		return s.fallback().Ask(ctx, req)
	}
	ans, ok := s.ops.Show(ctx, askCardRequest(req), 0)
	if err := ctx.Err(); err != nil {
		return tool.AskResponse{}, err
	}
	// The card has been shown, so this call has already spent its one wait:
	// whatever it says is the answer, including "nothing" (which the tool
	// turns into its best-judgment text). Falling to the fallback here would
	// wait ask.timeout a second time for a human who already declined.
	resp, _ := s.verdict(req, ans, ok)
	return resp, nil
}

// AskBatch answers a batch as one tabbed card, so several questions cost the
// human one interruption and one wait instead of N of each. The tool only
// routes here when its sink implements the seam (tool.AskBatchSink).
func (s *askCardSink) AskBatch(ctx context.Context, reqs []tool.AskRequest) ([]tool.AskResponse, error) {
	if len(reqs) == 1 {
		one, err := s.Ask(ctx, reqs[0])
		return []tool.AskResponse{one}, err
	}
	if s.ops == nil || s.ops.ShowBatch == nil {
		// No batch card: one Ask per question keeps the old behavior correct,
		// at the cost of one interruption each.
		out := make([]tool.AskResponse, 0, len(reqs))
		for _, req := range reqs {
			resp, err := s.Ask(ctx, req)
			if err != nil {
				return nil, err
			}
			out = append(out, resp)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		return out, nil
	}
	cardReqs := make([]tui.AskRequest, 0, len(reqs))
	for _, req := range reqs {
		cardReqs = append(cardReqs, askCardRequest(req))
	}
	answers, ok := s.ops.ShowBatch(ctx, cardReqs, 0)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(answers) != len(reqs) {
		ok = false // a half answer is no answer: fall back wholesale
	}
	out := make([]tool.AskResponse, len(reqs))
	for i, req := range reqs {
		var ans tui.AskAnswer
		if ok {
			ans = answers[i]
		}
		out[i], _ = s.verdict(req, ans, ok) // one wait, same rule as Ask
	}
	return out, nil
}

// askCardRequest converts one tool question to the overlay's shape.
func askCardRequest(req tool.AskRequest) tui.AskRequest {
	return tui.AskRequest{
		Question: req.Question, Options: askCardOptions(req.Options),
		Multi: req.Multi, ID: req.ID, Recommended: req.Recommended,
	}
}

// verdict is one card answer's verdict. answered=false means the question is
// unanswered even after the card (skipped, or timed out with no recommended
// option), which the tool reports as its best-judgment text. Picking the chat
// escape hatch IS an answer — it carries no labels on purpose — so it must not
// be mistaken for a skip.
func (s *askCardSink) verdict(req tool.AskRequest, ans tui.AskAnswer, ok bool) (tool.AskResponse, bool) {
	note := strings.TrimSpace(ans.Note)
	if ok && (len(ans.Labels) > 0 || note != "") {
		return tool.AskResponse{Labels: ans.Labels, Note: note}, true
	}
	// Skip, or a timeout with auto-answer on: the tool's policy is the
	// recommended option(s), so take them instead of waiting a second time.
	// With auto-answer off nobody asked for that — the human skipped, and
	// skipping is not permission to decide.
	if s.autoAnswer() && len(req.Recommended) > 0 {
		return tool.AskResponse{Labels: append([]string(nil), req.Recommended...)}, true
	}
	return tool.AskResponse{}, false
}

// askCardOptions converts the tool's option list to the overlay's shape.
func askCardOptions(in []tool.AskOption) []tui.AskOption {
	out := make([]tui.AskOption, len(in))
	for i, o := range in {
		out[i] = tui.AskOption{Label: o.Label, Description: o.Description}
	}
	return out
}

// The collab room state of this process's TUI: the relay the session hosts and
// the room it joined as a guest. Package-level, like the tui.Collab slot that
// drives them, because the send paths must ask the ROOM — both used to ask the
// WIRING (`tui.Collab != nil && tui.Collab.Forward != nil`), and every TUI
// installs that seam at startup, so the test was always true: F5 answered
// "joined as a guest — the host runs the turn" in plain local sessions, and a
// pasted image bounced with "a guest room forwards text only".
var (
	collabMu    sync.Mutex
	collabHost  *collab.Host
	collabGuest *collab.Guest
)

// collabGuestJoined reports whether this session is mirroring a room as a guest.
func collabGuestJoined() bool {
	collabMu.Lock()
	defer collabMu.Unlock()
	return collabGuest != nil
}

// --- collab helpers (M14 #59) ---

// collabSnapshot returns the session as JSONL bytes: the session file
// verbatim when it exists (guests replay it through the session store's
// context builder, so compaction and branches behave natively), else the
// in-memory entries re-marshaled (a session that never materialized).
func collabSnapshot(store *session.Store) []byte {
	if store == nil {
		return nil
	}
	if path := store.Path(); path != "" {
		if data, err := os.ReadFile(path); err == nil {
			return data
		}
	}
	var out []byte
	for _, line := range collabEntries(store) {
		out = append(out, line...)
		out = append(out, '\n')
	}
	return out
}

// collabEntries returns every current entry line in order; the relay diffs
// this list against the lines it already broadcast.
func collabEntries(store *session.Store) [][]byte {
	if store == nil {
		return nil
	}
	entries := store.Entries()
	out := make([][]byte, 0, len(entries))
	for _, e := range entries {
		line, err := session.MarshalEntry(e)
		if err != nil {
			continue
		}
		out = append(out, line)
	}
	return out
}

// collabShareText renders the /collab instructions for a live relay. A
// view-only share never prints the full link: possession of the token is
// what grants control.
func collabShareText(h *collab.Host, view bool) string {
	full, viewLink := h.URL(true), h.URL(false)
	var b strings.Builder
	if view {
		b.WriteString("· collab live — view-only link (guests read, cannot prompt)\n")
		fmt.Fprintf(&b, "  link  %s\n", viewLink)
		fmt.Fprintf(&b, "  join  xdev join \"%s\"\n", viewLink)
		b.WriteString("  /collab stop ends sharing; restart without `view` to grant control")
		return b.String()
	}
	b.WriteString("· collab live — full-control link (prompt + interrupt)\n")
	fmt.Fprintf(&b, "  full       %s\n", full)
	fmt.Fprintf(&b, "  view-only  %s\n", viewLink)
	fmt.Fprintf(&b, "  join       xdev join \"%s\"\n", full)
	if !collab.IsLoopback(strings.TrimPrefix(strings.TrimPrefix(full, "wss://"), "ws://")) {
		b.WriteString("  note: bound beyond loopback — guests must reach that address (use your LAN IP if you bound 0.0.0.0)")
	}
	return b.String()
}

// collabStatusText renders the active room, its links, and its guests.
func collabStatusText(h *collab.Host) string {
	parts := h.Participants()
	names := make([]string, 0, len(parts))
	writable := 0
	for _, p := range parts {
		label := p.Name
		if p.Writable {
			writable++
			label += " (full)"
		} else {
			label += " (view-only)"
		}
		names = append(names, label)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "· collab room %s — %d guest(s)", h.RoomID(), len(parts))
	if len(names) > 0 {
		fmt.Fprintf(&b, ", %d writable: %s", writable, strings.Join(names, ", "))
	}
	fmt.Fprintf(&b, "\n  full       %s", h.URL(true))
	fmt.Fprintf(&b, "\n  view-only  %s", h.URL(false))
	return b.String()
}

// orUnset keeps a mailbox notice readable when a peer sent no name/subject.
func orUnset(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

// liveTurn publishes the in-flight turn's cancel to every abort path: Esc,
// Ctrl+C, and a full-link guest's interrupt all abort THAT turn. The session
// context must never be the abort target — each turn derives its own ctx from
// baseCtx, so cancelling baseCtx leaves every later turn born already-canceled
// and the TUI prints "· turn canceled" to every future message until restart.
type liveTurn struct {
	mu     sync.Mutex
	cancel context.CancelFunc
}

func (t *liveTurn) set(cancel context.CancelFunc) {
	t.mu.Lock()
	t.cancel = cancel
	t.mu.Unlock()
}

func (t *liveTurn) clear() { t.set(nil) }

// abort cancels the live turn and reports whether one was running.
func (t *liveTurn) abort() bool {
	t.mu.Lock()
	cancel := t.cancel
	t.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}
