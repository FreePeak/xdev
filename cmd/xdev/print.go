package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/browser"
	"github.com/FreePeak/xdev/internal/computer"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/dap"
	"github.com/FreePeak/xdev/internal/eval"
	"github.com/FreePeak/xdev/internal/ext"
	hookbus "github.com/FreePeak/xdev/internal/hooks"
	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/lsp"
	"github.com/FreePeak/xdev/internal/mcpclient"
	"github.com/FreePeak/xdev/internal/memory"
	"github.com/FreePeak/xdev/internal/rules"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/skills"
	"github.com/FreePeak/xdev/internal/tiny"
	"github.com/FreePeak/xdev/internal/tool"
	"github.com/FreePeak/xdev/internal/tts"
)

// printOptions configures one one-shot run.
type printOptions struct {
	Model        string
	ContinueLast bool
	SystemPrompt string
	AppendSystem string
	// Personality is the preset tail (default|friendly|pragmatic|none);
	// empty falls back to settings.personality. PERSONALITY.md beats it.
	Personality  string
	ResumePrefix string
	MaxTurns     int
	MaxTokens    int
	// FromClaude/FromCodex import a foreign transcript (id prefix or path)
	// and continue it as a new xdev session (issue #28).
	FromClaude string
	FromCodex  string
	// ForkID forks a session by id prefix or path and continues the fork
	// (issue #11: --fork <id|path>).
	ForkID string
	// Prewalk enables the one-shot model handoff (research §5); the target
	// defaults to the @smol role.
	Prewalk     bool
	PrewalkInto string
	// Hooks are extra --hook specs (event=command, or a discovered hook
	// name); TrustedExtensions allowlists extension hook directories.
	Hooks             []string
	TrustedExtensions []string
	// Plan starts the run in plan mode (read-only + propose exit).
	Plan bool
	// PlanYolo auto-approves the first proposal (#36); PlanYoloInto is the
	// execution model to hand off to after that acceptance ("" = stay).
	PlanYolo     bool
	PlanYoloInto string
}

// buildAdvisor constructs the background reviewer when enabled. The
// reviewer model comes from the @advisor role; a missing role warns and
// disables (never fails the run).
func buildAdvisor(cfg *config.Config, settings *config.Settings) *agent.Advisor {
	if settings == nil || (!settings.Advisor && !launch.Advisor) {
		return nil
	}
	ref, _, err := resolveModel("@advisor", cfg, settings)
	if err != nil {
		logx.Errorf("advisor: modelRoles.advisor unresolved, disabled: %v", err)
		return nil
	}
	pName, mName, err := config.ParseModelRef(ref)
	if err != nil {
		logx.Errorf("advisor: %v, disabled", err)
		return nil
	}
	pc, ok := cfg.Providers[pName]
	if !ok {
		logx.Errorf("advisor: unknown provider %q, disabled", pName)
		return nil
	}
	prov, err := buildProvider(pName, pc, mName, cfg)
	if err != nil {
		logx.Errorf("advisor: provider unavailable, disabled: %v", err)
		return nil
	}
	// The reviewer sees the repo read-only plus its advise channel.
	reg := tool.NewRegistry()
	reg.Register(tool.NewReadTool())
	reg.Register(&tool.GrepTool{CWD: mustGetwd()})
	reg.Register(&tool.GlobTool{CWD: mustGetwd()})
	agent.RegisterAdviseTool(reg)
	return buildAdvisorRuntime(cfg, settings, prov, mName, reg)
}

// buildAdvisorRuntime fills an advisor with the M11 #39 tails: settings
// (immuneTurns, syncBacklog), WATCHDOG.md guidance, and the WATCHDOG.yml
// roster. A roster replaces the single legacy reviewer with one named
// reviewer per entry, each fed only the deltas matching its patterns.
func buildAdvisorRuntime(cfg *config.Config, settings *config.Settings, prov ai.Provider, model string, reg *tool.Registry) *agent.Advisor {
	wd, _ := os.Getwd()
	entries, instructions := agent.DiscoverWatchdogRoster(wd, config.DataDir())
	guidance := agent.DiscoverWatchdogGuidance(wd, config.DataDir())
	if instructions != "" {
		if guidance != "" {
			guidance += "\n\n"
		}
		guidance += instructions
	}
	peer := func(p ai.Provider, m string, patterns []string, r *tool.Registry) *agent.Advisor {
		a := agent.NewAdvisor(p, m, r)
		a.Guidance = guidance
		a.ImmuneTurns = settings.AdvisorImmuneTurns
		a.SyncBacklog = settings.AdvisorSyncBacklog
		a.Patterns = patterns
		return a
	}
	if len(entries) == 0 {
		return peer(prov, model, nil, reg)
	}
	peers := make([]*agent.Advisor, 0, len(entries))
	for _, e := range entries {
		p, m := prov, model
		// An entry may name its own model; an unresolvable one falls back
		// to the session's resolved reviewer model.
		if e.Model != "" {
			pName, mName, err := config.ParseModelRef(e.Model)
			switch {
			case err != nil:
				logx.Errorf("advisor %s: %v; using %s", e.Name, err, model)
			case cfg.Providers[pName] == nil:
				logx.Errorf("advisor %s: unknown provider %q; using %s", e.Name, pName, model)
			default:
				np, perr := buildProvider(pName, cfg.Providers[pName], mName, cfg)
				if perr != nil {
					logx.Errorf("advisor %s: provider unavailable; using %s", e.Name, model)
				} else {
					p, m = np, mName
				}
			}
		}
		r := tool.NewRegistry()
		r.Register(tool.NewReadTool())
		r.Register(&tool.GrepTool{CWD: wd})
		r.Register(&tool.GlobTool{CWD: wd})
		agent.RegisterAdviseTool(r)
		peers = append(peers, peer(p, m, e.Patterns, r))
	}
	return agent.NewAdvisorRoster(peers)
}

// buildChildAdvisorFactory resolves settings task.agentAdvisor (M11 #39)
// into the ChildAdvisor seam on the task tool: "on" → the advisor role's
// model, an explicit value → that model reference, "off"/unset → nil
// (children run unadvised, the omp default). The model resolve is lazy so
// a session that never spawns a child never pays for it.
func buildChildAdvisorFactory(settings *config.Settings) func() *agent.Advisor {
	v := ""
	if settings != nil {
		v = strings.TrimSpace(settings.TaskAgentAdvisor)
	}
	if v == "" || strings.EqualFold(v, "off") || strings.EqualFold(v, "false") {
		return nil
	}
	ref := "@advisor"
	if !strings.EqualFold(v, "on") && !strings.EqualFold(v, "true") {
		ref = v
	}
	return func() *agent.Advisor {
		cfg, err := config.LoadModelsLayered()
		if err != nil {
			logx.Errorf("task agentAdvisor %q: %v; children unadvised", v, err)
			return nil
		}
		resolved, _, err := resolveModel(ref, cfg, settings)
		if err != nil {
			logx.Errorf("task agentAdvisor %q: %v; children unadvised", v, err)
			return nil
		}
		pName, mName, err := config.ParseModelRef(resolved)
		if err != nil {
			logx.Errorf("task agentAdvisor %q: %v; children unadvised", v, err)
			return nil
		}
		pc, ok := cfg.Providers[pName]
		if !ok {
			logx.Errorf("task agentAdvisor %q: unknown provider %q; children unadvised", v, pName)
			return nil
		}
		prov, err := buildProvider(pName, pc, mName, cfg)
		if err != nil {
			logx.Errorf("task agentAdvisor %q: provider unavailable: %v; children unadvised", v, err)
			return nil
		}
		wd, _ := os.Getwd()
		reg := tool.NewRegistry()
		reg.Register(tool.NewReadTool())
		reg.Register(&tool.GrepTool{CWD: wd})
		reg.Register(&tool.GlobTool{CWD: wd})
		agent.RegisterAdviseTool(reg)
		adv := agent.NewAdvisor(prov, mName, reg)
		adv.Guidance = agent.DiscoverWatchdogGuidance(wd, config.DataDir())
		adv.ImmuneTurns = settings.AdvisorImmuneTurns
		return adv
	}
}

// wireAgentMode applies the seams EVERY mode must share on its agent: the
// deferred-tool catalog bridge (tool_call runs through this agent's own call
// path, so a catalogued call is policy-gated like a direct one) and the
// secrets redactor (placeholders out to the provider, real values back in on
// tool args). Both were once print-only — modes that hand-build an agent drift
// silently, which is why the wiring lives in one function with one test
// (#79 catalog, #80 redactor).
func wireAgentMode(ag *agent.Agent, reg *tool.Registry, cfg *config.Config, settings *config.Settings, role, provider, model, cwd string) *agent.FallbackState {
	if ag == nil {
		return nil
	}
	if reg != nil {
		ag.WireCatalog(reg.Catalog())
	}
	ag.Redactor = redactorFor(cwd)
	// #83: snapcompact's bitmap only helps a model that can read images.
	ag.Vision = func() bool { return modelVision(cfg, provider, model) }
	// M5 #25 depth (#84): arm the fallback state so the reserve policy,
	// cooldown revert and credential rotation actually run — the engine was
	// complete and unit-tested with no production caller, so a spent key
	// failed the run instead of stepping to its apiKeys sibling.
	// #82: the two compaction triggers were parsed, validated and listed in
	// `config list`, but no build site copied them into CompactionConfig, so
	// neither the idle boundary nor the async summarize could ever fire.
	if settings != nil {
		ag.Compaction.IdleAfter = settings.CompactionIdleAfter()
		ag.Compaction.Async = settings.CompactionAsyncOn()
	}
	// #86: a compaction summary must carry the memories the remote backend
	// recalled, or they are lost for the rest of the session.
	if h := hindsightFrom(settings); h != nil {
		ag.MemoryContext = h.CompactionContext
	}
	st := ag.ArmFallback(settings, role)
	if st != nil {
		st.Rotate = func(provider string) (ai.Provider, bool) {
			return rotateProviderCredential(cfg, provider, ag.Model)
		}
	}
	// #108: a rulebook rule scoped by globs (globs: *.go) was discovered,
	// listed in the prompt and rendered as an edit/write "shorthand", but
	// nothing consumed it. The matched guidance now rides the tool result of
	// the change it applies to, which is where the model acts on it.
	ag.Rulebook = rulebookNoteFor
	return st
}

// rulebookBudget caps one injected rulebook notice: rules can be long, and
// the tool result also carries the edit summary the model needs.
const rulebookBudget = 2400

// rulebookNoteFor renders the rules scoped to one touched path. Empty when
// nothing matches (the common case) so no noise enters the transcript.
func rulebookNoteFor(path string) string {
	matched := rules.ForPath(path)
	if len(matched) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "rulebook rules for %s (scoped by glob — these apply to this file):", path)
	used := 0
	for _, r := range matched {
		body := strings.TrimSpace(r.Content)
		if body == "" {
			continue
		}
		if r.Description != "" {
			body = r.Description + "\n\n" + body
		}
		if used+len(body) > rulebookBudget {
			fmt.Fprintf(&b, "\n\n[%d more scoped rule(s) omitted for space: %s]",
				len(matched)-1, strings.Join(ruleNames(matched), " "))
			break
		}
		used += len(body)
		fmt.Fprintf(&b, "\n\n### %s (%s)\n%s", r.Name, r.Source, body)
	}
	return b.String()
}

func ruleNames(rs []rules.Rule) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Name
	}
	return out
}

// modelVision reports whether a provider's model entry declares image input
// (models.yml `vision: true`). An unknown provider/model is false: the text
// path always works, so the conservative answer is to skip the bitmap.
func modelVision(cfg *config.Config, provider, model string) bool {
	if cfg == nil {
		return false
	}
	pc := cfg.Providers[provider]
	if pc == nil {
		return false
	}
	for _, m := range append(append([]config.ModelConfig(nil), pc.Models...), providerModels(provider, pc)...) {
		if m.ID == model {
			return m.Vision
		}
	}
	return false
}

