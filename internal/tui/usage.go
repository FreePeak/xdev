package tui

import (
	"fmt"
	"strings"
	"time"
)

// /usage: the session's own token-and-time report, in the three blocks a
// dashboard is read by — what it cost, what it took, and how many tool calls
// were spent along the way.
//
// The status row answers the same questions one glance at a time and cannot
// answer the ones a report exists for: the cache HIT RATE (the split only
// makes sense as a ratio), an average TTFT (the row shows the last turn's),
// and the tool-call count (the row has no room for it at all). This is the
// long form, not a second source of truth: every number is the same
// Status field the HUD reads, under the same lock.
//
// It is a system block, not an overlay, for the reason /help is: a report is
// something you read once and keep in the scrollback next to the turn that
// produced it, and it must not steal a key the ask card or the composer
// needs.

// usageReport is the read-only snapshot a report is rendered from, taken
// under one lock so a turn landing mid-render cannot mix two sessions' halves
// into one set of numbers.
type usageReport struct {
	in, out, cache, think int64
	total                 int64
	calls, errors         int
	toolWork, llmWork     time.Duration
	ttftSum               int64
	ttftCount             int64
	ctx, window           int64
}

// UsageReport renders the session's token, time and tool-call totals.
//
// Zero means the session has not done the thing the line describes, and a
// zero line is a claim about a measurement rather than a fact, so each block
// hides the halves it cannot know: an unwired cost is never drawn as $0.00,
// and a session with no finished turn reports no average TTFT.
func (a *App) UsageReport() string {
	a.mu.Lock()
	st := a.st
	work := a.activeWork()
	a.mu.Unlock()

	r := usageReport{
		in: st.TokensIn, out: st.TokensOut, cache: st.TokensCache, think: st.TokensThink,
		calls: st.ToolCalls, errors: st.ToolErrors, toolWork: st.ToolWork, llmWork: st.LLMWork,
		ctx: st.CtxUsed, window: st.CtxWindow,
		ttftSum: st.TTFTSum, ttftCount: st.TTFTCount,
	}
	// The provider's own normalization (input + output + cacheRead =
	// totalTokens) is the sum every wire already reports; total tokens
	// billed is the same arithmetic over the session, so one fmt and no
	// second field that could disagree with the ↑⇢↓ counters.
	r.total = r.in + r.out + r.cache

	var b strings.Builder
	fmt.Fprintf(&b, "Token usage\n")
	fmt.Fprintf(&b, "  %s tok\n", groupTokens(r.total))
	// Hit rate is cached / (cached + fresh input) — the share of PROMPT
	// tokens the cache served. Over total it would read low and mean
	// nothing, since output was never cacheable.
	if r.cache > 0 {
		fmt.Fprintf(&b, "  cache hit %d%%\n", int(100*r.cache/(r.in+r.cache)))
	}
	fmt.Fprintf(&b, "  uncached input %s tok\n", groupTokens(r.in))
	fmt.Fprintf(&b, "  cached input %s tok\n", groupTokens(r.cache))
	fmt.Fprintf(&b, "  output %s tok\n", groupTokens(r.out))
	if r.think > 0 {
		fmt.Fprintf(&b, "  (of which reasoning %s tok)\n", groupTokens(r.think))
	}
	if st.Cost > 0 {
		fmt.Fprintf(&b, "  cost $%.4f\n", st.Cost)
	}
	// The live context occupancy against the model's window, with the
	// share that number is. The status row shows the same pair without the
	// percentage, because there it competes with the path for width; here
	// it is the question the whole report is read under — a glance at
	// "how much room is left" is not a subtraction.
	if r.ctx > 0 && r.window > 0 {
		fmt.Fprintf(&b, "  context %s/%s (%d%%)\n", HumanTokens(r.ctx), HumanTokens(r.window),
			int(100*r.ctx/r.window))
	}

	b.WriteString("\nSession statistics\n")
	// LLM time and tool time are the two halves of a turn's wall time, and
	// they are reported apart because they are the two different problems:
	// a slow model and a slow tool look identical in the total.
	if r.llmWork > 0 {
		fmt.Fprintf(&b, "  LLM time %s\n", humanDur(r.llmWork))
	}
	if r.toolWork > 0 {
		fmt.Fprintf(&b, "  tool time %s\n", humanDur(r.toolWork))
	}
	if r.ttftCount > 0 {
		fmt.Fprintf(&b, "  avg time to first token %s\n",
			(time.Duration(r.ttftSum/r.ttftCount) * time.Millisecond).Round(10*time.Millisecond))
	}
	// The tool-call block is the third one the row has no room for: how
	// many calls the session spent, how many failed, and their share of
	// the session's active time.
	if r.calls > 0 {
		fmt.Fprintf(&b, "  tool calls %d", r.calls)
		if r.errors > 0 {
			fmt.Fprintf(&b, " (%d failed)", r.errors)
		}
		if r.toolWork > 0 && work > 0 {
			fmt.Fprintf(&b, " · %d%% of active time", int(100*r.toolWork/work))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "  active time %s\n", humanDur(work))
	return b.String()
}

// groupTokens renders a token count with thousands separators: the report's
// job is to be readable at a glance, and "1016717" is a number you have to
// parse while "1,016,717" is one you read. The status row's HumanTokens stays
// compact (1.0M) because it is competing for width there; a report has a line
// to itself.
func groupTokens(n int64) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// Usage implements CommandAPI: the report lands in the transcript as a
// system block, the way /help and /hotkeys do — it is read once, not
// operated, so an overlay would only add a key to close.
func (a *App) Usage() error {
	a.AddSystemBlock(a.UsageReport())
	return nil
}
