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
	// free up to when nothing is configured (omp's 16384). The effective
	// reserve is max(15% of the window, this), so the shipped reserve
	// never reserves less than 15% of the window.
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
	// ThresholdTokens pins the compaction trigger to an absolute token
	// count (omp compaction.thresholdTokens); 0 = unset, the head-room
	// threshold decides.
	ThresholdTokens int64
	// ThresholdPercent pins the compaction trigger to a percentage of the
	// context window, 1..99 (omp compaction.thresholdPercent); 0 = unset.
	ThresholdPercent int
}

// reserve is the context head-room a compaction frees up to (omp rule 25).
//
// omp's formula, copied as it stands:
//
//	raw  = max(floor(window*0.15), reserveTokens ?? 16384)
//	min  = max(1, floor(window*0.15))
//	effective = min, when the reserve is UNSET and raw >= window-min,
//	            or when raw >= window
//	            raw otherwise
//
// The 15% floor is the rule: a compaction must leave the session room to
// breathe, and the shipped reserve (16384) is the floor of that rule, not a
// replacement for it. The two collapse cases are the window's own: a window
// so small that the shipped reserve would swallow it, and a reserve at or
// above the whole window, both fall back to the plain 15%.
func (c CompactionConfig) reserve() int64 {
	w := int64(c.ContextWindow)
	if w <= 0 {
		return DefaultReserveTokens
	}
	minReserve := max(int64(1), w*15/100)
	r := minReserve
	if c.ReserveTokens > 0 {
		r = max(r, c.ReserveTokens)
	} else {
		r = max(r, DefaultReserveTokens)
	}
	// An UNSET reserve that would leave less than its own 15% of the
	// window collapses to the 15%, and so does a reserve that eats the
	// whole window.
	if (c.ReserveTokens <= 0 && r >= w-minReserve) || r >= w {
		return minReserve
	}
	return r
}

// baseWindow is the window a percentage trigger is measured against. omp
// measures it against min(contextWindow, baseWindowTokens); baseWindowTokens
// is not wired yet, so the seam is this one line.
func (c CompactionConfig) baseWindow(w int64) int64 {
	return w
}

// threshold is the token count at which a compaction fires (omp rule 25):
// the used context must be STRICTLY above it.
//
// The precedence is omp's:
//
//	thresholdTokens  — an absolute pin; a pin at or above the window
//	                   falls back to the head-room threshold
//	thresholdPercent — a percentage of the window, clamped to 1..99
//	head-room        — the window minus the effective reserve
func (c CompactionConfig) threshold() int64 {
	w := int64(c.ContextWindow)
	if w <= 0 {
		return 0
	}
	if c.ThresholdTokens > 0 {
		if c.ThresholdTokens >= w {
			return c.headroom(w)
		}
		return min(w-1, max(int64(1), c.ThresholdTokens))
	}
	if c.ThresholdPercent > 0 {
		pct := min(99, max(1, c.ThresholdPercent))
		return c.baseWindow(w) * int64(pct) / 100
	}
	return c.headroom(w)
}