// redactorFor opens the secrets redactor for one workspace, memoized per
// directory: the agent loop and the learn tool must scrub with the SAME
// configured values, and re-reading secrets.yml per registration is a wasted
// open on every model switch.
var (
	redactorMu    sync.Mutex
	redactorCache = map[string]*config.Redactor{}
)

func redactorFor(cwd string) *config.Redactor {
	redactorMu.Lock()
	defer redactorMu.Unlock()
	if r, ok := redactorCache[cwd]; ok {
		return r
	}
	r := config.OpenRedactor(cwd, func(w string) { logx.Debugf("%s", w) })
	redactorCache[cwd] = r
	return r
}

// modelRoleRef returns the role name a model reference was resolved from
// ("" for a literal provider/model). retry.fallbackChains keys can name a
// role, so the chain engine needs this to find a role-scoped chain after a
// role reassignment.
func modelRoleRef(ref string) string {
	if !strings.HasPrefix(ref, "@") {
		return ""
	}
	name := strings.TrimPrefix(ref, "@")
	if i := strings.IndexByte(name, ':'); i >= 0 {
		name = name[:i]
	}
	return name
}

// advisorDrainCap bounds the final headless review at run exit. Thirty
// seconds matches the TUI's error drain; a stalled reviewer must not hold
// the process.
const advisorDrainCap = 30 * time.Second

// advisorHistory rebuilds the reviewer's feed snapshot from the session
// store — the same authoritative view the TUI feeds.
func advisorHistory(store *session.Store) []ai.Message {
	res, err := session.BuildContext(store.Entries(), store.LeafID(), session.SystemPrompt{})
	if err != nil {
		return nil
	}
	return res.Messages
}

// resolvePrewalk builds the handoff target for a run. Returns nil when
// prewalk is off or the target cannot resolve (warn, start unarmed — the
// run proceeds on the primary model).
func resolvePrewalk(opts printOptions, cfg *config.Config, settings *config.Settings) *agent.FailoverTarget {
	if launch.NoPrewalk {
		return nil // --no-prewalk beats the flag, the setting and the profile
	}
	// The handoff is armed by either the flag or prewalk.enabled; the target
	// is --prewalk-into, then prewalk.into, then @smol.
	enabled := opts.Prewalk || (settings != nil && settings.Prewalk.Enabled)
	if !enabled {
		return nil
	}
	into := opts.PrewalkInto
	if strings.TrimSpace(into) == "" && settings != nil {
		into = settings.Prewalk.Into
	}
	if strings.TrimSpace(into) == "" {
		into = "@smol"
	}
	return resolveInto(into, cfg, settings, "prewalk")
}

// resolveInto resolves a model ref or @role to a handoff target (prewalk,
// plan-yolo). Returns nil after a warning when it cannot resolve: the run
// starts on the primary model rather than failing.
func resolveInto(refArg string, cfg *config.Config, settings *config.Settings, label string) *agent.FailoverTarget {
	ref, _, err := resolveModel(refArg, cfg, settings)
	if err != nil {
		logx.Errorf("%s: target %q unresolved, starting on primary: %v", label, refArg, err)
		return nil
	}
	pName, mName, err := config.ParseModelRef(ref)
	if err != nil {
		logx.Errorf("%s: target %q invalid, starting on primary: %v", label, ref, err)
		return nil
	}
	pc, ok := cfg.Providers[pName]
	if !ok {
		logx.Errorf("%s: unknown provider %q, starting on primary", label, pName)
		return nil
	}
	prov, err := buildProvider(pName, pc, mName, cfg)
	if err != nil {
		logx.Errorf("%s: provider for %q unavailable, starting on primary: %v", label, ref, err)
		return nil
	}
	return &agent.FailoverTarget{Provider: prov, Model: mName}
}

func runPrint(prompt string, opts printOptions) (exitCode int, err error) {
	cwd, err := os.Getwd()
	if err != nil {
		return 2, err
	}

	// --- config & model resolution ---
	cfg, err := config.LoadModelsLayered()
	if err != nil {
		return 2, fmt.Errorf("load config: %w", err)
	}
	settings := lastSettings()
	modelRef, effortRef, err := resolveModel(opts.Model, cfg, settings)
	if err != nil {
		return 2, err
	}
	// --thinking overrides whatever the model role pinned.
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

	// --- tools ---
	planMode := &agent.PlanMode{Active: opts.Plan || opts.PlanYolo}
	reg := newToolRegistry(cwd, prov, provName, modelName, settings, effortBudget(effortRef), planMode)
	defer closeSharedHub() // hub-started children are session-scoped (T3 #8)

	// MCP servers (optional; absent config = nothing happens).
	mgr := attachMCP(context.Background(), reg, true)
	if mgr != nil {
		defer mgr.Close()
	}

	// --- system prompt ---
	overrides := agent.LoadSystemPromptOverrides(cwd)
	preset := opts.Personality
	if preset == "" && settings != nil {
		preset = settings.Personality
	}
	if err := overrides.ApplyPersonalityPreset(preset); err != nil {
		return 2, err
	}
	mem := buildMemory(settings)
	// A remote memory backend that cannot be reached must say so once,
	// visibly: logx is off in print mode, and this warning names the URL.
	if h, ok := mem.(*memory.Hindsight); ok {
		h.SetWarnSink(func(msg string) { fmt.Fprintln(os.Stderr, "xdev: "+msg) })
	}
	// M15 #73: the sharpshooter backend consolidates friction through the
	// smol role, resolved here where the config is in hand.
	if ss, ok := mem.(*memory.SharpShooter); ok {
		ss.Complete = memoryRoleComplete(cfg, settings, "@smol")
	}
	buildSys := promptFnWithMemory(basePrompt(opts, cwd), cwd, reg,
		tailSystemPrompt(overrides, opts.AppendSystem), mem)
	_ = buildSys // resolved at Run time: late-registered tools must be in the prompt

	// --- session ---
	store, err := openStartupSession(cwd, opts)
	if err != nil {
		return 2, fmt.Errorf("session: %w", err)
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			logx.Errorf("session close: %v", cerr)
		}
	}()
	wireTaskParent(reg, store)

	// --- agent ---
	// M12 #44: mnemopi counts turns; every retainEveryNTurns turns this
	// enqueues a consolidation the exit drain below applies.
	ph := &printHooks{store: store, showThinking: showThinkingOn(settings)}
	hooks := memoryTurnHooks(ph, settings)
	ag := &agent.Agent{Provider: prov, Tools: reg, Hooks: hooks, MaxTokens: opts.MaxTokens, MaxTurns: opts.MaxTurns, Model: modelName, Store: store, Compaction: agent.CompactionConfig{ContextWindow: modelWindow(cfg, provName, modelName), Methods: agent.HandoffOrder(settings.CompactionMethodOrder())}, Failovers: failoverChain(cfg, settings, modelRoleRef(opts.Model), provName, modelName), Thinking: effortBudget(effortRef), PlanMode: planMode}
	if t := resolvePrewalk(opts, cfg, settings); t != nil {
		ag.Prewalk = &agent.Prewalk{Target: *t}
	}
	// Advisor (M11 #12; parity finding T3 #20): the reviewer used to have
	// exactly one caller — the TUI — so --advisor and `advisor: true` were
	// inert in print runs, which is where a long unattended run most wants
	// a watchdog. Feed after each clean turn; a bounded final review runs at
	// exit (below) because steering into a finished run is impossible — the
	// notes are printed instead.
	adv := buildAdvisor(cfg, settings)
	if adv != nil {
		adv.Primary = ag
		ph.advisorFeed = func() { adv.Feed(context.Background(), advisorHistory(store)) }
	}
	// #90: a message from another session arrives as a follow-up into the
	// live run rather than waiting for the model to read the inbox.
	setInboxSink(func(m agent.Message) bool {
		ag.FollowUp(inboxFollowUp(m))
		return true
	})
	defer func() {
		setInboxSink(nil)
		if stopInbox != nil {
			stopInbox()
		}
	}()
	applyPolicy(ag, settings)
	wireAgentMode(ag, reg, cfg, settings, modelRoleRef(opts.Model), provName, modelName, cwd)
	// Stream rules (M11 #35): settings-declared rules watch the deltas.
	// Sessions re-read settings at start, so a change needs a new session
	// (fired state is in-session only, never persisted).
	ag.TTSR = agent.NewTTSR(ttsrConfig(settings))
	// Handoff (M5 #23): the document side request mirrors the live turn's
	// transform on the @smol role, and settings handoff.saveToDisk mirrors
	// the document under <dataDir>/handoffs.
	ag.Handoff = agent.HandoffSettings{SaveDir: handoffSaveDir(settings)}
	if t := resolveInto("@smol", cfg, settings, "handoff"); t != nil {
		ag.Handoff.Target = *t
	}
	// Plan-mode exit: print runs are unattended, so there is no reviewer —
	// propose auto-accepts (a plan nobody can review must not trap the run
	// in read-only). --plan-yolo keeps that and hands the run to the
	// execution model on the first acceptance (#36).
	planMode.Propose = agent.NewProposeTool(planMode, nil)
	if opts.Plan && !opts.PlanYolo {
		// T3 #27: print runs have no reviewer, and auto-accepting made
		// `--plan` silently implement the plan — the flag's read-only
		// promise, voided. A plain headless --plan now ENDS at the
		// proposal; --plan-yolo keeps the approve-and-build behavior
		// explicit.
		planMode.PlanOnly = true
	}
	if opts.PlanYolo {
		planMode.Yolo = true
		if opts.PlanYoloInto != "" {
			if t := resolveInto(opts.PlanYoloInto, cfg, settings, "plan-yolo"); t != nil {
				planMode.OnAccept = func() { ag.SwitchToModel(*t, "plan-yolo") }
			}
		}
	}

	// Extension processes (optional): their tools join the registry and the
	// manager becomes the agent's fail-closed policy interceptor; runtime
	// actions steer the live run.
	exts := attachExtensions(context.Background(), reg, ag.Steer, ag.FollowUp, cfg)
	// Hooks and extensions compose into one interceptor chain; hooks must
	// fire even when no extensions are installed.
	hookBus := buildHookBus(cwd, opts, nil)
	if c := agent.NewChain(hookBus, exts); c != nil {
		ag.Intercept = c
	}
	// A compaction also reaches the bus as `session_compact` (compact.go's
	// OnCompaction call is the emission point; see agent.WithCompactionEvent).
	ag.Hooks = agent.WithCompactionEvent(ag.Hooks, ag.Intercept)
	// #92: omp's session_shutdown fires when the session ends, so a hook can
	// archive or notify on exit rather than only on a switch.
	defer ag.EmitSessionShutdown()
	if exts != nil {
		defer exts.Close()
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	// --max-time bounds the whole run (signal handling still cancels too).
	ctx, cancelTimeout := withMaxTime(ctx, launch.MaxTime)
	defer cancelTimeout()
	// -handoff (M5 #23): document the resumed session before the prompt, so
	// the run streams the handoff document instead of the raw history.
	if handoffMode && len(store.Entries()) > 0 {
		doc, herr := ag.HandoffDoc(ctx, buildSys(), "")
		if herr != nil {
			return 2, fmt.Errorf("handoff: %w", herr)
		}
		fmt.Fprintln(os.Stdout, doc)
	}

	// M15 #73: a resumed session keeps its friction history — replay the
	// persisted user turns before this run's own turn is appended.
	if ss, ok := mem.(*memory.SharpShooter); ok {
		if err := ss.Replay(store); err != nil {
			logx.Errorf("memory: sharpshooter replay: %v", err)
		}
	}

	history, err := initialHistory(store, prompt)
	if err != nil {
		return 2, err
	}
	// M12 #43: the turn boundary — the remote backend counts this run's new
	// user turn (autoRetain cadence) and flushes queued retains off the
	// critical path.
	noteMemoryTurn(mem, history)

	logx.Debugf("print: model=%s session=%s", modelRef, store.Path())
	started := time.Now()
	// buildSys() at the call site, never a boot-captured string: extension
	// and MCP tools register after startup and must be in the prompt the
	// model is told to use (see TestPromptReflectsLiveRegistry).
	final, err := ag.Run(ctx, hookBus.Context(ctx, buildSys()), history)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			fmt.Fprintf(os.Stderr, "\nxdev: -max-time %s exceeded\n", launch.MaxTime)
		} else {
			fmt.Fprintln(os.Stderr, "\nxdev: run aborted:", err)
		}
		exitCode = 1
	}
	// Advisor exit drain (T3 #20): a headless run cannot steer a finished
	// turn, so the last review is taken synchronously (30s cap) and printed.
	if adv != nil {
		dctx, dcancel := context.WithTimeout(context.Background(), advisorDrainCap)
		adv.Feed(dctx, advisorHistory(store))
		dcancel()
		for _, n := range adv.Dump() {
			fmt.Fprintf(os.Stderr, "advisor (%s): %s\n", n.Severity, n.Text)
		}
	}
	if planMode.Proposed() && planMode.Pending != "" {
		// The plan was the run's output: print it (the transcript shows the
		// propose call, not its argument).
		fmt.Fprintln(os.Stdout, planMode.Pending)
	}
	// Ai-title cascade (#107): one cheap request over the exchange that just
	// happened, bounded by its own 12s cap. history[0] is the user turn that
	// opened the run; final is the reply the title describes. Skipped by
	// --no-title (no title work at all) and --no-session (nothing to stamp).
	if !launch.NoTitle && !launch.NoSession && exitCode == 0 && final != nil {
		generateTitle(cfg, settings, cwd, provName, modelName, store,
			append(append([]ai.Message(nil), history...), *final))
	}
	_ = store.Append(&session.ModelChangeEntry{Model: modelRef})
	_ = store.Append(&session.CustomEntry{CustomType: "session_exit", Data: map[string]any{"code": exitCode}})
	// M12 #43: the session boundary — the remote backend queues any unfired
	// cadence turns as one final retain and drains its queue (bounded by the
	// retain timeout; a dead server leaves the queue behind).
	if h, ok := mem.(*memory.Hindsight); ok {
		if err := h.EndSession(); err != nil {
			logx.Debugf("memory: hindsight session end: %v", err)
		}
	}
	// M12 F1: local memory pipeline runs after the session ends, off the
	// critical path (off unless memory: local + memoryPipeline: on).
	if pipe := buildMemoryPipeline(cfg, settings, buildLocalMemory(settings)); pipe != nil {
		pipe.StartBackground(context.Background())
	}
	// M15 #73: sharpshooter's friction feed. A print run is one user turn; an
	// aborted run makes the next instruction a friction signal. Wait blocks
	// until the background consolidation landed — the process exits here.
	if ss, ok := mem.(*memory.SharpShooter); ok {
		if ag.Store != nil {
			ss.Session = ag.Store.ID()
		}
		ss.Observe(memory.Turn{Text: prompt, AfterFailure: exitCode != 0})
		ss.Wait()
	}
	// M12 #44: the mnemopi retain queue drains on exit inside a fixed budget
	// (memory.mnemopi.queueDrainMillis, default 1.5s); whatever does not fit
	// stays queued for the next run, so a slow synthesis can never delay the
	// exit.
	if mm := mnemopiFrom(settings); mm != nil {
		mm.Drain(context.Background())
	}
	// Text is already streamed live via OnEvent; only close the line.
	if final != nil {
		fmt.Println()
	}
	logx.Debugf("print: done in %s", time.Since(started).Round(time.Millisecond))
	return exitCode, nil
}

