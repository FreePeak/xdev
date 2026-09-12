package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/session"
)

// Compaction ladder members (M5 #24). The ladder walks the product members of
// compaction.methodOrder in order and the first one that yields a retained
// context wins. `handoff` is the provider summarize xdev shipped before the
// ladder; `snapcompact`, `shake` and `soft` are deterministic (no model call,
// so they also work offline); `remote` is a documented no-op — nothing wired
// here speaks provider-native streaming compaction, so it says so and falls
// through instead of dressing a local summary up as a remote one.
const (
	methodRemote      = "remote"
	methodSnapcompact = "snapcompact"
	// MethodHandoff is the ladder member name the provider summarize (and the
	// M5 #23 handoff-document variant) records on its entry, so a caller that
	// builds the entry itself can tag it without a magic string.
	MethodHandoff = "handoff"
	methodShake   = "shake"
	methodSoft    = "soft"
)

// compactTriggers are the methodOrder members that decide WHEN to compact
// rather than producing the retained context; the ladder skips them.
var compactTriggers = []string{methodThreshold, "overflow", "promotion"}

// errMethodUnavailable marks a member that cannot run in this process (remote
// without provider support): the ladder falls through silently — the member
// logged the reason itself — instead of reporting a failure.
var errMethodUnavailable = errors.New("compaction: method unavailable")

// compactMethodFunc builds the entry that retains the discarded span. It must
// not touch the store: the ladder persists the winner, so the anchor and the
// recorded Method come from one place.
type compactMethodFunc func(a *Agent, ctx context.Context, span *compactionSpan) (*session.CompactionEntry, error)

// compactMethodOverrides extends or replaces a ladder member without editing
// the ladder (the M5 #23 handoff-document variant registers here). Overrides
// win over the built-ins.
var compactMethodOverrides = map[string]compactMethodFunc{}

// compactMethods are the built-in members.
var compactMethods = map[string]compactMethodFunc{
	MethodHandoff:     methodHandoffRun,
	methodRemote:      methodRemoteRun,
	methodSnapcompact: methodSnapcompactRun,
	methodShake:       methodShakeRun,
	methodSoft:        methodSoftRun,
}

// RegisterCompactionMethod installs fn as the ladder member name (one of
// config.CompactionMethodNames), letting a companion file join the ladder
// without editing it — the M5 #23 handoff-document variant is the intended
// user. fn sees the discarded span as messages and returns the message that
// replaces it; the ladder still owns the anchor, the token count and the
// recorded method, so a member cannot half-persist itself. The registry is
// read on every compaction, so registering may happen at any point before the
// boundary; an unknown name or a nil func is refused loudly rather than
// failing silently at compaction time.
func RegisterCompactionMethod(name string, fn func(ctx context.Context, a *Agent, span []ai.Message) (ai.Message, error)) {
	n := strings.ToLower(strings.TrimSpace(name))
	if fn == nil || !slices.Contains(config.CompactionMethodNames, n) {
		logx.Errorf("compaction: refusing to register unknown method %q", name)
		return
	}
	compactMethodOverrides[n] = func(a *Agent, ctx context.Context, span *compactionSpan) (*session.CompactionEntry, error) {
		summary, err := fn(ctx, a, span.msgs[:span.cut])
		if err != nil {
			return nil, err
		}
		return compactEntry(span, summary), nil
	}
}

// lookupCompactMethod returns the member's implementation, or nil for a
// trigger (threshold/overflow/promotion) or an unknown name.
func lookupCompactMethod(name string) compactMethodFunc {
	if fn, ok := compactMethodOverrides[name]; ok {
		return fn
	}
	return compactMethods[name]
}

