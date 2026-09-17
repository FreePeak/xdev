package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/memlimit"
	"github.com/FreePeak/xdev/internal/session"
)

const (
	// DefaultReserveTokens is the context head-room a compaction aims to
	// free up to; the effective reserve is never below 15% of the window.
	DefaultReserveTokens = 16384
	// DefaultKeepRecentTokens is the recent tail a compaction preserves.
	DefaultKeepRecentTokens = 20000
	// MaxSummaryTokens caps the summarizer output (omp MAX_SUMMARY_TOKENS).
	MaxSummaryTokens = 16384
	// charsPerToken is the byte→token estimation ratio (boring, no
	// tokenizer dependency; ponytail: coarse estimate is fine for a
	// threshold trigger — provider-reported usage overrides when present).
	charsPerToken = 4
)

// Compaction strategies (compaction.methodOrder, M5 #24). `threshold` is the
// only trigger that acts on the token budget; `overflow` and `promotion` are
// reactive (they fire when a request actually overflows the window); the idle
// and async triggers live in compactionDue / compact_async.go. The ladder
// members that produce the retained context — remote, snapcompact, handoff,
// shake, soft — live in compact_ladder.go, and the accepted vocabulary has one
// spelling: config.CompactionMethodNames.
const methodThreshold = "threshold"

// compactionMethods is the shipped default order, whose members are all
// triggers: the product is then the builtin handoff summarize (see
// CompactionConfig.products), so an unset methodOrder behaves exactly as it
// did before the ladder tails existed.
var compactionMethods = strings.Split(config.DefaultCompactionMethodOrder, ",")

// memPressure samples live heap pressure (0..1 of the process memory
// limit). A package var so tests pin the trigger instead of allocating
// toward the real limit.
var memPressure = memlimit.Pressure

// compactionNow is the clock the idle trigger measures with; a package var so
// tests advance time instead of sleeping.
var compactionNow = time.Now