// buildHookBus resolves the session hook bus: --hook specs, the settings
// `hooks` record, and discovered hook files (project .xdev/hooks, user
// <dataDir>/hooks, trusted extension dirs). Warnings are reported, never
// fatal — a malformed hook file must not block a session.
//
// `warn` routes the notices to the surface the user is actually looking at:
// nil means stderr, which is right for print/acp; the TUI passes a transcript
// writer, because a bare stderr write there lands underneath the alternate
// screen where nobody can read it. This cannot be left to logx: logging is
// off by default, and a withheld repository hook is exactly the thing that
// must never be withheld silently (#241).
func buildHookBus(cwd string, opts printOptions, warn func(string)) *hookbus.Bus {
	b, warns := hookbus.Build(hookbus.Options{
		Settings:          lastSettings().Hooks,
		CLI:               opts.Hooks,
		CWD:               cwd,
		TrustedExtensions: opts.TrustedExtensions,
	})
	for _, w := range warns {
		logx.Errorf("hooks: %s", w)
		if warn != nil {
			warn(w)
			continue
		}
		fmt.Fprintln(os.Stderr, "xdev: hooks:"+w)
	}
	return b
}

func providerKeys(cfg *config.Config) []string {
	out := make([]string, 0, len(cfg.Providers))
	for k := range cfg.Providers {
		out = append(out, k)
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// buildProvider constructs the wire adapter for a provider config.
func buildProvider(name string, pc *config.ProviderConfig, modelName string, cfg *config.Config) (ai.Provider, error) {
	hc := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        8,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	headers := map[string]string{}
	for k, v := range pc.Headers {
		headers[k] = config.Resolve(v)
	}
	baseURL := config.Resolve(pc.BaseURL)
	settings := lastSettings()
	// Chain order: CLI key → models.yml (per-model then provider) → stored
	// OAuth → /login key → env.
	credReq := config.CredentialRequest{
		Provider: name, ProviderCfg: pc, CLIKey: cliAPIKey(),
		Refresh: refreshFunc(name, cfg),
		// M5 #25: the credential rotation position for this provider
		// (apiKeys pool). 0 unless a fallback has already rotated it.
		PoolIndex: poolIndexOf(name),
	}
	resolved, credErr := config.ResolveCredential(credReq)
	// auth: none — a local server (ollama, lm-studio) is configured by
	// definition, so it has no credential and that is not an error.
	configured := pc.KeylessAuth() || (credErr == nil && strings.TrimSpace(resolved.Value) != "")
	// Gate on the RESOLVED credential rather than a second copy of the
	// chain's rules: an env-only key is exactly as configured as a
	// models.yml one, and duplicating the lookup is how a gate ends up
	// rejecting valid setups.
	if err := settings.CheckProvider(name, configured); err != nil {
		return nil, err
	}
	apiKey := config.Resolve(pc.APIKey)
	if credErr == nil {
		apiKey = resolved.Value
	} else if pc.KeylessAuth() {
		apiKey = ""
	}
	// An explicit authHeader other than "Authorization" overrides the
	// adapter's own convention: the key rides that header and the adapter
	// gets no key, so it cannot also send its default. A silently ignored
	// authHeader is a confusing 401 waiting for a gateway user.
	if h := strings.TrimSpace(pc.AuthHeader); h != "" && h != "Authorization" && apiKey != "" {
		headers[h] = apiKey
		apiKey = ""
	}

	// Per-model overrides (models.yml `models[]` entries) are last-wins over
	// provider-level values: BaseURL/APIKey/Headers each replace their
	// provider-level counterpart. This is the models.yml merge contract the
	// issue names — without it, the declared fields are dead weight.
	for _, m := range pc.Models {
		if m.ID != modelName {
			continue
		}
		if m.BaseURL != "" {
			baseURL = config.Resolve(m.BaseURL)
		}
		if k := config.Resolve(m.APIKey); k != "" {
			apiKey = k
		}
		for hk, hv := range m.Headers {
			headers[hk] = config.Resolve(hv)
		}
		break
	}
	var prov ai.Provider
	switch pc.API {
	case ai.APIOpenAICompletions:
		prov = ai.NewOpenAICompletionsProvider(name, baseURL, apiKey, headers, hc)
	case ai.APIOpenAIResponses:
		prov = ai.NewOpenAIResponsesProvider(name, baseURL, apiKey, headers, hc)
	case ai.APIAzureOpenAIResponses:
		prov = ai.NewAzureResponsesProvider(name, baseURL, apiKey, headers,
			ai.AzureResponsesOptions{Deployment: pc.Deployment, APIVersion: pc.APIVersion}, hc)
	case ai.APIOpenAICodexResponses:
		prov = ai.NewOpenAICodexResponsesProvider(name, baseURL, apiKey, headers, hc)
	case ai.APIAnthropicMessages:
		prov = ai.NewAnthropicProvider(name, baseURL, apiKey, headers, hc)
	case ai.APIGoogleGenerativeAI:
		prov = ai.NewGoogleGenAIProvider(name, baseURL, apiKey, headers, hc)
	case ai.APIGoogleVertex:
		prov = ai.NewGoogleVertexProvider(name, baseURL, apiKey, headers,
			ai.GoogleVertexOptions{Project: pc.Project, Location: pc.Location}, hc)
	case ai.APIGeminiCLI:
		prov = ai.NewGeminiCLIProvider(name, baseURL, apiKey, headers,
			ai.GeminiCLIOptions{Project: pc.Project}, hc)
	default:
		return nil, fmt.Errorf("provider %q: unsupported api %q", name, pc.API)
	}
	// toolsFormat pins an in-band tool dialect for a model that cannot emit
	// native structured tool calls; empty leaves the provider's own tool calls.
	format, err := ai.ParseToolFormat(pc.ToolsFormat)
	if err != nil {
		return nil, fmt.Errorf("provider %q: %w", name, err)
	}
	return ai.NewInBandProvider(prov, format), nil
}

// modelWindow resolves the context window for one provider/model pair
// (0 when the model is undiscovered — compaction stays disabled then).
func modelWindow(cfg *config.Config, provider, model string) int {
	pc, ok := cfg.Providers[provider]
	if !ok {
		return 0
	}
	// Pinned entries first; discovered ones (a local server's live model
	// list) fill what models.yml never named, so compaction knows the real
	// window instead of silently disabling itself.
	for _, m := range providerModels(provider, pc) {
		if m.ID == model {
			return m.ContextWindow
		}
	}
	return 0
}

// providerModels merges pinned + discovered models once per provider per
// process: discovery is best-effort (a down server must not affect
// startup), and the result is cached so a stalled endpoint is not retried
// on every model lookup. The cache is mutex-guarded because the TUI warms
// discovery providers on a background goroutine while the key thread may
// read any provider's catalog through /model's picker.
var (
	providerModelCacheMu sync.Mutex
	providerModelCache   = map[string][]config.ModelConfig{}
)

func providerModels(name string, pc *config.ProviderConfig) []config.ModelConfig {
	providerModelCacheMu.Lock()
	cached, ok := providerModelCache[name]
	providerModelCacheMu.Unlock()
	if ok {
		return cached
	}
	out := pc.Models
	if pc.Discovery != nil {
		// Network I/O stays outside the lock: two racers may both probe a
		// cold provider, and the second write just overwrites an equal
		// result.
		ctx, cancel := context.WithTimeout(context.Background(), config.DiscoveryTimeout)
		found, err := config.DiscoverModels(ctx, pc)
		cancel()
		if err != nil {
			logx.Debugf("discovery %s: %v", name, err)
		} else {
			out = found
		}
	}
	providerModelCacheMu.Lock()
	providerModelCache[name] = out
	providerModelCacheMu.Unlock()
	return out
}

// promptFn builds the system prompt from the LIVE registry each time it
// is called. Tools can register after startup (async MCP, extensions), and
// a prompt frozen at boot would never mention them — the provider request
// would advertise tools the model was never told about.
func promptFn(base string, cwd string, reg *tool.Registry, appendSystem string) func() string {
	return promptFnWithMemory(base, cwd, reg, appendSystem, nil)
}

func promptFnWithMemory(base string, cwd string, reg *tool.Registry, appendSystem string, mem memory.Store) func() string {
	// --add-dir roots contribute their own AGENTS.md hierarchy; the launch
	// cwd stays first so its files keep precedence.
	ctxFiles := agent.LoadContextFiles(cwd)
	for _, dir := range launch.ExtraDirs {
		if extra := agent.LoadContextFiles(dir); extra != "" {
			ctxFiles += "\n" + extra
		}
	}
	if dirs := workspaceDirs(cwd); len(dirs) > 1 {
		ctxFiles += "\n\nWorkspace directories (in scope): " + strings.Join(dirs, ", ")
	}
	var ruleSet []rules.Rule
	if noRulesFlag {
		rules.Set(nil)
	} else {
		ruleSet = rules.Discover(cwd, lastSettings().EnabledProviders)
		rules.Set(ruleSet)
	}
	return func() string {
		defs := reg.Defs()
		named := make([]agent.NamedToolDef, 0, len(defs))
		for _, d := range defs {
			named = append(named, agent.NamedToolDef{Name: d.Name, Description: d.Description})
		}
		sys := agent.BuildSystemPrompt(base, ctxFiles, named)
		// M13 #54: the index keeps deferred tools discoverable without
		// putting their schemas in the eager tool list (empty when none).
		sys += agent.BuildDeferredIndex(reg.Deferred())
		if rb := agent.BuildRulesBlock(ruleSet); rb != "" {
			sys += "\n\n" + rb
		}
		if sb := skillPromptBlock(cwd); sb != "" {
			sys += "\n\n" + sb
		}
		if mem != nil {
			if gb := mem.GuidanceBlock(); gb != "" {
				sys += "\n\n" + gb
			}
		}
		if appendSystem != "" {
			sys += "\n\n" + appendSystem
		}
		return sys
	}
}

// tailSystemPrompt composes the after-tools tail of the system prompt:
// PERSONALITY.md (who the agent is, or the resolved personality preset —
// see ApplyPersonalityPreset; the file beats the preset) then
// APPEND_SYSTEM.md / the flag (extra instructions). A discovered
// PERSONALITY.md must actually reach the prompt — the field was
// previously set but never consumed. The flag goes through omp's
// text-or-file resolution.
func tailSystemPrompt(overrides agent.SystemPromptOverrides, flagAppend string) string {
	flagAppend = resolvePromptFlag(flagAppend)
	var b strings.Builder
	if overrides.Personality != "" {
		b.WriteString(overrides.Personality)
	}
	if flagAppend != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(flagAppend)
	} else if overrides.Append != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(overrides.Append)
	}
	return b.String()
}

