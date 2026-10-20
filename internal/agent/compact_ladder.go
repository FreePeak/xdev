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
// so they also work offline). `remote` is a member of omp's ladder that xdev
// cannot run — nothing wired here speaks provider-native streaming
// compaction — so the applicability check skips it instead of dressing a
// local summary up as a remote one.
const (
	methodRemote      = "remote"
	methodSnapcompact = "snapcompact"
	// MethodHandoff is the ladder member name the provider summarize (and the
	// M5 #23 handoff-document variant) records on its entry, so a caller that
	// builds the entry itself can tag it without a magic string.
	MethodHandoff      = "handoff"
	methodShake        = "shake"
	methodSoft         = "soft"
	methodMicrocompact = "microcompact"
)

// compactTriggers are the methodOrder members that decide WHEN to compact
// rather than producing the retained context; the ladder skips them.
var compactTriggers = []string{methodThreshold, "overflow", "promotion"}

// ompProductOrder is omp's compaction-method ladder in its default priority
// order (omp packages/coding-agent/src/session/compaction-methods.ts): remote,
// snapcompact, handoff, shake, soft. The first APPLICABLE member wins — see
// compactMethodApplicable.
var ompProductOrder = []string{methodRemote, methodSnapcompact, MethodHandoff, methodShake, methodSoft}

// compactMethodFunc builds the entry that retains the discarded span. It must
// not touch the store: the ladder persists the winner, so the anchor and the
// recorded Method come from one place.
type compactMethodFunc func(a *Agent, ctx context.Context, span *compactionSpan) (*session.CompactionEntry, error)

// compactMethodOverrides extends or replaces a ladder member without editing
// the ladder (the M5 #23 handoff-document variant registers here). Overrides
// win over the built-ins.
var compactMethodOverrides = map[string]compactMethodFunc{}

