package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
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
	OnContinuation(text string)      // provider cut-off: a continuation turn was injected
	OnEmptyTurn(text string)         // the turn said nothing: a nudge turn was injected
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
	OnContinuationF  func(text string)
	OnEmptyTurnF     func(text string)
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
func (h TurnHooksFunc) OnContinuation(text string) {
	if h.OnContinuationF != nil {
		h.OnContinuationF(text)
	}
}
func (h TurnHooksFunc) OnEmptyTurn(text string) {
	if h.OnEmptyTurnF != nil {
		h.OnEmptyTurnF(text)
	}
}
func (h TurnHooksFunc) OnTurnEnd(s ai.StopReason, err error) {
	if h.OnTurnEndF != nil {
		h.OnTurnEndF(s, err)
	}
}

// compactionNotifier bridges the compaction call path onto the hook bus:
// compact.go invokes TurnHooks.OnCompaction after persisting the summary
// (that file is owned elsewhere this wave, so the seam lives here).
type compactionNotifier struct {
	TurnHooks
	intercept Interceptor
}

func (c compactionNotifier) OnCompaction(tokensBefore int64) {
	c.TurnHooks.OnCompaction(tokensBefore)
	c.intercept.Emit(context.Background(), "session_compact", map[string]any{"tokens_before": tokensBefore})
}

// WithCompactionEvent decorates TurnHooks so a compaction also reaches the
// interceptor bus as the omp `session_compact` event (research §4):
//
//	ag.Hooks = agent.WithCompactionEvent(ag.Hooks, ag.Intercept)
//
// A nil interceptor or hooks returns the input unchanged.
func WithCompactionEvent(h TurnHooks, i Interceptor) TurnHooks {
	if h == nil || i == nil {
		return h
	}
	return compactionNotifier{TurnHooks: h, intercept: i}
}

// DefaultMaxTurns is retained for the -max-turns flag and historical reference.
// effectiveMaxTurns returns MaxTurns directly; MaxTurns=0 means unbounded.
const DefaultMaxTurns = 200

// DefaultTurnTokenBudget is the per-turn token cap. 0 means unbounded.
//
// This used to be a *session* budget (5M tokens cumulative), and crossing it
// ended the run with a wrap-up message asking the user to say "continue" —
// so a long task that legitimately burned millions of tokens across many
// turns stopped dead mid-work and needed a human to restart it. A session
// is not a single turn: the provider caps one turn's output, so the same
// number is a per-turn floor that a real turn can never reach, and when it
// does the turn wraps up and the session keeps going instead of ending.
const DefaultTurnTokenBudget = 5000000

// TurnBudgetPrompt is the synthetic user message injected when a single
// turn crosses the per-turn token cap: the model gets one wrap-up turn
// instead of the run ending. The session keeps going after it — the
// wrap-up is a turn boundary, not a run end — so the user is never asked
// to say "continue".
const TurnBudgetPrompt = "turn budget reached — wrap up the current step and report status; the session keeps going"

// TurnBudgetAttribution tags that wrap-up prompt as harness text: the user
// never typed it, so the transcript must not invent a ❯ block for it and
// stats must not count it as typed input (#283).
const TurnBudgetAttribution = "turn-budget"

// EmptyTurnNudgePrompt is the synthetic user message injected when a turn
// ends with no text and no tool call. The model never said it was done, so
// the alternative to asking again is a session that looks like it stopped on
// its own (#331).
const EmptyTurnNudgePrompt = "your last turn produced no answer and no tool call — reply with what you have, or state the next step"

// EmptyTurnAttribution tags that nudge as harness text (same contract as
// TurnBudgetAttribution: no ❯ block, never counted as typed input, never
// handed back as a rewind draft).
const EmptyTurnAttribution = "empty-turn"

// ErrEmptyTurn is the failure voice of a model that answered nothing
// after every nudge was spent. Ending the run is what the old code did;
// ending it with an ERROR is what makes the stop visible and retryable
// upward, instead of a silent "the session just stopped" (#331 field
// report: session 1883e928 painted a stop and no error).
// ErrEmptyTurnSuffix is appended to ErrEmptyTurn so callers can
// detect the condition and recover from it.
const ErrEmptyTurnSuffix = "empty-turn"

var ErrEmptyTurn = errors.New("agent: model produced no answer and no tool call: " + ErrEmptyTurnSuffix)

// maxEmptyTurnNudges bounds blank-turn recovery. One was the old bound and
// it is not enough: a thinking-mode upstream that answers every request
// with a lone reasoning block burned the single nudge and the run then
// ended with lastAssistant still nil.
const maxEmptyTurnNudges = 2