// basePrompt resolves the system prompt base: --system-prompt flag
// (text, or a file path — see resolvePromptFlag) → SYSTEM.md (project,
// then user) → the built-in default.
func basePrompt(opts printOptions, cwd string) string {
	if opts.SystemPrompt != "" {
		if flagPrompt := resolvePromptFlag(opts.SystemPrompt); flagPrompt != "" {
			return flagPrompt
		}
	}
	if o := agent.LoadSystemPromptOverrides(cwd); o.System != "" {
		return o.System
	}
	return agent.SystemPromptBase
}

// resolvePromptFlag applies omp's text-or-file semantics to the
// --system-prompt / --append-system-prompt values: a single-line value
// that names a readable file is loaded verbatim (relative paths resolve
// against the process cwd, which is the session cwd); multi-line values
// and unreadable paths stay literal. An unreadable path is not an error
// — it is simply a literal prompt.
func resolvePromptFlag(v string) string {
	if v == "" || strings.ContainsRune(v, '\n') {
		return v
	}
	raw, err := os.ReadFile(v)
	if err != nil {
		return v
	}
	return string(raw)
}

// hasStoredCredential reports whether credentials.json holds anything for a
// provider (a stored OAuth token counts as configured).
func hasStoredCredential(provider string) bool {
	store, err := config.LoadCredentials()
	if err != nil {
		return false // a corrupt store fails later with its own precise error
	}
	c, ok := store[provider]
	return ok && (strings.TrimSpace(c.APIKey) != "" || strings.TrimSpace(c.AccessToken) != "")
}

// poolIdx tracks the credential rotation position per provider (M5 #25,
// retry.fallbackChains depth). The engine's Rotate seam advances it and
// rebuilds the provider, so a spent key steps to its apiKeys sibling instead
// of failing the run. Guarded because a background agent can rotate while the
// primary turn reads its own provider.
var (
	poolMu  sync.Mutex
	poolIdx = map[string]int{}
)

func poolIndexOf(name string) int {
	poolMu.Lock()
	defer poolMu.Unlock()
	return poolIdx[name]
}

// rotateProviderCredential advances the provider to its next models.yml
// credential and rebuilds it. ok=false when the pool is exhausted (the caller
// then falls back to another provider instead).
func rotateProviderCredential(cfg *config.Config, provider, model string) (ai.Provider, bool) {
	pc := cfg.Providers[provider]
	if pc == nil {
		return nil, false
	}
	pool := config.CredentialPool(pc, model)
	poolMu.Lock()
	next := poolIdx[provider] + 1
	if pool == nil || next >= len(pool) {
		poolMu.Unlock()
		return nil, false
	}
	poolIdx[provider] = next
	poolMu.Unlock()
	prov, err := buildProvider(provider, pc, model, cfg)
	if err != nil {
		// The rebuilt provider failed: undo the step so the next attempt (or
		// the primary) keeps the credential that worked.
		poolMu.Lock()
		poolIdx[provider] = next - 1
		poolMu.Unlock()
		return nil, false
	}
	logx.Debugf("retry: rotated %s to credential %d/%d", provider, next+1, len(pool))
	return prov, true
}

// cliAPIKey holds the -api-key value for the run (set once in main before
// any provider is built, since buildProvider is called from helpers without
// flag access).
var cliKeyValue string

func cliAPIKey() string { return cliKeyValue }

// extensionsDir is <dataDir>/extensions: executables speaking the ext
// JSONL protocol (PRD §1.5 — extensions are processes, never code).
func extensionsDir() string {
	return filepath.Join(config.DataDir(), "extensions")
}

// attachExtensions loads extension processes, registers their tools, and
// returns the manager for use as the agent's Interceptor. Runtime actions
// route back into the agent as steering. Failures are logged, never
// fatal; a broken extension must not block a session.
// actionRouter turns extension runtime requests into agent steering or a
// session-scoped provider registration. The agent exposes both steering
// kinds, so followUp is routed distinctly rather than collapsed into steer;
// `aside` rides the followUp channel with a marker — the agent has no aside
// steering kind, and the marker is what keeps a side remark distinguishable
// from the user's next instruction (loop.go injects every queued kind at the
// next step boundary today, so aside lands there rather than after the run);
// `register_provider` installs a models.yml-shaped block in cfg so later
// model references resolve through it.
func actionRouter(steer, followUp func(text string), cfg *config.Config) func(ext.Action) {
	return func(a ext.Action) {
		switch a.Action {
		case "steer":
			steer(a.Text)
		case "aside":
			followUp(asidePrefix + a.Text)
		case "followUp":
			followUp(a.Text)
		case "register_provider":
			registerProvider(cfg, a)
		default:
			logx.Debugf("ext: unsupported action %q", a.Action)
		}
	}
}

// asidePrefix marks a routed aside so the model reads it as a side remark
// rather than as the user's next instruction.
const asidePrefix = "(aside) "

// registerProvider installs an extension-supplied provider block (models.yml
// shape: {name, baseUrl, api, models[]}). Failures are logged, never fatal:
// a broken payload must not take the session down.
func registerProvider(cfg *config.Config, a ext.Action) {
	var payload struct {
		Name string `json:"name"`
		config.ProviderConfig
	}
	if len(a.Data) > 0 {
		if err := json.Unmarshal(a.Data, &payload); err != nil {
			logx.Errorf("ext: register_provider: bad payload: %v", err)
			return
		}
	}
	name := strings.TrimSpace(payload.Name)
	if name == "" {
		name = strings.TrimSpace(a.Text)
	}
	if err := cfg.RegisterProvider(name, &payload.ProviderConfig); err != nil {
		logx.Errorf("ext: %v", err)
		return
	}
	logx.Infof("ext: register_provider: %q registered (session-scoped)", name)
}

// attachExtensions loads extension processes from the install's extensions
// dir. --no-extensions skips discovery entirely, so no extension tool,
// command, or policy hook loads in any mode.
func attachExtensions(ctx context.Context, reg *tool.Registry, steer, followUp func(text string), cfg *config.Config) *ext.Manager {
	if launch.NoExtensions {
		return nil
	}
	mgr := ext.NewManager()
	mgr.BindHost(actionRouter(steer, followUp, cfg))
	// Discovery first, then the explicit -e/--extension paths (their
	// failures are reported, not fatal: one bad path must not take down the
	// working set).
	if err := mgr.Load(ctx, extensionsDir()); err != nil {
		logx.Errorf("ext: %v", err)
	}
	for _, dir := range launch.Extensions {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			// logx is off in print mode, so an ignored -e path would be
			// silent. xdev loads a DIRECTORY of extension executables
			// (PRD §1.5); omp's -e names a file, so say which shape is
			// expected instead of no-op'ing.
			fmt.Fprintf(os.Stderr, "xdev: --extension %s: want a directory of extension executables (omp's single-file -e is not supported)\n", dir)
			continue
		}
		if err := mgr.Load(ctx, dir); err != nil {
			fmt.Fprintf(os.Stderr, "xdev: --extension %s: %v\n", dir, err)
		}
	}
	tools := mgr.Tools()
	// A policy-only extension (events, no tools/commands) must survive:
	// it is the fail-closed hook the protocol exists for, and discarding
	// it would silently disable the policy.
	if len(tools) == 0 && len(mgr.Commands()) == 0 && mgr.PolicyHooks() == 0 {
		mgr.Close()
		return nil
	}
	ext.Register(reg, tools)
	logx.Infof("ext: %d tool(s) from %s", len(tools), extensionsDir())
	return mgr
}

// mcpConfigPath is <dataDir>/mcp.yml (absent = MCP off, PRD §2).
func mcpConfigPath() string {
	return filepath.Join(config.DataDir(), "mcp.yml")
}

// attachMCP connects configured MCP servers and registers their tools on
// the parent registry only — children never inherit ambient MCP (PRD M6:
// subagents run with restricted tool sets). Individual server failures are
// reported and skipped, never fatal.
//
// Async by design: a server that starts but never answers `initialize`
// would otherwise stall startup for its whole timeout budget. Registration
// happens whenever it lands; the registry is mutex-guarded, and a prompt
// sent before then simply carries fewer tools (the next turn has them).
func attachMCP(ctx context.Context, reg *tool.Registry, wait bool) *mcpclient.Manager {
	cfg, err := mcpclient.LoadConfig(mcpConfigPath())
	if err != nil {
		logx.Errorf("mcp config: %v", err)
		return nil
	}
	if len(cfg.Servers) == 0 {
		return nil
	}
	mgr := mcpclient.NewManager()
	if wait {
		// One-shot modes (print) must have the tools before the first
		// turn: connect inline, bounded by the per-server init timeout.
		finishMCP(mgr, reg, ctx, cfg)
		return mgr
	}
	go finishMCP(mgr, reg, ctx, cfg)
	return mgr
}

// finishMCP connects and registers, reporting failures non-fatally.
func finishMCP(mgr *mcpclient.Manager, reg *tool.Registry, ctx context.Context, cfg *mcpclient.Config) {
	connected, errs := mgr.Connect(ctx, cfg)
	for _, e := range errs {
		fmt.Fprintln(os.Stderr, "xdev: mcp server unavailable —", e)
	}
	if connected == 0 {
		mgr.Close()
		return
	}
	mcpclient.Register(reg, mgr.Tools())
	logx.Infof("mcp: %d server(s), %d tool(s)", connected, len(mgr.Tools()))
}

