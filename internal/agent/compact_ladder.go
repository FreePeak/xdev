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
// member. It follows omp's record contract: the record names the member that
// produced it. omp writes the selected member verbatim — `method: Q ?
// undefined : P` (omp
// packages/coding-agent/src/session/session-maintenance.ts:763, the manual
// /compact loop) and `method: T ? undefined : f` (same file :3079, the auto
// loop), with the snapcompact rescue writing `method: "snapcompact"` (same
// file :2383); the record writer stores the value as it comes
// (omp packages/coding-agent/src/session/session-manager.ts:2339-2356).
// `undefined` there means the compaction came from an extension hook, not
// that the member was unnamed.
//
// One deliberate divergence: omp's shake rewrites the history in place and
// writes NO compaction record at all (omp
// packages/coding-agent/src/session/session-maintenance.ts:3245-3300), while
// xdev's shake persists the retained context like every other rung — so the
// xdev record names the rung that produced it. A member outside omp's five
// (xdev's microcompact extension) names nothing.
func RecordedMethod(member string) string {
	switch member {
	case methodRemote, methodSnapcompact, MethodHandoff, methodShake, methodSoft:
		return member
	}
	return ""
}

// products is compaction.methodOrder restricted to the members that produce a
// retained context, in order.
//
// omp's vocabulary: `methodOrder` holds the five PRODUCT members only —
// remote, snapcompact, handoff, shake, soft (omp
// packages/coding-agent/src/session/compaction-methods.ts:29-54) — and omp
// filters anything else out of it, which would leave the order empty and
// disable compaction (omp
// packages/coding-agent/src/session/session-maintenance.ts:13-15). xdev's
// shipped order names TRIGGERS instead: `threshold,overflow,promotion` are
// xdev's own names for WHEN to compact, not omp members (omp's triggers are
// the reason argument of runAutoCompaction: idle/threshold/overflow/
// incomplete, omp packages/coding-agent/src/session/session-maintenance.ts:
// 2559). So xdev draws its own line: an order that names no product means
// omp's ladder — remote, snapcompact, handoff, shake, soft, of which the
// first applicable one runs (see compactMethodApplicable) — and an order
// that names products is never rewritten: those products are used in the
// order it names them.
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
// active model, and why not when it cannot. These are omp's per-member
// applicability rules (omp
// packages/coding-agent/src/session/compaction-methods.ts:70-84, and the same
// rules in the loops that pick the member — omp
// packages/coding-agent/src/session/session-maintenance.ts:19-21, :552 and
// :2578-2596):
//
//	remote      — a configured remote compaction endpoint OR a provider that
//	              supports native compaction
//	snapcompact — the active model declares image input
//	handoff, shake, soft — always
//
// An inapplicable member is skipped, not failed: the ladder falls through to
// the next member, exactly as omp does. An overridden member decides for
// itself — the override may implement the rung differently (the M5 #23
// handoff-document variant is the model).
func (a *Agent) compactMethodApplicable(name string) (bool, string) {
	if _, overridden := compactMethodOverrides[name]; overridden {
		return true, ""
	}
	switch name {
	case methodRemote:
		// omp's remote rung applies with a configured remote compaction
		// endpoint OR a provider that supports native compaction — `hde`
		// ORs the two (omp
		// packages/coding-agent/src/session/compaction-methods.ts:70-72;
		// the provider-native half is the Responses /compact endpoint, the
		// v2 streaming compaction transport and Anthropic's server
		// compaction). xdev has NEITHER: there is no remote-endpoint
		// setting, and no transport here speaks provider-native compaction
		// (`internal/ai/openai_responses.go` has no /responses/compact, and
		// nothing in internal/ implements the other two). So the rung never
		// applies and the ladder falls through instead of dressing a local
		// summary up as a remote one. The comment is the reason nobody
		// re-adds the rung: the moment xdev gains either half, this check
		// has to come back.
		who := "the active provider"
		if a.Provider != nil {
			who = a.Provider.Name()
		}
		return false, fmt.Sprintf("remote (provider-native streaming) not supported by %s", who)
	case methodSnapcompact:
		// The rung renders the discarded span as a bitmap, so it needs a
		// model that can read one: the ACTIVE MODEL's declared input
		// modalities, not an image somewhere in the conversation (omp
		// `u?.input?.includes("image") === true`, omp
		// packages/coding-agent/src/session/compaction-methods.ts:77, and
		// `t?.input.includes("image")` in the applicability rule at omp
		// packages/coding-agent/src/session/session-maintenance.ts:20).
		// xdev's model metadata carries the same declaration (models.yml
		// `vision: true`), read through the agent's Vision hook.
		if !modelAcceptsImages(a) {
			return false, fmt.Sprintf("snapcompact needs a vision-capable model (%s is text-only)", modelLabel(a))
		}
	}
	return true, ""
}

// modelLabel names the active model for a message about it: the model id when
// there is one, a neutral phrase when there is not (the offline CLI path runs
// a member without an agent).
func modelLabel(a *Agent) string {
	if a == nil || a.Model == "" {
		return "the active model"
	}
	return a.Model
}

// firstProductIsHandoff reports whether the provider summarize is the member
// the ladder would pick — the only member whose round trip is worth
// backgrounding (see the async trigger in compact_async.go). The walk is the
// ladder's own: the first APPLICABLE member decides, so a rung that cannot
// run (no remote compaction for remote, a text-only model for snapcompact)
// never counts as the pick.
func (a *Agent) firstProductIsHandoff() bool {
	for _, name := range a.compaction().products() {
		if ok, _ := a.compactMethodApplicable(name); !ok {
			continue
		}
		return name == MethodHandoff
	}
	return false
}

// runCompactLadder tries the configured products in order and returns the
// entry the first successful member produced (its Method names the member
// that produced it). A member that does not apply to the active model is
// skipped, a failing member is logged, and the ladder continues down the
// list; only when every member failed does the caller see an error —
// compaction then degrades to the pre-compaction behavior instead of
// persisting a half-result.
func (a *Agent) runCompactLadder(ctx context.Context, span *compactionSpan) (*session.CompactionEntry, error) {
	var lastErr error
	for _, name := range a.compaction().products() {
		if ok, reason := a.compactMethodApplicable(name); !ok {
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
