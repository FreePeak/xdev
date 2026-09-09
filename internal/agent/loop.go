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

// MaxTurns bounds one Run against runaway tool loops.
const MaxTurns = 32

// MaxToolWorkers bounds the same-batch tool pool (PRD: ~4-8).
const MaxToolWorkers = 6

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
	// Model is the provider-specific model id passed as StreamRequest.Model.
	Model string
	// Store is the session mirror of record. When set, it feeds compaction
	// (threshold + overflow) and context rebuilds; nil disables compaction.
	Store *session.Store
	// Compaction configures context maintenance; ContextWindow 0 disables.
	Compaction CompactionConfig

	steerMu  sync.Mutex
	steering []Steering
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

// Run executes turns until the model stops calling tools or MaxTurns.
// history is the conversation so far (mutable within this run: assistant
// and toolResult messages are appended as the run progresses).
// Returns the terminal assistant message.
func (a *Agent) Run(ctx context.Context, system string, history []ai.Message) (*ai.Message, error) {
	var lastAssistant *ai.Message
	for range MaxTurns {
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

		msg, err := a.oneTurnWithRecovery(ctx, system, history)
		if err != nil {
			return lastAssistant, err
		}
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
	}
	return lastAssistant, fmt.Errorf("agent: exceeded %d turns", MaxTurns)
}

// turnError marks a failed turn and whether any content event already
// reached the hooks — replaying such a turn would double-emit it.
type turnError struct {
	err            error
	contentEmitted bool
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

// oneTurnWithRecovery wraps oneTurn with the recovery ladder (omp
// TurnRecovery): transient errors backoff-and-retry in place (history is
// unchanged and nothing was persisted, so replaying is safe); context
// overflow hands recovery to compaction and retries once on the rebuilt
// history; auth/bad-request failures surface as-is.
func (a *Agent) oneTurnWithRecovery(ctx context.Context, system string, history []ai.Message) (*ai.Message, error) {
	policy := a.Retry
	if policy.MaxRetries == 0 && policy.BaseDelay == 0 {
		policy = DefaultRetryPolicy()
	}

	msg, err := a.oneTurn(ctx, system, history)
	for attempt := 1; err != nil && attempt <= policy.MaxRetries; attempt++ {
		switch ai.Classify(err) {
		case ai.ClassTransient:
			// 429/5xx/network/stream stall: retry only when nothing
			// streamed yet — replaying after visible content would
			// double-emit it to the hooks (M5 tail: retain-partial +
			// continuation replaces this).
			if turnContentEmitted(err) {
				return nil, err
			}
		case ai.ClassContextOverflow:
			// Compaction owns recovery; one shot, then give up.
			logx.Errorf("context overflow: %v", err)
			rebuilt := a.recoverOverflow(ctx)
			if rebuilt == nil {
				return nil, fmt.Errorf("agent: context overflow unrecoverable: %w", err)
			}
			return a.oneTurn(ctx, system, rebuilt)
		default:
			return nil, err
		}
		if serr := sleepBackoff(ctx, policy.delay(attempt)); serr != nil {
			return nil, serr
		}
		logx.Debugf("retry %d/%d after: %v", attempt, policy.MaxRetries, err)
		msg, err = a.oneTurn(ctx, system, history)
	}
	return msg, err
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
	req := ai.StreamRequest{
		System:    system,
		Messages:  history,
		Tools:     a.toolDefs(),
		MaxTokens: a.MaxTokens,
		Model:     a.Model,
	}
	a.Hooks.OnStart(req)

	ch, err := a.Provider.Stream(ctx, req)
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
		case ai.EventThinkingStart:
			thinkOpen = true
			emitted = true
		case ai.EventThinkingDelta:
			thinking.WriteString(ev.Delta)
			emitted = true
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
			return nil, &turnError{err: fmt.Errorf("agent: stream: %w", ev.Err), contentEmitted: emitted}
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
	out := make([]ai.ToolDef, 0, len(defs))
	for _, d := range defs {
		out = append(out, ai.ToolDef{Name: d.Name, Description: d.Description, Parameters: d.Parameters})
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
	t, ok := a.Tools.Get(call.Name)
	started := time.Now()
	a.Hooks.OnToolStart(call)
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
	res, err := t.Execute(ctx, args)
	dur := time.Since(started)
	if err != nil {
		res = tool.Result{Text: fmt.Sprintf("tool %q failed: %v", call.Name, err), IsError: true}
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