// newToolRegistry builds the core four tools plus the parent-facing task
// tool (M6 subagents). ChildTools deliberately excludes the task tool, so
// a child can never spawn grandchildren (structural depth guard).
// thinking is the parent's resolved role effort, forwarded to children so
// delegation does not silently downgrade (or upgrade) the reasoning budget.
// registerURISchemes installs the read-tool URI resolvers. Idempotent:
// re-registering replaces the resolver (tests, repeated startup).
func registerURISchemes() {
	tool.RegisterURIScheme("skill", skills.Resolve)
	tool.RegisterURIScheme("rule", rules.Resolve)
}

// memoryBackend is this package's name for the internal/memory.Store seam
// (M12): the local markdown backend, the mnemopi SQLite store, the remote
// Hindsight server and the friction-gated sharpshooter all satisfy it, so
// the prompt injection, the memory:// read seam, the learn tool and /memory
// never branch on the backend.
type memoryBackend = memory.Store

// buildMemory returns the configured memory backend (nil = off). The Store
// seam keeps every consumer — prompt injection, the memory:// read seam, the
// learn tool, /memory — backend-agnostic across local, mnemopi, hindsight
// and sharpshooter.
func buildMemory(settings *config.Settings) memory.Store {
	if settings == nil {
		return nil
	}
	switch settings.Memory {
	case "local":
		b := buildLocalMemory(settings)
		if b == nil {
			return nil
		}
		return b
	case "mnemopi":
		return buildMnemopiMemory(settings)
	case "hindsight":
		return buildHindsightMemory(settings)
	case "sharpshooter":
		// M15 #73: friction-gated decision files under <dataDir>/memories.
		ss := &memory.SharpShooter{Dir: filepath.Join(config.DataDir(), "memories")}
		if err := ss.Ensure(); err != nil {
			logx.Errorf("memory: cannot create %s, disabling: %v", ss.Dir, err)
			return nil
		}
		tool.RegisterURIScheme("memory", ss.Read)
		return ss
	}
	return nil
}

// buildLocalMemory is the local backend alone: the two-phase pipeline writes
// MEMORY.md/learned.md, so its Backend field stays local-specific.
func buildLocalMemory(settings *config.Settings) *memory.Backend {
	if settings == nil || settings.Memory != "local" {
		return nil
	}
	b := &memory.Backend{Dir: filepath.Join(config.DataDir(), "memory")}
	if cap := settings.LessonCapOrDefault(); cap > 0 {
		b.LessonCap = cap // #88: the prompt window was a constant, not a setting
	}
	if err := b.Ensure(); err != nil {
		logx.Errorf("memory: cannot create %s, disabling: %v", b.Dir, err)
		return nil
	}
	tool.RegisterURIScheme("memory", b.Read)
	return b
}

// mnemopiCache keeps one mnemopi backend per settings identity: the prompt
// path, the tool registry and the exit drain all ask for the backend, and
// three *sql.DB handles on one file would only add write contention.
var (
	mnemopiMu    sync.Mutex
	mnemopiCache = map[string]*memory.Mnemopi{}
)

// mnemopiKey identifies a mnemopi backend by everything that changes its
// behavior (the store path, the bank scoping, and the project the cwd names).
func mnemopiKey(settings *config.Settings) string {
	mm := settings.MnemopiConfig()
	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}
	return strings.Join([]string{config.DataDir(), mm.Scope, mm.Tag, mm.LLMMode, cwd}, "|")
}

// mnemopiFrom returns the memoized backend when mnemopi is the configured
// memory backend, without constructing one: the drain, the turn counter and
// the /memory queue verbs must not create a store as a side effect.
func mnemopiFrom(settings *config.Settings) *memory.Mnemopi {
	if settings == nil || settings.Memory != "mnemopi" {
		return nil
	}
	mnemopiMu.Lock()
	defer mnemopiMu.Unlock()
	return mnemopiCache[mnemopiKey(settings)]
}

// buildMnemopiMemory opens (once) the SQLite backend (M12 #44). nil = off: a
// store that cannot be opened must degrade to "no memory", never to a failed
// run.
func buildMnemopiMemory(settings *config.Settings) *memory.Mnemopi {
	key := mnemopiKey(settings)
	// Construction happens under the lock: two entry points racing here
	// would otherwise open two connections to the same file.
	mnemopiMu.Lock()
	defer mnemopiMu.Unlock()
	if cached, ok := mnemopiCache[key]; ok {
		return cached
	}
	mm := settings.MnemopiConfig()
	cwd, _ := os.Getwd()
	m := &memory.Mnemopi{
		Dir:                 filepath.Join(config.DataDir(), "memory"),
		Scope:               mm.Scope,
		Tag:                 mm.Tag,
		CWD:                 cwd,
		LLMMode:             mm.LLMMode,
		RecallLimit:         mm.RecallLimit,
		InjectionTokenLimit: mm.InjectionTokenLimit,
		RetainEveryNTurns:   mm.RetainEveryNTurns,
		OnError:             func(err error) { logx.Debugf("memory: %v", err) },
	}
	// The synthesis seam: the reflect pass and the queue drain run on it.
	// llmMode none leaves it nil, which every caller reports honestly
	// instead of pretending the pass happened.
	if mm.LLMMode != "none" {
		role := "@smol"
		if mm.LLMMode == "remote" {
			role = "@default"
		}
		m.Complete = mnemopiSeam(settings, role)
	}
	if err := m.Ensure(); err != nil {
		logx.Errorf("memory: cannot open %s, disabling: %v", m.Dir, err)
		return nil
	}
	tool.RegisterURIScheme("memory", m.Read)
	mnemopiCache[key] = m
	return m
}

// mnemopiSeam resolves the synthesis role lazily (and once): the backend is
// built from settings alone, and only a reflect/sync call needs a model.
func mnemopiSeam(settings *config.Settings, role string) memory.CompleteFunc {
	var (
		once sync.Once
		fn   func(context.Context, string) (string, error)
		fail error
	)
	return func(ctx context.Context, prompt string) (string, error) {
		once.Do(func() {
			cfg, err := config.LoadModelsLayered()
			if err != nil {
				fail = fmt.Errorf("memory: models.yml: %w", err)
				return
			}
			fn = memoryRoleComplete(cfg, settings, role)
			if fn == nil {
				fail = fmt.Errorf("memory: no model for role %s", role)
			}
		})
		if fail != nil {
			return "", fail
		}
		return fn(ctx, prompt)
	}
}

// hindsightCache keeps one Hindsight backend per settings identity
// (M12 #43): the prompt builder, the turn boundary and the tool registry all
// call buildMemory, and they must share ONE instance — the remote backend
// owns the offline retain queue and the recall cache, and a second instance
// would silently split both.
var (
	hindsightMu    sync.Mutex
	hindsightCache = map[string]*memory.Hindsight{}
)

// hindsightKey identifies a backend by the settings that shape its scope.
func hindsightKey(settings *config.Settings) string {
	wd, _ := os.Getwd()
	h := settings.Hindsight
	return strings.Join([]string{h.APIURL, h.BankID, h.Scoping, h.RetainMode, wd}, "|")
}

// hindsightFrom returns the memoized backend when hindsight is the configured
// memory backend, without constructing one.
func hindsightFrom(settings *config.Settings) *memory.Hindsight {
	if settings == nil || settings.Memory != "hindsight" {
		return nil
	}
	hindsightMu.Lock()
	defer hindsightMu.Unlock()
	return hindsightCache[hindsightKey(settings)]
}

// mnemopiSyncReport renders one bounded consolidation pass for
// /memory sync|enqueue.
func mnemopiSyncReport(res memory.SyncResult) string {
	out := fmt.Sprintf("applied %d queued retain(s); %d still queued in %s",
		res.Applied, res.Remaining, res.Elapsed.Round(time.Millisecond))
	if res.Consolidated {
		out += fmt.Sprintf("; consolidated with %d reflection pass(es)", res.Reflections)
	}
	return out
}

// memoryTurnHooks wraps a run's hooks so the configured memory backend sees
// the turn boundary (M12 #44: mnemopi's retainEveryNTurns cadence enqueues a
// consolidation the exit drain or /memory sync applies). Every other backend
// is a pass-through.
func memoryTurnHooks(hooks agent.TurnHooks, settings *config.Settings) agent.TurnHooks {
	if mm := mnemopiFrom(settings); mm != nil {
		return mnemopiTurnHooks{TurnHooks: hooks, mem: mm}
	}
	return hooks
}

// buildHindsightMemory constructs (once) the remote backend. The cwd anchors
// the project scope; the HINDSIGHT_* environment overrides are applied by
// HindsightConfigFromSettings.
func buildHindsightMemory(settings *config.Settings) *memory.Hindsight {
	key := hindsightKey(settings)
	hindsightMu.Lock()
	defer hindsightMu.Unlock()
	if cached, ok := hindsightCache[key]; ok {
		return cached
	}
	h := memory.NewHindsight(memory.HindsightConfigFromSettings(settings, ""))
	bank, tag := h.Scope()
	logx.Debugf("memory: hindsight backend, bank %s, tag %q", bank, tag)
	tool.RegisterURIScheme("memory", h.Read)
	hindsightCache[key] = h
	return h
}

// noteMemoryTurn hands the backend this run's newest user turn (M12 #43).
// It is the print-mode turn boundary; only the remote backend has a cadence,
// so this is a no-op for every other backend. Resumed sessions whose history
// already ends with a tool result or an assistant message add no turn.
func noteMemoryTurn(mem memoryBackend, history []ai.Message) {
	h, ok := mem.(*memory.Hindsight)
	if !ok || h.Off() || len(history) == 0 {
		return
	}
	_ = ok
	last := history[len(history)-1]
	if last.Role != ai.RoleUser {
		return
	}
	if text := strings.TrimSpace(last.Text()); text != "" {
		h.NoteUserTurn(text)
	}
	h.RetainAsync()
}

// observeFriction feeds one user turn to the sharpshooter backend (M15 #73,
// #89): repeats and post-failure instructions are what its detector scores,
// and it has no cadence of its own, so every mode must call it per turn.
// `failed` is whether the PREVIOUS turn ended badly — an aborted run making
// this instruction a friction signal. Returns whether the turn crossed the
// detector's threshold (a decision is now due); false for every other
// backend, which has no friction feed at all.
func observeFriction(mem memoryBackend, text string, failed bool) bool {
	ss, ok := mem.(*memory.SharpShooter)
	if !ok || ss.Off() {
		return false
	}
	if strings.TrimSpace(text) == "" {
		return false
	}
	return ss.Observe(memory.Turn{Text: text, AfterFailure: failed})
}

// mnemopiTurnHooks feeds the mnemopi turn counter (M12 #44): every
// memoryMnemopi.retainEveryNTurns turns it enqueues a consolidation that the
// exit drain or /memory sync applies. Embedding forwards every other hook.
type mnemopiTurnHooks struct {
	agent.TurnHooks
	mem *memory.Mnemopi
}

func (h mnemopiTurnHooks) OnTurnEnd(s ai.StopReason, err error) {
	h.TurnHooks.OnTurnEnd(s, err)
	if err == nil {
		if _, nerr := h.mem.NoteTurn(); nerr != nil {
			logx.Debugf("memory: turn note: %v", nerr)
		}
	}
}