// headroom is the default threshold: the window minus the effective reserve,
// never below 0 and never above the window minus one token.
func (c CompactionConfig) headroom(w int64) int64 {
	return max(int64(0), min(w-1, w-c.reserve()))
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

// compactionPrompt is the system prompt of the history summarize call: prime's
// structured checkpoint template (pa-core/src/session_engine/compaction.rs:434)
// with xdev's existing prose rules folded in. The template is the point — a
// free-form summary drops the sections a continuation actually needs (what is
// done, what is blocked, the exact next move), and the model that reads it next
// re-litigates finished work.
//
// When a prior summary exists the summarizer is asked to UPDATE it rather than
// re-describe a window the live context no longer holds (compaction.rs:442):
// the previous summary rides the request as <previous-summary>.
const compactionPrompt = `Summarize the conversation so far for seamless continuation.

Use this EXACT format (omit sections that do not apply):

## Goal
- [what the user is trying to accomplish, one line per goal]

## Constraints & Preferences
- [constraints, preferences, requirements the user stated, or "(none)"]

## Progress
### Done
- [x] [completed tasks and changes]
### In Progress
- [ ] [current work]
### Blocked
- [blockers and why, or "(none)"]

## Key Decisions
- **[decision]**: [rationale]

## Next Steps
1. [ordered next actions]

## Critical Context
- [facts the next turn cannot continue without, or "(none)"]

Preserve exact file paths, function names, error messages, commands and their
outcomes. Keep each section to terse single-line bullets. Carry forward only
user requests that remain unanswered. Do not mention that a summary exists.`

// compactionTurnPrefixPrompt is the split-turn arm's system prompt: the span
// is the PREFIX of one turn whose suffix stays retained below the summary, so
// the summary exists to make that suffix legible — not to restate it
// (compaction.rs:440).
const compactionTurnPrefixPrompt = `This is the PREFIX of a turn that was too large to keep. The SUFFIX (recent work) is retained.

Summarize the prefix to provide context for the retained suffix:

## Original Request
[what the user asked for in this turn]

## Early Progress
- [key decisions and work done in the prefix]

## Context for Suffix
- [information needed to understand the retained recent work]

Be concise. Focus on what is needed to understand the kept suffix.`

// findCutPoint returns the history index where pre-compaction history ends:
// walking backwards, accumulate estimated tokens until the kept tail reaches
// keepRecent; the cut lands on a user or assistant message boundary, never
// between a tool call and its result, and never before index 1 (something
// must be summarized).
//
// The result also reports the SPLIT-TURN arm (prime's find_cut_point,
// pa-core/src/session_engine/compaction.rs:368-431): when the allowance lands
// inside a turn, the cut is still the first kept message, and turnStart names
// the user message that began the turn it sits in — so the caller can
// summarize that turn's prefix separately and keep the turn's own retained
// suffix out of the summary. Before this the cut walked FORWARD to the next
// user message, which moved the whole turn onto the discarded side: the
// summary then replaced a turn whose suffix was still live, and the model read
// the same turn twice.
func findCutPoint(msgs []ai.Message, keepRecent int64) cutPoint {
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
		return cutPoint{cut: -1}
	}
	// Never split a tool turn: advance past toolResults (and any dangling
	// assistant toolCalls at the boundary) so the kept window starts clean.
	for cut < len(msgs) && msgs[cut].Role == ai.RoleToolResult {
		cut++
	}
	if cut >= len(msgs) {
		return cutPoint{cut: -1}
	}
	// A cut on a user message sits on a turn boundary: there is no prefix to
	// split out. Otherwise the turn containing it started at an earlier user
	// message — or at the history start, which is not a separate arm.
	if msgs[cut].Role == ai.RoleUser {
		return cutPoint{cut: cut, turnStart: -1}
	}
	return cutPoint{cut: cut, turnStart: turnStart(msgs, cut)}
}

// turnStartOrBoundary is turnStart's answer when the caller already knows the
// cut sits on a turn boundary: a user message reports the boundary rather than
// the start of the turn containing it (which does not exist), and a
// tool-result cut reports the same.
func turnStartOrBoundary(msgs []ai.Message, i int) int {
	if i < 0 || i >= len(msgs) {
		return -1
	}
	if msgs[i].Role == ai.RoleUser {
		return -1
	}
	return turnStart(msgs, i)
}

// turnStart is the index of the user message that began the turn containing i,
// scanning backwards; -1 when i sits inside the first turn (its start is the
// history start, which needs no separate arm).
func turnStart(msgs []ai.Message, i int) int {
	for j := i - 1; j >= 0; j-- {
		if msgs[j].Role == ai.RoleUser {
			return j
		}
	}
	return -1
}

// cutPoint is one resolved compaction boundary.
type cutPoint struct {
	// cut is the first kept message index: everything before it is replaced by
	// the summary, everything from it is retained verbatim.
	cut int
	// turnStart is the start of the turn the cut lands inside (-1 when the cut
	// sits on a turn boundary): msgs[turnStart:cut] is that turn's prefix,
	// summarized apart from the older history.
	turnStart int
}

// ok reports whether the cut resolved to a droppable prefix.
func (c cutPoint) ok() bool { return c.cut >= 1 }

// split is whether this cut splits a turn: its prefix is summarized apart
// from the older history while its suffix is retained verbatim.
func (c cutPoint) split() bool { return c.turnStart >= 0 }

// historyEnd is the index the older (pre-turn) history ends at: the turn start
// for a split cut, the cut itself otherwise.
func (c cutPoint) historyEnd() int {
	if c.split() {
		return c.turnStart
	}
	return c.cut
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
	// cp is the resolved boundary: cut is the first kept message index and
	// turnStart (-1 when the cut sits on a turn boundary) names the start of
	// the turn the cut lands inside, whose prefix this span summarizes apart
	// from the older history (prime's split-turn arm).
	cp     cutPoint
	tokens int64
}

// cut is the first kept message index: everything before it is replaced by
// the summary.
func (s *compactionSpan) cut() int { return s.cp.cut }