// ParseMethodOrder parses the compaction.methodOrder setting into a
// validated priority list. Unknown names are dropped with a warning (a
// typo must not silently disable compaction) and duplicates collapse to
// their first position; an empty or all-invalid value falls back to the
// shipped default order. The vocabulary is config.CompactionMethodNames —
// the triggers plus the M5 #24 ladder members.
func ParseMethodOrder(raw string) []string {
	out := make([]string, 0, len(compactionMethods))
	for _, part := range strings.Split(raw, ",") {
		m := strings.ToLower(strings.TrimSpace(part))
		if m == "" || slices.Contains(out, m) {
			continue
		}
		if !slices.Contains(config.CompactionMethodNames, m) {
			logx.Errorf("compaction: methodOrder: unknown method %q dropped (want %s)", m, strings.Join(config.CompactionMethodNames, ","))
			continue
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return slices.Clone(compactionMethods)
	}
	return out
}

// CompactionConfig tunes context maintenance. ContextWindow 0 disables it.
type CompactionConfig struct {
	ContextWindow    int
	ReserveTokens    int64 // 0 → DefaultReserveTokens
	KeepRecentTokens int64 // 0 → DefaultKeepRecentTokens
	// Methods is the compaction.methodOrder priority list (see
	// ParseMethodOrder); nil/empty → the shipped default order.
	Methods []string
	// IdleAfter compacts a session that sat idle between two step
	// boundaries for at least this long (M5 #24 idle trigger); 0 disables
	// it. The trigger is independent of the token threshold, like memory
	// pressure.
	IdleAfter time.Duration
	// Async summarizes in the background and applies the result at the
	// next boundary instead of blocking the turn (M5 #24); only the
	// provider summarize (handoff) has a round-trip worth backgrounding,
	// so the deterministic methods stay synchronous.
	Async bool
}

func (c CompactionConfig) reserve() int64 {
	r := c.ReserveTokens
	if r <= 0 {
		r = DefaultReserveTokens
	}
	// Effective reserve ≥ 15% of the window (omp resolveBudgetReserveTokens).
	if w := int64(c.ContextWindow); w > 0 && r < w*15/100 {
		r = w * 15 / 100
	}
	return r
}

func (c CompactionConfig) threshold() int64 {
	if c.ContextWindow <= 0 {
		return 0
	}
	return int64(c.ContextWindow) - c.reserve()
}

func (c CompactionConfig) keepRecent() int64 {
	if c.KeepRecentTokens <= 0 {
		return DefaultKeepRecentTokens
	}
	return c.KeepRecentTokens
}

// methods is the effective strategy priority order for this config.
func (c CompactionConfig) methods() []string {
	if len(c.Methods) == 0 {
		return compactionMethods
	}
	return c.Methods
}

// estimateTokens approximates the model-visible size of messages in tokens
// (chars/4 over all block text, plus per-message overhead).
func estimateTokens(msgs []ai.Message) int64 {
	var chars int64
	for i := range msgs {
		chars += int64(len(msgs[i].Text())) + 8
		for _, b := range msgs[i].Content {
			if tb, ok := b.(ai.ThinkingBlock); ok {
				chars += int64(len(tb.Thinking))
			}
		}
	}
	return chars / charsPerToken
}

// ContextTokens is contextTokens for callers outside the agent loop: the
// TUI measures a replayed transcript (resume, switch, branch) with it, so the
// HUD's context segment and the compaction trigger can never disagree.
func ContextTokens(msgs []ai.Message) int64 { return contextTokens(msgs) }

// contextTokens is the honest trigger input: provider-reported usage when
// the last assistant message carries it (it counts the full request),
// else the estimate floor (omp compactionContextTokens).
func contextTokens(msgs []ai.Message) int64 {
	for i := len(msgs) - 1; i >= 0; i-- {
		if u := msgs[i].Usage; u != nil && u.TotalTokens > 0 {
			if est := estimateTokens(msgs); est > u.TotalTokens {
				return est
			}
			return u.TotalTokens
		}
	}
	return estimateTokens(msgs)
}

const compactionPrompt = `Summarize the conversation so far for seamless continuation.
Preserve concretely: the user's goals and constraints, decisions made and why,
file paths touched and their current state, commands run and their outcomes,
errors hit and fixes applied, and the exact next steps in flight.
Be dense and factual — the summary REPLACES this history in the live context.`

// findCutPoint returns the history index where pre-compaction history ends:
// walking backwards, accumulate estimated tokens until the kept tail reaches
// keepRecent; the cut lands on a user or assistant message boundary, never
// between a tool call and its result, and never before index 1 (something
// must be summarized). Returns -1 when nothing can be dropped.
func findCutPoint(msgs []ai.Message, keepRecent int64) int {
	acc := int64(0)
	cut := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		acc += estimateTokens(msgs[i : i+1])
		if acc >= keepRecent {
			cut = i
			break
		}
	}
	if cut < 1 {
		return -1
	}
	// Never split a tool turn: advance past toolResults (and any dangling
	// assistant toolCalls at the boundary) so the kept window starts clean.
	for cut < len(msgs) && msgs[cut].Role == ai.RoleToolResult {
		cut++
	}
	if cut >= len(msgs) {
		return -1
	}
	return cut
}

// compactionSpan prepares one boundary compaction: the context rebuilt from
// the store, the message index where the dropped prefix ends, the tokens
// that prefix currently costs, and the entry id the compaction anchors on.
// The store is the mirror of record — it holds every message produced so far
// (hooks persist on message_end / tool result) — so the cut resolves against
// store entry IDs via one BuildContext walk. The ladder and the async job
// share this snapshot.
type compactionSpan struct {
	msgs     []ai.Message
	entryIDs []string
	cut      int
	tokens   int64
}

