package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/protocol"
	"github.com/FreePeak/xdev/internal/rpc"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/tool"
)

// runRPC serves the M6 embedder contract on stdio: JSONL frames in, one
// response per command plus streamed agent events out (issue #7).
func runRPC(opts printOptions) (exitCode int, err error) {
	cwd, err := os.Getwd()
	if err != nil {
		return 2, err
	}
	cfg, err := config.LoadModelsLayered()
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
		return 2, fmt.Errorf("unknown provider %q", provName)
	}
	prov, err := buildProvider(provName, pc, modelName, cfg)
	if err != nil {
		return 2, err
	}

	reg := newToolRegistry(cwd, prov, provName, modelName, lastSettings(), effortBudget(effortRef), nil)
	mgr := attachMCP(context.Background(), reg, false)
	if mgr != nil {
		defer mgr.Close()
	}

	// Recomputed per prompt: async MCP/extension tools must reach the
	// model that is being told they exist.
	overrides := agent.LoadSystemPromptOverrides(cwd)
	buildSys := promptFn(basePrompt(opts, cwd), cwd, reg, tailSystemPrompt(overrides, opts.AppendSystem))

	store, err := openSession(cwd, opts.ContinueLast, opts.ResumePrefix)
	if err != nil {
		return 2, fmt.Errorf("session: %w", err)
	}
	_ = store.Append(&session.ModelChangeEntry{Model: modelRef})
	wireTaskParent(reg, store)

	h := &rpcHandler{
		cwd: cwd, buildSys: buildSys, reg: reg, cfg: cfg,
		provName: provName, modelName: modelName,
		maxTokens: opts.MaxTokens, maxTurns: opts.MaxTurns,
		store: store,
	}
	h.agent = &agent.Agent{
		Provider: prov, Tools: reg, Store: store, Model: modelName,
		MaxTokens: opts.MaxTokens, MaxTurns: opts.MaxTurns, Hooks: h,
		TTSR:       agent.NewTTSR(lastSettings().TTSR),
		Compaction: agent.CompactionConfig{ContextWindow: modelWindow(cfg, provName, modelName), Methods: agent.ParseMethodOrder(lastSettings().CompactionMethodOrder())},
		Policy:     agentPolicy(),
		Failovers:  failoverChain(cfg, provName, modelName),
		Thinking:   effortBudget(effortRef),
	}

	// Extension processes: tools join the registry, and the manager is the
	// agent's fail-closed policy interceptor; actions steer the live run.
	if exts := attachExtensions(context.Background(), reg, h.agent.Steer, h.agent.FollowUp, cfg); exts != nil {
		h.agent.Intercept = exts
		defer exts.Close()
	}
	defer func() {
		h.mu.Lock()
		st := h.store
		h.mu.Unlock()
		if cerr := st.Close(); cerr != nil {
			logx.Errorf("session close: %v", cerr)
		}
	}()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	h.srv = rpc.New(os.Stdin, os.Stdout, h)
	if err := h.srv.Serve(ctx); err != nil && ctx.Err() == nil {
		return 1, err
	}
	return 0, nil
}

// rpcHandler implements rpc.Handler over one store + agent. One turn runs
// at a time; prompt-while-running is refused (steer/follow_up queue into
// the live turn instead). It doubles as the agent TurnHooks, forwarding
// every stream event to the client.
type rpcHandler struct {
	srv *rpc.Server

	mu      sync.Mutex
	running bool
	curID   string
	cancel  context.CancelFunc

	store *session.Store
	agent *agent.Agent

	cwd                 string
	buildSys            func() string
	reg                 *tool.Registry
	cfg                 *config.Config
	provName, modelName string
	maxTokens, maxTurns int
}

// --- rpc.Handler ---

