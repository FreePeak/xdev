package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/FreePeak/xdev/internal/acp"
	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/tool"
)

// runACP serves the Agent Client Protocol on stdio (M14 #60): an editor drives
// the same agent loop print and rpc mode run — same provider, same tools, same
// session store — through ACP's JSON-RPC framing.
func runACP(opts printOptions) (exitCode int, err error) {
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

	// Tools are rooted at the agent's working directory: an editor spawns
	// `xdev acp` in the workspace, which is what session/new also asks for.
	reg := newToolRegistry(cwd, prov, provName, modelName, lastSettings(), effortBudget(effortRef), nil)
	defer closeSharedHub() // hub-started children are session-scoped (T3 #8)
	mgr := attachMCP(context.Background(), reg, false)
	if mgr != nil {
		defer mgr.Close()
	}

	overrides := agent.LoadSystemPromptOverrides(cwd)
	buildSys := promptFn(basePrompt(opts, cwd), cwd, reg, tailSystemPrompt(overrides, opts.AppendSystem))

	h := newACPHandler(cwd, cfg, prov, provName, modelName, reg, buildSys, effortBudget(effortRef), opts.MaxTokens, opts.MaxTurns)
	// Hooks and extension processes compose into one interceptor chain, the
	// same shape print mode uses; extensions' actions steer the live run.
	exts := attachExtensions(context.Background(), reg, h.steer, h.followUp, cfg)
	h.intercept = agent.NewChain(buildHookBus(cwd, opts, nil), exts)
	if exts != nil {
		defer exts.Close()
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	defer h.close()
	srv := acp.New(os.Stdin, os.Stdout, h)
	if err := srv.Serve(ctx); err != nil && ctx.Err() == nil {
		return 1, err
	}
	return 0, nil
}

// acpHandler is the ACP Handler over one tool registry and one agent per
// session: a prompt in one session cannot disturb another, and each session
// keeps its own transcript on disk.
type acpHandler struct {
	cwd       string
	cfg       *config.Config
	prov      ai.Provider
	provName  string
	modelName string
	reg       *tool.Registry
	buildSys  func() string
	thinking  *ai.ThinkingBudget
	maxTokens int
	maxTurns  int
	// intercept is the extension policy gate, installed on every session's
	// agent (set once, before the first session).
	intercept agent.Interceptor

	mu       sync.Mutex
	sessions map[string]*acpSession
}

func newACPHandler(cwd string, cfg *config.Config, prov ai.Provider, provName, modelName string, reg *tool.Registry, buildSys func() string, thinking *ai.ThinkingBudget, maxTokens, maxTurns int) *acpHandler {
	return &acpHandler{
		cwd: cwd, cfg: cfg, prov: prov, provName: provName, modelName: modelName,
		reg: reg, buildSys: buildSys, thinking: thinking,
		maxTokens: maxTokens, maxTurns: maxTurns,
		sessions: map[string]*acpSession{},
	}
}

// NewSession opens a session store rooted at the client's cwd and an agent
// bound to it.
func (h *acpHandler) NewSession(_ context.Context, cwd string) (string, error) {
	if cwd == "" {
		cwd = h.cwd
	}

	now := time.Now().UTC()
	if cwd != h.cwd {
		// read/write/grep resolve against the process working directory, so a
		// session opened elsewhere still edits the agent's workspace.
		logx.Debugf("acp: session cwd %q differs from the agent's working directory %q — tools stay rooted there", cwd, h.cwd)
	}
	store := session.OpenMem(cwd, "acp "+now.Format("2006-01-02 15:04"))
	store.EnableAutoPersist(session.SessionFilePath(config.DataDir(), cwd, now, store.ID()), session.Options{})
	wireTaskParent(h.reg, store)

	s := &acpSession{store: store, allowed: map[string]bool{}}
	s.ag = h.newAgent(s)
	h.mu.Lock()
	h.sessions[store.ID()] = s
	h.mu.Unlock()
	return store.ID(), nil
}

// Prompt runs one turn for a session and maps the agent's stop reason.
func (h *acpHandler) Prompt(ctx context.Context, sessionID string, blocks []acp.ContentBlock, emit acp.Emitter) (string, error) {
	h.mu.Lock()
	s := h.sessions[sessionID]
	h.mu.Unlock()
	if s == nil {
		return "", fmt.Errorf("unknown session %q", sessionID)
	}
	text := acp.PromptText(blocks)
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("prompt carries no text content")
	}
	s.setTurn(ctx, emit)
	defer s.setTurn(nil, nil)

	user := ai.Message{
		Role:        ai.RoleUser,
		Content:     []ai.Block{ai.TextBlock{Text: text}},
		Attribution: "user",
		UserTS:      time.Now().UnixMilli(),
	}
	if err := s.store.Append(&session.MessageEntry{Message: user}); err != nil {
		logx.Errorf("acp: persist user message: %v", err)
	}
	// The server turns a cancelled turn into stopReason "cancelled"; any other
	// error is answered as a JSON-RPC error.
	msg, err := s.ag.Run(ctx, h.buildSys(), history(s.store))
	if err != nil {
		return "", err
	}
	return stopReason(msg), nil
}