func (a *Agent) compactionSpan() (*compactionSpan, error) {
	if a.Store == nil {
		return nil, fmt.Errorf("compaction: no session store")
	}
	res, err := session.BuildContext(a.Store.Entries(), a.Store.LeafID(), session.SystemPrompt{})
	if err != nil {
		return nil, fmt.Errorf("compaction: build context: %w", err)
	}
	if len(res.Messages) < 2 {
		return nil, fmt.Errorf("compaction: nothing to summarize")
	}
	cut := findCutPoint(res.Messages, a.Compaction.keepRecent())
	if cut < 1 {
		return nil, fmt.Errorf("compaction: no droppable prefix")
	}
	if cut >= len(res.EntryIDs) || res.EntryIDs[cut] == "" {
		return nil, fmt.Errorf("compaction: cut %d has no anchor entry", cut)
	}
	return &compactionSpan{
		msgs:     res.Messages,
		entryIDs: res.EntryIDs,
		cut:      cut,
		tokens:   contextTokens(res.Messages),
	}, nil
}

// anchor is the entry the compaction keeps: everything before it is dropped.
func (s *compactionSpan) anchor() string { return s.entryIDs[s.cut] }

// compact runs the method ladder over the current history span, persists the
// retained-context entry the winning method produced, and notifies the hook
// bus. The entry records which member ran (M5 #24).
func (a *Agent) compact(ctx context.Context) error {
	span, err := a.compactionSpan()
	if err != nil {
		return err
	}
	entry, err := a.runCompactLadder(ctx, span)
	if err != nil {
		return err
	}
	return a.persistCompaction(entry)
}

// persistCompaction appends a ladder result to the session store and tells the
// hook bus how big the context was before it (the entry carries that number).
func (a *Agent) persistCompaction(entry *session.CompactionEntry) error {
	if err := a.Store.Append(entry); err != nil {
		return fmt.Errorf("compaction: persist: %w", err)
	}
	if a.Hooks != nil {
		a.Hooks.OnCompaction(entry.TokensBefore)
	}
	return nil
}

// summarize renders msgs as a transcript and compresses it through one
// provider call (no tools; hard MaxTokens clamp). It is the in-turn
// spelling of summarizeWith; the async trigger keeps its own snapshot.
func (a *Agent) summarize(ctx context.Context, msgs []ai.Message) (string, error) {
	extra := ""
	if a.MemoryContext != nil {
		extra = strings.TrimSpace(a.MemoryContext())
	}
	return summarizeWith(ctx, a.Provider, a.Model, msgs, extra)
}

// summarizeWith is summarize against a provider/model snapshot: the async
// job runs on its own goroutine, and a failover can move a.Provider or
// a.Model while that call is in flight, so the background job must carry
// copies rather than read the live fields.
func summarizeWith(ctx context.Context, provider ai.Provider, model string, msgs []ai.Message, memoryContext string) (string, error) {
	var b strings.Builder
	if memoryContext != "" {
		// #86: a compaction summary that forgets the recalled memories loses
		// them for the rest of the session. The backend's context rides
		// ahead of the transcript so the summarizer keeps what it must.
		b.WriteString("Recalled memories to preserve:\n\n" + memoryContext + "\n\n")
	}
	b.WriteString("Conversation transcript:\n\n")
	for i := range msgs {
		role := string(msgs[i].Role)
		txt := msgs[i].Text()
		if txt == "" {
			txt = "(tool call block)" // ponytail: tool args omitted from the summary input; upgrade path: render arguments
		}
		if len(txt) > 8000 {
			txt = txt[:8000] + "…[truncated]"
		}
		fmt.Fprintf(&b, "%s: %s\n\n", role, txt)
	}

	req := ai.StreamRequest{
		System:    compactionPrompt,
		Messages:  []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: b.String()}}}},
		MaxTokens: MaxSummaryTokens,
		Model:     model,
	}
	ch, err := provider.Stream(ctx, req)
	if err != nil {
		return "", err
	}
	var text strings.Builder
	for ev := range ch {
		switch ev.Type {
		case ai.EventTextDelta:
			text.WriteString(ev.Delta)
		case ai.EventDone:
			if ev.Message != nil && ev.Message.Text() != "" {
				return strings.TrimSpace(ev.Message.Text()), nil
			}
			return strings.TrimSpace(text.String()), nil
		case ai.EventError:
			return "", ev.Err
		}
	}
	return "", fmt.Errorf("compaction: stream ended without done")
}