// split is whether this span ends inside a turn, so its prefix is summarized
// with the turn-prefix prompt rather than the history prompt.
func (s *compactionSpan) split() bool { return s.cp.split() }

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
	cp := findCutPoint(res.Messages, a.compaction().keepRecent())
	if !cp.ok() {
		return nil, fmt.Errorf("compaction: no droppable prefix")
	}
	if cp.cut >= len(res.EntryIDs) || res.EntryIDs[cp.cut] == "" {
		return nil, fmt.Errorf("compaction: cut %d has no anchor entry", cp.cut)
	}
	return &compactionSpan{
		msgs:     res.Messages,
		entryIDs: res.EntryIDs,
		cp:       cp,
		tokens:   contextTokens(res.Messages),
	}, nil
}

// anchor is the entry the compaction keeps: everything before it is dropped.
func (s *compactionSpan) anchor() string { return s.entryIDs[s.cut()] }

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
	return a.persistCompaction(entry, span.msgs[span.cut():])
}

// persistCompaction appends a ladder result to the session store and tells the
// hook bus how big the context was before it (the entry carries that number).
//
// The record is filled by FillCompactionRecord — the ONE helper every
// compaction path uses — so the ladder, the handoff, the notes rollover and
// the CLI all say the same thing about themselves.
func (a *Agent) persistCompaction(entry *session.CompactionEntry, kept []ai.Message) error {
	FillCompactionRecord(entry, kept)
	if err := a.Store.Append(entry); err != nil {
		return fmt.Errorf("compaction: persist: %w", err)
	}
	a.notifyCompaction(entry)
	return nil
}

// FillCompactionRecord completes a compaction entry with the omp-parity
// record (omp rule 27): the token count of the context that SURVIVES the
// compaction, and a one-line rendering of the summary.
//
// The surviving size is measured with the estimator and not with the
// provider's reported usage: the usage that rides the kept messages describes
// the request BEFORE the compaction, so reporting it here would claim the
// compaction freed nothing. The estimate is the same unit the estimator floor
// uses everywhere else, and it is the size the NEXT request will carry.
//
// kept is the window the compaction kept; the surviving context is the
// summary plus that window. It is the ONE implementation every persist path
// uses — the ladder, the handoff, the notes rollover and the CLI — so all of
// them say the same thing about themselves.
//
// The entry's Method is left alone: each path names its own ladder member.
func FillCompactionRecord(entry *session.CompactionEntry, kept []ai.Message) {
	entry.ShortSummary = compactionShortSummary(entry.Summary)
	if entry.Summary.Text() == "" && len(kept) == 0 {
		// Nothing survived that can be measured: the record claims no
		// size and the wire omits the field. A compaction is never failed
		// over a missing measurement.
		entry.TokensAfter = 0
		return
	}
	post := make([]ai.Message, 0, len(kept)+1)
	post = append(post, entry.Summary)
	post = append(post, kept...)
	entry.TokensAfter = estimateTokens(post)
}

// compactionShortSummary renders a summary as ONE line: the text is
// flattened, trimmed, and cut at 120 characters on a rune boundary. A summary
// with no text yields "" — the field is optional on the wire, and an empty
// one says less than a line of noise.
func compactionShortSummary(m ai.Message) string {
	text := strings.Join(strings.Fields(m.Text()), " ")
	if text == "" {
		return ""
	}
	runes := []rune(text)
	if len(runes) > 120 {
		return string(runes[:119]) + "…"
	}
	return text
}

// notifyCompaction is the one emission point a compaction passes through, so
// the notice and the side call's tokens can never disagree about whether a
// compaction happened. A handoff is a compaction (it persists one) and goes
// through here rather than re-raising both events itself.
func (a *Agent) notifyCompaction(entry *session.CompactionEntry) {
	if a.Hooks == nil {
		return
	}
	a.Hooks.OnCompaction(entry.TokensBefore)
	// The side call's own usage: a summarize or a handoff document read the
	// discarded history and wrote the summary, and the provider billed both.
	// Before this the tokens went nowhere, so a session could compact over and
	// over and its bill still read $0.00. Nil for the deterministic members
	// (shake, soft, snapcompact) — they make no provider call.
	if u := entry.Summary.Usage; u != nil && CompactionUsageNotify(a.Hooks) != nil {
		CompactionUsageNotify(a.Hooks)(u)
	}
}

