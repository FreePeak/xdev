package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/tool"
)

// TurnHooks receive turn progress. print mode prints deltas; the session
// adapter persists messages on message_end.
type TurnHooks interface {
	OnStart(req ai.StreamRequest)
	OnEvent(ev ai.Event)
	OnToolStart(call ai.ToolCallBlock)
	OnToolEnd(call ai.ToolCallBlock, res tool.Result, dur time.Duration)
	OnMessageEnd(msg *ai.Message) // assistant message (persists here)
	OnToolResultMessage(msg *ai.Message)
	OnCompaction(tokensBefore int64) // history compacted
	OnTurnEnd(reason ai.StopReason, err error)
}

// TurnHooksFunc collects optional hook callbacks.
type TurnHooksFunc struct {
	OnStartF         func(ai.StreamRequest)
	OnEventF         func(ai.Event)
	OnToolStartF     func(ai.ToolCallBlock)
	OnToolEndF       func(ai.ToolCallBlock, tool.Result, time.Duration)
	OnMessageEndF    func(*ai.Message)
	OnToolResultMsgF func(*ai.Message)
	OnCompactionF    func(tokensBefore int64)
	OnTurnEndF       func(ai.StopReason, error)
}

func (h TurnHooksFunc) OnStart(r ai.StreamRequest) {
	if h.OnStartF != nil {
		h.OnStartF(r)
	}
}
func (h TurnHooksFunc) OnEvent(e ai.Event) {
	if h.OnEventF != nil {
		h.OnEventF(e)
	}
}
func (h TurnHooksFunc) OnToolStart(c ai.ToolCallBlock) {
	if h.OnToolStartF != nil {
		h.OnToolStartF(c)
	}
}
func (h TurnHooksFunc) OnToolEnd(c ai.ToolCallBlock, r tool.Result, d time.Duration) {
	if h.OnToolEndF != nil {
		h.OnToolEndF(c, r, d)
	}
}
func (h TurnHooksFunc) OnMessageEnd(m *ai.Message) {
	if h.OnMessageEndF != nil {
		h.OnMessageEndF(m)
	}
}
func (h TurnHooksFunc) OnToolResultMessage(m *ai.Message) {
	if h.OnToolResultMsgF != nil {
		h.OnToolResultMsgF(m)
	}
}
func (h TurnHooksFunc) OnCompaction(before int64) {
	if h.OnCompactionF != nil {
		h.OnCompactionF(before)
	}
}
func (h TurnHooksFunc) OnTurnEnd(s ai.StopReason, err error) {
	if h.OnTurnEndF != nil {
		h.OnTurnEndF(s, err)
	}
}

// DefaultMaxTurns bounds one Run against runaway tool loops.
const DefaultMaxTurns = 200

// TurnBudgetPrompt is the synthetic user message injected when a Run hits
// its turn cap: the model gets one wrap-up turn instead of a hard error.
const TurnBudgetPrompt = "turn budget reached — wrap up the current step and report status; the user can say \"continue\""

// MaxToolWorkers bounds the same-batch tool pool (PRD: ~4-8).
const MaxToolWorkers = 6

// ApprovalFunc asks the user to approve one tool call. It returns the
// verdict; an implementation with no user available (print mode, a child
// agent) must return false, which is the safe answer.
type ApprovalFunc func(call ai.ToolCallBlock, reason string) bool

// Interceptor is the extension policy seam (M7 #8): tool calls may be
// blocked or revised before execution, results may be patched after. The
// implementation (internal/ext.Manager) is fail-closed — an extension that
// dies or times out denies the call unless it opted into fail-open — and
// never runs foreign code in-process.
type Interceptor interface {
	ToolCall(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error)
	ToolResult(ctx context.Context, name string, args, result json.RawMessage) json.RawMessage
	Emit(ctx context.Context, event string, payload any)
}

