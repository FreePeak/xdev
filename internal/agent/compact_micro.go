package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// microcompactPlaceholder is the text a cleared tool result carries after a
// microcompact pass: the turn skeleton stays (which tool ran, against what)
// while the body that the model no longer needs is dropped.
//
// It says WHAT HAPPENED and WHAT TO DO, because the model reads it as its own
// record of the call. The first shape was a flat "[Old tool result content
// cleared]", and the model read that as "this call produced nothing" — so it
// ran the same command again to find out. Live 2026-10-21 (session
// 8efefc9e): 182 tool calls, and every one of them was issued at a point
// where an earlier result from the same tool had already been cleared. The
// notice now names the tool and says the call can be repeated.
const microcompactPlaceholder = "[the result of this %s call was cleared from the context to make room — re-run it if you still need it]"

// microcompactPlaceholderFor is the placeholder for one tool result, naming
// the tool that produced it.
func microcompactPlaceholderFor(toolName string) string {
	if toolName == "" {
		toolName = "tool"
	}
	return fmt.Sprintf(microcompactPlaceholder, toolName)
}

// defaultMicrocompactKeepRecent is the number of tool results whose bodies
// survive a microcompact pass — the newest N, the rest cleared. Matches
// ZCode's DEFAULT_MICROCOMPACT_KEEP_RECENT_TOOL_RESULTS.
//
// It is kept as the FLOOR for the ladder rung's own window and as the
// documented name of the rule this file replaced. The pre-request tier no
// longer counts results: a count cannot say what a window is worth, and
// counting five was what capped a survey's findings at five and sent the
// model back to re-run its commands. The budget (keepTokens) decides.
const defaultMicrocompactKeepRecent = 5

// microcompactMinTokenSavings is the floor below which a pass that clears
// nothing worth clearing is reported as a no-op, so the ladder and the
// pre-request tier never churn for a handful of tokens.
const microcompactMinTokenSavings = 256

// microcompactToolResults clears the bodies of stale tool results in msgs,
// keeping the newest results whose total size fits keepTokens. It returns a
// (possibly new) slice and whether anything changed. No provider call:
// deterministic.
//
// The window is a TOKEN budget, not a count. The count alone is the wrong
// unit: five 50 KB results are 250 KB of context and five 200-byte results
// are 1 KB, so a fixed count either throws away a survey's findings or
// keeps a context no provider will serve. keepTokens is the same budget the
// compaction ladder keeps after a summary (CompactionConfig.KeepRecentTokens),
// so the two moves agree on what "recent" means.
func microcompactToolResults(msgs []ai.Message, keepTokens int64) ([]ai.Message, bool) {
	trIdx := make([]int, 0)
	for i := range msgs {
		if msgs[i].Role == ai.RoleToolResult {
			trIdx = append(trIdx, i)
		}
	}
	if len(trIdx) == 0 {
		return msgs, false
	}

	var before int64
	for i := range msgs {
		before += int64(len(msgs[i].Text()))
	}

	// Keep the newest results while their cumulative cost fits the budget.
	// The newest result is kept whatever it weighs: a run whose newest
	// finding is gone cannot use anything it has left.
	keep := make(map[int]struct{}, len(trIdx))
	var used int64
	for k := len(trIdx) - 1; k >= 0; k-- {
		idx := trIdx[k]
		cost := messageTokens(msgs[idx])
		if len(keep) > 0 && used+cost > keepTokens {
			break
		}
		keep[idx] = struct{}{}
		used += cost
	}
	if len(keep) == len(trIdx) {
		return msgs, false // every result fits: nothing to clear
	}

	out := make([]ai.Message, len(msgs))
	copy(out, msgs)
	for _, idx := range trIdx {
		if _, ok := keep[idx]; ok {
			continue
		}
		m := out[idx]
		placeholder := microcompactPlaceholderFor(m.ToolName)
		newContent := make([]ai.Block, 0, len(m.Content))
		replaced := false
		for _, b := range m.Content {
			if _, ok := b.(ai.TextBlock); ok {
				if !replaced {
					newContent = append(newContent, ai.TextBlock{Text: placeholder})
					replaced = true
				}
				continue
			}
			newContent = append(newContent, b)
		}
		if !replaced {
			newContent = append(newContent, ai.TextBlock{Text: placeholder})
		}
		m.Content = newContent
		out[idx] = m
	}

	var after int64
	for i := range out {
		after += int64(len(out[i].Text()))
	}
	savings := (before - after) / charsPerToken
	if savings < microcompactMinTokenSavings {
		return msgs, false
	}
	return out, true
}

// messageTokens is the estimated token cost of one message, on the same
// scale estimateTokens uses.
func messageTokens(m ai.Message) int64 {
	return (int64(len(m.Text())) + 8) / charsPerToken
}

// methodMicrocompactRun is the ladder member: it clears stale tool results in
// the discarded span (keeping the newest ones) and retains the result as
// text. Deterministic, no provider call.
func methodMicrocompactRun(_ *Agent, _ context.Context, span *compactionSpan) (*session.CompactionEntry, error) {
	dropped := span.msgs[:span.cut()]
	msgs := make([]ai.Message, len(dropped))
	copy(msgs, dropped)
	cleared, _ := microcompactToolResults(msgs, DefaultKeepRecentTokens)

	var b strings.Builder
	for i := range cleared {
		m := &cleared[i]
		txt := m.Text()
		if txt == "" {
			txt = "(empty)"
		}
		b.WriteString(string(m.Role))
		b.WriteString(": ")
		b.WriteString(txt)
		b.WriteString("\n")
	}
	if strings.TrimSpace(b.String()) == "" {
		return nil, errors.New("microcompact: span rendered empty")
	}
	return compactEntry(span, textSummary(strings.TrimRight(b.String(), "\n"))), nil
}