// maxPostContentContinuations bounds retain-and-continue when the ladder is
// bounded (retry.infinite, default on, lifts it). A mid-stream failure
// after visible content cannot be replayed — that would double-emit it —
// so recovery resumes from the retained partial instead.
const maxPostContentContinuations = 3

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
	// ToolCall gates one call before it runs and may revise its arguments.
	// The whole call block travels (not just the name) so an interceptor can
	// correlate pre and post events by id — hooks carry toolCallId (#92).
	ToolCall(ctx context.Context, call ai.ToolCallBlock) (json.RawMessage, error)
	// ToolResult sees the executed call's result after the fact. isError is
	// separate because the rendered payload carries text, not the outcome
	// class an interceptor needs to stay consistent with.
	ToolResult(ctx context.Context, call ai.ToolCallBlock, result json.RawMessage, isError bool) json.RawMessage
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
	// Fallback is the retry.fallbackChains state (M5 #25): chain cooldowns,
	// usage-aware reserve fallback, credential rotation, and the
	// revert-to-primary policy. nil disables the whole feature (see
	// fallback_recovery.go).
	Fallback *FallbackState
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

	// compactIdle is the previous step boundary's wall clock: the idle
	// compaction trigger measures the gap between two of them (M5 #24,
	// compact.go). Single-goroutine Run, like everything else here.
	compactIdle time.Time
	// compactAsync holds the one background summarize the async trigger
	// may have in flight (nil = none; see compact_async.go).
	compactAsync *asyncCompactState
	// MaxTurns caps one Run's turns; 0 means unbounded (no cap).
	MaxTurns int
	// TurnTokenBudget caps one turn's token spend (provider requests +
	// retries). 0 → DefaultTurnTokenBudget. It is per-turn, not cumulative:
	// a session is not a turn, so capping the session ends a long task that
	// legitimately burned millions of tokens across many turns and asks the
	// user to say "continue" to restart it.
	TurnTokenBudget int
	// CancelGrace bounds how long a cancelled turn waits for a tool that is
	// already running (#126); 0 means DefaultCancelGrace.
	CancelGrace time.Duration
	// Offload is the artifact-offload seam for oversized tool results
	// (#283 RCA §4, backend owned by #115); nil keeps results verbatim.
	Offload ArtifactOffloader
	// Intercept routes tool calls/results through the extension bus
	// (nil disables interception).
	Intercept Interceptor
	// Policy is the approval configuration; Approve prompts the user when a
	// decision requires it (nil means an unattended run: prompts deny).
	Policy tool.ApprovalPolicy
	// MemoryContext returns the extra context a compaction summary must see
	// (the remote backend's recalled memories). nil = nothing extra — the
	// transcript alone. #86: CompactionContext existed with no caller.
	MemoryContext func() string
	// Vision reports whether the active model accepts image input. The
	// snapcompact rung keeps the dropped span as text instead of a bitmap when
	// the model cannot read one; nil means "unknown", which is treated as no.
	Vision func() bool
	// Rulebook returns the guidance of rules scoped to a touched path (globs
	// from .cursor/rules, RULES.md, plugin rulebooks). Called after a
	// successful edit/write so a path-scoped rulebook reaches the model on the
	// turn that made the change instead of only being advertised in the
	// prompt (parity finding #108: rules.ForPath had no consumer). nil =
	// advisory prompt block only, which was the whole behavior before.
	Rulebook func(path string) string
	// Redactor hides configured secrets in provider-visible text and
	// restores placeholders in inbound tool arguments (M13 #55). nil = off.
	Redactor Redactor
	Approve  ApprovalFunc
	// Thinking requests reasoning on every turn — the resolved ":effort" of
	// the active model. nil asks for none.
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

	// Goals is the session-scoped goal state (M11 #40; nil = goal mode
	// off). The goal tool mutates it; Run injects a bounded reminder at
	// each turn start and counts token spend at turn end. Left nil it is
	// discovered from the tool registry on the first Run.
	Goals *GoalState

	// GoalContinuation lets an active goal keep the run going: a turn that
	// ends with no tool calls injects the goal's continuation prompt and
	// continues instead of idling (omp's goal-continuation message). Only a
	// mode with a user sitting in front of it opts in — omp's
	// goal.continuationModes defaults to interactive — because the loop runs
	// until the goal is completed, dropped, budget-exhausted, or the run's
	// turn budget is spent.
	GoalContinuation bool

	// Handoff configures the handoff-document compaction (M5 #23): the
	// side-request target, the artifact mirror, and the per-branch reset
	// seam. See handoff.go.
	Handoff HandoffSettings

	// prewalk is the live state machine; Run is single-goroutine, no lock.
	prewalk prewalkState

	steerMu  sync.Mutex
	steering []Steering
	// requestStart is the wall clock oneTurn stamps at the start of
	// a streaming request; OnMessageEnd/Run end both read it under
	// requestMu to compute the turn's ttft (ms) and reset it so a
	// stale clock can't overwrite a real value. zero = no request in flight.
	requestStart time.Time
	requestMu    sync.Mutex
}

func (a *Agent) effectiveMaxTurns() int {
	return a.MaxTurns
}

