package agent

import (
	"context"
	"fmt"

	"github.com/FreePeak/xdev/internal/ai"
)

// Offline compaction for the `xdev compress` subcommand (issue #34): the same
// ladder members a live boundary runs, over a history read from a session file
// instead of a live Agent. PlanCompaction mirrors compactionSpan's cut and
// anchor rules, so the CLI can never compact a span a real boundary would
// refuse to touch (nothing before index 1, never splitting a tool turn).

// CompactPlan is one CLI compaction opportunity.
type CompactPlan struct {
	Messages     []ai.Message
	EntryIDs     []string
	Cut          int
	TokensBefore int64
}

// PlanCompaction builds the plan for msgs/entryIDs: the cut keeps the trailing
// keepRecent tokens (0 = DefaultKeepRecentTokens) and anchors on the entry at
// that index.
func PlanCompaction(msgs []ai.Message, entryIDs []string, keepRecent int64) (*CompactPlan, error) {
	if len(msgs) < 2 {
		return nil, fmt.Errorf("nothing to compact: the history holds %d messages", len(msgs))
	}
	if keepRecent <= 0 {
		keepRecent = DefaultKeepRecentTokens
	}
	cut := findCutPoint(msgs, keepRecent)
	if cut < 1 {
		return nil, fmt.Errorf("no droppable prefix: the whole history fits in the %d-token kept tail", keepRecent)
	}
	if cut >= len(entryIDs) || entryIDs[cut] == "" {
		return nil, fmt.Errorf("cut %d has no anchor entry", cut)
	}
	return &CompactPlan{
		Messages:     msgs,
		EntryIDs:     entryIDs,
		Cut:          cut,
		TokensBefore: contextTokens(msgs),
	}, nil
}

// Anchor is the entry the compaction keeps: everything before it is dropped.
func (p *CompactPlan) Anchor() string { return p.EntryIDs[p.Cut] }

// Dropped reports how many messages the retained summary replaces.
func (p *CompactPlan) Dropped() int { return p.Cut }

// TokensAfter estimates the context once summary replaces the dropped prefix.
func (p *CompactPlan) TokensAfter(summary ai.Message) int64 {
	kept := make([]ai.Message, 0, 1+len(p.Messages)-p.Cut)
	kept = append(kept, summary)
	kept = append(kept, p.Messages[p.Cut:]...)
	return estimateTokens(kept)
}

// deterministicMethods lists the ladder members that need no provider call —
// the ones an offline CLI compaction can always run.
func deterministicMethods() []string { return []string{methodShake, methodSoft, methodSnapcompact} }

// IsDeterministicMethod reports whether name is a no-model ladder member.
func IsDeterministicMethod(name string) bool {
	for _, m := range deterministicMethods() {
		if m == name {
			return true
		}
	}
	return false
}

// Compact runs one ladder member over the plan's dropped prefix and returns
// the message that retains it. Provider/model are consulted only by the
// handoff member (the provider summarize); the deterministic members ignore
// them, which is what makes an offline compaction possible at all.
func (p *CompactPlan) Compact(ctx context.Context, method string, provider ai.Provider, model string) (ai.Message, error) {
	span := &compactionSpan{msgs: p.Messages, entryIDs: p.EntryIDs, cut: p.Cut, tokens: p.TokensBefore}
	switch method {
	case methodShake, methodSoft, methodSnapcompact:
		fn := compactMethods[method]
		if fn == nil {
			return ai.Message{}, fmt.Errorf("compaction: %s: unknown method", method)
		}
		entry, err := fn(nil, ctx, span)
		if err != nil {
			return ai.Message{}, fmt.Errorf("compaction: %s: %w", method, err)
		}
		return entry.Summary, nil
	case MethodHandoff:
		if provider == nil {
			return ai.Message{}, fmt.Errorf("method handoff needs a reachable provider; use --method shake for an offline compaction")
		}
		text, err := summarizeWith(ctx, provider, model, span.msgs[:span.cut])
		if err != nil {
			return ai.Message{}, fmt.Errorf("compaction: handoff: %w", err)
		}
		return textSummary(text), nil
	case methodRemote:
		// Same honest verdict a live boundary reaches: nothing wired here
		// speaks provider-native streaming compaction, so the member is a
		// no-op rather than a locally faked "remote" summary.
		return ai.Message{}, fmt.Errorf("method remote: no wired transport speaks provider-native streaming compaction — use shake, soft, snapcompact or handoff")
	default:
		return ai.Message{}, fmt.Errorf("unknown method %q (want shake, soft, snapcompact or handoff)", method)
	}
}
