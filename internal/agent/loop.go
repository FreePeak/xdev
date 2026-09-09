package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
	"strings"
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
	// Model is the provider-specific model id passed as StreamRequest.Model.
	Model string

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

// steer adds a steering message.
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

		// Step boundary: inject queued steering as user messages.
		for _, s := range a.drainSteering() {
			history = append(history, ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: s.Text}}})
		}

		msg, err := a.oneTurn(ctx, system, history)
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
				history = append(history, ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: s.Text}}})
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
		case ai.EventTextDelta:
			text.WriteString(ev.Delta)
		case ai.EventThinkingStart:
			thinkOpen = true
		case ai.EventThinkingDelta:
			thinking.WriteString(ev.Delta)
		case ai.EventToolcallStart:
			closeBlock()
			toolCalls[ev.StreamIndex] = &ai.ToolCallBlock{ID: ev.ToolCallID, Name: ev.ToolName, StreamIndex: ev.StreamIndex}
			order = append(order, ev.StreamIndex)
		case ai.EventToolcallDelta:
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
			return nil, fmt.Errorf("agent: stream: %w", ev.Err)
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