// effectiveTurnTokenBudget returns the per-turn token cap. 0 →
// DefaultTurnTokenBudget.
func (a *Agent) effectiveTurnTokenBudget() int64 {
	if a.TurnTokenBudget > 0 {
		return int64(a.TurnTokenBudget)
	}
	return DefaultTurnTokenBudget
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

// Run executes turns until the model stops calling tools.
// history is the
// conversation so far (mutable within this run: assistant and toolResult
// messages are appended as the run progresses).
// The per-turn token cap wraps up a turn inline and the session keeps
// going; only the turn cap (MaxTurns) ends the run, and it does so with
// one wrap-up message rather than an error.
// Returns the terminal assistant message.
func (a *Agent) Run(ctx context.Context, system string, history []ai.Message) (final *ai.Message, runErr error) {
	if a.Hooks == nil {
		a.Hooks = TurnHooksFunc{} // no-op: an unwired agent must not panic mid-turn
	}
	// emit publishes one lifecycle event on the interceptor bus (nil-safe).
	emit := func(event string, payload map[string]any) {
		if a.Intercept != nil {
			a.Intercept.Emit(ctx, event, payload)
		}
	}
	emit("session_start", map[string]any{"model": a.Model})
	emit("before_agent_start", map[string]any{"model": a.Model, "system": system})
	emit("agent_start", map[string]any{"model": a.Model})
	var lastAssistant *ai.Message
	turnsUsed := 0
	// agent_end carries the run outcome. A nil payload reached hooks as
	// literal `null` (parity finding T3 #15), so an agent_end hook could
	// never match, report, or log anything about the run it closes.
	defer func() {
		if a.Intercept == nil {
			return
		}
		payload := map[string]any{"model": a.Model, "turns": turnsUsed}
		if final != nil {
			payload["stopReason"] = string(final.StopReason)
		}
		if runErr != nil {
			payload["error"] = runErr.Error()
		}
		a.Intercept.Emit(ctx, "agent_end", payload)
	}()
	// Plan mode reminder: teach the read-only shape for this run.
	if a.PlanMode != nil && a.PlanMode.Active() {
		system += "\n\n" + planModeSystemReminder(a.PlanMode.Note())
	}
	// Goal mode (M11 #40): the goal tool owns the session-scoped objective;
	// bind it (and this run's event hook) so the loop can inject the
	// per-turn reminder and account the budget.
	if a.Goals == nil {
		a.Goals = GoalStateOf(a.Tools)
	}
	if a.Goals != nil {
		a.Goals.SetOnUpdate(GoalNotify(a.Hooks))
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
	limit := a.effectiveMaxTurns()
	// Per-turn token cap (RCA #1): a session is not a turn, so capping the
	// session ended a long task that legitimately burned millions of tokens
	// across many turns and asked the user to say "continue" to restart it.
	// The cap is per-turn: the provider caps one turn's output, so this
	// number is a per-turn floor a real turn cannot reach, and when it does
	// the turn wraps up and the session keeps going instead of ending.
	turnTokenBudget := a.effectiveTurnTokenBudget()
	// nudges is per run, not per turn: the empty-completion nudge below is
	// spent at most maxEmptyTurnNudges times, so a model that can only ever
	// emit reasoning cannot make the loop spend unbounded turns on it (the
	// same shape as the TTSR interrupt budget and maxEscalationRounds).
	nudges := 0
	for turn := 0; limit == 0 || turn < limit; turn++ {
		select {
		case <-ctx.Done():
			// An abort also drops any in-flight background summarize: nothing
			// it produces would be applied, and leaving it running spends a
			// provider round-trip on a conversation the user just stopped
			// (#82 — CancelAsyncCompaction had no caller).
			a.CancelAsyncCompaction()
			return lastAssistant, ctx.Err()
		default:
		}

		turnsUsed = turn + 1
		emit("turn_start", map[string]any{"turn": turn})
		// Step boundary: inject queued steering as user messages. Persisted
		// too (a compaction rebuild from the store must not drop them).
		for _, s := range a.drainSteering() {
			m := ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: s.Text}}}
			history = append(history, m)
			a.persist(m)
		}

		// Threshold maintenance: compact before the window overflows.
		// The handoff method (M5 #23) owns this boundary when the method
		// order selects it — the document replaces the summary — and
		// degrades to the summary path when its side request fails.
		if rebuilt, ok := a.HandoffRung(ctx, a.goalSystem(system), history); ok {
			history = rebuilt
		} else {
			history = a.maybeCompact(ctx, history)
		}
		// Notes-backed rollover (M12 #45): a pending new_context boundary
		// rebuilds history from the store so the dropped middle leaves this
		// run's provider request; a no-op otherwise.
		history = a.notesRollover(history)
		var hist []ai.Message
		msg, hist, err := a.oneTurnWithRecovery(ctx, a.goalSystem(system), history)
		if err != nil {
			return lastAssistant, err
		}
		history = hist
		lastAssistant = msg
		// Goal budget accounting (M11 #40): count this turn's tokens and
		// flip an overdrawn goal to budget_exhausted — reminders stop, and
		// the objective is never completed implicitly.
		if a.Goals != nil && msg.Usage != nil {
			a.Goals.AddUsage(msg.Usage.TotalTokens)
		}

		// Per-turn token cap (RCA #1): a session is not a turn, so capping
		// the session ended a long task that legitimately burned millions of
		// tokens across many turns and asked the user to say "continue" to
		// restart it. The cap is per-turn: the provider caps one turn's
		// output, so this number is a per-turn floor a real turn cannot
		// reach. When a single turn does cross it, the turn wraps up with a
		// status report and the session keeps going instead of ending — the
		// wrap-up's tool calls run, because the session is staying alive and
		// the model is still working (the old "do not execute them" contract
		// only made sense while the run was ending).
		if turnTokenBudget > 0 && msg.Usage != nil && msg.Usage.TotalTokens >= turnTokenBudget {
			wrap := ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: TurnBudgetPrompt}}, Attribution: TurnBudgetAttribution}
			history = append(history, wrap)
			a.persist(wrap)
			wrapMsg, _, werr := a.oneTurnWithRecovery(ctx, a.goalSystem(system), history)
			if werr != nil {
				return lastAssistant, werr
			}
			a.Hooks.OnMessageEnd(wrapMsg)
			lastAssistant = wrapMsg
			emit("turn_end", map[string]any{"turn": turn})
			continue
		}

		if len(msg.ToolCalls()) == 0 {
			a.Hooks.OnMessageEnd(msg)
			// Messages queued during the final turn continue the run
			// (queued steering is never discarded).
			queued := a.drainSteering()
			// Goal continuation (M11 #40 tail): an active goal must not idle.
			// This yield would have ended the run with the objective
			// untouched — the reminder only ever rode along with a turn the
			// user started, so a goal created interactively did nothing.
			// The continuation is a hidden user message (the user did not
			// type it), persisted so a store rebuild keeps it; the goal's own
			// complete/drop/budget_exhausted is what ends the run.
			var cont string
			if len(queued) == 0 && a.GoalContinuation && a.Goals != nil {
				cont = a.Goals.ContinuationPrompt()
			}
			// Empty completion (#331): the model ended its turn with no text
			// and no tool call — only reasoning, or nothing at all. That is
			// the shape a thinking-mode upstream leaves behind when the
			// turn's content never materialized: a lone "Thought for 0.1s"
			// whose body is the gateway's own "(context elided)" reasoning
			// replay placeholder, and the run then ends with nothing on
			// screen (field report, session 1883e928). Returning here is the
			// one outcome that cannot be right — the model never said it was
			// done. One nudge asks it to actually answer.
			if nudges < maxEmptyTurnNudges && isEmptyAssistant(*msg) && len(queued) == 0 && cont == "" {
				nudges++
				nudge := ai.Message{
					Role:        ai.RoleUser,
					Content:     []ai.Block{ai.TextBlock{Text: EmptyTurnNudgePrompt}},
					Attribution: EmptyTurnAttribution,
				}
				history = append(history, nudge)
				a.persist(nudge)
				a.Hooks.OnEmptyTurn(EmptyTurnNudgePrompt)
				emit("turn_end", map[string]any{"turn": turn})
				continue
			}
			if isEmptyAssistant(*msg) && len(queued) == 0 && cont == "" {
				emit("turn_end", map[string]any{"turn": turn})
				return msg, ErrEmptyTurn
			}
			if len(queued) == 0 && cont == "" {
				emit("turn_end", map[string]any{"turn": turn})
				return msg, nil
			}
			history = append(history, *msg)
			for _, s := range queued {
				m := ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: s.Text}}}
				history = append(history, m)
				a.persist(m)
			}
			if cont != "" {
				m := ai.Message{
					Role:        ai.RoleUser,
					Content:     []ai.Block{ai.TextBlock{Text: cont}},
					Attribution: GoalContinuationAttribution,
				}
				history = append(history, m)
				a.persist(m)
			}
			emit("turn_end", map[string]any{"turn": turn})
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
		// Plan-only run (headless -plan): the proposal is the deliverable,
		// so the run ends at this boundary rather than continuing toward
		// implementation it was never allowed to start.
		if a.PlanMode.Proposed() {
			emit("turn_end", map[string]any{"turn": turn})
			return msg, nil
		}
		emit("turn_end", map[string]any{"turn": turn})
	}

	// Turn cap reached: ask for one wrap-up message rather than erroring.
	// The per-turn token cap is handled inline above and never ends the
	// run; only a user-set turn limit stops the session here.
	if limit > 0 {
		wrap := ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: TurnBudgetPrompt}}, Attribution: TurnBudgetAttribution}
		history = append(history, wrap)
		a.persist(wrap)
		msg, _, err := a.oneTurnWithRecovery(ctx, a.goalSystem(system), history)
		if err != nil {
			return lastAssistant, err
		}
		a.Hooks.OnMessageEnd(msg)
		a.flushTTFT()
		emit("turn_end", map[string]any{"turn": limit})
		return msg, nil
	}

	return lastAssistant, nil
}