// previousSummary is the summary the newest compaction in this span's history
// left behind, trimmed of any file-list block a prior version appended. It is
// the anchor of the iterative-update arm (prime's prepare_compaction): the new
// summarize call is asked to UPDATE this text rather than re-describe a window
// the live context no longer holds. "" when the span carries no prior
// compaction — the first compaction of a session summarizes from scratch.
func (a *Agent) previousSummary(span *compactionSpan) string {
	entries := a.Store.Entries()
	// The newest compaction written BEFORE the span's anchor is the one the
	// new summarize updates; an entry sitting at or after the anchor belongs to
	// history the rebuild already dropped, so re-summarizing it would duplicate
	// what the retained summary already says.
	anchor := span.anchor()
	for i, e := range entries {
		if e.Envelope().ID != anchor {
			continue
		}
		for _, prev := range entries[:i] {
			if c, ok := prev.(*session.CompactionEntry); ok {
				return c.Summary.Text()
			}
		}
		return ""
	}
	return ""
}

// summarize renders msgs as a transcript and compresses it through one
// provider call (no tools; hard MaxTokens clamp). It is the in-turn
// spelling of summarizeWith; the async trigger keeps its own snapshot.
//
// The usage comes back because the call is real spend: it read a long
// prefix and wrote the summary, and the provider billed both. Dropping it
// here is what made a compacted session's totals read as if compaction
// were free.
func (a *Agent) summarize(ctx context.Context, msgs []ai.Message) (string, *ai.Usage, error) {
	return a.summarizeSpan(ctx, msgs, false, "")
}

// summarizeSpan is summarize over one span of the span resolution: splitTurn
// selects the turn-prefix prompt (the span is the prefix of a turn whose
// suffix is retained below the summary) and previousSummary, when the store
// already holds a summary, is fed back so the new one updates it instead of
// re-describing a window the live context no longer holds.
func (a *Agent) summarizeSpan(ctx context.Context, msgs []ai.Message, splitTurn bool, previousSummary string) (string, *ai.Usage, error) {
	extra := ""
	if a.MemoryContext != nil {
		extra = strings.TrimSpace(a.MemoryContext())
	}
	return summarizeWith(ctx, a.Provider, a.Model, msgs, extra, splitTurn, previousSummary)
}

// summarizeWith is summarize against a provider/model snapshot: the async
// job runs on its own goroutine, and a failover can move a.Provider or
// a.Model while that call is in flight, so the background job must carry
// copies rather than read the live fields. The usage is the summarize
// call's own, reported by the provider on its done event.
//
// The usage is returned rather than banked here because two callers want
// it differently: the ladder attaches it to the summary it persists, and
// the async job cannot touch live agent state from its goroutine. Both end
// up on the summary message, which is where a resume reads it back.
func summarizeWith(ctx context.Context, provider ai.Provider, model string, msgs []ai.Message, memoryContext string, splitTurn bool, previousSummary string) (string, *ai.Usage, error) {
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
	if previousSummary != "" {
		// The iterative-update arm (prime's prepare_compaction, compaction.rs:
		// 588-608): a second compaction merges the new span into the summary
		// that already governs this history, so the durable record keeps what
		// the first summarize paid for instead of re-describing a window the
		// live context no longer holds.
		b.WriteString("Update the existing summary in the <previous-summary> tags below with the conversation above. PRESERVE everything it already records, add what is new, and move finished work into the Done section.\n\n<previous-summary>\n" + previousSummary + "\n</previous-summary>\n\n")
	}
	system := compactionPrompt
	if splitTurn {
		system = compactionTurnPrefixPrompt
	}

	req := ai.StreamRequest{
		System:    system,
		Messages:  []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: b.String()}}}},
		MaxTokens: MaxSummaryTokens,
		Model:     model,
	}
	ch, err := provider.Stream(ctx, req)
	if err != nil {
		return "", nil, err
	}
	var text strings.Builder
	for ev := range ch {
		switch ev.Type {
		case ai.EventTextDelta:
			text.WriteString(ev.Delta)
		case ai.EventDone:
			// The message carries the provider's final shape and wins over
			// the accumulated deltas; a wire that ends without one falls back
			// to them. The usage rides out of the same event — that is the
			// summarize call's own bill.
			out := text.String()
			if ev.Message != nil && ev.Message.Text() != "" {
				out = ev.Message.Text()
			}
			return strings.TrimSpace(out), ev.Usage, nil
		case ai.EventError:
			return "", nil, ev.Err
		}
	}
	return "", nil, fmt.Errorf("compaction: stream ended without done")
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
//
// The background summarize is ARMED before the threshold is due — inside the
// lead window (compact_spec.go) — and its result is consumed at the boundary
// where the compaction becomes DUE, so that boundary costs no provider
// round-trip.
func (a *Agent) maybeCompact(ctx context.Context, history []ai.Message) []ai.Message {
	if a.Store == nil || a.compaction().ContextWindow <= 0 {
		return history
	}
	// A background summarize that finished since the last boundary waits
	// here: an ARM is applied at the boundary where the compaction is due,
	// a DEFERRAL at the next boundary (the trigger that handed it over may
	// be gone by then — the idle trigger is consumed by the attempt).
	a.harvestAsyncResult()
	// Pre-request tier (microcompact): clear stale tool results before paying
	// for a summary. Cheap, deterministic, no provider call.
	if cleared, changed := microcompactToolResults(history, defaultMicrocompactKeepRecent); changed {
		history = cleared
	}
	if !a.compactionDue(history) {
		// Not due by trigger — but a compaction that WAS due may have been
		// handed to the background, and it is still owed. The idle trigger
		// fires once and is gone; without this the deferred compaction
		// would never be applied and the provider call it spent would buy
		// nothing.
		if job := a.compactAsync; job != nil && job.owed && job.pending != nil {
			if rebuilt, ok := a.applyAsyncCompaction(history); ok {
				a.forgetAsyncCompaction()
				return rebuilt
			}
			// The result cannot be applied: the owed compaction still
			// happens, synchronously.
			a.forgetAsyncCompaction()
			return a.compactNow(ctx, history)
		}
		// Not due: this is where a speculative compaction is armed, so its
		// summary is ready when the threshold is crossed.
		a.maybeArm(ctx, history)
		return history
	}
	// Due. An armed result applies HERE: its summary was produced before the
	// threshold was crossed, so this boundary costs no provider round-trip.
	if job := a.compactAsync; job != nil && job.pending != nil {
		if rebuilt, ok := a.applyAsyncCompaction(history); ok {
			// The result is consumed: the next boundary may arm a fresh one.
			a.forgetAsyncCompaction()
			return rebuilt
		}
		// The arm is unusable: drop it and compact synchronously. A
		// discarded arm costs latency, never correctness.
		a.forgetAsyncCompaction()
		return a.compactNow(ctx, history)
	}
	// Async hands the provider round-trip to a goroutine; the deterministic
	// members are already cheap, so they stay on this goroutine. Memory
	// pressure never defers: the OOM backstop must act now.
	if a.compaction().Async && a.firstProductIsHandoff() && memPressure() < memlimit.HighPressure {
		if a.compactAsync != nil {
			// One job at a time: it lands at a later boundary, and blocking
			// here on a second summarize would defeat the whole trigger.
			// The compaction is DUE and this job will carry it: the job
			// owes the result, so it lands even if the trigger is gone.
			a.compactAsync.owed = true
			return history
		}
		if a.kickAsyncCompaction(ctx, true) {
			return history
		}
	}
	return a.compactNow(ctx, history)
}

