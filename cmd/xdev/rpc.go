package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/ext"
	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/protocol"
	"github.com/FreePeak/xdev/internal/rpc"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/tool"
)

// runRPC serves the M6 embedder contract on stdio: JSONL frames in, one
// response per command plus streamed agent events out (issue #7).
func runRPC(opts printOptions) (exitCode int, err error) {
	h, cleanup, err := newSessionCore(opts)
	if err != nil {
		return 2, err
	}
	defer cleanup()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	srv := rpc.New(os.Stdin, os.Stdout, h)
	h.srv = rpcServer{srv}
	if err := srv.Serve(ctx); err != nil && ctx.Err() == nil {
		return 1, err
	}
	return 0, nil
}

// newSessionCore builds the one-agent-per-session core the transports share:
// the provider, the live tool registry, the store, and the rpcHandler that
// bridges both to a commandSink. runRPC binds the stdout JSONL transport on
// top; runHeadless binds the HTTP/SSE service.
func newSessionCore(opts printOptions) (*rpcHandler, func(), error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, nil, err
	}
	cfg, err := config.LoadModelsLayered()
	if err != nil {
		return nil, nil, err
	}
	modelRef, effortRef, err := resolveModel(opts.Model, cfg, lastSettings())
	if err != nil {
		return nil, nil, err
	}
	provName, modelName, err := config.ParseModelRef(modelRef)
	if err != nil {
		return nil, nil, err
	}
	// The request-side thinking level: --thinking wins, else the persisted
	// `thinking` key, else the model's own ":effort" (applyThinkingFlag's
	// "auto" branch), with a non-reasoning model falling back to "auto".
	if effortRef, err = applyThinkingFlag(thinkingForModel(thinkingLevel(lastSettings(), launch.Thinking), provName, modelName, cfg), effortRef); err != nil {
		return nil, nil, err
	}
	pc, ok := cfg.Providers[provName]
	if !ok {
		return nil, nil, fmt.Errorf("unknown provider %q", provName)
	}
	prov, err := buildProvider(provName, pc, modelName, cfg)
	if err != nil {
		return nil, nil, err
	}

	reg := newToolRegistry(cwd, prov, provName, modelName, lastSettings(), effortBudget(effortRef), nil, false)
	closeHub := func() { closeSharedHub() } // hub-started children are session-scoped (T3 #8)
	mgr := attachMCP(context.Background(), reg, false, nil, nil)
	// The rebuild-time interruption notice names the calls free to repeat.
	session.ReplaySafety = reg.ReplaySafe

	// Recomputed per prompt: async MCP/extension tools must reach the
	// model that is being told they exist.
	overrides := agent.LoadSystemPromptOverrides(cwd)
	buildSys := promptFn(basePrompt(opts, cwd), cwd, reg, tailSystemPrompt(overrides, opts.AppendSystem))

	store, err := openSession(cwd, opts.ContinueLast, opts.ResumePrefix)
	if err != nil {
		if mgr != nil {
			mgr.Close()
		}
		closeHub()
		return nil, nil, fmt.Errorf("session: %w", err)
	}
	_ = store.Append(&session.ModelChangeEntry{Model: modelRef})
	wireTaskParent(reg, store)
	// A resumed session runs at the level its own transcript records, and at
	// the tool surface that level was reached through. The tool set is a
	// VIEW of the shared registry: this run's own deferred set.
	runTools := reg.View(sessionRung(store, lastSettings().SessionEffort()))

	h := &rpcHandler{
		cwd: cwd, buildSys: buildSys, reg: reg, cfg: cfg,
		provName: provName, modelName: modelName,
		maxTokens: opts.MaxTokens, maxTurns: opts.MaxTurns,
		maxTime: launch.MaxTime,
		store:   store,
	}
	h.agent = &agent.Agent{
		Provider: prov, Tools: runTools, Store: store, Model: modelName,
		MaxTokens: opts.MaxTokens, MaxTurns: opts.MaxTurns, Hooks: h,
		TTSR:       agent.NewTTSR(ttsrConfig(lastSettings())),
		Compaction: compactionConfigFor(pinnedWindow(cfg, provName, modelName), lastSettings(), agent.ParseMethodOrder(lastSettings().CompactionMethodOrder())),
		Policy:     agentPolicy(),
		Failovers:  failoverChain(cfg, lastSettings(), provName, modelName),
		Thinking:   effortThinking(sessionLevelFor(store, effortRef, provName, modelName, cfg), provName, modelName, cfg),
	}
	// Shared per-mode seams: catalog bridge + secrets redactor (#79/#80).
	// The agent's tool set is the VIEW, so the catalog it is wired to must
	// be the VIEW's: the bridge runs the tools the agent sees.
	wireAgentMode(h.agent, cfg, lastSettings(), provName, modelName, cwd, false)
	// #90: mailbox arrivals become follow-ups in this session's agent (one
	// long-lived agent per RPC process, so no indirection is needed).
	setInboxSink(func(m agent.Message) bool {
		h.agent.FollowUp(inboxFollowUp(m))
		return true
	})

	// Extension processes: tools join the registry, and the manager is the
	// agent's fail-closed policy interceptor; actions steer the live run.
	var exts *ext.Manager
	if e := attachExtensions(context.Background(), reg, h.agent.Steer, h.agent.FollowUp, cfg); e != nil {
		exts = e
		h.agent.Intercept = e
	}

	cleanup := func() {
		setInboxSink(nil)
		if exts != nil {
			exts.Close()
		}
		if mgr != nil {
			mgr.Close()
		}
		closeHub()
		h.mu.Lock()
		defer h.mu.Unlock()
		if cerr := h.store.Close(); cerr != nil {
			logx.Errorf("session close: %v", cerr)
		}
	}
	return h, cleanup, nil
}