// flushTTFT hands the last completed turn's ttft (ms) to OnTurnEnd
// (nil-safe). Called at Run end so a turn whose OnMessageEnd already
// wrote msg.TTFTMS still surfaces it — Run outlives oneTurn, so a
// zero clock there means the turn's hook still has to deliver it.
func (a *Agent) flushTTFT() {
	a.requestMu.Lock()
	start := a.requestStart
	a.requestStart = time.Time{}
	a.requestMu.Unlock()
	if f, ok := a.Hooks.(interface{ FlushTTFT() }); ok && !start.IsZero() {
		f.FlushTTFT()
	}
}

// goalSystem appends the bounded goal reminder for the active goal to the
// turn's system context (unchanged when no goal is active). It is recomputed
// per turn so "budget remaining" stays current; the base prompt is untouched,
// so a cached prefix still matches.
func (a *Agent) goalSystem(system string) string {
	if a.Goals == nil {
		return system
	}
	if rem := a.Goals.Reminder(); rem != "" {
		return system + "\n\n" + rem
	}
	return system
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
// ladder. Auth / bad-request / unknown failures are retried from the
// current context window (bounded by maxEscalationRounds; retry.infinite
// lifts the bound) so a transient upstream verdict does not end the
// session. The returned history carries everything recovery appended
// (partials, continuation prompts, compacted rebuilds) so the caller's
// loop stays consistent.
func (a *Agent) oneTurnWithRecovery(ctx context.Context, system string, history []ai.Message) (*ai.Message, []ai.Message, error) {
	a.ttsrBeginTurn()
	// A turn that burned its interrupt budget stays quiet until it ends.
	defer a.ttsrSetQuiet(false)
	// Fallback machinery (M5 #25): a fallback cooldown may have expired
	// (revert to the primary), and the usage-reserve policy is checked
	// before a turn is spent on a near-quota target.
	a.fallbackPreTurn()
	policy := a.Retry.withDefaults()
	attempt, continued, compacted := 0, false, false
	escalation := 0
	interrupted := 0
	for {
		// Health-check the active provider before spending a turn: a dead
		// host is not a transient blip, and the backoff ladder burns
		// its attempts on an endpoint that cannot serve. Fail over
		// before the ladder drains so a restart reads as waiting.
		if hcErr := a.healthCheckProvider(ctx); hcErr != nil {
			if nxt := a.nextFailoverTarget(); nxt > 0 {
				a.switchTarget(nxt, "health-check")
				attempt = 0
				continue
			}
			return nil, history, fmt.Errorf("agent: health check failed and all targets drained: %w", hcErr)
		}
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
		// Usage-limit recovery (M5 #25): a spent quota is not a blip —
		// rotate to a sibling credential or step the chain before the
		// backoff ladder burns its attempts on a target that cannot serve.
		if a.recoverUsageLimit(err) {
			attempt = 0
			continue
		}
		switch ai.Classify(err) {
		case ai.ClassTransient:
			if turnContentEmitted(err) {
				// Retain-and-continue (M5 tail): persist the partial,
				// follow with a continuation prompt, resume. On a BOUNDED
				// ladder (retry.infinite off) the budget is
				// maxPostContentContinuations; retry.infinite (the default)
				// lifts it. Only text/thinking partials qualify — a tool
				// call without its result is not a request a provider
				// would accept.
				var te *turnError
				resumeLeft := maxPostContentContinuations
				if policy.Infinite {
					resumeLeft = -1
				}
				if !continued && (policy.Infinite || resumeLeft > 0) && errors.As(err, &te) && te.partial != nil {
					history = append(history, *te.partial)
					a.persist(*te.partial)
					cont := ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: ContinuationPrompt}}, Attribution: ContinuationAttribution}
					history = append(history, cont)
					a.persist(cont)
					continued = true
					a.Hooks.OnContinuation(ContinuationPrompt)
					if policy.Infinite {
						logx.Errorf("recovery: post-content failure on an infinite ladder (retry.infinite)")
					} else {
						logx.Errorf("recovery: post-content failure %d of %d", maxPostContentContinuations-resumeLeft+1, maxPostContentContinuations)
					}
					if serr := sleepBackoff(ctx, policy.delay(1)); serr != nil {
						return nil, history, serr
					}
					continue
				}
				if continued && !policy.Infinite {
					return nil, history, err
				}
				continue
			}
			if attempt >= policy.MaxRetries {
				// Ladder drained: fail over to the next model-host.
				if nxt := a.nextFailoverTarget(); nxt > 0 {
					a.switchTarget(nxt, "recovery")
					attempt = 0
					continue
				}
				// Chain drained too: re-run the ladder on the current
				// target. Rounds are bounded so a hard failure
				// misclassified as transient still ends the turn (see
				// maxEscalationRounds); retry.infinite lifts the bound and
				// announces each round on the event stream, so an outage
				// of any length reads as waiting rather than hanging.
				if escalation < maxEscalationRounds || policy.Infinite {
					escalation++
					attempt = 0
					d := policy.delay(policy.MaxRetries + 1)
					logx.Errorf("recovery: all targets drained, escalation round %d after backoff", escalation)
					if policy.Infinite {
						a.noticeAllTargetsDown(escalation, d, err)
						// Re-enter the pre-turn pass so an expired fallback
						// cooldown restores the primary mid-outage: the wait
						// re-walks the chain from the target the user chose,
						// not from wherever the last failover landed.
						a.fallbackPreTurn()
					}
					if serr := sleepBackoff(ctx, d); serr != nil {
						return nil, history, serr
					}
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
				// "promotion" (not "recovery"): a window upgrade is a
				// deliberate, persistent climb, not a fallback — it must
				// not arm the revert-to-primary policy (M5 #25).
				a.switchTarget(nxt, "promotion")
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
		case ai.ClassEmptyTurn:
			// Empty turn (#389, #331): the model answered nothing —
			// reasoning-only, or nothing at all. Rebuild context
			// from the persisted history and re-run the ladder.
			rebuilt, rerr := a.recoverEmptyTurn(ctx, history)
			if rerr != nil {
				return nil, history, fmt.Errorf("agent: empty turn unrecoverable: %w", rerr)
			}
			if rebuilt == nil {
				return nil, history, err
			}
			history = rebuilt
			continue
		default:
			// Retry everything else (auth, bad request, unknown):
			// rebuild history from the persisted session when one
			// is attached and re-send the same request on the
			// current context window. Escalation is bounded by
			// maxEscalationRounds; retry.infinite lifts it so an
			// outage of any length is survived. The last provider
			// error is returned as-is once the bound is spent.
			if escalation < maxEscalationRounds || policy.Infinite {
				escalation++
				attempt = 0
				logx.Errorf("recovery: retrying %v from current context", ai.Classify(err))
				if a.Store != nil {
					if res, buildErr := session.BuildContext(a.Store.Entries(), a.Store.LeafID(), session.SystemPrompt{}); buildErr == nil {
						history = res.Messages
					}
				}
				if serr := sleepBackoff(ctx, policy.delay(attempt)); serr != nil {
					return nil, history, serr
				}
				continue
			}
			return nil, history, err
		}
		// omp's auto_retry_* pair (#92): a hook that logs provider health
		// needs the attempt and the reason, not just a debug line. Emitted
		// before the backoff so a long wait is visible while it happens.
		a.emitIntercept(ctx, "auto_retry_start", map[string]any{
			"attempt": attempt, "maxAttempts": policy.MaxRetries,
			"kind": string(ai.Classify(err)), "error": err.Error(),
			"delayMs": policy.delay(attempt).Milliseconds(),
		})
		serr := sleepBackoff(ctx, policy.delay(attempt))
		// "ok" is whether the BACKOFF completed (false = the run was aborted
		// during it), not whether the retry succeeded — the next
		// auto_retry_start / agent_end carries the outcome.
		a.emitIntercept(ctx, "auto_retry_end", map[string]any{
			"attempt": attempt, "ok": serr == nil,
		})
		if serr != nil {
			return nil, history, serr
		}
		logx.Debugf("retry %d/%d after: %v", attempt, policy.MaxRetries, err)
	}
}

