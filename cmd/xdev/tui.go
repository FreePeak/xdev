package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/FreePeak/xdev/internal/memory"
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
	"github.com/FreePeak/xdev/internal/fscache"
	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/theme"
	"github.com/FreePeak/xdev/internal/tool"
	"github.com/FreePeak/xdev/internal/tui"
)

// runTUI drives the interactive TUI mode (M4).
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
	// --thinking overrides whatever the model role pinned (the live holder
	// below reads the resolved value).
	if effortRef, err = applyThinkingFlag(launch.Thinking, effortRef); err != nil {
		return 2, err
	}
	provName, modelName, err := config.ParseModelRef(modelRef)
	if err != nil {
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
	// AND modelRoles.advisor resolves. It watches transcript snapshots and
	// steers into the live run.
	adv := buildAdvisor(cfg, lastSettings())
	var advisorOn atomic.Bool
	advisorOn.Store(adv != nil)
	modelMu := &sync.Mutex{}
	live := &struct {
		prov     ai.Provider
		model    string
		provName string
		effort   string
	}{prov, modelName, provName, effortRef}

	// Tools + system prompt (shared with print mode).
	// The propose reviewer (TUI): surface the plan and hold the decision
	// for the user — /plan off resolves accept, any next prompt is the
	// revision note. Headless runs auto-accept (nil reviewer).
	reg := newToolRegistry(cwd, prov, provName, modelName, lastSettings(), effortBudget(effortRef), planMode)
	defer closeSharedHub() // hub-started children are session-scoped (T3 #8)
	mgr := attachMCP(context.Background(), reg, false)
	if mgr != nil {
		defer mgr.Close()
	}
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
	// The breadcrumb keys --continue for this pane. A fresh session is
	// memory-only until its first assistant message, so record the
	// AUTO-PERSIST path: --continue already guards with os.Stat, and a
	// breadcrumb naming the live session beats silently reopening the
	// previous one (the /new, /drop defect).
	saveBreadcrumb(breadcrumbPath(store))
	wireTaskParent(reg, store)
	// The resume line on the way out. Registered BEFORE the defers that flush
	// and close the store and release the screen, so LIFO order prints it last
	// of the three: tcell has left the alt screen (anything written before Fini
	// is wiped) and the session file is closed. It reads `store` at exit, so
	// /new, /fork and /resume change what the line names.
	defer func() {
		if hint := resumeHint(store, cwd); hint != "" {
			fmt.Println(hint)
		}
	}()
	defer func() {
		modelMu.Lock()
		lm := live.provName + "/" + live.model
		modelMu.Unlock()
		_ = store.Append(&session.ModelChangeEntry{Model: lm})
		_ = store.Append(&session.CustomEntry{CustomType: "session_exit", Data: map[string]any{"mode": "tui", "code": exitCode}})
		if cerr := store.Close(); cerr != nil {
			logx.Errorf("session close: %v", cerr)
		}
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

	app := tui.New(scr, th, modelRef, store.ID())
	// A frozen TUI is otherwise undiagnosable after the fact: the UI loop is
	// single-goroutine, so anything that fails to return there kills keys,
	// Ctrl+C and output together while the process stays alive. If one loop
	// iteration stalls, xdev now writes the goroutine stacks where `xdev gc`
	// already collects them.
	app.SetStallDumpDir(filepath.Join(config.DataDir(), "dumps"))
	// showThinking drives the reasoning display (issue #20): the layered
	// config is the source of truth, with --hide-thinking / --print-thoughts
	// overriding it for this run (display only — the model still thinks).
	app.SetShowThinking(showThinkingOn(lastSettings()))
	// HUD segments (settings statusLine.segments): unknown names are
	// skipped with a warning, unset keeps the shipped layout.
	app.SetStatusSegments(lastSettings().StatusLineSegments())
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
	if len(store.Entries()) > 0 {
		if res, err := session.BuildContext(store.Entries(), store.LeafID(), session.SystemPrompt{}); err == nil {
			replayTranscript(app, res.Messages)
		}
	}
	// Live conversation is the store: user/assistant/toolResult messages
	// are persisted by the hooks and Agent.persist, so each submit rebuilds
	// history from the store (compaction entries replay correctly through
	// BuildContext). Seeding from a separate slice went stale and dropped
	// assistant turns (conversation amnesia).
	rebuildHistory := func() []ai.Message {
		res, err := session.BuildContext(store.Entries(), store.LeafID(), session.SystemPrompt{})
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

	var running atomic.Bool

	// turn publishes the in-flight turn's cancel to the abort paths (Esc,
	// Ctrl+C, a full-link guest's interrupt).
	var turn liveTurn
	// Serializes conversation accumulation across turns (one run at a time;
	// guarded for the UI thread that reads nothing here).
	var sessMu sync.Mutex

	// Handoff (M5 #23): /handoff and -handoff replace the live context with
	// a handoff document committed as a normal compaction entry, so the
	// next turn continues from the document. The side request mirrors a
	// live turn's transform (system + history + a trailing instruction, no
	// tools) on the @smol role; the reset closure rewinds the advisor feed
	// cursor — the todo list and plan mode are the agent's own seams — and
	// settings handoff.saveToDisk mirrors the document to disk.
	handoffSettings := func() agent.HandoffSettings {
		hs := agent.HandoffSettings{SaveDir: handoffSaveDir(lastSettings())}
		if t := resolveInto("@smol", cfg, lastSettings(), "handoff"); t != nil {
			hs.Target = *t
		}
		if adv != nil {
			hs.Reset = func(kept []ai.Message) { adv.Reset(kept) }
		}
		return hs
	}
	runHandoff := func(instruction string) (string, error) {
		if running.Load() {
			return "", fmt.Errorf("a turn is running — Esc cancels it first")
		}
		modelMu.Lock()
		lp, lm, lpn := live.prov, live.model, live.provName
		modelMu.Unlock()
		ag := &agent.Agent{
			Provider:   lp,
			Tools:      reg,
			Model:      lm,
			Store:      store,
			Compaction: agent.CompactionConfig{ContextWindow: modelWindow(cfg, lpn, lm), Methods: agent.HandoffOrder(lastSettings().CompactionMethodOrder())},
			PlanMode:   planMode,
			Handoff:    handoffSettings(),
		}
		wireAgentMode(ag, reg, cfg, lastSettings(), modelRoleRef(opts.Model), lpn, lm, cwd, true)
		return ag.HandoffDoc(baseCtx, buildSys(), instruction)
	}
	// #272: the alt screen swallows stderr, which is where discovery
	// warnings used to go — so an empty or half-broken agent set looked
	// exactly like a working one. Say what loaded, before the first turn.
	if notice, _, _ := taskAgentsAtStartup(cwd); notice != "" {
		app.SetStartupNotice(notice)
	}

	// -handoff: document the resumed session before the first turn.
	if handoffMode && len(store.Entries()) > 0 {
		if doc, err := runHandoff(""); err != nil {
			logx.Errorf("handoff: %v", err)
		} else {
			app.AddSystemBlock(doc)
		}
	}

	ts := &tuiSession{store: store, app: app}

	var swapStoreTo func(*session.Store) error

	// swapStore closes the current session and opens a fresh one (issue #11).
	// drop=true deletes the old file first. The transcript clears and the
	// live hooks/agent point at the new store (single source of truth: the
	// captured `store` variable, which all closures re-read).
	swapStore := func(drop bool) error {
		// Vibe mode is session-scoped: a new session would orphan the
		// director's workers, so the switch is refused until it is off
		// (omp rejects start/fork while the mode is active).
		if vibeActive() {
			return fmt.Errorf("vibe mode is active — /vibe off first")
		}
		old := store
		ns, err := openSession(cwd, false, "")
		if err != nil {
			return err
		}
		// /new and /drop ARE session switches, so they fire the same hook
		// events /resume does — they used to emit nothing, and a
		// session_switch hook (archive, notify) silently never ran on them
		// (parity finding T3 #16).
		bus := buildHookBus(cwd, opts, app.AddSystemBlock) // resolved per switch: /settings edits land
		emitSwitchEvents(bus, true, shortSessionID(ns.ID()), ns.Title())
		if drop && old.Path() != "" {
			_ = old.Close()
			if rmErr := os.Remove(old.Path()); rmErr != nil && !os.IsNotExist(rmErr) {
				logx.Errorf("drop session file: %v", rmErr)
			}
		} else {
			_ = old.Close()
		}
		store = ns
		ts.store = ns
		wireTaskParent(reg, ns) // children must link to the ACTIVE session
		app.Reset()
		app.SetSessionStart(sessionStart(ns))
		saveBreadcrumb(breadcrumbPath(ns))
		app.AddSystemBlock("· new session " + shortSessionID(ns.ID()))
		emitSwitchEvents(bus, false, shortSessionID(ns.ID()), ns.Title())
		return nil
	}

	// swapStoreTo adopts an already-open store (fork/resume): replays its
	// transcript and points hooks/agent at it.
	swapStoreTo = func(ns *session.Store) error {
		if vibeActive() {
			return fmt.Errorf("vibe mode is active — /vibe off first")
		}
		// A session interrupted mid-write is repaired on open (#122); in the
		// TUI the transcript is the only surface the user can actually read, so
		// the notice goes there rather than to stderr under the alt screen.
		if n := sessionRepairNotice(ns); n != "" {
			app.AddSystemBlock(n)
		}
		old := store
		bus := buildHookBus(cwd, opts, app.AddSystemBlock) // resolved per switch: /settings edits land
		emitSwitchEvents(bus, true, shortSessionID(ns.ID()), ns.Title())
		store = ns
		ts.store = ns
		wireTaskParent(reg, ns)
		// The resumed session carries its own director state: adopt it
		// (workers rehydrate as idle — nothing runs in a fresh process).
		if vibeScope != nil {
			workers, on := agent.LoadVibe(ns.Entries())
			vibeScope.Restore(workers, on)
		}
		app.Reset()
		app.SetSessionStart(sessionStart(ns))
		saveBreadcrumb(breadcrumbPath(ns))
		if res, err := session.BuildContext(ns.Entries(), ns.LeafID(), session.SystemPrompt{}); err == nil {
			replayTranscript(app, res.Messages)
		}
		app.AddSystemBlock("· session " + shortSessionID(ns.ID()) + " — " + ns.Title())
		emitSwitchEvents(bus, false, shortSessionID(ns.ID()), ns.Title())
		_ = old
		return nil
	}

	// Session lifecycle (issue #11): /new swaps in a fresh session file,
	// /clear resets in place (durable reset_boundary, history kept on
	// disk), /drop deletes the file and starts fresh. All refuse while a
	// @-file completion runs through the SAME shared FS-scan cache
	// grep/glob use, so the menu costs one walk per TTL, not per keystroke.
	app.SetPathCompletion(cwd, func() []string {
		entries, _, _ := tool.SharedFSCache().Scan(fscache.Options{
			Roots: workspaceDirs(cwd), RespectGitignore: true,
		})
		out := make([]string, 0, len(entries))
		for _, e := range entries {
			if !e.IsDir && e.Rel != "." {
				out = append(out, e.Rel)
			}
		}
		return out
	})

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
		return deleteSessionByShortID(id, store.Path())
	})
	app.SetResumeList(func(cwd string) error {
		metas, err := session.List(sessionDataDir())
		if err != nil {
			return nil
		}
		out := make([]tui.ResumeOption, 0, 12)
		for _, m := range metas {
			if m.CWD != cwd || m.TitleSource == session.TitleSourceSubagent {
				continue
			}
			out = append(out, tui.ResumeOption{
				ID:      m.ID,
				Title:   m.Title,
				Detail:  humanSize(m.SizeBytes) + " · " + m.ModTime.Format("Jan 02 15:04"),
				Current: m.ID == store.ID(),
			})
			if len(out) >= 12 {
				break
			}
		}
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
		res, err := session.BuildContext(store.Entries(), store.LeafID(), session.SystemPrompt{})
		if err != nil {
			return
		}
		app.Reset()
		replayTranscript(app, res.Messages)
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
		e := store.Entry(entryID)
		if e == nil {
			return "", fmt.Errorf("no entry %q in this session", entryID)
		}
		target, draft := treeRewindTarget(e)
		if summarize {
			if err := summarizeAndBranch(store, target); err != nil {
				return "", err
			}
		} else if target == "" {
			if err := store.ResetLeaf(); err != nil {
				return "", err
			}
		} else if err := store.Branch(target); err != nil {
			return "", err
		}
		branchReplay()
		return draft, nil
	}
	// branchToEntry moves the live leaf to an entry and replays the new
	// branch's transcript into the TUI (/branch <id-prefix>).
	branchToEntry := func(entryID string) error {
		if err := store.Branch(entryID); err != nil {
			return fmt.Errorf("branch: %v", err)
		}
		branchReplay()
		app.AddSystemBlock("· branched to " + entryID[:min(8, len(entryID))] + " — replayed")
		return nil
	}
	app.SetSessionBranch(func(args string) error {
		query := strings.TrimSpace(args)
		if query == "" {
			return fmt.Errorf("branch: entry-id prefix required (ids are listed by /tree)")
		}
		// First prefix match wins (omp addresses entries by full id; the
		// selector hands Enter the full id — the prefix form is a typed
		// convenience).
		for _, e := range store.Entries() {
			if env := e.Envelope(); strings.HasPrefix(env.ID, query) {
				return branchToEntry(env.ID)
			}
		}
		return fmt.Errorf("branch: no entry matching %q (entries before the last /clear or compaction are not addressable)", query)
	})
	// /tree selector: entry rows built from the live store, labels from
	// the dataDir sidecar (UI state — the session package stays label-free).
	app.SetTreeData(func() []tui.TreeEntry { return treeEntries(store) })
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
	// Vibe mode (M14 #58): the director scope over the session hub and the
	// task tool's subagent machinery. Workers inherit the task tool's tool
	// surface and approval posture; the tier selects the bundled agent
	// prompt and the resolved role model (parent model as the fallback).
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
				Resolve: func(role string) (*agent.VibeModel, error) {
					if ref, effort, err := resolveModel(role, cfg, lastSettings()); err == nil {
						pn, mn, perr := config.ParseModelRef(ref)
						if perr != nil {
							return nil, perr
						}
						pc, has := cfg.Providers[pn]
						if !has {
							return nil, fmt.Errorf("unknown provider %q", pn)
						}
						prov, berr := buildProvider(pn, pc, mn, cfg)
						if berr != nil {
							return nil, berr
						}
						return &agent.VibeModel{Provider: prov, Model: mn, Thinking: effortBudget(effort)}, nil
					}
					// Unset role: the parent's active model is the fallback
					// (the task tool's routing).
					modelMu.Lock()
					lp, lm, le := live.prov, live.model, live.effort
					modelMu.Unlock()
					return &agent.VibeModel{Provider: lp, Model: lm, Thinking: effortBudget(le)}, nil
				},
				Persist: func(customType string, data map[string]any) {
					if err := store.Append(&session.CustomEntry{CustomType: customType, Data: data}); err != nil {
						logx.Errorf("vibe: persist %s: %v", customType, err)
					}
				},
				ParentID: func() string { return store.ID() },
				Conflicts: func() []string {
					var out []string
					if planMode.Active {
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
			workers, on := agent.LoadVibe(store.Entries())
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
			if !running.CompareAndSwap(false, true) {
				return fmt.Errorf("a turn is running — Esc cancels it first")
			}
			defer running.Store(false)
			// A fresh session lives memory-only until its first
			// assistant message — materialize it so the fork has a
			// source file to copy.
			if store.Path() == "" {
				if _, err := store.EnsureOnDisk(
					session.SessionFilePath(sessionDataDir(), cwd, time.Now(), store.ID()), session.Options{}); err != nil {
					return err
				}
			}
			fork, err := session.ForkSession(store.Path(),
				session.SessionFilePath(sessionDataDir(), cwd, time.Now(), session.NewSessionID()), "")
			if err != nil {
				return err
			}
			return swapStoreTo(fork)
		},
		Dump: func() (string, error) {
			return dumpSession(store)
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
			return shareLive(store, buildSys(), modelNow())
		},
		Resume: func(query string) error {
			if running.Load() {
				return fmt.Errorf("a turn is running — Esc cancels it first")
			}
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
		NavigateTree: navigateTree,
		New: func() error {
			if !running.CompareAndSwap(false, true) {
				return fmt.Errorf("a turn is running — Esc cancels it first")
			}
			defer running.Store(false)
			return swapStore(false)
		},
		Fresh: func() error {
			if running.Load() {
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
			return swapStoreTo(store)
		},
		Clear: func() error {
			if !running.CompareAndSwap(false, true) {
				return fmt.Errorf("a turn is running — Esc cancels it first")
			}
			defer running.Store(false)
			if err := store.ResetLeaf(); err != nil {
				return err
			}
			app.Reset()
			// The transcript must not go fully blank: draw() renders the
			// welcome screen whenever there are no blocks, and a command
			// that answers with nothing reads as if it were swallowed.
			app.AddSystemBlock("· context cleared — history kept on disk")
			return nil
		},
		Recent: func() []tui.ResumeOption {
			metas, err := session.List(sessionDataDir())
			if err != nil {
				return nil
			}
			out := make([]tui.ResumeOption, 0, 12)
			for _, m := range metas {
				if m.CWD != cwd || m.TitleSource == session.TitleSourceSubagent || m.ID == store.ID() {
					continue
				}
				out = append(out, tui.ResumeOption{
					ID:     m.ID,
					Title:  m.Title,
					Detail: m.ID[:8] + " · " + m.ModTime.Format("Jan 02 15:04"),
				})
				if len(out) >= 12 {
					break
				}
			}
			return out
		},
		Drop: func() error {
			if !running.CompareAndSwap(false, true) {
				return fmt.Errorf("a turn is running — Esc cancels it first")
			}
			defer running.Store(false)
			return swapStore(true)
		},
		// /handoff (M5 #23): replace the live context with a handoff
		// document committed as a compaction entry.
		Handoff: runHandoff,
		// /rename: a manual title, written into the fixed-width slot so
		// /resume and the breadcrumb show it (#107).
		Rename: func(title string) error { return store.Rename(title, session.TitleSourceManual) },
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
		_ = nprov
		if err := store.Append(&session.ModelChangeEntry{Model: nprovName + "/" + nmodelName}); err != nil {
			logx.Errorf("model change entry: %v", err)
		}
		modelMu.Lock()
		live.prov, live.model, live.provName, live.effort = nprov, nmodelName, nprovName, ne
		modelMu.Unlock()
		app.SetStatusModel(nprovName + "/" + nmodelName)
		// The HUD context segment measures against the new window.
		app.SetContextWindow(int64(modelWindow(cfg, nprovName, nmodelName)))
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
		SetRole: func(role, ref string) error {
			if !config.IsKnownRole(role) {
				return fmt.Errorf("unknown role @%s", role)
			}
			if err := config.Set(config.GlobalSettingsPath(), "modelRoles."+role, ref); err != nil {
				return err
			}
			// Keep the in-memory layer in sync so the follow-up
			// "@role" switch resolves without a restart.
			s := lastSettings()
			if s.ModelRoles == nil {
				s.ModelRoles = map[string]string{}
			}
			s.ModelRoles[role] = ref
			return nil
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
	planMode.Propose = agent.NewProposeTool(planMode, func(context.Context, string) (bool, string) {
		return false, "awaiting user review — the user will /plan off to approve or send revision feedback"
	})
	// ask (#36): surface the question in the transcript. The blocking card
	// that lets the user pick an option is #46's scope, so this sink is
	// deliberately one-way — it shows the question and lets the headless
	if at, ok := reg.Get(tool.AskToolName); ok {
		if at2, isAsk := at.(*tool.AskTool); isAsk {
			// The overlay is the answer path; the headless policy is the
			// skip/timeout fallback.
			at2.Sink = &askCardSink{
				ops:      app.NewAskOps(lastSettings().AskTimeout()),
				fallback: tool.NewHeadlessAskSink(lastSettings().AskTimeout()),
			}
		}
	}
	app.SetMemoryOps(memoryOps(sessionMemory))
	app.SetAdvisorOps(&tui.AdvisorOps{
		Enabled: func() bool { return adv != nil },
		Set: func(on bool) error {
			if on && adv == nil {
				return fmt.Errorf("advisor unavailable: set modelRoles.advisor and advisor: true in settings")
			}
			advisorOn.Store(on)
			return nil
		},
		Status: func() string {
			if adv == nil {
				return "off (no modelRoles.advisor configured)"
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
				ref := into
				if ref == "" {
					ref = "@smol"
				}
				nref, _, err := resolveModel(ref, cfg, lastSettings())
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
		Get: func() bool { return planMode.Active },
		Set: func(on bool) error {
			// The director's reduced toolset and read-only planning
			// contradict each other: one mode at a time.
			if on && vibeActive() {
				return fmt.Errorf("vibe mode is active — /vibe off first")
			}
			planMode.Active = on
			return nil
		},
	})

	// goalKick runs the goal's first turn. Setting a goal must actually start
	// it: the goal state on its own only decorates the next user-driven turn,
	// so `/goal create …` printed "goal created" and then nothing ran. The
	// run continues from there (agent.Agent.GoalContinuation), and the
	// objective becomes the session title and the first prompt the user sees.
	// Safe before SetHandlers: SendPrompt is a no-op while onSend is unwired.
	goalKick := func(objective string) { app.SendPrompt(objective) }

	// /goal drives the same GoalState the goal tool owns: the verbs mutate
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
		Create: func(objective string) (string, error) {
			gs := agent.GoalStateOf(reg)
			if gs == nil {
				return "", fmt.Errorf("goal not wired")
			}
			if _, err := gs.Create(objective, 0); err != nil {
				return "", err
			}
			goalKick(objective)
			return "goal created\n" + gs.Describe(), nil
		},
		Resume: func(objective string) (string, error) {
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
		Evidence: func(note string) (string, error) {
			gs := agent.GoalStateOf(reg)
			if gs == nil {
				return "", fmt.Errorf("goal not wired")
			}
			if _, err := gs.AddEvidence(note); err != nil {
				return "", err
			}
			return "evidence recorded\n" + gs.Describe(), nil
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
	var (
		collabMu    sync.Mutex
		collabHost  *collab.Host
		collabGuest *collab.Guest
	)
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
					Snapshot: func() []byte { return collabSnapshot(store) },
					Prompt: func(name, text string) {
						app.AddSystemBlock("· collab " + name + ": " + text)
						app.SendPrompt(text)
					},
					// Cancel the live turn, never baseCtx (see liveTurn).
					Interrupt: func() { turn.abort() },
				},
				Entries: func() [][]byte { return collabEntries(store) },
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

	// runTurn owns one submit: persist the user message, then drive the agent.
	// imgs is the pasted images riding with it (nil for a plain prompt). It
	// reports whether the turn was taken, so a draft carrying attachments can go
	// back to the composer instead of being sent without them — see
	// tui.App.SetImageSend.
	runTurn := func(text string, imgs []tui.PasteImage) bool {
		// Joined as a guest: the host owns the turn, so the prompt goes over
		// the room instead of starting one here. The room carries text only,
		// so a draft with attachments is neither forwarded nor run: returning
		// false hands it back to the composer, where the notice says why.
		// Sending the text alone would let the host answer a screenshot nobody
		// delivered.
		if tui.Collab != nil && tui.Collab.Forward != nil {
			if imgs != nil {
				return false
			}
			if tui.Collab.Forward(text) {
				return true
			}
		}
		if !running.CompareAndSwap(false, true) {
			app.AddSystemBlock("a turn is already running — Esc cancels it")
			return false
		}
		// Name the session after its first prompt: /resume and the
		// breadcrumb read the title slot, and "print <timestamp>" hides
		// everything about the conversation. Called before the first
		// assistant message materializes the file, so the title lands in
		// the slot without needing a rewrite pass; later prompts keep
		// the first one's title (omp's first-prompt cascade).
		if store.Path() == "" {
			if t := titleFromPrompt(text); t != "" {
				store.SetTitle(t)
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
		if err := store.Append(&session.MessageEntry{Message: msg}); err != nil {
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
		ctx, cancel := context.WithCancel(baseCtx)
		turn.set(cancel)
		go func() {
			// LIFO: clear runs FIRST so this turn can never nil a slot
			// that a newer turn already claimed (running=false admits the
			// next submit before cancel() unwinds).
			defer cancel()
			defer running.Store(false)
			defer turn.clear()
			app.SetRunning(true)
			feedAdvisor := func() {}
			modelMu.Lock()
			lp, lm, lpn, le := live.prov, live.model, live.provName, live.effort
			modelMu.Unlock()
			ag := &agent.Agent{
				Provider: lp,
				Tools:    toolsForTurn(),
				// feedAdvisor is assigned after the agent exists, so go
				// through an indirection: a direct field copy would
				// capture the nil func at literal time.
				Hooks:      memoryTurnHooks(&tuiHooks{ts: ts, feed: func() { feedAdvisor() }}, lastSettings()),
				TTSR:       agent.NewTTSR(ttsrConfig(lastSettings())),
				MaxTokens:  opts.MaxTokens,
				MaxTurns:   opts.MaxTurns,
				Model:      lm,
				Store:      store,
				Compaction: agent.CompactionConfig{ContextWindow: modelWindow(cfg, lpn, lm), Methods: agent.HandoffOrder(lastSettings().CompactionMethodOrder())},
				Failovers:  failoverChain(cfg, lastSettings(), modelRoleRef(opts.Model), lpn, lm),
				Thinking:   effortBudget(le),
				// Intercept set below from exts (only when non-nil).
				Policy:  agentPolicy(),
				Handoff: handoffSettings(),
			}
			// Shared per-mode seams: catalog bridge + secrets redactor
			// (#79/#80). The TUI is the daily driver; an unredacted tool
			// result here is the case that mattered.
			if st := wireAgentMode(ag, reg, cfg, lastSettings(), modelRoleRef(opts.Model), lpn, lm, cwd, true); st != nil {
				// The TUI has a console: a silent provider swap or a
				// cooldown revert is otherwise invisible to the user.
				st.Notify = func(msg string) { app.AddSystemBlock("· " + msg) }
			}
			// The HUD context segment measures against this window.
			app.SetContextWindow(int64(modelWindow(cfg, lpn, lm)))
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
			agentMu.Unlock()
			defer func() {
				agentMu.Lock()
				curAgent = nil
				agentMu.Unlock()
			}()
			sessMu.Lock()
			hist := rebuildHistory() // store mirror is authoritative
			sessMu.Unlock()
			sys := buildSys()
			if vibeActive() {
				sys = vibeSys() // director prompt for the restricted toolset
			}
			finalMsg, err := ag.Run(ctx, hookBus.Context(ctx, sys), hist)
			app.EndAssistant()
			app.FinishRun()
			// Ai-title cascade (#107): the TUI sessions are the ones the
			// picker lists, and they are the ones stuck with "tui
			// <timestamp>". Async on purpose: the user's next keystroke
			// must not wait on a title request.
			lastTurnFailed.Store(err != nil)
			if err == nil && finalMsg != nil && !launch.NoTitle && !launch.NoSession {
				go generateTitle(cfg, lastSettings(), cwd, lpn, lm, store,
					append(append([]ai.Message(nil), hist...), *finalMsg))
			}
			if err != nil {
				if ctx.Err() != nil {
					app.AddSystemBlock("· turn canceled")
				} else {
					app.AddSystemBlock("error: " + err.Error())
				}
			}
		}()
		return true
	}
	// The composer holds the two send paths apart: a plain prompt cannot ask
	// for images it has none of, and a draft that has them must not be
	// silently demoted to text.
	app.SetHandlers(
		func(text string) { runTurn(text, nil) },
		// Esc / Ctrl+C aborts the live turn only; see liveTurn. This handler
		// used to call baseCancel(), which bricked every future turn after
		// the first cancel while the TUI still looked alive.
		func() { turn.abort() },

		func() { app.Quit() },
	)
	app.SetImageSend(func(text string, imgs []tui.PasteImage) bool { return runTurn(text, imgs) })
	// Vision is the live model's property, not the launch model's: /model
	// mid-session changes whether an attachment can be read at all.
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

// roleNamesSorted lists the configured role names in stable order (the
// /model roles tab and the usage report both want determinism).
func roleNamesSorted(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
}

// tuiHooks implements agent.TurnHooks for the TUI.
type tuiHooks struct {
	ts *tuiSession
	// feed (optional) hands the advisor a fresh transcript snapshot after
	// each assistant message — the reviewer steers into the live run.
	feed func()
}

func (h *tuiHooks) OnStart(req ai.StreamRequest) {}

func (h *tuiHooks) OnEvent(ev ai.Event) {
	switch ev.Type {
	case ai.EventTextStart:
		h.ts.app.BeginAssistant()
	case ai.EventTextDelta:
		h.ts.app.AppendAssistant(ev.Delta)
	case ai.EventTextEnd:
		h.ts.app.EndAssistant()
	case ai.EventThinkingStart:
		h.ts.app.BeginThinking()
	case ai.EventThinkingDelta:
		h.ts.app.AppendThinking(ev.Delta)
	case ai.EventThinkingEnd:
		h.ts.app.EndThinking()
	case ai.EventDone:
		h.ts.app.EndAssistant()
		if ev.Usage != nil {
			h.ts.app.AddUsage(ev.Usage.Input, ev.Usage.Output)
			if ev.Usage.Cost != nil {
				h.ts.app.AddCost(ev.Usage.Cost.Total)
			}
		}
	case ai.EventError:
		// A transient blip is being retried by the recovery ladder: the
		// wire error would flash once per attempt, so it collapses to a
		// notice. Hard errors still print verbatim — the turn ends on them.
		if ai.Classify(ev.Err) == ai.ClassTransient {
			h.ts.app.AddSystemBlock("· stream error — retrying")
			break
		}
		h.ts.app.AddSystemBlock("stream error: " + ev.Err.Error())
	}
}

// sessionStart anchors the HUD time segment for a store being adopted
// mid-run: the span already on disk (header → newest entry) carries over,
// so the segment shows total session time across resumes. A fresh or
// empty store starts the clock now.
func sessionStart(st *session.Store) time.Time {
	now := time.Now()
	first := st.StartedAt()
	es := st.Entries()
	if first.IsZero() || len(es) == 0 {
		return now
	}
	span := es[len(es)-1].Envelope().Timestamp.Sub(first)
	if span <= 0 {
		return now
	}
	return now.Add(-span)
}

// shortSessionID renders the first 8 chars of a session id (matches the TUI
// status line convention).
func shortSessionID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// OnToolStart hands the transcript the call's raw arguments: the renderer
// reads the naming field out of them (omp's `name · detail` row), so nothing
// here pre-flattens the JSON into a preview the terminal then has to unpick.
func (h *tuiHooks) OnToolStart(call ai.ToolCallBlock) {
	h.ts.app.AddToolBlock(call.Name, string(call.Arguments))
}

// OnToolEnd passes the outcome facts the status footer shows — exit code,
// dropped output, and the change the tool made to a file — flattened from the
// tool's own structured details.
func (h *tuiHooks) OnToolEnd(call ai.ToolCallBlock, res tool.Result, dur time.Duration) {
	out := tool.OutcomeOf(res.Details)
	h.ts.app.FinishTool(call.Name, res.IsError, res.Text, tui.ToolOutcome{
		Dur:       dur.Round(time.Millisecond).String(),
		Exit:      out.Exit,
		HasExit:   out.HasExit,
		Truncated: out.Truncated,
		Diff:      out.Diff,
	})
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
}

func (h *tuiHooks) OnToolResultMessage(msg *ai.Message) {
	if err := h.ts.store.Append(&session.MessageEntry{Message: *msg}); err != nil {
		logx.Errorf("persist toolResult: %v", err)
	}
}

func (h *tuiHooks) OnTurnEnd(reason ai.StopReason, err error) {}
func (h *tuiHooks) OnCompaction(tokensBefore int64) {
	h.ts.app.AddSystemBlock(fmt.Sprintf("· context compacted (~%d tokens)", tokensBefore))
}

// OnGoalUpdated implements agent.GoalHook: goal transitions land in the
// transcript.
func (h *tuiHooks) OnGoalUpdated(g agent.Goal) {
	h.ts.app.AddSystemBlock("· goal " + g.Status + " — " + g.Objective)
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
func replayTranscript(app *tui.App, msgs []ai.Message) {
	for _, m := range msgs {
		switch m.Role {
		case ai.RoleUser:
			// Goal-continuation prompts are harness text the user never
			// typed: replaying them as ❯ blocks would invent turns that
			// never happened.
			if m.Attribution == agent.GoalContinuationAttribution {
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
		}
	}
}

// modelPickerViews builds the /model selector's tabs: a roles view whose
// rows open the assignment list, then "All models" (sectioned by provider)
// and one view per provider — the same shape omp's /model shows.
func modelPickerViews(cfg *config.Config, s *config.Settings, current string, app *tui.App) []tui.PickerView {
	items := modelPickerItems(cfg, s, current)
	roles := make([]tui.PickerItem, 0, len(config.RoleNames))
	for _, name := range config.RoleNames {
		ref, effort := "", ""
		if s != nil {
			ref = strings.TrimSpace(s.ModelRoles[name])
			effort = strings.TrimSpace(s.ModelRolesEffort[name])
		}
		// An unset @default is not "unconfigured": resolution falls back to
		// models.yml's defaultModel, so show that as the effective value.
		if ref == "" && name == "default" && cfg != nil {
			ref = cfg.DefaultModelRef()
		}
		detail := "unset"
		if ref != "" {
			// The arrow reads as "this slot resolves to"; a bare ref would
			// look like a model row.
			detail = "→ " + ref
			if effort != "" {
				detail += ":" + effort
			}
		}
		roles = append(roles, tui.PickerItem{
			Label:   "@" + name,
			Detail:  detail,
			Value:   "@" + name,
			Current: sameModelRef(ref, current),
		})
	}
	roleView := tui.PickerView{
		Name: "Roles", Items: roles, Action: "set",
		OnSelect: func(role string) { app.OpenRolePicker(strings.TrimPrefix(role, "@")) },
	}
	views := []tui.PickerView{roleView}
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
// "@slow:high" and the "onegw/dev" it resolves to mark the same session.
func sameModelRef(a, b string) bool {
	strip := func(s string) string {
		if i := strings.LastIndex(s, ":"); i >= 0 {
			return s[:i]
		}
		return s
	}
	return a != "" && strip(a) == strip(b)
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
		})
		if len(out) >= 50 {
			break
		}
	}
	return out
}

// searchPickerItems ranks sessions for the picker query: every
// whitespace-separated token must match the id or title (the TUI re-checks
// scope + id/title itself), and sessions whose JSONL body contains the
// tokens count as prompt matches — matches rank by match count: id/title
// hits count double, body hits count occurrences. The body scan is capped
// (64 KiB per file, 50 files) — the picker runs per keystroke.
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
		hay := strings.ToLower(it.ID + " " + it.Title)
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
// every other entry becomes the leaf itself with no draft.
func treeRewindTarget(e session.Entry) (target, draft string) {
	env := e.Envelope()
	if msg, ok := e.(*session.MessageEntry); ok && msg.Message.Role == ai.RoleUser {
		return env.ParentID, msg.Message.Text()
	}
	return env.ID, ""
}

// treeEntries snapshots the session entry graph as tree-selector rows:
// file order, depth from the parent chain, active = current leaf.
// ponytail: depth walks parents per entry (O(n·depth)); session files are
// small — memoize if trees ever grow.
func treeEntries(store *session.Store) []tui.TreeEntry {
	entries := store.Entries()
	leaf := store.LeafID()
	parent := make(map[string]string, len(entries))
	for _, e := range entries {
		env := e.Envelope()
		parent[env.ID] = env.ParentID
	}
	out := make([]tui.TreeEntry, 0, len(entries))
	for _, e := range entries {
		env := e.Envelope()
		te := tui.TreeEntry{ID: env.ID, Type: env.Type, Active: env.ID == leaf}
		for p := parent[env.ID]; p != "" && te.Depth < len(parent); p = parent[p] {
			te.Depth++
		}
		switch t := e.(type) {
		case *session.MessageEntry:
			te.Role = string(t.Message.Role)
			te.Summary = clipSummary(t.Message.Text(), 60)
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

// askCardSink is the ask tool's TUI answer path (#36 → #106): the interactive
// option card answers when the user picks one. When nobody picks, the card has
// already waited out ask.timeout and put the question in the transcript, so
// the sink answers with the recommended labels itself — falling through to the
// headless sink here would wait ask.timeout a second time and then report
// "no answer within" a wait the user never saw. The headless sink stays the
// fallback only when no card can be shown at all.
type askCardSink struct {
	ops      *tui.AskOps
	fallback tool.AskSink
}

func (s *askCardSink) Ask(ctx context.Context, req tool.AskRequest) (tool.AskResponse, error) {
	if s.ops == nil || s.ops.Show == nil {
		return s.fallback.Ask(ctx, req)
	}
	ans, ok := s.ops.Show(ctx, askCardRequest(req), 0)
	if err := ctx.Err(); err != nil {
		return tool.AskResponse{}, err
	}
	// The card has been shown, so this call has already spent its one wait:
	// whatever it says is the answer, including "nothing" (which the tool
	// turns into its best-judgment text). Falling to the fallback here would
	// wait ask.timeout a second time for a human who already declined.
	resp, _ := askCardAnswer(req, ans, ok)
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
		out[i], _ = askCardAnswer(req, ans, ok) // one wait, same rule as Ask
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

// askCardAnswer is one card answer's verdict. answered=false means the question
// is unanswered even after the card (skipped or timed out with no recommended
// option), which the tool reports as its best-judgment text. Picking the chat
// escape hatch IS an answer — it carries no labels on purpose — so it must not
// be mistaken for a skip.
func askCardAnswer(req tool.AskRequest, ans tui.AskAnswer, ok bool) (tool.AskResponse, bool) {
	note := strings.TrimSpace(ans.Note)
	if ok && (len(ans.Labels) > 0 || note != "") {
		return tool.AskResponse{Labels: ans.Labels, Note: note}, true
	}
	// Skip or timeout, after the card's own wait: the tool's policy is the
	// recommended option(s), so take them instead of waiting a second time.
	if len(req.Recommended) > 0 {
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