// close releases every session store.
func (h *acpHandler) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.sessions {
		if err := s.store.Close(); err != nil {
			logx.Errorf("acp: session close: %v", err)
		}
	}
}

// steer / followUp route extension actions to the session running a turn.
func (h *acpHandler) steer(text string) {
	if s := h.live(); s != nil {
		s.ag.Steer(text)
	}
}

func (h *acpHandler) followUp(text string) {
	if s := h.live(); s != nil {
		s.ag.FollowUp(text)
	}
}

func (h *acpHandler) live() *acpSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.sessions {
		if s.running() {
			return s
		}
	}
	return nil
}

// newAgent builds the agent for one session: the same shape rpc and print
// use, plus the ACP approval seam.
func (h *acpHandler) newAgent(s *acpSession) *agent.Agent {
	ag := &agent.Agent{
		Provider: h.prov, Tools: h.reg, Store: s.store, Model: h.modelName,
		MaxTokens: h.maxTokens, MaxTurns: h.maxTurns, Hooks: s.hooks(),
		TTSR:       agent.NewTTSR(ttsrConfig(lastSettings())),
		Compaction: agent.CompactionConfig{ContextWindow: modelWindow(h.cfg, h.provName, h.modelName), Methods: agent.ParseMethodOrder(lastSettings().CompactionMethodOrder())},
		Policy:     agentPolicy(),
		Failovers:  failoverChain(h.cfg, lastSettings(), "", h.provName, h.modelName),
		Thinking:   h.thinking,
		Intercept:  h.intercept,
		Approve:    s.approve,
	}
	// Shared per-mode seams: catalog bridge + secrets redactor (#79/#80).
	wireAgentMode(ag, h.reg, h.cfg, lastSettings(), "", h.provName, h.modelName, h.cwd)
	// NOTE (#90): ACP deliberately installs no mailbox sink. An ACP host can
	// hold several sessions in one process, and there is one mailbox owner per
	// process — routing every arrival to whichever agent was built last would
	// deliver to the wrong session. Declining instead leaves messages UNREAD
	// (the poller's contract), so `inbox` and any mode that does listen still
	// see them.
	// A compaction also reaches the bus as session_compact.
	ag.Hooks = agent.WithCompactionEvent(ag.Hooks, ag.Intercept)
	return ag
}

// acpSession is one ACP session: its store, its agent and the turn currently
// streaming (the emitter the hooks write to).
type acpSession struct {
	store *session.Store
	ag    *agent.Agent

	mu      sync.Mutex
	ctx     context.Context
	emit    acp.Emitter
	allowed map[string]bool // tool names the client approved "always"
}

func (s *acpSession) setTurn(ctx context.Context, emit acp.Emitter) {
	s.mu.Lock()
	s.ctx, s.emit = ctx, emit
	s.mu.Unlock()
}

func (s *acpSession) running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.emit != nil
}

// turn returns the live turn's emitter (nil when no turn is running).
func (s *acpSession) turn() (context.Context, acp.Emitter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctx, s.emit
}

