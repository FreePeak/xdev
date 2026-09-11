package main

import (
	"context"
	"fmt"
	"os"
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
		routeToLiveAgent(func(a *agent.Agent, s string) { a.FollowUp(s) }))
	if exts != nil {
		defer exts.Close()
	}

	overrides := agent.LoadSystemPromptOverrides(cwd)
	buildSys := promptFnWithMemory(basePrompt(opts, cwd), cwd, reg,
		tailSystemPrompt(overrides, opts.AppendSystem), buildMemory(lastSettings()))

	// Session.
	store, err := openSession(cwd, opts.ContinueLast, opts.ResumePrefix)
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
	// setLiveModel applies a resolved model everywhere it matters: the live
	// holder the next submit reads, the status line, and the task tool —
	// children must spawn on the current model, not the one captured at
	// startup (the same fix RPC's set_model already makes). Logging the
	// change to the session is the caller's call: a resume SEEDS from the
	// log and must not append a duplicate entry.
	setLiveModel := func(p ai.Provider, pname, mname, effort string) {
		modelMu.Lock()
		live.prov, live.model, live.provName, live.effort = p, mname, pname, effort
		modelMu.Unlock()
		app.SetStatusModel(pname + "/" + mname)
		if t, ok := reg.Get(agent.TaskToolName); ok {
			if tt, ok := t.(*agent.TaskTool); ok {
				tt.Provider, tt.Model = p, mname
			}
		}
	}

	// buildModel resolves a concrete provider/model to a provider.
	buildModel := func(ref string) (ai.Provider, string, string, error) {
		pname, mname, err := config.ParseModelRef(ref)
		if err != nil {
			return nil, "", "", err
		}
		pc, ok := cfg.Providers[pname]
		if !ok {
			return nil, "", "", fmt.Errorf("unknown provider %q (have: %v)", pname, providerKeys(cfg))
		}
		np, err := buildProvider(pname, pc, mname, cfg)
		if err != nil {
			return nil, "", "", err
		}
		return np, pname, mname, nil
	}

	// seedModelFromSession honors a resumed session's last model_change: the
	// switch is recorded in the file, and a relaunch that ignored it would
	// quietly run a different model than the user left. An explicit -model
	// flag still wins.
	seedModelFromSession := func(s *session.Store) {
		if opts.Model != "" || len(s.Entries()) == 0 {
			return
		}
		res, err := session.BuildContext(s.Entries(), s.LeafID(), session.SystemPrompt{})
		if err != nil || res.Model == "" {
			return
		}
		// History can carry a ref that no longer resolves (a deleted model,
		// a pre-validation typo): keep the configured model rather than
		// adopting something that would 404 on the next turn.
		if _, _, rerr := resolveModel(res.Model, cfg, lastSettings()); rerr != nil {
			logx.Debugf("resume model %q unusable: %v", res.Model, rerr)
			return
		}
		np, pname, mname, err := buildModel(res.Model)
		if err != nil {
			logx.Debugf("resume model %q: %v", res.Model, err)
			return
		}
		setLiveModel(np, pname, mname, "")
	}
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
		seedModelFromSession(store)
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

	ts := &tuiSession{store: store, app: app}

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
		saveBreadcrumb(breadcrumbPath(ns))
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
		saveBreadcrumb(breadcrumbPath(ns))
		if res, err := session.BuildContext(ns.Entries(), ns.LeafID(), session.SystemPrompt{}); err == nil {
			replayTranscript(app, res.Messages)
		}
		seedModelFromSession(ns)
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

	// /resume with no argument opens the session picker (recent sessions
	// in this directory); Enter hands the full id back to Resume, which
	// resolves by prefix.
	recentSessions := func() []tui.ResumeOption {
		metas, err := session.List(config.DataDir())
		if err != nil {
			return nil
		}
		out := make([]tui.ResumeOption, 0, 12)
		for _, m := range metas {
			if m.CWD != cwd || m.TitleSource == session.TitleSourceSubagent {
				continue
			}
			// The live session is not a resume target; the row list is
			// title-led with the id second, since /resume <prefix> takes
			// the id but the title is what a user recognizes.
			if m.ID == store.ID() {
				continue
			}
			out = append(out, tui.ResumeOption{
				ID:     m.ID,
				Title:  m.Title,
				Detail: fmt.Sprintf("%s · %s", m.ID[:8], m.ModTime.Format("Jan 02 15:04")),
			})
			if len(out) >= 12 {
				break
			}
		}
		return out
	}

	app.SetSessionTree(func() string { return store.Tree() })
	app.SetSessionBranch(func(args string) error {
		query := strings.TrimSpace(args)
		if query == "" {
			return fmt.Errorf("branch: entry-id prefix required (ids are listed by /tree)")
		}
		// Only message entries are branch targets: a leaf on a marker
		// (model_change, reset boundary) would terminate the context in a
		// non-message, and prefixes must be unambiguous — first-match-wins
		// silently picked a different entry than the user meant.
		var (
			match   session.Entry
			matches int
		)
		for _, e := range store.Entries() {
			if strings.HasPrefix(e.Envelope().ID, query) {
				match, matches = e, matches+1
			}
		}
		if matches == 0 {
			return fmt.Errorf("branch: no entry matching %q (entries before the last /clear or compaction are not addressable)", query)
		}
		if matches > 1 {
			return fmt.Errorf("branch: %q matches %d entries — use a longer prefix", query, matches)
		}
		env := match.Envelope()
		if _, ok := match.(*session.MessageEntry); !ok {
			return fmt.Errorf("branch: %s is a %s entry — /branch switches to a message", env.ID[:8], env.Type)
		}
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
	})
	app.SetLocation(cwd)
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
				// Unreachable from the TUI (the picker supplies an id);
				// kept as a guard for hosts that wire Recent == nil.
				return fmt.Errorf("resume: session id prefix required")
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
		Recent: recentSessions,
		Drop: func() error {
			if !running.CompareAndSwap(false, true) {
				return fmt.Errorf("a turn is running — Esc cancels it first")
			}
			defer running.Store(false)
			return swapStore(true)
		},
	})

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
		Set: func(ref string) error {
			nr, ne, err := resolveModel(ref, cfg, lastSettings())
			if err != nil {
				return err
			}
			nprov, nprovName, nmodelName, err := buildModel(nr)
			if err != nil {
				return err
			}
			if err := store.Append(&session.ModelChangeEntry{Model: nprovName + "/" + nmodelName}); err != nil {
				logx.Errorf("model change entry: %v", err)
			}
			setLiveModel(nprov, nprovName, nmodelName, ne)
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
						known = true
					}
				}
				if !known {
					return fmt.Errorf("unknown theme %q", name)
				}
			}
			app.SetTheme(nt)
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
	app.SetCommandDir(cwd)

	app.SetHandlers(
		func(text string) {
			if !running.CompareAndSwap(false, true) {
				app.AddSystemBlock("a turn is already running — Esc cancels it")
				return
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
					Compaction: agent.CompactionConfig{ContextWindow: modelWindow(cfg, lpn, lm)},
					Failovers:  failoverChain(cfg, lpn, lm),
					Thinking:   effortBudget(le),
					// Intercept set below from exts (only when non-nil).
					Policy: agentPolicy(),
				}
				if t := resolvePrewalk(opts, cfg, lastSettings()); t != nil {
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

// breadcrumbPath prefers the materialized file path and falls back to the
// auto-persist target, so a session that has not written its first
// assistant message yet still owns the pane's --continue breadcrumb.
func breadcrumbPath(s *session.Store) string {
	if p := s.Path(); p != "" {
		return p
	}
	return s.AutoPath()
}

// titleFromPrompt derives a session title from the first user prompt: the
// first line, whitespace-collapsed, capped at 40 runes. Empty prompts yield
// "" (the openSession fallback keeps its timestamped title).
func titleFromPrompt(text string) string {
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(text), "\n", 2)[0])
	line = strings.Join(strings.Fields(line), " ")
	runes := []rune(line)
	if len(runes) > 40 {
		return string(runes[:40]) + "…"
	}
	return line
}

// tuiSession accumulates the live conversation and persists messages.
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
			views = append(views, tui.PickerView{Name: name, Items: mine, Action: "use"})
		}
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