// Agent runs turns: provider streaming + tool execution + steering.
type Agent struct {
	Provider ai.Provider
	Tools    *tool.Registry
	Hooks    TurnHooks
	// MaxTokens caps assistant output (0 → provider default).
	MaxTokens int
	// Retry tunes the transient-error backoff ladder; zero value →
	// DefaultRetryPolicy.
	Retry RetryPolicy
	// Failovers is the ordered backup-model chain (M5): overflow promotes
	// to a bigger window, a drained retry ladder fails over to the next
	// target. nil disables both ladders.
	Failovers []FailoverTarget
	// Model is the provider-specific model id passed as StreamRequest.Model.
	Model string
	// Store is the session mirror of record. When set, it feeds compaction
	// (threshold + overflow) and context rebuilds; nil disables compaction.
	Store *session.Store
	// curTarget indexes the active model: 0 = primary Provider/Model,
	// n ≥ 1 = Failovers[n-1] (see failover.go). Single-goroutine Run.
	curTarget int
	// Compaction configures context maintenance; ContextWindow 0 disables.
	Compaction CompactionConfig
	// MaxTurns caps one Run's turns; 0 means DefaultMaxTurns.
	MaxTurns int
	// Intercept routes tool calls/results through the extension bus
	// (nil disables interception).
	Intercept Interceptor
	// Policy is the approval configuration; Approve prompts the user when a
	// decision requires it (nil means an unattended run: prompts deny).
	Policy  tool.ApprovalPolicy
	Approve ApprovalFunc
	// Thinking requests reasoning on every turn — the resolved ":effort" of
	// the active model role. nil asks for none.
	Thinking *ai.ThinkingBudget
	// Prewalk is the one-shot model handoff (nil = disabled): after the
	// first successful edit/write, the run switches to the target model
	// through the failover machinery (see prewalk.go).
	Prewalk *Prewalk
	// TTSR is the stream-rules engine (M11 #35, nil = disabled): deltas
	// are matched against the configured rules, which may abort the turn
	// or fold a reminder into a tool result. See ttsr.go.
	TTSR *TTSR

	// PlanMode is the read-only sub-state (nil = plain mode). While
	// active, mutating tools are denied and propose is the exit; see
	// planmode.go.
	PlanMode *PlanMode

	// prewalk is the live state machine; Run is single-goroutine, no lock.
	prewalk prewalkState

	steerMu  sync.Mutex
	steering []Steering
}

func (a *Agent) effectiveMaxTurns() int {
	if a.MaxTurns > 0 {
		return a.MaxTurns
	}
	return DefaultMaxTurns
}

// Steering is a queued user message injected at a step boundary.
// Kind steer: inject into the current in-flight run's next step.
// Kind followUp: start a new run after the current one.
type Steering struct {
	Kind string // "steer" | "followUp"
	Text string
}

func (a *Agent) steer(s Steering) {
	a.steerMu.Lock()
	defer a.steerMu.Unlock()
	a.steering = append(a.steering, s)
}

// Steer queues text injected at the next step boundary of the current run.
func (a *Agent) Steer(text string) { a.steer(Steering{Kind: "steer", Text: text}) }

// FollowUp queues text that starts a new run after the current one.
func (a *Agent) FollowUp(text string) { a.steer(Steering{Kind: "followUp", Text: text}) }

// drainSteering pops all queued steering messages.
func (a *Agent) drainSteering() []Steering {
	a.steerMu.Lock()
	defer a.steerMu.Unlock()
	out := a.steering
	a.steering = nil
	return out
}