// registerMemoryTools adds the configured backend's model-facing tools. The
// lesson recorder (learn) rides the Store seam, so every backend gets it; the
// backend-specific trio rides the concrete type: mnemopi adds polyphonic
// recall, retain, reflect and the bounded memory_edit, while the remote
// Hindsight backend exposes recall/retain/reflect and deliberately no
// memory_edit (upstream memories are not edited through this backend).
func registerMemoryTools(reg *tool.Registry, mem memory.Store, settings *config.Settings, cwd string) {
	if reg == nil || mem == nil {
		return
	}
	reg.Register(&memory.LearnTool{
		Backend:   mem,
		SkillsDir: skills.ManagedRoot(),
		// #88: secrets are scrubbed at write time (the prompt-level
		// instruction alone let a live credential into learned.md, which is
		// re-injected into every later session), and autolearn.enabled can
		// turn the recorder off.
		Redactor: func(text string) string { return redactorFor(cwd).Apply(text) },
		Disabled: !settings.AutolearnOn(),
	})
	switch m := mem.(type) {
	case *memory.Mnemopi:
		reg.Register(&memory.MnemopiRecallTool{Mem: m})
		reg.Register(&memory.MnemopiRetainTool{Mem: m})
		reg.Register(&memory.MnemopiReflectTool{Mem: m})
		reg.Register(&memory.MemoryEditTool{Mem: m})
	case *memory.Hindsight:
		reg.Register(&memory.RecallTool{Backend: m})
		reg.Register(&memory.RetainTool{Backend: m})
		reg.Register(&memory.ReflectTool{Backend: m})
	}
}

// buildMemoryPipeline constructs the two-phase local memory pipeline
// (M12 F1): extraction over changed sessions by the @smol model, then
// consolidation into MEMORY.md/learned.md. nil = off (memory backend
// absent, memoryPipeline not "on", or the role unresolvable).
func buildMemoryPipeline(cfg *config.Config, settings *config.Settings, backend *memory.Backend) *memory.Pipeline {
	if backend == nil || settings == nil || !settings.MemoryPipelineOn() {
		return nil
	}
	bySmol := memoryRoleComplete(cfg, settings, "@smol")
	if bySmol == nil {
		return nil
	}
	// M15 #70: an explicit local-tiny opt-in must never silently fall back
	// to the API (docs/decisions/local-tiny-models.md).
	bySmol, selErr := tiny.Select(tiny.TaskMemoryExtract, bySmol)
	if selErr != nil {
		logx.Errorf("memory pipeline: %v", selErr)
		return nil
	}
	return &memory.Pipeline{
		Backend:     backend,
		DataDir:     config.DataDir(),
		Complete:    bySmol,
		Consolidate: bySmol,
		OnError:     func(err error) { logx.Errorf("memory pipeline: %v", err) },
	}
}

// memoryRoleComplete resolves a model role into a one-shot completion seam
// (prompt in, text out) — the shared model-call plumbing for every memory
// background pass (the pipeline's extraction/consolidation, sharpshooter's
// friction consolidation). nil when the role is unresolvable.
func memoryRoleComplete(cfg *config.Config, settings *config.Settings, role string) func(context.Context, string) (string, error) {
	if cfg == nil || settings == nil {
		return nil
	}
	ref, _, err := resolveModel(role, cfg, settings)
	if err != nil {
		logx.Errorf("memory: %s unresolved: %v", role, err)
		return nil
	}
	pName, mName, err := config.ParseModelRef(ref)
	if err != nil {
		logx.Errorf("memory pipeline: %v", err)
		return nil
	}
	pc, ok := cfg.Providers[pName]
	if !ok {
		logx.Errorf("memory pipeline: unknown provider %q", pName)
		return nil
	}
	prov, err := buildProvider(pName, pc, mName, cfg)
	if err != nil {
		logx.Errorf("memory pipeline: provider unavailable: %v", err)
		return nil
	}
	return func(ctx context.Context, prompt string) (string, error) {
		ch, err := prov.Stream(ctx, ai.StreamRequest{
			Messages:  []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: prompt}}}},
			Model:     mName,
			MaxTokens: 2048,
		})
		if err != nil {
			return "", err
		}
		var text strings.Builder
		for ev := range ch {
			switch ev.Type {
			case ai.EventTextDelta:
				text.WriteString(ev.Delta)
			case ai.EventError:
				return "", ev.Err
			case ai.EventDone:
				if ev.Message != nil && ev.Message.Text() != "" {
					return ev.Message.Text(), nil
				}
				return text.String(), nil
			}
		}
		return text.String(), nil
	}
}

// noRulesFlag mirrors the --no-rules CLI flag (main sets it after flag
// parsing): it disables rulebook discovery, prompt injection, and the
// rule:// read seam.
var noRulesFlag bool

func newToolRegistry(cwd string, prov ai.Provider, provName, modelName string, settings *config.Settings, thinking *ai.ThinkingBudget, planMode *agent.PlanMode) *tool.Registry {
	registerURISchemes()
	// M13 #49: github tool + pr://issue:// reader URLs. One instance per
	// registry so the URL cache and the pr_checkout registry are shared.
	ghTool := tool.NewGithubTool(cwd)
	ghTool.RegisterURISchemes()
	// xd:// proposal devices (#36): read xd://propose serves the pending
	// plan; writes to xd://resolve / xd://reject finalize it — the same
	// seam the skill/memory read schemes ride.
	tool.RegisterURIScheme("xd", planMode.DeviceRead)
	tool.RegisterWriteDevice("xd", "resolve", planMode.ResolveDevice)
	tool.RegisterWriteDevice("xd", "reject", planMode.RejectDevice)
	pol := settingsPolicy(settings)
	reg := tool.NewRegistry()
	for _, t := range []tool.Tool{
		tool.NewReadTool(),
		tool.NewWriteTool(),
		tool.NewEditTool(),
		tool.NewBashTool(cwd),
		&tool.GrepTool{CWD: cwd},
		&tool.GlobTool{CWD: cwd},
		&tool.ASTGrepTool{CWD: cwd},
		&tool.ASTEditTool{CWD: cwd},
		eval.NewTool(cwd),
		ghTool,
		// ask is the parent's channel to the user; children (ChildTools
		// below) deliberately omit it — a scoped subagent has no user.
		tool.NewAskTool(settings.AskTimeout()),
	} {
		reg.Register(t)
	}
	reg.Register(tool.NewTodoTool())
	// M13 #48: web_search — ordered provider chain (keys from env or
	// settings); keyless DuckDuckGo keeps it answering without config.
	reg.Register(tool.NewWebSearchTool(settings.WebSearchConfig()))
	// M15 #69: generate_image — cloud image providers over the same
	// credential chain; generated bytes land in the session blob store.
	reg.Register(tool.NewImageGenTool(settings.ImageGenConfig(), imageGenCreds(), session.NewBlobStore(config.DataDir())))
	// M15 #65: computer — desktop control, off unless computer.enabled is
	// set (parent-only: it drives the real desktop, not a sandbox).
	reg.Register(computer.NewTool(computer.FromSettings(settings)))
	// M15 #67: security_scan — merges the scanners the host has (go vet,
	// govulncheck, semgrep, gitleaks); a missing binary is reported, not fatal.
	reg.Register(tool.NewSecurityScanTool(cwd))
	// The hub coordinates background subagents for this session (M11 #12).
	hub := agent.NewHub()
	sharedHub = hub
	reg.Register(&agent.TaskTool{
		// M11 #39: task.agentAdvisor — per-subagent advisor (on|off|model).
		ChildAdvisor: buildChildAdvisorFactory(settings),
		Hub:          hub,
		Policy:       pol,
		Thinking:     thinking,
		Provider:     prov,
		Model:        childModel(settings, provName, modelName),
		CWD:          cwd,
		// Children live in their own subtree: session.List(config.DataDir())
		// must never surface them to --continue/--resume.
		DataDir: filepath.Join(config.DataDir(), "subagents"),
		System:  agent.SubagentSystemPromptBase,
		ChildTools: []tool.Tool{
			tool.NewReadTool(),
			tool.NewWriteTool(),
			tool.NewEditTool(),
			tool.NewBashTool(cwd),
			&tool.GrepTool{CWD: cwd},
			&tool.GlobTool{CWD: cwd},
			&tool.ASTGrepTool{CWD: cwd},
			&tool.ASTEditTool{CWD: cwd},
			eval.NewTool(cwd),
		},
		// M11 #12: named task agents discovered from markdown files
		// (.xdev/agents/, ~/.xdev/agent/agents/). The task tool resolves
		// the "agent" argument against these; a named agent runs with its
		// own system prompt, tools, and model.
		Agents: func() []agent.AgentDefinition {
			defs, warnings := agent.DiscoverAgents(cwd)
			for _, w := range warnings {
				fmt.Fprintln(os.Stderr, "warning: "+w)
			}
			return defs
		}(),
	})
	reg.Register(&agent.HubTool{Hub: hub})
	// M11 #42: cross-session mailbox — persisted agent-to-agent channel,
	// distinct from the same-process hub. Owner session id is stamped by
	// wireTaskParent once the session opens.
	mailbox := agent.NewMailbox(config.DataDir())
	reg.Register(&agent.SendMessageTool{Mailbox: mailbox})
	reg.Register(&agent.InboxTool{Mailbox: mailbox})
	// #90: start the push path once per process (the poller shipped with no
	// Start caller, so arrivals waited for a read). stopInbox is deferred by
	// each run mode on exit.
	sharedMailbox = mailbox
	if stopInbox == nil {
		stopInbox = startInboxPoller(mailbox)
	}
	// M11 #40: goal mode — one session-scoped objective with an optional
	// token budget. The agent loop reads the same state for the per-turn
	// reminder and budget accounting; wireTaskParent binds the store.
	reg.Register(&agent.GoalTool{Goals: agent.NewGoalState(nil)})
	// M13 #51: checkpoint/rewind — named session-tree bookmarks. rewind
	// re-points the leaf at a checkpoint and records the caller's report as
	// a branch summary; wireTaskParent binds the live session.
	reg.Register(&tool.CheckpointTool{})
	reg.Register(&tool.RewindTool{})
	registerMemoryTools(reg, buildMemory(settings), settings, cwd)
	// M13 #52: language-server queries. Servers launch lazily on the first
	// lsp call (lsp.lazy: false opts into eager warmup).
	// --no-lsp: never register it, so no server is spawned and Prewarm is
	// not reached.
	if !launch.NoLSP {
		lspTool := lsp.NewTool(cwd, settings)
		reg.Register(lspTool)
		lspTool.Prewarm()
	}
	// M15 #68: local speech synthesis (macOS say, Linux spd-say/espeak-ng,
	// Windows PowerShell SAPI), voice/rate from the tts: settings group. A
	// platform without a backend still registers: the model gets the
	// actionable error instead of a silently missing tool.
	reg.Register(tts.NewTool(settings.TTSConfig()))
	// M15 #66: DAP debug driver — the adapters in debug.adapters (dlv,
	// debugpy, lldb-dap) start lazily on the first launch or attach.
	reg.Register(dap.NewTool(cwd, settings))
	// M12 #45: experimental notes-backed context windows — context_notes (a
	// branch-scoped 16 KiB notebook) and new_context (rollover). The state is
	// built from the finished registry: notes-backed rollover stays disabled
	// unless context_notes, new_context, read and grep are all active.
	notes := agent.NewNotesState(reg, nil)
	// #88: the gate now exists (omp ships these tools opt-in because
	// rollover changes what the model sees mid-session). Unregistered tools
	// also leave the notes-backed rollover unreachable, which is exactly the
	// conservative default the gate asks for.
	if settings.ExperimentalContextManagementOn() {
		reg.Register(&agent.NotesTool{Notes: notes})
		reg.Register(&agent.NewContextTool{Notes: notes})
		tool.RegisterURIScheme("history", notes.ResolveHistory)
	}
	// M13 #54: deferred tool catalog. The long tail leaves the eager tool
	// schema and the prompt recap (Registry.Defs omits it) and is listed as a
	// one-line index instead; the model finds it with tool_search, reads its
	// schema with tool_describe, and runs it through tool_call, which re-enters
	// the normal call path (approval policy, interception, hooks).
	for _, d := range []struct {
		name  string
		index string
		tags  []string
	}{
		{"ast_grep", "structural code search with ast-grep patterns", []string{"search", "code"}},
		{"ast_edit", "AST-aware codemod rewrites", []string{"edit", "codemod", "code"}},
		{"github", "GitHub operations: PRs, issues, files, search, Actions", []string{"git", "pr", "remote"}},
		{agent.HubToolName, "message and inspect the subagents running in this session", []string{"subagent", "agent"}},
		{agent.SendMessageToolName, "send a message to another xdev session (mailbox)", []string{"mailbox", "agent"}},
		{agent.InboxToolName, "read messages other sessions sent this one (mailbox)", []string{"mailbox", "agent"}},
		{tool.CheckpointToolName, "bookmark the session tree at this point", []string{"session", "rewind"}},
		{tool.RewindToolName, "return the session to an earlier checkpoint", []string{"session", "rewind"}},
	} {
		reg.Defer(d.name, d.index, d.tags...)
	}
	cat := reg.Catalog()
	reg.Register(tool.NewToolSearchTool(cat))
	reg.Register(tool.NewToolDescribeTool(cat))
	reg.Register(tool.NewToolCallTool(cat))
	// M13 #50: browser — CDP attach to an already-running Chrome. Never
	// launches a browser; screenshots land in the session blob store.
	reg.Register(browser.NewTool(settings.BrowserConfig(), session.NewBlobStore(config.DataDir())))
	// --tools / --no-tools (issue #33): narrow the built-in set before any
	// prompt or agent sees it. A name that matched nothing is reported —
	// silently narrowing less than asked is how a launch flag lies.
	for _, unknown := range applyToolFilter(reg, launch.Tools, launch.NoTools) {
		logx.Errorf("tools: no tool named %q (see -h for the built-in set)", unknown)
	}

	return reg
}