// products is compaction.methodOrder restricted to the members that produce a
// retained context, in order. An order with no product — the shipped default
// `threshold,overflow,promotion`, which only names triggers — means the
// builtin handoff summarize: exactly what xdev did before the ladder existed.
func (c CompactionConfig) products() []string {
	var out []string
	for _, m := range c.methods() {
		if slices.Contains(compactTriggers, m) {
			continue
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return []string{MethodHandoff}
	}
	return out
}

// firstProductIsHandoff reports whether the provider summarize is the member
// the ladder would pick — the only member whose round trip is worth
// backgrounding (see the async trigger in compact_async.go).
func (a *Agent) firstProductIsHandoff() bool {
	p := a.Compaction.products()
	return len(p) > 0 && p[0] == MethodHandoff
}

// runCompactLadder tries the configured products in order and returns the
// entry the first successful member produced (its Method names it). A failing
// member is logged and the ladder continues down the list; only when every
// member failed does the caller see an error — compaction then degrades to the
// pre-compaction behavior instead of persisting a half-result.
func (a *Agent) runCompactLadder(ctx context.Context, span *compactionSpan) (*session.CompactionEntry, error) {
	var lastErr error
	for _, name := range a.Compaction.products() {
		fn := lookupCompactMethod(name)
		if fn == nil {
			lastErr = fmt.Errorf("compaction: %s: unknown method", name)
			logx.Errorf("%v — falling through", lastErr)
			continue
		}
		entry, err := fn(a, ctx, span)
		if err == nil {
			entry.Method = name
			return entry, nil
		}
		if errors.Is(err, errMethodUnavailable) {
			continue
		}
		lastErr = fmt.Errorf("compaction: %s: %w", name, err)
		logx.Errorf("%v — falling through", lastErr)
	}
	if lastErr == nil {
		return nil, errors.New("compaction: no method ran")
	}
	return nil, fmt.Errorf("compaction: every method failed: %w", lastErr)
}

// hasExplicitProduct reports whether compaction.methodOrder names a member
// that produces a retained context, as opposed to naming triggers only.
// Naming one is a request to use it at a boundary, so it also arms the token
// trigger (see compactionDue) — without that, an omp-style order such as
// `remote,snapcompact,handoff,shake,soft` would never compact on tokens.
func (a *Agent) hasExplicitProduct() bool {
	for _, m := range a.Compaction.methods() {
		if !slices.Contains(compactTriggers, m) {
			return true
		}
	}
	return false
}

// methodHandoffRun is the provider summarize: one streaming call compresses
// the discarded span into the retained context.
func methodHandoffRun(a *Agent, ctx context.Context, span *compactionSpan) (*session.CompactionEntry, error) {
	summary, err := a.summarize(ctx, span.msgs[:span.cut])
	if err != nil {
		return nil, err
	}
	return compactEntry(span, textSummary(summary)), nil
}

// methodRemoteRun is provider-native streaming compaction (omp's remote v2):
// the provider, not xdev, produces the retained context. No transport wired
// here speaks it, so the member is an honest no-op that falls through — it
// never fabricates a "remote" summary out of a local call.
func methodRemoteRun(a *Agent, _ context.Context, _ *compactionSpan) (*session.CompactionEntry, error) {
	who := "the active provider"
	if a.Provider != nil {
		who = a.Provider.Name()
	}
	logx.Infof("compaction: remote (provider-native streaming) not supported by %s — falling through", who)
	return nil, errMethodUnavailable
}

// methodShakeRun mechanically elides the discarded span: tool-call argument
// bodies and thinking are dropped, results are cut to a head, and runs of
// repeated boilerplate collapse. The skeleton — who spoke, which tool ran —
// survives without a model call.
func methodShakeRun(_ *Agent, _ context.Context, span *compactionSpan) (*session.CompactionEntry, error) {
	text := elideShake(span.msgs[:span.cut])
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("shake: span rendered empty")
	}
	return compactEntry(span, textSummary(text)), nil
}

// methodSoftRun prunes the discarded span structurally — superseded reads and
// results that carry nothing — and retains what is left as text.
func methodSoftRun(_ *Agent, _ context.Context, span *compactionSpan) (*session.CompactionEntry, error) {
	kept, dropped := pruneUseless(span.msgs[:span.cut])
	var b strings.Builder
	fmt.Fprintf(&b, "soft prune: %d of %d dropped messages carried nothing (superseded reads, empty results), %d kept.\n\n",
		dropped, span.cut, len(kept))
	b.WriteString(renderTranscript(kept, elideResultChars, elideTotalChars))
	if strings.TrimSpace(b.String()) == "" {
		return nil, errors.New("soft: span rendered empty")
	}
	return compactEntry(span, textSummary(b.String())), nil
}

// textSummary is the assistant message a text-retaining member persists.
func textSummary(text string) ai.Message {
	return ai.Message{
		Role:       ai.RoleAssistant,
		Content:    []ai.Block{ai.TextBlock{Text: text}},
		StopReason: ai.StopReasonStop,
	}
}

// compactEntry anchors a retained-context message at the span's cut: the
// summary replaces everything before the anchor, the kept tail follows it.
func compactEntry(span *compactionSpan, summary ai.Message) *session.CompactionEntry {
	anchor := span.anchor()
	return &session.CompactionEntry{
		Summary:          summary,
		FirstKeptEntryID: &anchor,
		TokensBefore:     span.tokens,
	}
}