// Run executes turns until the model stops calling tools or the turn budget
// (Agent.MaxTurns, else DefaultMaxTurns) runs out. history is the
// conversation so far (mutable within this run: assistant and toolResult
// messages are appended as the run progresses).
// On budget exhaustion it asks the model for one wrap-up message instead of
// failing the run.
// Returns the terminal assistant message.
func (a *Agent) Run(ctx context.Context, system string, history []ai.Message) (*ai.Message, error) {
	if a.Hooks == nil {
		a.Hooks = TurnHooksFunc{} // no-op: an unwired agent must not panic mid-turn
	}
	if a.Intercept != nil {
		a.Intercept.Emit(ctx, "session_start", map[string]any{"model": a.Model})
		a.Intercept.Emit(ctx, "agent_start", map[string]any{"model": a.Model})
	}
	defer func() {
		if a.Intercept != nil {
			a.Intercept.Emit(ctx, "agent_end", nil)
		}
	}()
	// Plan mode reminder: teach the read-only shape for this run.
	if a.PlanMode != nil && a.PlanMode.Active {
		system += "\n\n" + planModeSystemReminder(a.PlanMode.Note)
	}
	// Magic keywords (research §8): standalone prose words in the user's
	// prompt inject a hidden, user-attributed notice for this turn. The
	// notice is persisted so a compaction rebuild replays it consistently.
	if n := len(history); n > 0 && history[n-1].Role == ai.RoleUser {
		var text string
		for _, b := range history[n-1].Content {
			if tb, ok := b.(ai.TextBlock); ok {
				text += tb.Text + "\n"
			}
		}
		for _, m := range MagicKeywordMessagesForTurns(text, a.hasTool("task")) {
			history = append(history, m)
			a.persist(m)
		}
	}
	var lastAssistant *ai.Message
	limit := a.effectiveMaxTurns()
	for turn := 0; turn < limit; turn++ {
		select {
		case <-ctx.Done():
			return lastAssistant, ctx.Err()
		default:
		}

		// Step boundary: inject queued steering as user messages. Persisted
		// too (a compaction rebuild from the store must not drop them).
		for _, s := range a.drainSteering() {
			m := ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: s.Text}}}
			history = append(history, m)
			a.persist(m)
		}

		// Threshold maintenance: compact before the window overflows.
		history = a.maybeCompact(ctx, history)
		var hist []ai.Message
		msg, hist, err := a.oneTurnWithRecovery(ctx, system, history)
		if err != nil {
			return lastAssistant, err
		}
		history = hist
		lastAssistant = msg

		if len(msg.ToolCalls()) == 0 {
			a.Hooks.OnMessageEnd(msg)
			// Messages queued during the final turn continue the run
			// (queued steering is never discarded).
			queued := a.drainSteering()
			if len(queued) == 0 {
				return msg, nil
			}
			history = append(history, *msg)
			for _, s := range queued {
				m := ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: s.Text}}}
				history = append(history, m)
				a.persist(m)
			}
			continue
		}
		// Execute tool calls concurrently on a bounded pool.
		results := a.runTools(ctx, msg.ToolCalls())
		history = append(history, *msg)
		a.Hooks.OnMessageEnd(msg)
		for _, rm := range results {
			history = append(history, rm)
			a.Hooks.OnToolResultMessage(&rm)
		}
		a.prewalkNote(results)
	}
	// Budget exhausted: ask for one wrap-up message rather than erroring.
	// The prompt is persisted so a store rebuild keeps it, and tool calls in
	// the wrap-up reply are NOT executed (session/context neutralizes the
	// dangling calls on rebuild — the budget is spent by design).
	wrap := ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: TurnBudgetPrompt}}}
	history = append(history, wrap)
	a.persist(wrap)
	msg, _, err := a.oneTurnWithRecovery(ctx, system, history)
	if err != nil {
		return lastAssistant, err
	}
	a.Hooks.OnMessageEnd(msg)
	return msg, nil
}

// turnError marks a failed turn and whether any content event already
// reached the hooks — replaying such a turn would double-emit it.
// partial carries the accumulated text/thinking when the stream died
// mid-content: the retain-and-continue path persists it and resumes the
// turn instead of replaying (M5 tail). Tool calls are never captured —
// an unpaired call would make the continuation request invalid.
type turnError struct {
	err            error
	contentEmitted bool
	partial        *ai.Message
}

func (e *turnError) Error() string { return e.err.Error() }

func (e *turnError) Unwrap() error { return e.err }

// contentEmitted reports whether the (stream) error fired after visible
// content. Errors that aren't turnErrors (e.g. stream-start failures)
// count as pre-content.
func turnContentEmitted(err error) bool {
	var te *turnError
	return errors.As(err, &te) && te.contentEmitted
}