// emitUpdate streams one update to the client of the running turn.
func (s *acpSession) emitUpdate(u acp.Update) {
	if _, emit := s.turn(); emit != nil {
		emit.Update(u)
	}
}

// approve answers the agent's approval prompt through ACP's permission
// request. "Allow always" is remembered for the session (not persisted).
func (s *acpSession) approve(call ai.ToolCallBlock, reason string) bool {
	ctx, emit := s.turn()
	if emit == nil {
		// No live turn to ask through: refuse, like print/rpc's Approve==nil
		// contract.
		return false
	}
	s.mu.Lock()
	allowed := s.allowed[call.Name]
	s.mu.Unlock()
	if allowed {
		return true
	}
	title := call.Name
	if reason != "" {
		title += ": " + reason
	}
	out, err := emit.Permission(ctx, acp.PermissionRequest{
		ToolCall: acp.PermissionToolCall{
			ToolCallID: call.ID,
			Title:      title,
			Kind:       acp.ToolKind(call.Name),
			Status:     acp.StatusPending,
			RawInput:   toolArgs(call),
		},
		Options: acp.ApprovalOptions(),
	})
	if err != nil {
		// A cancelled or unanswered request is a refusal, never an approval.
		logx.Debugf("acp: permission request failed: %v", err)
		return false
	}
	if out.OptionID == acp.OptionAllowAlways {
		s.mu.Lock()
		s.allowed[call.Name] = true
		s.mu.Unlock()
	}
	return out.Allowed()
}

// hooks maps the agent's stream onto ACP session updates.
func (s *acpSession) hooks() agent.TurnHooks {
	store := s.store
	return agent.TurnHooksFunc{
		OnEventF: func(ev ai.Event) {
			switch ev.Type {
			case ai.EventTextDelta:
				s.emitUpdate(acp.Chunk(acp.UpdateAgentMessageChunk, ev.Delta))
			case ai.EventThinkingDelta:
				s.emitUpdate(acp.Chunk(acp.UpdateAgentThoughtChunk, ev.Delta))
			}
		},
		OnToolStartF: func(call ai.ToolCallBlock) {
			s.emitUpdate(acp.ToolCallStarted(call.ID, call.Name, acp.ToolKind(call.Name), toolArgs(call)))
		},
		OnToolEndF: func(call ai.ToolCallBlock, res tool.Result, _ time.Duration) {
			status := acp.StatusCompleted
			if res.IsError {
				status = acp.StatusFailed
			}
			s.emitUpdate(acp.ToolCallUpdated(call.ID, status, res.Text))
		},
		OnMessageEndF: func(m *ai.Message) {
			if err := store.Append(&session.MessageEntry{Message: *m}); err != nil {
				logx.Errorf("acp: persist assistant message: %v", err)
			}
		},
		OnToolResultMsgF: func(m *ai.Message) {
			if err := store.Append(&session.MessageEntry{Message: *m}); err != nil {
				logx.Errorf("acp: persist tool result: %v", err)
			}
		},
	}
}

// stopReason maps the agent's stop reason onto ACP's vocabulary.
func stopReason(msg *ai.Message) string {
	if msg == nil {
		return acp.StopEndTurn
	}
	switch msg.StopReason {
	case ai.StopReasonLength:
		return acp.StopMaxTokens
	case ai.StopReasonAborted:
		return acp.StopCancelled
	case ai.StopReasonError:
		return acp.StopRefusal
	default:
		return acp.StopEndTurn
	}
}

// toolArgs is the tool call's raw input, complete or still streaming.
func toolArgs(call ai.ToolCallBlock) json.RawMessage {
	if len(call.Arguments) > 0 {
		return call.Arguments
	}
	if call.PartialArgs != "" {
		return json.RawMessage(call.PartialArgs)
	}
	return nil
}

// history rebuilds the context from the store mirror.
func history(store *session.Store) []ai.Message {
	res, err := session.BuildContext(store.Entries(), store.LeafID(), session.SystemPrompt{})
	if err != nil {
		return nil
	}
	return res.Messages
}
