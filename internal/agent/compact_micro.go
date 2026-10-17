package agent

import (
	"context"
	"errors"
	"strings"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// microcompactPlaceholder is the text a cleared tool result carries after a
// microcompact pass: the turn skeleton stays (which tool ran, against what)
// while the body that the model no longer needs is dropped.
const microcompactPlaceholder = "[Old tool result content cleared]"

// defaultMicrocompactKeepRecent is the number of tool results whose bodies
// survive a microcompact pass — the newest N, the rest cleared. Matches
// ZCode's DEFAULT_MICROCOMPACT_KEEP_RECENT_TOOL_RESULTS.
const defaultMicrocompactKeepRecent = 5

// microcompactMinTokenSavings is the floor below which a pass that clears
// nothing worth clearing is reported as a no-op, so the ladder and the
// pre-request tier never churn for a handful of tokens.
const microcompactMinTokenSavings = 256

// microcompactToolResults clears the bodies of stale tool results in msgs,
// keeping the newest keepRecent tool results intact. It returns a (possibly
// new) slice and whether anything changed. No provider call: deterministic.
func microcompactToolResults(msgs []ai.Message, keepRecent int) ([]ai.Message, bool) {
	if keepRecent < 0 {
		keepRecent = 0
	}
	trIdx := make([]int, 0)
	for i := range msgs {
		if msgs[i].Role == ai.RoleToolResult {
			trIdx = append(trIdx, i)
		}
	}
	if len(trIdx) <= keepRecent {
		return msgs, false
	}

	var before int64
	for i := range msgs {
		before += int64(len(msgs[i].Text()))
	}

	out := make([]ai.Message, len(msgs))
	copy(out, msgs)
	clearSet := make(map[int]struct{}, len(trIdx)-keepRecent)
	for _, idx := range trIdx[:len(trIdx)-keepRecent] {
		clearSet[idx] = struct{}{}
	}
	for idx := range clearSet {
		m := out[idx]
		newContent := make([]ai.Block, 0, len(m.Content))
		replaced := false
		for _, b := range m.Content {
			if _, ok := b.(ai.TextBlock); ok {
				if !replaced {
					newContent = append(newContent, ai.TextBlock{Text: microcompactPlaceholder})
					replaced = true
				}
				continue
			}
			newContent = append(newContent, b)
		}
		if !replaced {
			newContent = append(newContent, ai.TextBlock{Text: microcompactPlaceholder})
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

// methodMicrocompactRun is the ladder member: it clears stale tool results in
// the discarded span (keeping the newest 5) and retains the result as text.
// Deterministic, no provider call.
func methodMicrocompactRun(_ *Agent, _ context.Context, span *compactionSpan) (*session.CompactionEntry, error) {
	dropped := span.msgs[:span.cut()]
	msgs := make([]ai.Message, len(dropped))
	copy(msgs, dropped)
	cleared, _ := microcompactToolResults(msgs, defaultMicrocompactKeepRecent)

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