// sharedHub is the session's agent hub (one per process, like
// loadedSettings): the run modes reach it on exit to stop the processes the
// model started through the hub tool.
var sharedHub *agent.Hub

// sharedMailbox is the session's cross-session mailbox (one per process): the
// `send_message`/`inbox` tools and the push poller share it, so the poller's
// owner binding follows the same session identity the tools use.
var sharedMailbox *agent.Mailbox

// stopInbox is the poller's stop func, installed once per process by
// newToolRegistry and deferred by each run mode.
var stopInbox func()

// closeSharedHub stops every background subagent and every hub-started child
// process. Every run mode defers it right after building its registry:
// `hub start` used to leave the process running after xdev exited (T3 #8),
// and now that jobs are detached from the turn that spawned them, the session
// boundary is the only thing that reaps them (#93 sibling of that finding).
func closeSharedHub() {
	if sharedHub != nil {
		sharedHub.Close()
		sharedHub.StopProcesses()
	}
}

// lastSettings returns the layered settings main() resolved for this run
// (main owns loading so every mode sees the same layering).
func lastSettings() *config.Settings {
	if loadedSettings == nil {
		// Pre-main callers (tests, tooling): defaults with no gating.
		return &config.Settings{}
	}
	return loadedSettings
}

// resolveModel applies the M9 precedence: explicit value (flag, already
// merged over settings.defaultModel by main) → XDEV_MODEL → @role
// expansion → models.yml default. It returns the resolved provider/model
// and the effort the role pinned ("" = none).
func resolveModel(explicit string, cfg *config.Config, settings *config.Settings) (string, string, error) {
	ref := explicit
	if ref == "" {
		ref = os.Getenv("XDEV_MODEL")
	}
	if ref == "" && settings != nil && settings.DefaultModel != "" {
		ref = settings.DefaultModel
	}
	if strings.HasPrefix(ref, "@") && settings != nil {
		rr, err := config.ResolveModelRef(settings, ref)
		if err != nil {
			return "", "", err
		}
		return rr.Ref, rr.Effort, nil
	}
	if ref == "" {
		ref = cfg.DefaultModelRef()
	}
	if ref == "" {
		return "", "", fmt.Errorf("no model configured: add ~/.xdev/agent/models.yml, set defaultModel, or pass -model provider/model")
	}
	// A literal ":effort" suffix belongs to the request, not the model id:
	// forwarding "dev:high" to the wire 404s one turn later.
	effort := ""
	if base, suffix, ok := strings.Cut(ref, ":"); ok && slices.Contains(config.EffortLevels, suffix) {
		ref, effort = base, suffix
	}
	// --provider forces the provider when the ref names none (omp's
	// --provider onegw --model dev); an unknown provider fails here rather
	// than one turn later at the wire.
	if launch.Provider != "" {
		if _, ok := cfg.Providers[launch.Provider]; !ok {
			return "", "", fmt.Errorf("unknown provider %q (have: %v)", launch.Provider, providerKeys(cfg))
		}
		if !strings.Contains(ref, "/") {
			ref = launch.Provider + "/" + ref
		}
	}
	// A bare id ("dev") resolves against the configured catalogs, the same
	// convenience omp's model resolver offers for `-model`.
	if !strings.Contains(ref, "/") {
		expanded, err := expandBareModelID(cfg, ref)
		if err != nil {
			return "", "", err
		}
		ref = expanded
	}
	// Gateways routinely serve far more than models.yml pins, so an
	// explicit provider/model ref is accepted even when the catalog does
	// not list it — rejecting it here would make unpinned-but-served
	// models unusable. The catalog check stays advisory (logged); the hard
	// guards are the bare-id resolution above and seedModelFromSession,
	// which refuses to adopt history refs that do not resolve. The picker
	// itself only ever offers catalog rows, so the interactive path cannot
	// produce a typo.
	if err := validateModelRef(cfg, ref); err != nil {
		logx.Debugf("model %q not in the configured catalog: %v", ref, err)
	}
	return ref, effort, nil
}