// oneTurnWithRecovery wraps oneTurn with the full M5 recovery ladder (omp
// TurnRecovery): pre-content transient errors backoff-and-retry in place
// and fail over to the next chain target when the ladder drains; post-
// content transient errors retain the partial message and continue once
// (replaying would double-emit the visible content); context overflow
// promotes to a bigger window first and compacts only at the top of the
// ladder; auth/bad-request failures surface as-is. The returned history
// carries everything recovery appended (partials, continuation prompts,
// compacted rebuilds) so the caller's loop stays consistent.
func (a *Agent) oneTurnWithRecovery(ctx context.Context, system string, history []ai.Message) (*ai.Message, []ai.Message, error) {
	a.ttsrBeginTurn()
	// A turn that burned its interrupt budget stays quiet until it ends.
	defer a.ttsrSetQuiet(false)
	policy := a.Retry
	if policy.MaxRetries == 0 && policy.BaseDelay == 0 {
		policy = DefaultRetryPolicy()
	}
	attempt, continued, compacted := 0, false, false
	interrupted := 0
	for {
		msg, err := a.oneTurn(ctx, system, history)
		if err == nil {
			return msg, history, nil
		}
		// TTSR interrupt (M11 #35): the turn was aborted mid-stream. The
		// injected system-interrupt re-steers the model and the turn is
		// retried — deliberately NOT counted as a retry-ladder attempt,
		// and never retried by the transient/overflow branches below.
		var ti *ttsrInterrupt
		if errors.As(err, &ti) {
			history = a.ttsrResume(ctx, ti, history)
			interrupted++
			if interrupted >= ttsrMaxInterruptsPerTurn {
				a.ttsrSetQuiet(true)
			}
			continue
		}
		switch ai.Classify(err) {
		case ai.ClassTransient:
			if turnContentEmitted(err) {
				// Retain-and-continue (M5 tail): persist the partial,
				// follow with a continuation prompt, resume once. Only
				// text/thinking partials qualify — a tool call without
				// its result is not a request a provider would accept.
				var te *turnError
				if !continued && errors.As(err, &te) && te.partial != nil {
					history = append(history, *te.partial)
					a.persist(*te.partial)
					cont := ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: ContinuationPrompt}}}
					history = append(history, cont)
					a.persist(cont)
					continued = true
					if serr := sleepBackoff(ctx, policy.delay(1)); serr != nil {
						return nil, history, serr
					}
					continue
				}
				return nil, history, err
			}
			if attempt >= policy.MaxRetries {
				// Ladder drained: fail over to the next model-host;
				// without one the error surfaces.
				if nxt := a.nextFailoverTarget(); nxt > 0 {
					a.switchTarget(nxt, "recovery")
					attempt = 0
					continue
				}
				return nil, history, err
			}
			attempt++
		case ai.ClassContextOverflow:
			// Promotion before compaction (M5 tail): a bigger window may
			// just fit; each overflow climbs one ladder step. At the top
			// compaction owns recovery, once.
			if nxt := a.promotionTarget(); nxt > 0 {
				a.switchTarget(nxt, "recovery")
				continue
			}
			if compacted {
				return nil, history, fmt.Errorf("agent: context overflow unrecoverable: %w", err)
			}
			logx.Errorf("context overflow: %v", err)
			rebuilt := a.recoverOverflow(ctx)
			if rebuilt == nil {
				return nil, history, fmt.Errorf("agent: context overflow unrecoverable: %w", err)
			}
			history, compacted = rebuilt, true
		default:
			return nil, history, err
		}
		if serr := sleepBackoff(ctx, policy.delay(attempt)); serr != nil {
			return nil, history, serr
		}
		logx.Debugf("retry %d/%d after: %v", attempt, policy.MaxRetries, err)
	}
}