// maybeCompact applies the compaction ladder at a step boundary. Triggers
// (M5 #24): the token threshold, a live heap near the process memory limit
// (PRD §3.7: degrade into "compact now", never an OOM kill), and an idle
// session — the gap between the previous boundary and this one, which after
// a pause between runs is the pause itself. With compaction.async on and the
// ladder's first product being the provider summarize, a due boundary kicks
// that summarize off in the background instead of blocking; a later boundary
// applies it. Silent on failure (logged, never fatal): a failed compaction
// degrades to the pre-compaction behavior, and the overflow path re-tries it.
func (a *Agent) maybeCompact(ctx context.Context, history []ai.Message) []ai.Message {
	if a.Store == nil || a.Compaction.ContextWindow <= 0 {
		return history
	}
	// A background summarize that finished since the last boundary applies
	// here, whatever the trigger state; an applied result is this boundary's
	// whole job (M5 #24 async).
	if rebuilt, ok := a.applyAsyncCompaction(history); ok {
		return rebuilt
	}
	if !a.compactionDue(history) {
		return history
	}
	// Async hands the provider round-trip to a goroutine; the deterministic
	// members are already cheap, so they stay on this goroutine. Memory
	// pressure never defers: the OOM backstop must act now.
	if a.Compaction.Async && a.firstProductIsHandoff() && memPressure() < memlimit.HighPressure {
		if a.compactAsync != nil {
			// One job at a time: it lands at a later boundary, and blocking
			// here on a second summarize would defeat the whole trigger.
			return history
		}
		if a.kickAsyncCompaction(ctx) {
			return history
		}
	}
	if err := a.compact(ctx); err != nil {
		logx.Errorf("compaction: %v", err)
		return history
	}
	// Rebuild from the store: summary + kept tail + everything after.
	if res, err := session.BuildContext(a.Store.Entries(), a.Store.LeafID(), session.SystemPrompt{}); err == nil {
		return res.Messages
	}
	return history
}

// compactionDue reports whether this step boundary should compact.
func (a *Agent) compactionDue(history []ai.Message) bool {
	// Memory pressure overrides the token budget: the hard backstop must
	// fire even while the context still fits its window.
	if p := memPressure(); p >= memlimit.HighPressure {
		logx.Infof("compaction: memory pressure %.0f%% of the process limit — compacting", p*100)
		return true
	}
	if a.idleDue() {
		return true
	}
	// The token budget triggers when the order names the threshold member —
	// or any product member, because naming a method is a request to use it
	// at the boundary (the omp ladder, `remote,snapcompact,handoff,shake,
	// soft`, names no trigger at all). An order of triggers alone (the
	// shipped default names exactly those) keeps the historical rule:
	// whether a boundary compacts on the token budget is the threshold
	// member's business.
	if !slices.Contains(a.Compaction.methods(), methodThreshold) && !a.hasExplicitProduct() {
		return false
	}
	return contextTokens(history) > a.Compaction.threshold()
}

// idleDue reports — and closes — the idle trigger: the session sat between
// two step boundaries for at least compaction.idleAfter. The clock updates on
// every boundary, so only the gap between two runs can reach the threshold (a
// run's own boundaries follow each other in milliseconds); that gap IS the
// idle time the trigger is about. Like memory pressure it needs no threshold
// member in the method order.
func (a *Agent) idleDue() bool {
	now := compactionNow()
	last := a.compactIdle
	a.compactIdle = now
	if a.Compaction.IdleAfter <= 0 || last.IsZero() {
		return false
	}
	if now.Sub(last) < a.Compaction.IdleAfter {
		return false
	}
	logx.Infof("compaction: session idle %s (≥ %s) — compacting",
		now.Sub(last).Round(time.Second), a.Compaction.IdleAfter)
	return true
}
