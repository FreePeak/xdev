package main

import (
	"context"
	"encoding/json"
	"fmt"
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
	modelRef, effortRef, err := resolveModel(opts.Model, cfg, lastSettings())
	if err != nil {
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
	mgr := attachMCP(context.Background(), reg, false)
	if mgr != nil {
		defer mgr.Close()
	}
	// Recomputed per submit: MCP and extension processes register tools
	// after startup, and a boot-frozen prompt would never mention them.
	// Extension processes: loaded ONCE (handshakes are expensive), their
	// tools join the live registry so the lazy prompt picks them up, and
	// each per-submit agent gets the same fail-closed Interceptor.
	var (
		agentMu  sync.Mutex
		curAgent *agent.Agent
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
	exts := attachExtensions(context.Background(), reg,
		routeToLiveAgent(func(a *agent.Agent, s string) { a.Steer(s) }),
		routeToLiveAgent(func(a *agent.Agent, s string) { a.FollowUp(s) }), cfg)
	if exts != nil {
		defer exts.Close()
	}

	overrides := agent.LoadSystemPromptOverrides(cwd)
	buildSys := promptFnWithMemory(basePrompt(opts, cwd), cwd, reg,
		tailSystemPrompt(overrides, opts.AppendSystem), buildMemory(lastSettings()))

	// Session.
	store, err := openStartupSession(cwd, opts)
	if err != nil {
		return 2, fmt.Errorf("session: %w", err)
	}
	saveBreadcrumb(store.Path())
	wireTaskParent(reg, store)
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
	defer scr.Fini()
	defer setCursorReset()

	app := tui.New(scr, th, modelRef, store.ID())
	// showThinking drives the reasoning display (issue #20); the resolved
	// layered config is the source of truth at startup.
	app.SetShowThinking(lastSettings().ShowThinkingOn())
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

	baseCtx, baseCancel := context.WithCancel(context.Background())
	defer baseCancel()

	var running atomic.Bool
	// Serializes conversation accumulation across turns (one run at a time;
	// guarded for the UI thread that reads nothing here).
	var sessMu sync.Mutex

	ts := &tuiSession{store: store, app: app, model: modelName, api: prov.API(), provider: provName}

	var swapStoreTo func(*session.Store) error

	// swapStore closes the current session and opens a fresh one (issue #11).
	// drop=true deletes the old file first. The transcript clears and the
	// live hooks/agent point at the new store (single source of truth: the
	// captured `store` variable, which all closures re-read).
	swapStore := func(drop bool) error {
		old := store
		ns, err := openSession(cwd, false, "")
		if err != nil {
			return err
		}
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
		saveBreadcrumb(ns.Path())
		app.AddSystemBlock("· new session " + shortSessionID(ns.ID()))
		return nil
	}

	// swapStoreTo adopts an already-open store (fork/resume): replays its
	// transcript and points hooks/agent at it.
	swapStoreTo = func(ns *session.Store) error {
		old := store
		bus := buildHookBus(cwd, opts) // resolved per switch: /settings edits land
		emitSwitchEvents(bus, true, shortSessionID(ns.ID()), ns.Title())
		store = ns
		ts.store = ns
		wireTaskParent(reg, ns)
		app.Reset()
		saveBreadcrumb(ns.Path())
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
			Roots: []string{cwd}, RespectGitignore: true,
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
	app.SetPickerSearch(func(query string) []tui.PickerItem {
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
		metas, err := session.List(config.DataDir())
		if err != nil {
			return err
		}
		var lines []string
		count := 0
		for _, m := range metas {
			if m.CWD != cwd || m.TitleSource == "subagent" {
				continue
			}
			count++
			msg := fmt.Sprintf("%-8s  %s  (%s, last %s)", m.ID[:8], m.Title, humanSize(m.SizeBytes), m.ModTime.Format("Jan 02 15:04"))
			lines = append(lines, msg)
			if count >= 12 {
				break
			}
		}
		if len(lines) == 0 {
			app.AddSystemBlock("no other sessions in this directory")
			return nil
		}
		app.AddSystemBlock("sessions in " + cwd + " (use /resume <id-prefix>):\n" + strings.Join(lines, "\n"))
		return nil
	})
	// branchReplay rebuilds the transcript from the live leaf.
	branchReplay := func() {
		res, err := session.BuildContext(store.Entries(), store.LeafID(), session.SystemPrompt{})
		if err != nil {
			return
		}
		app.Reset()
		replayTranscript(app, res.Messages)
		app.AddSystemBlock("· branched to " + store.LeafID()[:8] + " — replayed")
	}
	// branchToEntry moves the live leaf to an entry and replays the new
	// branch's transcript into the TUI; shared by /branch and the tree
	// selector's Enter.
	branchToEntry := func(entryID string) error {
		if err := store.Branch(entryID); err != nil {
			return fmt.Errorf("branch: %v", err)
		}
		branchReplay()
		return nil
	}
	app.SetSessionBranch(func(args string) error {
		query := strings.TrimSpace(args)
		if query == "" {
			return fmt.Errorf("branch: entry-id prefix required")
		}
		for _, e := range store.Entries() {
			env := e.Envelope()
			if strings.HasPrefix(env.ID, query) {
				return branchToEntry(env.ID)
			}
		}
		return fmt.Errorf("branch: no entry matching %q", query)
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
	// turn is in flight.
	app.SetSessionOps(&tui.SessionOps{
		Fork: func() error {
			if running.Load() {
				return fmt.Errorf("a turn is running — Esc cancels it first")
			}
			// A fresh session lives memory-only until its first
			// assistant message — materialize it so the fork has a
			// source file to copy.
			if store.Path() == "" {
				if _, err := store.EnsureOnDisk(
					session.SessionFilePath(config.DataDir(), cwd, time.Now(), store.ID()), session.Options{}); err != nil {
					return err
				}
			}
			fork, err := session.ForkSession(store.Path(),
				session.SessionFilePath(config.DataDir(), cwd, time.Now(), session.NewSessionID()), "")
			if err != nil {
				return err
			}
			return swapStoreTo(fork)
		},
		Dump: func() (string, error) {
			return dumpSession(store)
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
		// summarizeAndBranch appends the branch_summary AND moves the leaf;
		// only the transcript refresh is left here (a second store.Branch
		// would append a redundant marker branch).
		SummarizeAndBranch: func(entryID string) error {
			if err := summarizeAndBranch(store, entryID); err != nil {
				return err
			}
			branchReplay()
			return nil
		},
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
			if running.Load() {
				return fmt.Errorf("a turn is running — Esc cancels it first")
			}
			if err := store.ResetLeaf(); err != nil {
				return err
			}
			app.Reset()
			return nil
		},
		Drop: func() error {
			if !running.CompareAndSwap(false, true) {
				return fmt.Errorf("a turn is running — Esc cancels it first")
			}
			defer running.Store(false)
			return swapStore(true)
		},
	})
	// /model: current + available from settings roles, switch by ref.
	app.SetModelOps(&tui.ModelOps{
		Current: func() string {
			modelMu.Lock()
			defer modelMu.Unlock()
			return live.provName + "/" + live.model
		},
		List: func() []string {
			return availableModelRefs(lastSettings())
		},
		Set: func(ref string) error {
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
			at2.Sink = &askCardSink{app: app, fallback: tool.NewHeadlessAskSink(lastSettings().AskTimeout())}
		}
	}
	if mem := buildMemory(lastSettings()); mem != nil {
		app.SetMemoryOps(&tui.MemoryOps{
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
		})
	}
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
		Set: func(on bool) error { planMode.Active = on; return nil },
	})

	app.SetGoalOps(&tui.GoalOps{
		View: func() string {
			gs := agent.GoalStateOf(reg)
			if gs == nil {
				return "goal: not wired"
			}
			return gs.Describe()
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
	app.SetThemeOps(&tui.ThemeOps{
		Current: func() string { return th.Name },
		List:    func() []string { return theme.AvailableThemes(theme.CustomDir()) },
		Set: func(name string) error {
			nt := theme.LoadNamed(name, theme.CustomDir())
			if nt == nil || (nt.Name != name && name != "") {
				// LoadNamed falls back on failure; only accept an exact hit
				// so a typo is reported rather than silently ignored.
				for _, avail := range theme.AvailableThemes(theme.CustomDir()) {
					if avail == name {
						app.SetTheme(applyTheme(nt))
						th = nt
						return nil
					}
				}
				return fmt.Errorf("unknown theme %q", name)
			}
			app.SetTheme(applyTheme(nt))
			th = nt
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
		collabMu         sync.Mutex
		collabHost       *collab.Host
		collabGuest      *collab.Guest
		collabTurnCancel context.CancelFunc
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
					Interrupt: func() {
						collabMu.Lock()
						cancel := collabTurnCancel
						collabMu.Unlock()
						if cancel != nil {
							cancel()
						}
					},
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

	app.SetHandlers(
		func(text string) {
			// Joined as a guest: the host owns the turn, so send the
			// prompt over the room instead of starting one here.
			if tui.Collab != nil && tui.Collab.Forward != nil && tui.Collab.Forward(text) {
				return
			}
			if !running.CompareAndSwap(false, true) {
				app.AddSystemBlock("a turn is already running — Esc cancels it")
				return
			}
			sessMu.Lock()
			msg := ai.Message{
				Role:        ai.RoleUser,
				Content:     []ai.Block{ai.TextBlock{Text: text}},
				Attribution: "user",
				UserTS:      time.Now().UnixMilli(),
			}
			sessMu.Unlock()
			if err := store.Append(&session.MessageEntry{Message: msg}); err != nil {
				logx.Errorf("persist user message: %v", err)
			}
			ctx, cancel := context.WithCancel(baseCtx)
			// Published so a full-link guest's interrupt can cancel the
			// live turn (baseCancel would kill every future turn).
			collabMu.Lock()
			collabTurnCancel = cancel
			collabMu.Unlock()
			go func() {
				defer cancel()
				defer running.Store(false)
				defer func() {
					collabMu.Lock()
					collabTurnCancel = nil
					collabMu.Unlock()
				}()
				app.SetRunning(true)
				feedAdvisor := func() {}
				modelMu.Lock()
				lp, lm, lpn, le := live.prov, live.model, live.provName, live.effort
				modelMu.Unlock()
				ag := &agent.Agent{
					Provider: lp,
					Tools:    reg,
					// feedAdvisor is assigned after the agent exists, so go
					// through an indirection: a direct field copy would
					// capture the nil func at literal time.
					Hooks:      &tuiHooks{ts: ts, feed: func() { feedAdvisor() }},
					TTSR:       agent.NewTTSR(lastSettings().TTSR),
					MaxTokens:  opts.MaxTokens,
					MaxTurns:   opts.MaxTurns,
					Model:      lm,
					Store:      store,
					Compaction: agent.CompactionConfig{ContextWindow: modelWindow(cfg, lpn, lm), Methods: agent.ParseMethodOrder(lastSettings().CompactionMethodOrder())},
					Failovers:  failoverChain(cfg, lpn, lm),
					Thinking:   effortBudget(le),
					// Intercept set below from exts (only when non-nil).
					Policy: agentPolicy(),
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
				hookBus := buildHookBus(cwd, opts)
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
				_, err := ag.Run(ctx, hookBus.Context(ctx, buildSys()), hist)
				app.EndAssistant()
				app.FinishRun()
				if err != nil {
					if ctx.Err() != nil {
						app.AddSystemBlock("· turn canceled")
					} else {
						app.AddSystemBlock("error: " + err.Error())
					}
				}
			}()
		},
		func() { baseCancel() }, // Esc: abort the in-flight turn (all runs share baseCtx)

		func() { app.Quit() },
	)

	app.Run() // blocks until Quit
	return 0, nil
}

// tuiSession accumulates the live conversation and persists messages.
type tuiSession struct {
	store    *session.Store
	app      *tui.App
	model    string
	api      string
	provider string
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
		h.ts.app.AddSystemBlock("stream error: " + ev.Err.Error())
	}
}

// shortSessionID renders the first 8 chars of a session id (matches the TUI
// status line convention).
func shortSessionID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (h *tuiHooks) OnToolStart(call ai.ToolCallBlock) {
	preview := strings.Join(strings.Fields(string(call.Arguments)), " ")
	if len(preview) > 120 {
		preview = preview[:120] + "…"
	}
	h.ts.app.AddToolBlock(call.Name, preview)
}

func (h *tuiHooks) OnToolEnd(call ai.ToolCallBlock, res tool.Result, dur time.Duration) {
	h.ts.app.FinishTool(call.Name, res.IsError, res.Text, dur.Round(time.Millisecond).String())
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

// availableModelRefs lists the switchable model refs: the settings default,
// each provider's configured models (from models.yml refs cached in roles),
// and the @role aliases — the same superset the model flag accepts. Dedup
// keeps the list stable. ponytail: runtime discovery (network) is not part
// of this listing; a provider's models that appear only via its discovery
// endpoint are not enumerated here.
func availableModelRefs(settings *config.Settings) []string {
	seen := map[string]bool{}
	var out []string
	add := func(ref string) {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[ref] {
			return
		}
		seen[ref] = true
		out = append(out, ref)
	}
	if settings != nil {
		if settings.DefaultModel != "" {
			add(settings.DefaultModel)
		}
		for _, role := range roleNamesSorted(settings.ModelRoles) {
			add("@" + role) // /model "@smol" is a valid resolveModel input
			add(settings.ModelRoles[role])
		}
	}
	// Add each model ref present in any role value (dedup handles repeats).
	return out
}

func roleNamesSorted(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// resumePickerItems lists resumable sessions as picker rows across ALL
// projects (session.List already scans every bucket): subagent children
// are filtered out (same rules as the text listing); session.List is
// newest-first. Rows carry InCwd so the TUI can window the
// current-folder scope without a second scan, and Pinned from the
// session-pins.json sidecar. Capped at 50 — enough for Tab-all-projects
// browsing while the picker windows to 8 visible rows.
func resumePickerItems(cwd string) []tui.PickerItem {
	metas, err := session.List(config.DataDir())
	if err != nil {
		return nil
	}
	pins := loadSessionPins()
	var out []tui.PickerItem
	for _, m := range metas {
		if m.TitleSource == "subagent" || len(m.ID) < 8 {
			continue
		}
		out = append(out, tui.PickerItem{
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
func searchPickerItems(cwd, query string) []tui.PickerItem {
	items := resumePickerItems(cwd)
	tokens := strings.Fields(strings.ToLower(query))
	if len(tokens) == 0 {
		return items
	}
	type ranked struct {
		item  tui.PickerItem
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
	res := make([]tui.PickerItem, 0, len(out))
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
	root := session.SessionsRoot(config.DataDir())
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
		case *session.ResetBoundaryEntry:
			te.Summary = "(reset boundary)"
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

// askCardSink is the ask tool's TUI answer path (#36): the question is
// surfaced as a system block, then the headless timeout→recommended policy
// answers it. The interactive option card is #46.
type askCardSink struct {
	app      *tui.App
	fallback tool.AskSink
}

func (s *askCardSink) Ask(ctx context.Context, req tool.AskRequest) (tool.AskResponse, error) {
	var b strings.Builder
	b.WriteString("ask: " + req.Question)
	for _, o := range req.Options {
		b.WriteString("\n  - " + o.Label)
		if o.Description != "" {
			b.WriteString(": " + o.Description)
		}
	}
	if req.Multi {
		b.WriteString("\n  (multi-select)")
	}
	if len(req.Recommended) > 0 {
		b.WriteString("\n  recommended: " + strings.Join(req.Recommended, ", "))
	}
	s.app.AddSystemBlock(b.String())
	return s.fallback.Ask(ctx, req)
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