// recoverOverflow forces a compaction (ignoring the threshold — the
// provider just proved the context does not fit) and returns the rebuilt
// history. nil means compaction was impossible or failed.
func (a *Agent) recoverOverflow(ctx context.Context) []ai.Message {
	if a.Store == nil {
		return nil
	}
	if err := a.compact(ctx); err != nil {
		logx.Errorf("overflow compaction failed: %v", err)
		return nil
	}
	res, err := session.BuildContext(a.Store.Entries(), a.Store.LeafID(), session.SystemPrompt{})
	if err != nil {
		return nil
	}
	return res.Messages
}

// persist appends m to the session mirror when one is attached. The hooks
// persist assistant/toolResult messages themselves; this covers synthetic
// user messages (steering, continuations) the hooks never see.
// hasTool reports whether name is in the live tool registry (used to gate
// task-conditional magic keywords). A nil registry reports false.
func (a *Agent) hasTool(name string) bool {
	if a.Tools == nil {
		return false
	}
	_, ok := a.Tools.Get(name)
	return ok
}

func (a *Agent) persist(m ai.Message) {
	if a.Store == nil {
		return
	}
	if err := a.Store.Append(&session.MessageEntry{Message: m}); err != nil {
		logx.Errorf("persist message: %v", err)
	}
}

func (a *Agent) popFollowUp() string {
	a.steerMu.Lock()
	defer a.steerMu.Unlock()
	for i, s := range a.steering {
		if s.Kind == "followUp" {
			a.steering = append(a.steering[:i], a.steering[i+1:]...)
			return s.Text
		}
	}
	return ""
}

// oneTurn streams one assistant message.
func (a *Agent) oneTurn(ctx context.Context, system string, history []ai.Message) (*ai.Message, error) {
	// A child context aborts the provider stream on a TTSR interrupt; the
	// caller's context is untouched.
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req := ai.StreamRequest{
		System:    system,
		Messages:  history,
		Tools:     a.toolDefs(),
		MaxTokens: a.MaxTokens,
		Model:     a.Model,
		Thinking:  a.Thinking,
	}
	a.Hooks.OnStart(req)

	ch, err := a.Provider.Stream(sctx, req)
	if err != nil {
		return nil, fmt.Errorf("agent: stream start: %w", err)
	}

	var (
		msg                 ai.Message
		started             = time.Now()
		text                strings.Builder
		thinking            strings.Builder
		toolCalls           = map[int]*ai.ToolCallBlock{}
		order               []int
		usage               *ai.Usage
		stop                ai.StopReason
		textOpen, thinkOpen bool
		emitted             bool // any content event reached the hooks
	)
	closeBlock := func() {
		if thinkOpen {
			msg.Content = append(msg.Content, ai.ThinkingBlock{Thinking: thinking.String()})
			thinkOpen = false
		}
		if textOpen {
			msg.Content = append(msg.Content, ai.TextBlock{Text: text.String()})
			textOpen = false
		}
	}
	// ttsrAbort tears the stream down and reports the interrupt upward
	// (oneTurnWithRecovery owns the retry-with-injection path).
	ttsrAbort := func(m *TTSRMatch) (*ai.Message, error) {
		closeBlock() // flush streamed text/thinking into msg.Content
		cancel()
		ai.Drain(ch) // let the provider goroutine exit
		ti := &ttsrInterrupt{match: m}
		if len(msg.Content) > 0 {
			partial := msg
			partial.Role = ai.RoleAssistant
			ti.partial = &partial
		}
		return nil, ti
	}
	for ev := range ch {
		a.Hooks.OnEvent(ev)
		switch ev.Type {
		case ai.EventStart:
			msg.Provider, msg.API, msg.Model = ev.Provider, ev.API, ev.Model
		case ai.EventTextStart:
			textOpen = true
			emitted = true
		case ai.EventTextDelta:
			text.WriteString(ev.Delta)
			emitted = true
			if m := a.ttsrObserve(ctx, ttsrProse, ev.Delta, ev.StreamIndex); m != nil {
				return ttsrAbort(m)
			}
		case ai.EventThinkingStart:
			thinkOpen = true
			emitted = true
		case ai.EventThinkingDelta:
			thinking.WriteString(ev.Delta)
			emitted = true
			if m := a.ttsrObserve(ctx, ttsrProse, ev.Delta, ev.StreamIndex); m != nil {
				return ttsrAbort(m)
			}
		case ai.EventToolcallStart:
			closeBlock()
			emitted = true
			toolCalls[ev.StreamIndex] = &ai.ToolCallBlock{ID: ev.ToolCallID, Name: ev.ToolName, StreamIndex: ev.StreamIndex}
			order = append(order, ev.StreamIndex)
		case ai.EventToolcallDelta:
			emitted = true
			if tc := toolCalls[ev.StreamIndex]; tc != nil {
				tc.PartialArgs = ev.PartialJSON
			}
			if m := a.ttsrObserve(ctx, ttsrTool, ev.PartialJSON, ev.StreamIndex); m != nil {
				return ttsrAbort(m)
			}
		case ai.EventToolcallEnd:
			if tc := toolCalls[ev.StreamIndex]; tc != nil {
				tc.Arguments = json.RawMessage(ev.PartialJSON)
			}
		case ai.EventDone:
			closeBlock()
			stop = ev.StopReason
			usage = ev.Usage
			if ev.Message != nil {
				msg = *ev.Message
			}
		case ai.EventError:
			closeBlock() // flush any open text/thinking into msg.Content
			te := &turnError{err: fmt.Errorf("agent: stream: %w", ev.Err), contentEmitted: emitted}
			// Retain-and-continue candidate: accumulated text/thinking
			// only (tool calls stay out — an unpaired call would make
			// the continuation request invalid at the provider).
			if emitted && len(msg.Content) > 0 {
				partial := msg
				if partial.Role == "" {
					partial.Role = ai.RoleAssistant
				}
				te.partial = &partial
			}
			return nil, te
		}
	}
	msg.StopReason = stop
	msg.Usage = usage
	msg.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	msg.DurationMS = time.Since(started).Milliseconds()
	// Preserve toolCall blocks in emission order (provider-done messages
	// already carry content; rebuild only if provider didn't).
	if len(msg.Content) == 0 {
		for _, idx := range order {
			msg.Content = append(msg.Content, *toolCalls[idx])
		}
	}
	if msg.Role == "" {
		msg.Role = ai.RoleAssistant
	}
	return &msg, nil
}

