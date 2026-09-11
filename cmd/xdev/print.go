package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/ext"
	hookbus "github.com/FreePeak/xdev/internal/hooks"
	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/mcpclient"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/skills"
	"github.com/FreePeak/xdev/internal/tool"
)

// printOptions configures one one-shot run.
type printOptions struct {
	Model        string
	ContinueLast bool
	SystemPrompt string
	AppendSystem string
	ResumePrefix string
	MaxTurns     int
	MaxTokens    int
	// Prewalk enables the one-shot model handoff (research §5); the target
	// defaults to the @smol role.
	Prewalk     bool
	PrewalkInto string
	// Plan starts the run in plan mode (read-only + propose exit).
	Plan bool
}

// buildAdvisor constructs the background reviewer when enabled. The
// reviewer model comes from the @advisor role; a missing role warns and
// disables (never fails the run).
func buildAdvisor(cfg *config.Config, settings *config.Settings) *agent.Advisor {
	if settings == nil || !settings.Advisor {
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
	return agent.NewAdvisor(prov, mName, reg)
}

// resolvePrewalk builds the handoff target for a run. Returns nil when
// prewalk is off or the target cannot resolve (warn, start unarmed — the
// run proceeds on the primary model).
func resolvePrewalk(opts printOptions, cfg *config.Config, settings *config.Settings) *agent.FailoverTarget {
	if !opts.Prewalk {
		return nil
	}
	ref, _, err := resolveModel(opts.PrewalkInto, cfg, settings)
	if err != nil {
		logx.Errorf("prewalk: target %q unresolved, starting on primary: %v", opts.PrewalkInto, err)
		return nil
	}
	pName, mName, err := config.ParseModelRef(ref)
	if err != nil {
		logx.Errorf("prewalk: target %q invalid, starting on primary: %v", ref, err)
		return nil
	}
	pc, ok := cfg.Providers[pName]
	if !ok {
		logx.Errorf("prewalk: unknown provider %q, starting on primary", pName)
		return nil
	}
	prov, err := buildProvider(pName, pc, mName, cfg)
	if err != nil {
		logx.Errorf("prewalk: provider for %q unavailable, starting on primary: %v", ref, err)
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
	planMode := &agent.PlanMode{Active: opts.Plan}
	reg := newToolRegistry(cwd, prov, provName, modelName, settings, effortBudget(effortRef), planMode)

	// MCP servers (optional; absent config = nothing happens).
	mgr := attachMCP(context.Background(), reg, true)
	if mgr != nil {
		defer mgr.Close()
	}

	// --- system prompt ---
	overrides := agent.LoadSystemPromptOverrides(cwd)
	buildSys := promptFn(basePrompt(opts, cwd), cwd, reg, tailSystemPrompt(overrides, opts.AppendSystem))
	_ = buildSys // resolved at Run time: late-registered tools must be in the prompt

	// --- session ---
	store, err := openSession(cwd, opts.ContinueLast, opts.ResumePrefix)
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
	hooks := &printHooks{store: store, showThinking: settings.ShowThinkingOn()}
	ag := &agent.Agent{Provider: prov, Tools: reg, Hooks: hooks, MaxTokens: opts.MaxTokens, MaxTurns: opts.MaxTurns, Model: modelName, Store: store, Compaction: agent.CompactionConfig{ContextWindow: modelWindow(cfg, provName, modelName)}, Failovers: failoverChain(cfg, provName, modelName), Thinking: effortBudget(effortRef), PlanMode: planMode}
	if t := resolvePrewalk(opts, cfg, settings); t != nil {
		ag.Prewalk = &agent.Prewalk{Target: *t}
	}
	applyPolicy(ag, settings)

	// Extension processes (optional): their tools join the registry and the
	// manager becomes the agent's fail-closed policy interceptor; runtime
	// actions steer the live run.
	exts := attachExtensions(context.Background(), reg, ag.Steer, ag.FollowUp)
	// Hooks and extensions compose into one interceptor chain; hooks must
	// fire even when no extensions are installed.
	hookBus := hookbus.FromSettings(settings.Hooks)
	if c := agent.NewChain(hookBus, exts); c != nil {
		ag.Intercept = c
	}
	if exts != nil {
		defer exts.Close()
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	history, err := initialHistory(store, prompt)
	if err != nil {
		return 2, err
	}

	logx.Debugf("print: model=%s session=%s", modelRef, store.Path())
	started := time.Now()
	// buildSys() at the call site, never a boot-captured string: extension
	// and MCP tools register after startup and must be in the prompt the
	// model is told to use (see TestPromptReflectsLiveRegistry).
	final, err := ag.Run(ctx, buildSys(), history)
	if err != nil {
		fmt.Fprintln(os.Stderr, "\nxdev: run aborted:", err)
		exitCode = 1
	}
	_ = store.Append(&session.ModelChangeEntry{Model: modelRef})
	_ = store.Append(&session.CustomEntry{CustomType: "session_exit", Data: map[string]any{"code": exitCode}})
	// Text is already streamed live via OnEvent; only close the line.
	if final != nil {
		fmt.Println()
	}
	logx.Debugf("print: done in %s", time.Since(started).Round(time.Millisecond))
	return exitCode, nil
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
	switch pc.API {
	case ai.APIOpenAICompletions:
		return ai.NewOpenAICompletionsProvider(name, baseURL, apiKey, headers, hc), nil
	case ai.APIOpenAIResponses:
		return ai.NewOpenAIResponsesProvider(name, baseURL, apiKey, headers, hc), nil
	case ai.APIAnthropicMessages:
		return ai.NewAnthropicProvider(name, baseURL, apiKey, headers, hc), nil
	case ai.APIGoogleGenerativeAI:
		return ai.NewGoogleGenAIProvider(name, baseURL, apiKey, headers, hc), nil
	default:
		return nil, fmt.Errorf("provider %q: unsupported api %q", name, pc.API)
	}
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
// on every model lookup.
var providerModelCache = map[string][]config.ModelConfig{}

func providerModels(name string, pc *config.ProviderConfig) []config.ModelConfig {
	if cached, ok := providerModelCache[name]; ok {
		return cached
	}
	out := pc.Models
	if pc.Discovery != nil {
		ctx, cancel := context.WithTimeout(context.Background(), config.DiscoveryTimeout)
		found, err := config.DiscoverModels(ctx, pc)
		cancel()
		if err != nil {
			logx.Debugf("discovery %s: %v", name, err)
		} else {
			out = found
		}
	}
	providerModelCache[name] = out
	return out
}

// promptFn builds the system prompt from the LIVE registry each time it
// is called. Tools can register after startup (async MCP, extensions), and
// a prompt frozen at boot would never mention them — the provider request
// would advertise tools the model was never told about.
func promptFn(base string, cwd string, reg *tool.Registry, appendSystem string) func() string {
	ctxFiles := agent.LoadContextFiles(cwd)
	return func() string {
		defs := reg.Defs()
		named := make([]agent.NamedToolDef, 0, len(defs))
		for _, d := range defs {
			named = append(named, agent.NamedToolDef{Name: d.Name, Description: d.Description})
		}
		sys := agent.BuildSystemPrompt(base, ctxFiles, named)
		if sb := skillPromptBlock(cwd); sb != "" {
			sys += "\n\n" + sb
		}
		if appendSystem != "" {
			sys += "\n\n" + appendSystem
		}
		return sys
	}
}

// tailSystemPrompt composes the after-tools tail of the system prompt:
// PERSONALITY.md (who the agent is) then APPEND_SYSTEM.md / the flag
// (extra instructions). A discovered PERSONALITY.md must actually reach
// the prompt — the field was previously set but never consumed.
func tailSystemPrompt(overrides agent.SystemPromptOverrides, flagAppend string) string {
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

// basePrompt resolves the system prompt base: --system-prompt flag →
// SYSTEM.md (project, then user) → the built-in default.
func basePrompt(opts printOptions, cwd string) string {
	if opts.SystemPrompt != "" {
		return opts.SystemPrompt
	}
	if o := agent.LoadSystemPromptOverrides(cwd); o.System != "" {
		return o.System
	}
	return agent.SystemPromptBase
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
// actionRouter turns extension runtime requests into agent steering. The
// agent exposes both kinds, so followUp is routed distinctly rather than
// collapsed into steer; `aside` has no loop surface yet (nearest behavior
// is steer) and `register_provider` needs the M9 provider registry.
func actionRouter(steer, followUp func(text string)) func(ext.Action) {
	return func(a ext.Action) {
		switch a.Action {
		case "steer", "aside":
			steer(a.Text)
		case "followUp":
			followUp(a.Text)
		default:
			logx.Debugf("ext: unsupported action %q", a.Action)
		}
	}
}

func attachExtensions(ctx context.Context, reg *tool.Registry, steer, followUp func(text string)) *ext.Manager {
	mgr := ext.NewManager()
	mgr.BindHost(actionRouter(steer, followUp))
	if err := mgr.Load(ctx, extensionsDir()); err != nil {
		logx.Errorf("ext: %v", err)
		return nil
	}
	tools := mgr.Tools()
	if len(tools) == 0 && len(mgr.Commands()) == 0 {
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
}

func newToolRegistry(cwd string, prov ai.Provider, provName, modelName string, settings *config.Settings, thinking *ai.ThinkingBudget, planMode *agent.PlanMode) *tool.Registry {
	registerURISchemes()
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
	} {
		reg.Register(t)
	}
	// The hub coordinates background subagents for this session (M11 #12).
	hub := agent.NewHub()
	reg.Register(&agent.TaskTool{
		Hub:      hub,
		Policy:   pol,
		Thinking: thinking,
		Provider: prov,
		Model:    childModel(settings, provName, modelName),
		CWD:      cwd,
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

	return reg
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
	return ref, "", nil
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
	return pol
}

// agentPolicy resolves the approval policy for a freshly built agent.
func agentPolicy() tool.ApprovalPolicy {
	pol, err := lastSettings().Policy()
	if err != nil {
		fmt.Fprintln(os.Stderr, "xdev:", err)
		return tool.ApprovalPolicy{}
	}
	return pol
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
	ag.Policy = pol
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
// tool once the store exists (lineage for post-hoc inspection).
func wireTaskParent(reg *tool.Registry, store *session.Store) {
	if t, ok := reg.Get(agent.TaskToolName); ok {
		if tt, ok := t.(*agent.TaskTool); ok {
			tt.ParentSessionID = store.ID()
		}
	}
}

// failoverChain builds the M5 resilience chain from models.yml: every
// other pinned model, biggest window first (outage failover walks the
// chain in order; overflow promotion picks the smallest window that
// fits). Providers are built eagerly — the HTTP clients stay idle until
// a failover actually streams.
func failoverChain(cfg *config.Config, primaryProv, primaryModel string) []agent.FailoverTarget {
	var out []agent.FailoverTarget
	names := make([]string, 0, len(cfg.Providers))
	for k := range cfg.Providers {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, pname := range names {
		pc := cfg.Providers[pname]
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
// one. New sessions auto-persist into the cwd bucket when the first
// assistant message lands.
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

		metas, err := session.List(config.DataDir())
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
	title := "print " + time.Now().Format("2006-01-02 15:04")
	if cont {
		title = "continued " + title
	}
	s := session.OpenMem(cwd, title)
	now := time.Now().UTC()
	s.EnableAutoPersist(
		session.SessionFilePath(config.DataDir(), cwd, now, s.ID()),
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

func (h *printHooks) OnTurnEnd(reason ai.StopReason, err error) {}
func (h *printHooks) OnCompaction(tokensBefore int64) {
	fmt.Fprintf(os.Stderr, "\n[context compacted at ~%d tokens]\n", tokensBefore)
}

var _ = filepath.Join

// skillPromptBlock lists discovered skills for the model: name +
// description only, with the full body reachable via read skill://name.
// Hidden and model-invocation-disabled skills stay out of the list but
// remain reachable explicitly.
func skillPromptBlock(cwd string) string {
	list := skills.Discover(cwd)
	var b strings.Builder
	for _, s := range list {
		if s.Hide || s.DisableModelInvocation {
			continue
		}
		fmt.Fprintf(&b, "\n%s: %s", s.Name, s.Description)
	}
	if b.Len() == 0 {
		return ""
	}
	return "# Skills\n\nLoad a skill with read skill://<name> before acting on a task it covers." + b.String()
}