// noticeAllTargetsDown raises one unbounded-wait round on the event stream
// (retry.infinite only). Nil-safe: modes that never install hooks stay quiet
// and keep their logx line.
func (a *Agent) noticeAllTargetsDown(round int, d time.Duration, last error) {
	if a == nil || a.Hooks == nil {
		return
	}
	a.Hooks.OnEvent(ai.Errorf(&AllTargetsDownError{Round: round, Delay: d, LastErr: last}))
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

func (a *Agent) recoverEmptyTurn(ctx context.Context, history []ai.Message) ([]ai.Message, error) {
	if a.Store == nil {
		return nil, nil
	}
	res, err := session.BuildContext(a.Store.Entries(), a.Store.LeafID(), session.SystemPrompt{})
	if err != nil {
		return nil, err
	}
	return res.Messages, nil
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

// healthCheckProvider probes the active provider's liveness via the
// ai.HealthChecker seam (nil-safe: nil provider or no interface
// method = probe unavailable = probe passes). Called once per
// turn, before any request is spent: a provider that answers
// gets the turn; one that does not fail over before the backoff
// ladder burns its attempts on a dead endpoint.
func (a *Agent) healthCheckProvider(ctx context.Context) error {
	if a == nil || a.Provider == nil {
		return nil
	}
	hc, ok := a.Provider.(ai.HealthChecker)
	if !ok {
		return nil
	}
	return hc.HealthCheck(ctx)
}

// oneTurn streams one assistant message.
func (a *Agent) oneTurn(ctx context.Context, system string, history []ai.Message) (*ai.Message, error) {
	// A child context aborts the provider stream on a TTSR interrupt; the
	// caller's context is untouched.
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.requestMu.Lock()
	a.requestStart = time.Now()
	a.requestMu.Unlock()
	req := a.liveRequest(system, history, a.toolDefs())
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
			if th := ai.CleanUTF8(thinking.String()); th != "" {
				msg.Content = append(msg.Content, ai.ThinkingBlock{Thinking: th})
			}
			thinkOpen = false
		}
		if textOpen {
			if tx := ai.CleanUTF8(text.String()); tx != "" {
				msg.Content = append(msg.Content, ai.TextBlock{Text: tx})
			}
			textOpen = false
		}
	}
	// ttsrAbort tears the stream down and reports the interrupt upward
	// (oneTurnWithRecovery owns the retry-with-injection path).
	// lastDigest remembers the tool-call digest that most recently fed the
	// engine, so the interrupt notice can name the file a rule fired on.
	var lastDigest string
	ttsrAbort := func(m *TTSRMatch) (*ai.Message, error) {
		closeBlock() // flush streamed text/thinking into msg.Content
		cancel()
		ai.Drain(ch) // let the provider goroutine exit
		ti := &ttsrInterrupt{match: m, path: ttsrRulePath(m.Kind, lastDigest)}
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
			if m := a.ttsrObserve(ctx, ttsrThinking, ev.Delta, ev.StreamIndex); m != nil {
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
			lastDigest = ev.PartialJSON
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
	a.requestMu.Lock()
	if a.requestStart.IsZero() {
		msg.TTFTMS = time.Since(started).Milliseconds()
	} else {
		msg.TTFTMS = time.Since(a.requestStart).Milliseconds()
	}
	a.requestMu.Unlock()
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
		out = append(out, ai.ToolDef{Name: d.Name, Description: d.Description, Parameters: guardToolParams(d.Name, d.Parameters)})
	}
	// Plan mode exposes its exit tool only while the sub-state is live —
	// normal-mode registries never contain propose.
	if a.PlanMode != nil && a.PlanMode.Active() && a.PlanMode.Propose() != nil {
		if d, ok := a.PlanMode.Propose().(interface {
			Name() string
			Description() string
			Parameters() json.RawMessage
		}); ok {
			out = append(out, ai.ToolDef{Name: d.Name(), Description: d.Description(), Parameters: guardToolParams(d.Name(), d.Parameters())})
		}
	}
	return out
}

// guardToolParams keeps one malformed tool schema from poisoning the whole
// request: a RawMessage with, say, a trailing comma makes json.Marshal fail
// at stream start, and every tool — and the run — dies with it. MCP and
// extension servers are outside our control, so the bad schema is swapped
// for an empty object schema (that one tool degrades, loudly named in the
// log) and the rest of the turn proceeds.
func guardToolParams(name string, raw json.RawMessage) json.RawMessage {
	if len(raw) > 0 && !json.Valid(raw) {
		logx.Errorf("tool %q has a malformed parameters schema; sent as {} — fix the tool/provider: %s", name, string(raw))
		return json.RawMessage(`{}`)
	}
	return raw
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
			out[i] = a.redactToolMessage(a.runOneTool(ctx, calls[i]))
		}(i)
	}
	wg.Wait()
	return out
}