// expandBareModelID resolves a bare model id against every provider's
// merged catalog (pinned + discovered, providerModels' cache). One match
// wins; zero or several are an error naming the candidates.
func expandBareModelID(cfg *config.Config, id string) (string, error) {
	if cfg == nil {
		return id, nil
	}
	var hits []string
	for _, name := range providerKeys(cfg) {
		pc := cfg.Providers[name]
		if pc == nil {
			continue
		}
		for _, m := range providerModels(name, pc) {
			if strings.EqualFold(m.ID, id) {
				hits = append(hits, name+"/"+m.ID)
			}
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return "", fmt.Errorf("unknown model %q (add it to models.yml or use provider/model)", id)
	default:
		return "", fmt.Errorf("model %q matches %s — use provider/model", id, strings.Join(hits, ", "))
	}
}

// validateModelRef reports a ref whose model id is not in the provider's
// merged catalog. Callers decide severity: resolveModel logs it (explicit
// refs may name models the gateway serves but models.yml does not pin),
// while seedModelFromSession treats it as fatal for that seed. A provider
// with an empty catalog (override-only, no discovery block) never fails —
// there is nothing to compare against.
func validateModelRef(cfg *config.Config, ref string) error {
	if cfg == nil {
		return nil
	}
	pname, mname, err := config.ParseModelRef(ref)
	if err != nil {
		return err
	}
	pc, ok := cfg.Providers[pname]
	if !ok {
		return fmt.Errorf("unknown provider %q (have: %v)", pname, providerKeys(cfg))
	}
	catalog := providerModels(pname, pc)
	if len(catalog) == 0 {
		return nil
	}
	for _, m := range catalog {
		if m.ID == mname {
			return nil
		}
	}
	ids := make([]string, 0, len(catalog))
	for _, m := range catalog {
		ids = append(ids, m.ID)
	}
	if len(ids) > 6 {
		ids = append(ids[:6], "…")
	}
	return fmt.Errorf("unknown model %q for provider %q (configured: %s)", mname, pname, strings.Join(ids, ", "))
}

// applyPolicy attaches the configured approval policy to an agent. Print
// and RPC are unattended: a rule that resolves to "prompt" therefore
// refuses (the agent loop's Approve==nil contract). The TUI will surface a
// blocking card when M12's dialog chrome lands; until then it behaves the
// same way, which is fail-safe rather than fail-open.
// effortBudget turns a resolved role effort into the reasoning budget the
// adapters translate into their own vocabularies. An unpinned or unknown
// effort means no thinking requested.
func effortBudget(effort string) *ai.ThinkingBudget {
	tokens, ok := config.EffortBudget(effort)
	if !ok {
		return nil
	}
	return &ai.ThinkingBudget{Tokens: tokens}
}

// settingsPolicy resolves the approval policy from settings (nil or a bad
// configuration yields the yolo default, with the error reported).
func settingsPolicy(settings *config.Settings) tool.ApprovalPolicy {
	if settings == nil {
		return tool.ApprovalPolicy{}
	}
	pol, err := settings.Policy()
	if err != nil {
		fmt.Fprintln(os.Stderr, "xdev:", err)
		return tool.ApprovalPolicy{}
	}
	return autoApprovePolicy(pol)
}

// autoApprovePolicy applies --auto-approve: the MODE becomes yolo, so no call
// is held for a prompt. The written rules survive — a per-tool deny or a bash
// pattern is an explicit decision, and a launch flag that silently overruled
// it would turn a deny rule into decoration.
func autoApprovePolicy(pol tool.ApprovalPolicy) tool.ApprovalPolicy {
	if launch.AutoApprove {
		pol.Mode = tool.Yolo
	}
	return pol
}

// agentPolicy resolves the approval policy for a freshly built agent.
func agentPolicy() tool.ApprovalPolicy {
	pol, err := lastSettings().Policy()
	if err != nil {
		fmt.Fprintln(os.Stderr, "xdev:", err)
		return tool.ApprovalPolicy{}
	}
	return autoApprovePolicy(pol)
}

func applyPolicy(ag *agent.Agent, settings *config.Settings) {
	if settings == nil {
		return
	}
	pol, err := settings.Policy()
	if err != nil {
		fmt.Fprintln(os.Stderr, "xdev:", err)
		return
	}
	ag.Policy = autoApprovePolicy(pol)
}

// childModel resolves the @task role for subagents (M9: roles resolve
// across session and children). An unconfigured or role-ineligible @task
// (it names the same provider/model as the parent, or it fails to resolve)
// keeps the parent's model: the parent asked for a worker, not a specific
// switch. A different provider requires rebuilding that provider, which
// newToolRegistry does through the same models.yml entry.
func childModel(settings *config.Settings, provName, modelName string) string {
	if settings == nil || len(settings.ModelRoles) == 0 {
		return modelName
	}
	rr, err := config.ResolveModelRef(settings, "@task")
	if err != nil || rr.Role == "" {
		return modelName
	}
	p, m, err := config.ParseModelRef(rr.Ref)
	if err != nil || p != provName {
		return modelName // cross-provider children need their own client
	}
	return m
}

// wireTaskParent stamps the parent session id onto the registry's task
// tool once the store exists (lineage for post-hoc inspection), and gives
// the todo tool its session sink (every state change lands as a
// user_todo_edit entry). Called by print, TUI, and RPC entrypoints.
func wireTaskParent(reg *tool.Registry, store *session.Store) {
	if t, ok := reg.Get(agent.TaskToolName); ok {
		if tt, ok := t.(*agent.TaskTool); ok {
			tt.ParentSessionID = store.ID()
		}
	}
	// M11 #42: bind the mailbox to the active session; called again on
	// /resume and session switches so the owner follows the live session.
	if t, ok := reg.Get(agent.InboxToolName); ok {
		if it, ok := t.(*agent.InboxTool); ok {
			it.Mailbox.SetOwner(store.ID())
		}
	}
	tool.WireTodoSink(reg, store)
	// M14 #63: --alias names this session's agent identity so other
	// sessions can address it by name; re-registered on /resume and session
	// switches, so the name follows the live session like the owner above.
	if a := config.Alias(); a != "" {
		if t, ok := reg.Get(agent.InboxToolName); ok {
			if it, ok := t.(*agent.InboxTool); ok {
				if err := it.Mailbox.RegisterIdentity(a, store.ID()); err != nil {
					logx.Errorf("alias %q: %v", a, err)
				}
			}
		}
	}
	// M11 #40: bind the goal state to the active session (again on /resume
	// and session switches — the new session starts with its own goal).
	if t, ok := reg.Get(agent.GoalToolName); ok {
		if gt, ok := t.(*agent.GoalTool); ok {
			gt.Goals.Bind(store)
		}
	}
	// M12 #45: bind the notebook to the active session (again on /resume and
	// session switches — a new session starts with its own notebook).
	if t, ok := reg.Get(agent.ContextNotesToolName); ok {
		if nt, ok := t.(*agent.NotesTool); ok {
			nt.Notes.Bind(store)
		}
	}
	// M13 #51: bind checkpoint/rewind to the active session (again on
	// /resume and session switches). The running probe stays nil here: a
	// model rewind runs inside its own turn, so a run-wide probe would
	// refuse every call. See tool.WireCheckpoint.
	tool.WireCheckpoint(reg, store, nil)
}

// failoverChain builds the M5 resilience chain from models.yml: every
// other pinned model, biggest window first (outage failover walks the
// chain in order; overflow promotion picks the smallest window that
// fits). Providers are built eagerly — the HTTP clients stay idle until
// a failover actually streams.
// failoverChain is the ordered failover target list for one model. A declared
// retry.fallbackChains entry for the active model (or its role) wins and is
// resolved through the chain engine, so a hand-written order is honored
// instead of being overridden by window size (#84). With no chain configured
// — the common case — every configured provider/model is offered, best window
// first.
func failoverChain(cfg *config.Config, settings *config.Settings, role, primaryProv, primaryModel string) []agent.FailoverTarget {
	if settings != nil && len(settings.Retry.FallbackChains) > 0 {
		targets := agent.ResolveFallbackChain(settings, role, primaryProv, primaryModel,
			agent.ConfigCatalog{Config: cfg}, declaredChainTargets(cfg, primaryProv, primaryModel))
		if out := buildChainTargets(cfg, targets); len(out) > 0 {
			return out
		}
	}
	return defaultFailoverTargets(cfg, primaryProv, primaryModel)
}

// buildChainTargets turns resolved chain entries into live failover targets.
// An entry whose provider is unknown or cannot be built is REPORTED, never
// silently dropped: a typo in a chain is otherwise invisible until the primary
// fails and the chain turns out to be empty.
func buildChainTargets(cfg *config.Config, targets []agent.ChainTarget) []agent.FailoverTarget {
	out := make([]agent.FailoverTarget, 0, len(targets))
	for _, t := range targets {
		pc := cfg.Providers[t.Provider]
		if pc == nil {
			logx.Errorf("retry.fallbackChains: unknown provider %q skipped", t.Provider)
			continue
		}
		prov, err := buildProvider(t.Provider, pc, t.Model, cfg)
		if err != nil {
			logx.Errorf("retry.fallbackChains: %s/%s unavailable: %v", t.Provider, t.Model, err)
			continue
		}
		out = append(out, agent.FailoverTarget{Provider: prov, Model: t.Model, ContextWindow: t.ContextWindow})
	}
	return out
}

// declaredChainTargets is the unconstrained chain (provider name, model id,
// window) the chain engine dedupes a declared order against.
func declaredChainTargets(cfg *config.Config, primaryProv, primaryModel string) []agent.ChainTarget {
	out := []agent.ChainTarget{}
	names := make([]string, 0, len(cfg.Providers))
	for k := range cfg.Providers {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, pname := range names {
		pc := cfg.Providers[pname]
		if pc == nil {
			continue
		}
		for _, m := range pc.Models {
			if pname == primaryProv && m.ID == primaryModel {
				continue
			}
			out = append(out, agent.ChainTarget{Provider: pname, Model: m.ID, ContextWindow: m.ContextWindow})
		}
	}
	return out
}

// defaultFailoverTargets ranks every configured model by context window.
func defaultFailoverTargets(cfg *config.Config, primaryProv, primaryModel string) []agent.FailoverTarget {
	var out []agent.FailoverTarget
	names := make([]string, 0, len(cfg.Providers))
	for k := range cfg.Providers {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, pname := range names {
		pc := cfg.Providers[pname]
		if pc == nil {
			continue
		}
		prov, err := buildProvider(pname, pc, "", cfg)
		if err != nil {
			continue
		}
		for _, m := range pc.Models {
			if pname == primaryProv && m.ID == primaryModel {
				continue
			}
			out = append(out, agent.FailoverTarget{Provider: prov, Model: m.ID, ContextWindow: m.ContextWindow})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ContextWindow > out[j].ContextWindow })
	return out
}

// openSession resumes the latest session in cwd (--continue) or starts a new
// one. New sessions auto-persist into the cwd bucket when the first assistant
// message lands — unless --no-session asked for an ephemeral one.
func openSession(cwd string, cont bool, resumePrefix string) (*session.Store, error) {
	if resumePrefix != "" {
		// Explicit --resume wins over everything: prefix resolution
		// (case-insensitive startsWith, mtime desc). A prefix that
		// matches nothing is an error — never a silent new session.
		path, err := resolveResumeID(cwd, resumePrefix)
		if err != nil {
			return nil, err
		}
		return session.Open(path)
	}
	if cont {
		// Breadcrumb-first resume (omp parity): the pane's last session wins.
		if crumb := readBreadcrumb(); crumb != "" {
			if _, err := os.Stat(crumb); err == nil {
				if st, err := session.Open(crumb); err == nil {
					return st, nil
				}
			}
		}

		metas, err := session.List(sessionDataDir())
		if err == nil {
			for _, m := range metas {
				if m.CWD != cwd || m.TitleSource == session.TitleSourceSubagent {
					continue // subagent children are never user continuations
				}
				if s, err := session.Open(m.Path); err == nil {
					return s, nil
				}
			}
		}
	}
	// Titles are mechanical (M10 #32): no ai-title call exists yet, so
	// TITLE_SYSTEM.md is discovered but unused — agent.SystemPromptOverrides
	// .TitleSystemPrompt() is where a model-generated title would read its
	// prompt override. --no-title skips the stamp, so the listing carries no
	// generated title.
	title := "print " + time.Now().Format("2006-01-02 15:04")
	if cont {
		title = "continued " + title
	}
	if launch.NoTitle {
		title = ""
	}
	s := session.OpenMem(cwd, title)
	if launch.NoSession {
		// --no-session: memory-only. Nothing is written, no breadcrumb path
		// is produced, and the store's Path() stays empty.
		return s, nil
	}
	now := time.Now().UTC()
	s.EnableAutoPersist(
		session.SessionFilePath(sessionDataDir(), cwd, now, s.ID()),
		session.Options{},
	)
	return s, nil
}

// initialHistory builds the first user message (or replays context on --continue).
func initialHistory(store *session.Store, prompt string) ([]ai.Message, error) {
	if len(store.Entries()) == 0 {
		msg := ai.Message{
			Role:        ai.RoleUser,
			Content:     []ai.Block{ai.TextBlock{Text: prompt}},
			Attribution: "user",
			UserTS:      time.Now().UnixMilli(),
		}
		if err := store.Append(&session.MessageEntry{Message: msg}); err != nil {
			return nil, err
		}
		return []ai.Message{msg}, nil
	}
	// Resume: reconstruct context from the tree, append the new prompt.
	ctxRes, err := session.BuildContext(store.Entries(), store.LeafID(), session.SystemPrompt{})
	if err != nil {
		return nil, err
	}
	history := ctxRes.Messages
	if strings.TrimSpace(prompt) != "" {
		msg := ai.Message{
			Role:        ai.RoleUser,
			Content:     []ai.Block{ai.TextBlock{Text: prompt}},
			Attribution: "user",
			UserTS:      time.Now().UnixMilli(),
		}
		if err := store.Append(&session.MessageEntry{Message: msg}); err != nil {
			return nil, err
		}
		history = append(history, msg)
	}
	return history, nil
}

// printHooks streams to stdout/stderr and persists on message_end.
type printHooks struct {
	store *session.Store
	// showThinking mirrors settings.showThinking: off suppresses the
	// stderr reasoning stream (issue #20).
	showThinking bool
	// advisorFeed snapshots the history for the background reviewer after
	// each turn (nil = no advisor). Assigned after the agent exists — the
	// reviewer needs the run to steer into, and the run needs the hooks at
	// construction — the same late-assignment the TUI uses.
	advisorFeed func()
}

func (h *printHooks) OnStart(req ai.StreamRequest) {}

func (h *printHooks) OnEvent(ev ai.Event) {
	switch ev.Type {
	case ai.EventTextDelta:
		fmt.Print(ev.Delta)
	case ai.EventThinkingDelta:
		// print mode: thinking goes to stderr — only when enabled.
		if h.showThinking {
			fmt.Fprint(os.Stderr, ev.Delta)
		}
	case ai.EventToolcallStart:
		fmt.Fprintf(os.Stderr, "\n⟨%s⟩\n", ev.ToolName)
	case ai.EventError:
		fmt.Fprintf(os.Stderr, "\n[stream error: %v]\n", ev.Err)
	}
}

func (h *printHooks) OnToolStart(call ai.ToolCallBlock) {
	if b, err := json.Marshal(call.Arguments); err == nil && len(b) < 300 {
		fmt.Fprintf(os.Stderr, "→ %s(%s)\n", call.Name, b)
	} else {
		fmt.Fprintf(os.Stderr, "→ %s(...)\n", call.Name)
	}
}

func (h *printHooks) OnToolEnd(call ai.ToolCallBlock, res tool.Result, dur time.Duration) {
	status := "ok"
	if res.IsError {
		status = "error"
	}
	fmt.Fprintf(os.Stderr, "← %s [%s, %s]\n", call.Name, status, dur.Round(time.Millisecond))
	if strings.TrimSpace(res.Text) != "" {
		fmt.Fprintln(os.Stderr, res.Text)
	}
}

// OnMessageEnd persists the assistant message (persistence on message_end only).
func (h *printHooks) OnMessageEnd(msg *ai.Message) {
	if msg == nil || msg.Role != ai.RoleAssistant {
		return
	}
	if err := h.store.Append(&session.MessageEntry{Message: *msg}); err != nil {
		logx.Errorf("persist assistant message: %v", err)
	}
}

func (h *printHooks) OnToolResultMessage(msg *ai.Message) {
	if err := h.store.Append(&session.MessageEntry{Message: *msg}); err != nil {
		logx.Errorf("persist toolResult: %v", err)
	}
}

func (h *printHooks) OnTurnEnd(reason ai.StopReason, err error) {
	// A failed turn has nothing worth reviewing (the retry ladder owns
	// that path); feed the delta only after a clean one.
	if h.advisorFeed != nil && err == nil {
		go h.advisorFeed()
	}
}
func (h *printHooks) OnCompaction(tokensBefore int64) {
	fmt.Fprintf(os.Stderr, "\n[context compacted at ~%d tokens]\n", tokensBefore)
}

// OnGoalUpdated implements agent.GoalHook: goal transitions surface on stderr
// in print mode.
func (h *printHooks) OnGoalUpdated(g agent.Goal) {
	fmt.Fprintf(os.Stderr, "\n[goal %s] %s\n", g.Status, g.Objective)
}

var _ = filepath.Join

// skillPromptBlock lists discovered skills for the model: name +
// description only, with the full body reachable via read skill://name.
// Hidden and model-invocation-disabled skills stay out of the list but
// remain reachable explicitly. --skills globs and --no-skills (issue #33)
// filter what is advertised: discovery is the surface the model acts on, so
// filtering it is what makes the flags mean something.
func skillPromptBlock(cwd string) string {
	list := skills.Discover(cwd)
	var b strings.Builder
	for _, s := range list {
		if s.Hide || s.DisableModelInvocation || !skillAllowed(s.Name) {
			continue
		}
		fmt.Fprintf(&b, "\n%s: %s", s.Name, s.Description)
	}
	if b.Len() == 0 {
		return ""
	}
	return "# Skills\n\nLoad a skill with read skill://<name> before acting on a task it covers." + b.String()
}
