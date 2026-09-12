package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/memlimit"
	"github.com/FreePeak/xdev/internal/session"
)

// Compaction defaults (omp engine constants, PRD M5).
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

// Compaction strategies (compaction.methodOrder). `threshold` is the only
// one that acts at a step boundary; `overflow` and `promotion` are reactive
// (they fire when a request actually overflows the window) and appear in the
// order for completeness — the ladder is one knob. The one spelling of the
// default order is config.DefaultCompactionMethodOrder; config cannot import
// agent, so the vocabulary is derived from it here.
const methodThreshold = "threshold"

var compactionMethods = strings.Split(config.DefaultCompactionMethodOrder, ",")

// memPressure samples live heap pressure (0..1 of the process memory
// limit). A package var so tests pin the trigger instead of allocating
// toward the real limit.
var memPressure = memlimit.Pressure

// ParseMethodOrder parses the compaction.methodOrder setting into a
// validated priority list. Unknown names are dropped with a warning (a
// typo must not silently disable compaction) and duplicates collapse to
// their first position; an empty or all-invalid value falls back to the
// shipped default order.
func ParseMethodOrder(raw string) []string {
	out := make([]string, 0, len(compactionMethods))
	for _, part := range strings.Split(raw, ",") {
		m := strings.ToLower(strings.TrimSpace(part))
		if m == "" || slices.Contains(out, m) {
			continue
		}
		if !slices.Contains(compactionMethods, m) {
			logx.Errorf("compaction: methodOrder: unknown method %q dropped (want %s)", m, strings.Join(compactionMethods, ","))
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

// compact summarizes history[:cut] via the provider, persists a compaction
// entry anchored at history[cut]'s entry, and returns the rebuilt context.
// The store is the mirror of record: it holds every message produced so far
// (hooks persist on message_end / tool result), so the cut resolves against
// store entry IDs via one BuildContext walk.
func (a *Agent) compact(ctx context.Context) error {
	if a.Store == nil {
		return fmt.Errorf("compaction: no session store")
	}
	res, err := session.BuildContext(a.Store.Entries(), a.Store.LeafID(), session.SystemPrompt{})
	if err != nil {
		return fmt.Errorf("compaction: build context: %w", err)
	}
	msgs := res.Messages
	if len(msgs) < 2 {
		return fmt.Errorf("compaction: nothing to summarize")
	}
	cut := findCutPoint(msgs, a.Compaction.keepRecent())
	if cut < 1 {
		return fmt.Errorf("compaction: no droppable prefix")
	}
	if cut >= len(res.EntryIDs) || res.EntryIDs[cut] == "" {
		return fmt.Errorf("compaction: cut %d has no anchor entry", cut)
	}

	summary, err := a.summarize(ctx, msgs[:cut])
	if err != nil {
		return fmt.Errorf("compaction: summarize: %w", err)
	}

	tokensBefore := contextTokens(msgs)
	entry := &session.CompactionEntry{
		Summary: ai.Message{
			Role:       ai.RoleAssistant,
			Content:    []ai.Block{ai.TextBlock{Text: summary}},
			StopReason: ai.StopReasonStop,
		},
		FirstKeptEntryID: &res.EntryIDs[cut],
		TokensBefore:     tokensBefore,
	}
	if err := a.Store.Append(entry); err != nil {
		return fmt.Errorf("compaction: persist: %w", err)
	}
	a.Hooks.OnCompaction(tokensBefore)
	return nil
}

// summarize renders msgs[:cut] as a transcript and compresses it through
// one provider call (no tools; hard MaxTokens clamp).
func (a *Agent) summarize(ctx context.Context, msgs []ai.Message) (string, error) {
	var b strings.Builder
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
		Model:     a.Model,
	}
	ch, err := a.Provider.Stream(ctx, req)
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

// maybeCompact applies the compaction ladder at a step boundary: the
// methodOrder setting decides whether the token threshold check runs at
// all, and a live heap near the process memory limit forces compaction
// regardless of tokens (PRD §3.7: degrade into "compact now", never an OOM
// kill). It is silent on failure (logged, never fatal): a failed
// compaction degrades to the pre-compaction behavior, and the overflow
// path re-tries it.
func (a *Agent) maybeCompact(ctx context.Context, history []ai.Message) []ai.Message {
	if a.Store == nil || a.Compaction.ContextWindow <= 0 {
		return history
	}
	if !a.compactionDue(history) {
		return history
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
	// Only `threshold` can act at a boundary; the reactive methods reached
	// here are no-ops by design (loop.go consults them on real overflow).
	if !slices.Contains(a.Compaction.methods(), methodThreshold) {
		return false
	}
	return contextTokens(history) > a.Compaction.threshold()
}