func (a *Agent) toolDefs() []ai.ToolDef {
	defs := a.Tools.Defs()
	out := make([]ai.ToolDef, 0, len(defs)+1)
	for _, d := range defs {
		out = append(out, ai.ToolDef{Name: d.Name, Description: d.Description, Parameters: d.Parameters})
	}
	// Plan mode exposes its exit tool only while the sub-state is live —
	// normal-mode registries never contain propose.
	if a.PlanMode != nil && a.PlanMode.Active && a.PlanMode.Propose != nil {
		if d, ok := a.PlanMode.Propose.(interface {
			Name() string
			Description() string
			Parameters() json.RawMessage
		}); ok {
			out = append(out, ai.ToolDef{Name: d.Name(), Description: d.Description(), Parameters: d.Parameters()})
		}
	}
	return out
}

// runTools executes calls concurrently (bounded), returning toolResult
// messages IN CALL ORDER.
func (a *Agent) runTools(ctx context.Context, calls []ai.ToolCallBlock) []ai.Message {
	out := make([]ai.Message, len(calls))
	sem := make(chan struct{}, MaxToolWorkers)
	var wg sync.WaitGroup
	for i := range calls {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = a.runOneTool(ctx, calls[i])
		}(i)
	}
	wg.Wait()
	return out
}

