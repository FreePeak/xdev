package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/fscache"
	hookbus "github.com/FreePeak/xdev/internal/hooks"
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
		store = ns
		ts.store = ns
		wireTaskParent(reg, ns)
		app.Reset()
		saveBreadcrumb(ns.Path())
		if res, err := session.BuildContext(ns.Entries(), ns.LeafID(), session.SystemPrompt{}); err == nil {
			replayTranscript(app, res.Messages)
		}
		app.AddSystemBlock("· session " + shortSessionID(ns.ID()) + " — " + ns.Title())
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
	app.SetSessionTree(func() string { return store.Tree() })
	app.SetSessionBranch(func(args string) error {
		query := strings.TrimSpace(args)
		if query == "" {
			return fmt.Errorf("branch: entry-id prefix required")
		}
		for _, e := range store.Entries() {
			env := e.Envelope()
			if strings.HasPrefix(env.ID, query) {
				if err := store.Branch(env.ID); err != nil {
					return fmt.Errorf("branch: %v", err)
				}
				// Replay the new branch's transcript into the TUI.
				if res, err := session.BuildContext(store.Entries(), store.LeafID(), session.SystemPrompt{}); err == nil {
					app.Reset()
					replayTranscript(app, res.Messages)
					app.AddSystemBlock("· branched to " + env.ID[:8] + " — replayed")
				}
				return nil
			}
		}
		return fmt.Errorf("branch: no entry matching %q", query)
	})
	app.SetLocation(cwd)
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
				// Interactive picker (omp/Claude Code /resume): rows are
				// this project's sessions, newest first; Up/Down + Enter
				// resumes, Esc closes. No candidates → text listing.
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
			return nil
		},
	})
	// In the TUI, propose HOLDS the decision: the plan shows in the tool
	// block, the model is told to wait, and the user resolves with /plan
	// off (approve) or feedback (revise).
	planMode.Propose = agent.NewProposeTool(planMode, func(context.Context, string) (bool, string) {
		return false, "awaiting user review — the user will /plan off to approve or send revision feedback"
	})
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
			return fmt.Sprintf("prewalk on — hands off to %s/%s after the first edit/write", prewalkTarget.Provider.Name(), prewalkTarget.Model)
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
	if themeName != "" {
		stopWatch := theme.Watch(theme.CustomDir(), themeName, func(nt *theme.Theme) {
			app.SetTheme(nt)
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
						app.SetTheme(nt)
						th = nt
						return nil
					}
				}
				return fmt.Errorf("unknown theme %q", name)
			}
			app.SetTheme(nt)
			th = nt
			return nil
		},
	})
	app.SetCommandDir(cwd)

	app.SetHandlers(
		func(text string) {
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
			go func() {
				defer cancel()
				defer running.Store(false)
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
				agentMu.Lock()
				curAgent = ag
				hookBus := hookbus.FromSettings(lastSettings().Hooks)
				if c := agent.NewChain(hookBus, exts); c != nil {
					ag.Intercept = c
				}
				agentMu.Unlock()
				defer func() {
					agentMu.Lock()
					curAgent = nil
					agentMu.Unlock()
				}()
				sessMu.Lock()
				hist := rebuildHistory() // store mirror is authoritative
				sessMu.Unlock()
				_, err := ag.Run(ctx, buildSys(), hist)
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

// resumePickerItems lists this project's resumable sessions as picker
// rows: short id, title, mtime, size. Subagent children and other
// projects are filtered out (same rules as the text listing); session.List
// is newest-first. Capped at 12 rows — the picker windows, but a giant
// list is not useful to scroll through either.
func resumePickerItems(cwd string) []tui.PickerItem {
	metas, err := session.List(config.DataDir())
	if err != nil {
		return nil
	}
	var out []tui.PickerItem
	for _, m := range metas {
		if m.CWD != cwd || m.TitleSource == "subagent" || len(m.ID) < 8 {
			continue
		}
		out = append(out, tui.PickerItem{
			ID:    m.ID[:8],
			Title: m.Title,
			Mtime: m.ModTime.Format("Jan 02 15:04"),
			Size:  humanSize(m.SizeBytes),
		})
		if len(out) >= 12 {
			break
		}
	}
	return out
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