// DefaultCancelGrace is how long a cancelled turn waits for a tool that is
// already in flight before abandoning it (#126).
const DefaultCancelGrace = 5 * time.Second

// toolOutcome carries one Execute result across the goroutine boundary,
// including a panic: nothing in xdev recovers tool panics today, so moving the
// call off the turn's goroutine must not quietly convert a crash into a
// swallowed error.
type toolOutcome struct {
	res      tool.Result
	err      error
	panicVal any
	stack    []byte
}

func (o toolOutcome) unwrap() (tool.Result, error) {
	if o.panicVal != nil {
		panic(fmt.Sprintf("tool panicked: %v\n%s", o.panicVal, o.stack))
	}
	return o.res, o.err
}

// executeTool runs one tool call, bounding how long a cancelled turn waits for
// it. The loop already refuses to *start* a tool once cancelled; that answers
// the question for every tool, including the ones with no entry check of their
// own (grep, glob) and the third-party ext_*/mcp_* tools whose code we cannot
// assume checks anything. It cannot un-start a call that was already running
// when the cancel landed, and waiting on it forever means a stopped turn is not
// stopped — the user pressed cancel and the harness is still blocked on
// someone else's loop. So: wait for the tool, and if cancellation arrives first,
// give it the grace period to notice, then stop waiting and say so. The
// abandoned call keeps running (Go cannot kill a goroutine); its result is
// dropped and any side effect it makes after this point is named as untracked
// rather than reported as cancelled-clean.
func (a *Agent) executeTool(ctx context.Context, t tool.Tool, args json.RawMessage) (tool.Result, error) {
	done := make(chan toolOutcome, 1) // buffered: an abandoned tool never parks on the send
	go func() {
		out := toolOutcome{}
		defer func() {
			if p := recover(); p != nil {
				out.panicVal, out.stack = p, debug.Stack()
			}
			done <- out
		}()
		out.res, out.err = t.Execute(ctx, args)
	}()
	select {
	case o := <-done:
		return o.unwrap()
	case <-ctx.Done():
	}
	grace := a.CancelGrace
	if grace <= 0 {
		grace = DefaultCancelGrace
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case o := <-done:
		return o.unwrap()
	case <-timer.C:
		// Signalled through the result, not the error: runOneTool's error
		// branch renders a generic "tool failed", which would bury the one
		// sentence the user needs. IsError keeps it honest to the model, and
		// skipping the ToolResult interceptors is deliberate — an abandoned
		// call has no result to hand third-party code (#115).
		return tool.Result{
			Text: fmt.Sprintf("tool %q was still running %s after cancellation and was abandoned: any side effect it makes from here is not reported by this turn",
				t.Name(), grace),
			IsError: true,
		}, nil
	}
}