func (h *rpcHandler) Prompt(id, text string) {
	h.mu.Lock()
	if h.running {
		h.mu.Unlock()
		h.srv.Respond(id, protocol.Response{Ok: false, Err: "a turn is already running — send steer/follow_up instead"})
		return
	}
	h.running, h.curID = true, id
	user := ai.Message{
		Role:        ai.RoleUser,
		Content:     []ai.Block{ai.TextBlock{Text: text}},
		Attribution: "user",
		UserTS:      time.Now().UnixMilli(),
	}
	if err := h.store.Append(&session.MessageEntry{Message: user}); err != nil {
		logx.Errorf("persist user message: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	hist := h.historyLocked()
	h.mu.Unlock()

	go func() {
		defer cancel()
		msg, err := h.agent.Run(ctx, h.buildSys(), hist)
		resp := protocol.Response{Ok: err == nil}
		if err != nil {
			resp.Err = err.Error()
		} else if msg != nil {
			resp.Text, resp.StopReason = msg.Text(), string(msg.StopReason)
		}
		h.mu.Lock()
		h.running = false
		h.mu.Unlock()
		h.srv.Respond(id, resp)
	}()
}

func (h *rpcHandler) Steer(text string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.running {
		return fmt.Errorf("no turn running — send prompt")
	}
	h.agent.Steer(text)
	return nil
}

func (h *rpcHandler) FollowUp(text string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.agent.FollowUp(text)
	return nil
}

func (h *rpcHandler) Abort() error {
	h.mu.Lock()
	running, cancel := h.running, h.cancel
	h.mu.Unlock()
	if !running || cancel == nil {
		return fmt.Errorf("no turn running")
	}
	cancel()
	return nil
}

func (h *rpcHandler) NewSession() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.running {
		return fmt.Errorf("a turn is running — abort it first")
	}
	now := time.Now().UTC()
	s := session.OpenMem(h.cwd, "rpc "+now.Format("2006-01-02 15:04"))
	s.EnableAutoPersist(
		session.SessionFilePath(config.DataDir(), h.cwd, now, s.ID()),
		session.Options{},
	)
	old := h.store
	h.store = s
	h.agent.Store = s
	wireTaskParent(h.reg, s)
	if cerr := old.Close(); cerr != nil {
		logx.Errorf("previous session close: %v", cerr)
	}
	return nil
}

func (h *rpcHandler) State() protocol.State {
	h.mu.Lock()
	defer h.mu.Unlock()
	return protocol.State{
		SessionID: h.store.ID(),
		Title:     h.store.Title(),
		Model:     h.provName + "/" + h.modelName,
		CWD:       h.cwd,
		Running:   h.running,
	}
}

func (h *rpcHandler) SetModel(ref string) error {
	provName, modelName, err := config.ParseModelRef(ref)
	if err != nil {
		return err
	}
	pc, ok := h.cfg.Providers[provName]
	if !ok {
		return fmt.Errorf("unknown provider %q", provName)
	}
	prov, err := buildProvider(provName, pc, modelName, h.cfg)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.running {
		return fmt.Errorf("a turn is running — abort it first")
	}
	h.provName, h.modelName = provName, modelName
	h.agent.Provider = prov
	h.agent.Model = modelName
	h.agent.Compaction = agent.CompactionConfig{ContextWindow: modelWindow(h.cfg, provName, modelName), Methods: agent.ParseMethodOrder(lastSettings().CompactionMethodOrder())}
	h.agent.Failovers = failoverChain(h.cfg, provName, modelName)
	// Children must spawn on the current model, not the one captured at
	// startup.
	if t, ok := h.reg.Get(agent.TaskToolName); ok {
		if tt, ok := t.(*agent.TaskTool); ok {
			tt.Provider, tt.Model = prov, modelName
		}
	}
	_ = h.store.Append(&session.ModelChangeEntry{Model: ref})
	return nil
}

// historyLocked rebuilds the context from the store mirror. Call with mu held.
func (h *rpcHandler) historyLocked() []ai.Message {
	res, err := session.BuildContext(h.store.Entries(), h.store.LeafID(), session.SystemPrompt{})
	if err != nil {
		return nil
	}
	return res.Messages
}

// --- agent.TurnHooks (event forwarding + persistence on message_end) ---

func (h *rpcHandler) OnStart(req ai.StreamRequest) {}

func (h *rpcHandler) OnEvent(ev ai.Event) {
	h.mu.Lock()
	id := h.curID
	h.mu.Unlock()
	if id != "" {
		h.srv.SendEvent(id, protocol.EventFromAI(ev))
	}
}

func (h *rpcHandler) OnToolStart(call ai.ToolCallBlock) {}

func (h *rpcHandler) OnToolEnd(call ai.ToolCallBlock, res tool.Result, d time.Duration) {}

// OnMessageEnd persists the assistant message (persistence on message_end only).
func (h *rpcHandler) OnMessageEnd(msg *ai.Message) {
	if err := h.store.Append(&session.MessageEntry{Message: *msg}); err != nil {
		logx.Errorf("persist assistant message: %v", err)
	}
}

// OnToolResultMessage persists the tool-result message.
func (h *rpcHandler) OnToolResultMessage(m *ai.Message) {
	if err := h.store.Append(&session.MessageEntry{Message: *m}); err != nil {
		logx.Errorf("persist tool result: %v", err)
	}
}

// OnCompaction and OnTurnEnd: no RPC surface yet (v2 frames).
func (h *rpcHandler) OnCompaction(before int64) {}

func (h *rpcHandler) OnTurnEnd(s ai.StopReason, err error) {}
