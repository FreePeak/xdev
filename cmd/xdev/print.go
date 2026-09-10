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
	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/mcpclient"
	"github.com/FreePeak/xdev/internal/session"
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
	modelRef := opts.Model
	if modelRef == "" {
		modelRef = os.Getenv("XDEV_MODEL")
	}
	if modelRef == "" {
		modelRef = cfg.DefaultModelRef()
	}
	if modelRef == "" {
		return 2, fmt.Errorf("no model configured: add ~/.xdev/agent/models.yml or pass --model provider/model")
	}
	provName, modelName, err := config.ParseModelRef(modelRef)
	if err != nil {
		return 2, err
	}
	pc, ok := cfg.Providers[provName]
	if !ok {
		return 2, fmt.Errorf("unknown provider %q (have: %v)", provName, providerKeys(cfg))
	}
	prov, err := buildProvider(provName, pc)
	if err != nil {
		return 2, err
	}

	// --- tools ---
	reg := newToolRegistry(cwd, prov, modelName)

	// MCP servers (optional; absent config = nothing happens).
	mgr := attachMCP(context.Background(), reg, true)
	if mgr != nil {
		defer mgr.Close()
	}

	// --- system prompt ---
	sys := opts.SystemPrompt
	if sys == "" {
		sys = agent.SystemPromptBase
	}
	ctxFiles := agent.LoadContextFiles(cwd)
	defs := reg.Defs()
	named := make([]agent.NamedToolDef, 0, len(defs))
	for _, d := range defs {
		named = append(named, agent.NamedToolDef{Name: d.Name, Description: d.Description})
	}
	sys = agent.BuildSystemPrompt(sys, ctxFiles, named)
	if opts.AppendSystem != "" {
		sys += "\n\n" + opts.AppendSystem
	}

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
	hooks := &printHooks{store: store}
	ag := &agent.Agent{Provider: prov, Tools: reg, Hooks: hooks, MaxTokens: opts.MaxTokens, MaxTurns: opts.MaxTurns, Model: modelName, Store: store, Compaction: agent.CompactionConfig{ContextWindow: modelWindow(cfg, provName, modelName)}, Failovers: failoverChain(cfg, provName, modelName)}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	history, err := initialHistory(store, prompt)
	if err != nil {
		return 2, err
	}

	logx.Debugf("print: model=%s session=%s", modelRef, store.Path())
	started := time.Now()
	final, err := ag.Run(ctx, sys, history)
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
func buildProvider(name string, pc *config.ProviderConfig) (ai.Provider, error) {
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
	apiKey := config.Resolve(pc.APIKey)
	switch pc.API {
	case ai.APIOpenAICompletions:
		return ai.NewOpenAICompletionsProvider(name, baseURL, apiKey, headers, hc), nil
	case ai.APIOpenAIResponses:
		return ai.NewOpenAIResponsesProvider(name, baseURL, apiKey, headers, hc), nil
	case ai.APIAnthropicMessages:
		return ai.NewAnthropicProvider(name, baseURL, apiKey, headers, hc), nil
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
	for _, m := range pc.Models {
		if m.ID == model {
			return m.ContextWindow
		}
	}
	return 0
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
func newToolRegistry(cwd string, prov ai.Provider, modelName string) *tool.Registry {
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
	reg.Register(&agent.TaskTool{
		Provider: prov,
		Model:    modelName,
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
	})
	return reg
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
		prov, err := buildProvider(pname, pc)
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
}

func (h *printHooks) OnStart(req ai.StreamRequest) {}

func (h *printHooks) OnEvent(ev ai.Event) {
	switch ev.Type {
	case ai.EventTextDelta:
		fmt.Print(ev.Delta)
	case ai.EventThinkingDelta:
		// print mode: thinking goes to stderr.
		fmt.Fprint(os.Stderr, ev.Delta)
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