// compactNow runs the compaction ladder on this goroutine and rebuilds the
// context from the store: summary + kept tail + everything after.
func (a *Agent) compactNow(ctx context.Context, history []ai.Message) []ai.Message {
	if err := a.compact(ctx); err != nil {
		logx.Errorf("compaction: %v", err)
		return history
	}
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
	if !a.tokenTriggerEnabled() {
		return false
	}
	// A zero (or negative) context window has no threshold to cross:
	// threshold() is 0 there, so "used > 0" would fire on any history at
	// all. omp never compacts without a window (its check requires a
	// positive threshold), so a session with no window never compacts on
	// the token budget. The memory-pressure and idle backstops above are
	// not the token budget and still fire.
	if a.compaction().ContextWindow <= 0 {
		return false
	}
	// A zero (or negative) context window has no threshold to cross:
	// threshold() is 0 there, so "used > 0" would fire on any history at
	// all. omp never compacts without a window (its check requires a
	// positive threshold), so a session with no window never compacts on
	// the token budget. The memory-pressure and idle backstops above are
	// not the token budget and still fire.
	if a.compaction().ContextWindow <= 0 {
		return false
	}
	return contextTokens(history) > a.compaction().threshold()
}

// tokenTriggerEnabled reports whether the token budget can trigger a
// compaction at all: the method order must name the threshold member or a
// member that produces a retained context.
//
// The ARM asks the same question (compact_spec.go's armEligible): a method
// order that disables the token trigger must not spend a provider call on a
// speculative summary either.
func (a *Agent) tokenTriggerEnabled() bool {
	return slices.Contains(a.compaction().methods(), methodThreshold) || a.hasExplicitProduct()
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
	if a.compaction().IdleAfter <= 0 || last.IsZero() {
		return false
	}
	if now.Sub(last) < a.compaction().IdleAfter {
		return false
	}
	logx.Infof("compaction: session idle %s (≥ %s) — compacting",
		now.Sub(last).Round(time.Second), a.compaction().IdleAfter)
	return true
}