// commandSink is what a handler needs to answer: one terminal response per
// prompt (correlated by id) and a stream of events in between. Both transports
// implement it — stdout JSONL (rpc.Server) and the headless HTTP/SSE service
// (headlessSink) — which is what lets one session core drive either without
// branching.
type commandSink interface {
	Respond(id string, resp protocol.Response)
	SendEvent(id string, ev protocol.Event)
}

// rpcServer is the stdio transport: the JSONL server owns the read loop, so it
// is its own sink.
type rpcServer struct{ srv *rpc.Server }

func (s rpcServer) Respond(id string, resp protocol.Response) { s.srv.Respond(id, resp) }
func (s rpcServer) SendEvent(id string, ev protocol.Event)    { s.srv.SendEvent(id, ev) }

// rpcHandler implements rpc.Handler over one store + agent. One turn runs
// at a time; prompt-while-running is refused (steer/follow_up queue into
// the live turn instead). It doubles as the agent TurnHooks, forwarding
// every stream event to the client.
type rpcHandler struct {
	srv commandSink

	mu      sync.Mutex
	running bool
	curID   string
	cancel  context.CancelFunc

	// notify receives the lifecycle frames only the headless transport carries
	// (nil on stdio RPC, where the contract has no frame for them yet).
	notify headlessNotifier

	store *session.Store
	agent *agent.Agent

	cwd                 string
	buildSys            func() string
	reg                 *tool.Registry
	cfg                 *config.Config
	provName, modelName string
	maxTokens, maxTurns int
	// maxTime is --max-time: one prompt may not outlive it (0 = uncapped).
	// The server itself is long-lived, so the cap is per turn — bounding
	// the process would end the session the client is still driving.
	maxTime time.Duration
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
	ctx, cancel := boundedCtx(context.Background(), h.maxTime)
	h.cancel = cancel
	hist := h.historyLocked()
	h.mu.Unlock()

	go func() {
		defer cancel()
		msg, err := h.agent.Run(ctx, h.buildSys(), hist)
		if err != nil && errors.Is(err, agent.ErrEmptyTurn) {
			// The model answered nothing after every nudge was spent
			// (a thinking-mode upstream leaving only a reasoning block,
			// or nothing at all). Rebuild context from the persisted
			// history and re-run the agent so the session auto-resumes
			// instead of dying with a dead-end error (#331).
			ctxRes, rerr := session.BuildContext(h.store.Entries(), h.store.LeafID(), session.SystemPrompt{})
			if rerr == nil {
				msg, err = h.agent.Run(ctx, h.buildSys(), ctxRes.Messages)
			}
		}
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

// CurPromptID is the id of the turn in flight ("" when idle). A transport that
// publishes a frame outside the event stream — the headless tool-result frame
// — needs to correlate it with the turn the client started, and the stream
// correlates by prompt id.
func (h *rpcHandler) CurPromptID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.curID
}

func (h *rpcHandler) SetModel(ref string) error {
	// Route through the shared resolver so RPC accepts what the flag and
	// /model accept (bare ids, provider/model refs, :effort) and a bad id fails
	// here instead of 404ing on the next turn.
	resolved, _, err := resolveModel(ref, h.cfg, lastSettings())
	if err != nil {
		return err
	}
	provName, modelName, err := config.ParseModelRef(resolved)
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
	h.agent.Compaction = compactionConfigFor(pinnedWindow(h.cfg, provName, modelName), lastSettings(), agent.ParseMethodOrder(lastSettings().CompactionMethodOrder()))
	h.agent.Failovers = failoverChain(h.cfg, lastSettings(), provName, modelName)
	// Children must spawn on the current model, not the one captured at
	// startup.
	if t, ok := h.reg.Get(agent.TaskToolName); ok {
		if tt, ok := t.(*agent.TaskTool); ok {
			tt.Provider, tt.Model = prov, modelName
		}
	}
	// Persist the RESOLVED ref: a bare id in the session log
	// would not resolve on resume (print/tui already store provider/model).
	_ = h.store.Append(&session.ModelChangeEntry{Model: provName + "/" + modelName})
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

// OnToolResultMessage persists the tool-result message, and — on the headless
// transport — publishes it: an event stream that shows the assistant's tool
// CALL but never its result is a client watching half a turn. It rides the
// notifier seam because a stdio RPC client reads the persisted entry instead
// and must see no new frame.
func (h *rpcHandler) OnToolResultMessage(m *ai.Message) {
	if err := h.store.Append(&session.MessageEntry{Message: *m}); err != nil {
		logx.Errorf("persist tool result: %v", err)
	}
	if h.notify != nil {
		h.notify.NotifyToolResult(m)
	}
}

// headlessNotifier is set by the headless transport: the frames the SSE
// stream should carry that the RPC contract does not have yet. A stdio client
// reads the persisted entries instead, so a bare rpcHandler (nil notifier)
// behaves exactly as before.
type headlessNotifier interface {
	// NotifyCompaction maps to the SSE `session_compact` frame: the history a
	// client just read is being replaced by a summary plus the kept tail, and
	// it has no local peer to learn that from (the TUI paints a system block
	// for the same event).
	NotifyCompaction(before int64)
	// NotifyTurnEnd maps to the SSE `turn_end` frame: the terminal reason, so
	// a client can tell "the model finished" from "the stream dropped" even
	// when the terminal response frame never arrives (an abort).
	NotifyTurnEnd(reason ai.StopReason, err error)
	// NotifyToolResult maps to the SSE `tool_result` frame.
	NotifyToolResult(m *ai.Message)
}

// OnCompaction bridges the compaction notice to the headless stream when one
// is attached.
func (h *rpcHandler) OnCompaction(before int64) {
	h.notifier().NotifyCompaction(before)
}

// OnTurnEnd bridges the terminal reason to the headless stream when one is
// attached.
func (h *rpcHandler) OnTurnEnd(s ai.StopReason, err error) {
	h.notifier().NotifyTurnEnd(s, err)
}

// notifier is whichever transport attached one, or a no-op — the RPC contract
// has no frames for these yet, and a stdio client must see no behaviour change.
func (h *rpcHandler) notifier() headlessNotifier {
	if h.notify != nil {
		return h.notify
	}
	return nopNotifier{}
}

type nopNotifier struct{}

func (nopNotifier) NotifyCompaction(int64)             {}
func (nopNotifier) NotifyTurnEnd(ai.StopReason, error) {}
func (nopNotifier) NotifyToolResult(*ai.Message)       {}

// OnContinuation: same — the injected turn persists as an attributed
// message entry, so readers of the store can still count it (#283).
func (h *rpcHandler) OnContinuation(text string) {}
func (h *rpcHandler) OnEmptyTurn(text string)    {}