func (a *Agent) runOneTool(ctx context.Context, call ai.ToolCallBlock) ai.Message {
	started := time.Now()
	a.Hooks.OnToolStart(call)
	// The plan-mode exit tool lives outside the registry (it is exposed
	// only while the sub-state is live), so route it before lookup.
	if a.PlanMode != nil && a.PlanMode.Active() && a.PlanMode.Propose() != nil && call.Name == ProposeToolName {
		res, rerr := a.PlanMode.Propose().Execute(ctx, call.Arguments)
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
	if a.Redactor != nil && len(args) > 0 {
		// The model sees placeholders; the tool must run on real values
		// (M13 #55).
		args = json.RawMessage(a.Redactor.Expand(string(args)))
	}
	// Plan mode (M11): mutating/unmodeled tools are denied with a pointer
	// to propose while the sub-state is active. Checked before approval —
	// a read-only run must never reach an approval prompt for a mutation.
	if denied, blocked := applyPlanMode(a.PlanMode, call); blocked {
		a.Hooks.OnToolEnd(call, denied, time.Since(started))
		return toolResultMsg(call, denied)
	}
	// bash.interceptor (M13 #56): a settings-declared external review of the
	// proposed command, run at the same seam as the pattern rules. It is
	// fail-closed (a broken, hung, or unreadable interceptor denies) and
	// never an approver: "allow" only means "no objection" — the policy
	// below still decides — and a rewrite is judged here like a freshly
	// proposed command, so a rewrite into a denied command is still denied.
	interceptReason := ""
	if call.Name == "bash" {
		verdict, rewritten, verr := a.Policy.ReviewBash(ctx, args)
		if verr != nil {
			res := tool.Result{Text: "tool call denied: " + verr.Error(), IsError: true}
			a.Hooks.OnToolEnd(call, res, time.Since(started))
			return toolResultMsg(call, res)
		}
		if verdict.Action == tool.ActionDeny {
			res := tool.Result{Text: "tool call denied: " + verdict.Reason, IsError: true}
			a.Hooks.OnToolEnd(call, res, time.Since(started))
			return toolResultMsg(call, res)
		}
		if verdict.Action == tool.ActionPrompt {
			interceptReason = verdict.Reason
		}
		if len(rewritten) > 0 {
			args = rewritten
			call.Arguments = rewritten
		}
	}
	// Approval policy: deny/prompt are resolved before anything runs.
	dec, decErr := a.Policy.Decide(call.Name, args)
	if decErr != nil {
		dec = tool.Decision{Action: tool.ActionAllow}
	}
	if interceptReason != "" && dec.Action == tool.ActionAllow {
		// The interceptor asked for a human verdict and cannot grant one
		// itself: this prompt runs even under yolo, and an unattended run
		// refuses (no approver) rather than treating it as an allow.
		dec = tool.Decision{Action: tool.ActionPrompt, Reason: interceptReason}
	}
	if dec.Action != tool.ActionAllow {
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
		revised, berr := a.Intercept.ToolCall(ctx, call)
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
	// Cancellation is checked at the last possible instant — here, rather than
	// only at the top of the next loop iteration — because a turn can be
	// cancelled while the stream is still decoding, while a policy hook runs,
	// or while an approval prompt parks this goroutine at a.Approve above.
	// Anything earlier is race-losing by construction: the approval case in
	// particular resolves and then executes. Checking before the call means a
	// cancelled turn reports a cancelled result instead of writing files,
	// running commands, or otherwise mutating state.
	if cerr := ctx.Err(); cerr != nil {
		res := tool.Result{Text: fmt.Sprintf("tool %q canceled before execution: %v", call.Name, cerr), IsError: true}
		a.Hooks.OnToolEnd(call, res, time.Since(started))
		return toolResultMsg(call, res)
	}
	res, err := a.executeTool(ctx, t, args)
	dur := time.Since(started)
	if err != nil {
		res = tool.Result{Text: fmt.Sprintf("tool %q failed: %v", call.Name, err), IsError: true}
	}
	if a.Intercept != nil {
		// Extensions may rewrite the text an already-run tool produced.
		enc, _ := json.Marshal(map[string]any{"text": res.Text, "isError": res.IsError})
		if patched := a.Intercept.ToolResult(ctx, call, enc, res.IsError); len(patched) > 0 {
			var p struct {
				Text    string `json:"text"`
				IsError bool   `json:"isError"`
			}
			if json.Unmarshal(patched, &p) == nil && p.Text != "" {
				res.Text, res.IsError = p.Text, p.IsError
			}
		}
	}
	if note := a.rulebookNote(call, res); note != "" {
		// A glob-scoped rule (globs: *.go) is guidance tied to the file the
		// model just changed: it rides the tool result, where the model will
		// act on it, rather than sitting unused in the prompt index.
		res.Text = strings.TrimRight(res.Text, "\n") + "\n\n" + note
	}
	if rem := a.ttsrReminder(call.StreamIndex); rem != "" {
		// Non-interrupting tool match (M11 #35): the rule may not cut the
		// call short, so its notice rides along in the tool result — LEADING
		// it, like omp, because the reminder is the thing the model must not
		// miss in a long output (T3 #40 had it appended and buried).
		res.Text = rem + "\n\n" + strings.TrimLeft(res.Text, "\n")
	}
	if a.Offload != nil && !res.IsError && len(res.Text) > OffloadThresholdBytes {
		if stub, ok, oerr := a.Offload.Offload(call.Name, call.ID, res.Text); oerr != nil {
			// A failed spill must be a named error, never a silent
			// marker claiming bytes that were not written (#115).
			logx.Errorf("artifact offload: %v", oerr)
		} else if ok {
			res.Text = stub
		}
	}
	a.Hooks.OnToolEnd(call, res, dur)
	return toolResultMsg(call, res)
}

// toolResultMsg builds the toolResult message for one finished call. Every
// result the model is shown passes through here, so this is where a silent tool
// gets text a provider accepts: an empty toolResult serialized to an
// openai-responses function_call_output carrying no `output` field at all, and
// the upstream answered the whole turn with HTTP 400 [invalid_request_error]
// "`input[185]` missing required field `output`" — a bad request the retry
// ladder does not retry, so the run ended (ai.Message.EnsureToolOutput).
func toolResultMsg(call ai.ToolCallBlock, res tool.Result) ai.Message {
	return ai.Message{
		Role:       ai.RoleToolResult,
		Content:    []ai.Block{ai.TextBlock{Text: res.Text}},
		ToolCallID: call.ID,
		ToolName:   call.Name,
		IsError:    res.IsError,
		Details:    res.Details,
	}.EnsureToolOutput()
}

// Redactor hides configured secrets in provider-visible text and restores
// placeholders in inbound tool arguments (M13 #55). nil = off.
type Redactor interface {
	Apply(string) string
	Expand(string) string
}

// redactMessages copies history with every text block redacted. The
// caller's slice is never mutated: the session store keeps raw values, so
// only the provider request carries placeholders.
func redactMessages(msgs []ai.Message, r Redactor) []ai.Message {
	out := make([]ai.Message, len(msgs))
	for i, m := range msgs {
		out[i] = m
		if len(m.Content) == 0 {
			continue
		}
		blocks := make([]ai.Block, len(m.Content))
		copy(blocks, m.Content)
		for j, b := range blocks {
			if tb, ok := b.(ai.TextBlock); ok {
				tb.Text = r.Apply(tb.Text)
				blocks[j] = tb
			}
		}
		out[i].Content = blocks
	}
	return out
}

// EmitSessionShutdown publishes session_shutdown (omp's end-of-session
// event). Idempotent-safe: a nil bus is a no-op, and a run that never opened
// a bus simply emits nothing.
func (a *Agent) EmitSessionShutdown() {
	a.emitIntercept(context.Background(), "session_shutdown", map[string]any{"model": a.Model})
}

// emitIntercept publishes one lifecycle event on the interceptor bus
// (nil-safe): the loop and the modes share this so an event cannot be emitted
// from one place and forgotten in the others.
func (a *Agent) emitIntercept(ctx context.Context, event string, payload any) {
	if a == nil || a.Intercept == nil {
		return
	}
	a.Intercept.Emit(ctx, event, payload)
}

// rulebookNote renders the path-scoped rulebook guidance for one completed
// tool call: only edit/write carry a target path, and a failing call has no
// path worth advising on.
func (a *Agent) rulebookNote(call ai.ToolCallBlock, res tool.Result) string {
	if a.Rulebook == nil || res.IsError {
		return ""
	}
	switch call.Name {
	case "edit", "write":
	default:
		return ""
	}
	var args struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(call.Arguments, &args) != nil || strings.TrimSpace(args.Path) == "" {
		return ""
	}
	return strings.TrimSpace(a.Rulebook(args.Path))
}

// redactToolMessage hides secrets in a tool result before it enters the
// provider request (M13 #55); nil Redactor is the identity.
func (a *Agent) redactToolMessage(m ai.Message) ai.Message {
	if a.Redactor == nil || m.Role != ai.RoleToolResult || len(m.Content) == 0 {
		return m
	}
	for i, b := range m.Content {
		if tb, ok := b.(ai.TextBlock); ok {
			tb.Text = a.Redactor.Apply(tb.Text)
			m.Content[i] = tb
		}
	}
	return m
}