// compactMethods are the built-in members. `remote` is not among them: it is
// provider-native streaming compaction, and xdev wires no transport that
// speaks it — the ladder decides that up front (see
// compactMethodApplicable) instead of running a member that could only say
// no.
var compactMethods = map[string]compactMethodFunc{
	MethodHandoff:      methodHandoffRun,
	methodSnapcompact:  methodSnapcompactRun,
	methodShake:        methodShakeRun,
	methodSoft:         methodSoftRun,
	methodMicrocompact: methodMicrocompactRun,
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
		summary, err := fn(ctx, a, span.msgs[:span.cut()])
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

// RecordedMethod returns the name a compaction record carries for a ladder
// member. It follows omp's record contract (omp
// packages/coding-agent/src/session/compaction-methods.ts): remote, handoff
// and soft name themselves on the record, snapcompact and shake do not — a
// record must not claim a provider round trip that never happened, and these
// two rungs are pure local work (a snapshot and an elision).
func RecordedMethod(member string) string {
	switch member {
	case methodRemote, MethodHandoff, methodSoft:
		return member
	}
	return ""
}

// products is compaction.methodOrder restricted to the members that produce a
// retained context, in order. An order with no product — the shipped default
// `threshold,overflow,promotion`, which only names triggers — means omp's
// ladder: remote, snapcompact, handoff, shake, soft, of which the first
// applicable one runs (see compactMethodApplicable). A configured order is
// never rewritten: the products it names are used in the order it names them.
func (c CompactionConfig) products() []string {
	var out []string
	for _, m := range c.methods() {
		if slices.Contains(compactTriggers, m) {
			continue
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return slices.Clone(ompProductOrder)
	}
	return out
}

// compactMethodApplicable reports whether a ladder member can run against the
// compaction input, and why not when it cannot. These are omp's per-member
// applicability rules (omp
// packages/coding-agent/src/session/compaction-methods.ts):
//
//	remote      — needs a remote compaction endpoint
//	snapcompact — needs an image in the input
//	handoff, shake, soft — always
//
// An inapplicable member is skipped, not failed: the ladder falls through to
// the next member, exactly as omp does. An overridden member decides for
// itself — the override may implement the rung differently (the M5 #23
// handoff-document variant is the model).
func (a *Agent) compactMethodApplicable(name string, msgs []ai.Message) (bool, string) {
	if _, overridden := compactMethodOverrides[name]; overridden {
		return true, ""
	}
	switch name {
	case methodRemote:
		// The remote rung is provider-native streaming compaction: it needs
		// an endpoint to talk to. xdev ships no setting for one and no
		// transport speaks it, so the rung never applies here — the ladder
		// falls through instead of dressing a local summary up as a remote
		// one.
		who := "the active provider"
		if a.Provider != nil {
			who = a.Provider.Name()
		}
		return false, fmt.Sprintf("remote (provider-native streaming) not supported by %s", who)
	case methodSnapcompact:
		if !messagesContainImage(msgs) {
			return false, "snapcompact needs an image in the compaction input"
		}
	}
	return true, ""
}

// firstProductIsHandoff reports whether the provider summarize is the member
// the ladder would pick — the only member whose round trip is worth
// backgrounding (see the async trigger in compact_async.go). The walk is the
// ladder's own: the first APPLICABLE member decides, so a rung that cannot
// run (no endpoint for remote, no image for snapcompact) never counts as the
// pick.
func (a *Agent) firstProductIsHandoff(history []ai.Message) bool {
	for _, name := range a.compaction().products() {
		if ok, _ := a.compactMethodApplicable(name, history); !ok {
			continue
		}
		return name == MethodHandoff
	}
	return false
}

// runCompactLadder tries the configured products in order and returns the
// entry the first successful member produced (its Method names it, where the
// record contract says a member names itself). A member that does not apply to
// the input is skipped, a failing member is logged, and the ladder continues
// down the list; only when every member failed does the caller see an error —
// compaction then degrades to the pre-compaction behavior instead of
// persisting a half-result.
func (a *Agent) runCompactLadder(ctx context.Context, span *compactionSpan) (*session.CompactionEntry, error) {
	var lastErr error
	for _, name := range a.compaction().products() {
		if ok, reason := a.compactMethodApplicable(name, span.msgs); !ok {
			logx.Infof("compaction: %s — falling through", reason)
			continue
		}
		fn := lookupCompactMethod(name)
		if fn == nil {
			lastErr = fmt.Errorf("compaction: %s: unknown method", name)
			logx.Errorf("%v — falling through", lastErr)
			continue
		}
		entry, err := fn(a, ctx, span)
		if err == nil {
			entry.Method = RecordedMethod(name)
			// The mechanical working-state block is prepended here, at the
			// one place every member passes through, so a rung cannot forget
			// it. It is extracted from the discarded span because that is the
			// only point where the facts still exist in full: once a member
			// has rendered, it has already decided what to keep.
			//
			// Only for the summarizing rung. A model asked to compress the
			// span drops exactly these facts -- which files, which commands --
			// because they look like detail. The deterministic members do not
			// need help: shake and soft keep structure by construction, and
			// adding a file list to shake would re-introduce the argument
			// values its whole contract is to delete.
			if name == MethodHandoff {
				entry.Summary = withWorkingState(entry.Summary, extractWorkingState(span.msgs[:span.cut()]))
			}
			return entry, nil
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
	for _, m := range a.compaction().methods() {
		if !slices.Contains(compactTriggers, m) {
			return true
		}
	}
	return false
}

// methodHandoffRun is the provider summarize: one streaming call compresses
// the discarded span into the retained context. The call's usage rides on
// the summary message, which is the one place a resume can read it back:
// the tokens the compaction read and wrote are part of the session's bill,
// and dropping them is how a compacted session showed $0.00 for the very
// call that replaced its history.
func methodHandoffRun(a *Agent, ctx context.Context, span *compactionSpan) (*session.CompactionEntry, error) {
	// The summarize input is the span's DROPPED span: the older history up to
	// the turn start, or — for a split cut — that turn's prefix, whose suffix is
	// retained below the summary. The span's own prior summary rides along so
	// the call updates it instead of re-describing history already summarized.
	summary, usage, err := a.summarizeSpan(ctx, span.msgs[:span.cp.historyEnd()], span.split(), a.previousSummary(span))
	if err != nil {
		return nil, err
	}
	msg := textSummary(summary)
	msg.Usage = usage
	msg.Model = a.Model
	return compactEntry(span, msg), nil
}

// methodShakeRun mechanically elides the discarded span: tool-call argument
// bodies and thinking are dropped, results are cut to a head, and runs of
// repeated boilerplate collapse. The skeleton — who spoke, which tool ran —
// survives without a model call.
func methodShakeRun(_ *Agent, _ context.Context, span *compactionSpan) (*session.CompactionEntry, error) {
	text := elideShake(span.msgs[:span.cut()])
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("shake: span rendered empty")
	}
	return compactEntry(span, textSummary(text)), nil
}

// methodSoftRun prunes the discarded span structurally — superseded reads and
// results that carry nothing — and retains what is left as text.
func methodSoftRun(_ *Agent, _ context.Context, span *compactionSpan) (*session.CompactionEntry, error) {
	kept, dropped := pruneUseless(span.msgs[:span.cut()])
	var b strings.Builder
	fmt.Fprintf(&b, "soft prune: %d of %d dropped messages carried nothing (superseded reads, empty results), %d kept.\n\n",
		dropped, span.cut(), len(kept))
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