func (a *Agent) runOneTool(ctx context.Context, call ai.ToolCallBlock) ai.Message {
	started := time.Now()
	a.Hooks.OnToolStart(call)
	// The plan-mode exit tool lives outside the registry (it is exposed
	// only while the sub-state is live), so route it before lookup.
	if a.PlanMode != nil && a.PlanMode.Active && a.PlanMode.Propose != nil && call.Name == ProposeToolName {
		res, rerr := a.PlanMode.Propose.Execute(ctx, call.Arguments)
		if rerr != nil {
			res = tool.Result{Text: "propose: " + rerr.Error(), IsError: true}
		}
		a.Hooks.OnToolEnd(call, res, time.Since(started))
		return toolResultMsg(call, res)
	}
	t, ok := a.Tools.Get(call.Name)
	if !ok {
		res := tool.Result{Text: fmt.Sprintf("unknown tool %q", call.Name), IsError: true}
		a.Hooks.OnToolEnd(call, res, time.Since(started))
		return toolResultMsg(call, res)
	}
	var args json.RawMessage
	if len(call.Arguments) > 0 {
		args = call.Arguments
	} else if call.PartialArgs != "" {
		args = json.RawMessage(call.PartialArgs)
	}
	// Plan mode (M11): mutating/unmodeled tools are denied with a pointer
	// to propose while the sub-state is active. Checked before approval —
	// a read-only run must never reach an approval prompt for a mutation.
	if denied, blocked := applyPlanMode(a.PlanMode, call); blocked {
		a.Hooks.OnToolEnd(call, denied, time.Since(started))
		return toolResultMsg(call, denied)
	}
	// Approval policy: deny/prompt are resolved before anything runs.
	if dec, err := a.Policy.Decide(call.Name, args); err == nil && dec.Action != tool.ActionAllow {
		if dec.Action == tool.ActionDeny {
			res := tool.Result{Text: "tool call denied: " + dec.Reason, IsError: true}
			a.Hooks.OnToolEnd(call, res, time.Since(started))
			return toolResultMsg(call, res)
		}
		reason := dec.Reason
		if reason == "" {
			reason = "approval required for " + call.Name
		}
		if a.Approve == nil || !a.Approve(call, reason) {
			res := tool.Result{Text: "tool call refused by user: " + reason, IsError: true}
			a.Hooks.OnToolEnd(call, res, time.Since(started))
			return toolResultMsg(call, res)
		}
	}
	if a.Intercept != nil {
		// Fail-closed policy gate: a blocked call never executes, and a
		// revised payload replaces what the model asked for.
		revised, berr := a.Intercept.ToolCall(ctx, call.Name, args)
		if berr != nil {
			res := tool.Result{Text: "tool call blocked: " + berr.Error(), IsError: true}
			a.Hooks.OnToolEnd(call, res, time.Since(started))
			return toolResultMsg(call, res)
		}
		if len(revised) > 0 {
			args = revised
			call.Arguments = revised
		}
	}
	res, err := t.Execute(ctx, args)
	dur := time.Since(started)
	if err != nil {
		res = tool.Result{Text: fmt.Sprintf("tool %q failed: %v", call.Name, err), IsError: true}
	}
	if a.Intercept != nil {
		// Extensions may rewrite the text an already-run tool produced.
		enc, _ := json.Marshal(map[string]any{"text": res.Text, "isError": res.IsError})
		if patched := a.Intercept.ToolResult(ctx, call.Name, args, enc); len(patched) > 0 {
			var p struct {
				Text    string `json:"text"`
				IsError bool   `json:"isError"`
			}
			if json.Unmarshal(patched, &p) == nil && p.Text != "" {
				res.Text, res.IsError = p.Text, p.IsError
			}
		}
	}
	if rem := a.ttsrReminder(call.StreamIndex); rem != "" {
		// Non-interrupting tool match (M11 #35): the rule may not cut the
		// call short, so its notice rides along in the tool result.
		res.Text = strings.TrimRight(res.Text, "\n") + "\n\n" + rem
	}
	a.Hooks.OnToolEnd(call, res, dur)
	return toolResultMsg(call, res)
}

func toolResultMsg(call ai.ToolCallBlock, res tool.Result) ai.Message {
	return ai.Message{
		Role:       ai.RoleToolResult,
		Content:    []ai.Block{ai.TextBlock{Text: res.Text}},
		ToolCallID: call.ID,
		ToolName:   call.Name,
		IsError:    res.IsError,
		Details:    res.Details,
	}
}
