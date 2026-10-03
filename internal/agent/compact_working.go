package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/FreePeak/xdev/internal/ai"
)

// The working-state extractor.
//
// Every compaction method in the ladder has the same blind spot: it is handed
// the discarded span and asked to produce a *narrative*, so what survives is
// whatever a model (or a regex) thought was worth a sentence. The facts a
// resumed session actually needs are not narrative. They are: which files
// were touched, what was run, what failed.
//
// This extracts those facts mechanically, for free, before any rung gets to
// decide what matters. It is the cheap tier of the ladder in the spirit of
// the snapcompact/shake members, and it composes with all of them rather than
// replacing any.
//
// The deliberate limit: no model call, no interpretation. Anything requiring
// a judgement ("why was this file changed") stays with the summarizing rungs;
// this only records what happened.

const (
	// workingStateMaxFiles caps the file list. A long session touches more
	// files than are worth carrying; the head of the list is what matters.
	workingStateMaxFiles = 25
	// workingStateMaxCommands caps the shell commands, which are the most
	// repetitive part of a long session.
	workingStateMaxCommands = 10
	// workingStateMaxFailures caps recorded failures.
	workingStateMaxFailures = 8
	// workingStateMaxChars bounds the whole block so the "free" tier cannot
	// become the most expensive part of a compaction.
	workingStateMaxChars = 3000
)

// workingState is what the extractor found in a discarded span.
type workingState struct {
	read      []string
	edited    []string
	commands  []string
	failures  []string
	questions []string
}

// extractWorkingState walks the discarded span and records what happened, with
// no model involved.
//
// It reads the session's own data model -- assistant messages carry
// ToolCallBlock, and a tool result is its own RoleToolResult message with an
// IsError flag -- rather than parsing prose, so what is recorded is what the
// harness actually did rather than what a transcript claims it did.
func extractWorkingState(msgs []ai.Message) workingState {
	var ws workingState
	seen := map[string]map[string]bool{
		"read": {}, "edit": {}, "cmd": {}, "fail": {}, "ask": {},
	}
	for _, m := range msgs {
		if m.Role == ai.RoleToolResult {
			if m.IsError {
				ws.add(&ws.failures, seen["fail"],
					fmt.Sprintf("%s: %s", m.ToolName, oneline(textOf(m), 100)))
			}
			continue
		}
		for _, b := range m.Content {
			blk, ok := b.(ai.ToolCallBlock)
			if !ok {
				continue
			}
			switch blk.Name {
			case "read":
				if p := stringArg(blk.Arguments, "path", "file"); p != "" {
					ws.add(&ws.read, seen["read"], p)
				}
			case "write", "edit":
				if p := stringArg(blk.Arguments, "path", "file"); p != "" {
					ws.add(&ws.edited, seen["edit"], p)
				} else if p := pathFromPatch(blk.Arguments); p != "" {
					// The edit tool's patch grammar carries the path on the first
					// input line, not in a JSON field.
					ws.add(&ws.edited, seen["edit"], p)
				}
			case "bash":
				if c := stringArg(blk.Arguments, "command"); c != "" {
					ws.add(&ws.commands, seen["cmd"], oneline(c, 120))
				}
			case "ask":
				if q := stringArg(blk.Arguments, "question"); q != "" {
					ws.add(&ws.questions, seen["ask"], oneline(q, 120))
				}
			}
		}
	}
	return ws
}

// textOf joins a message's text blocks, which is where a tool result's body
// lives in this data model.
func textOf(m ai.Message) string {
	var b strings.Builder
	for _, blk := range m.Content {
		if t, ok := blk.(ai.TextBlock); ok {
			b.WriteString(t.Text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func (ws *workingState) add(dst *[]string, seen map[string]bool, v string) {
	if v == "" || seen[v] {
		return
	}
	seen[v] = true
	*dst = append(*dst, v)
}

// render produces the block that goes into the retained context. It returns ""
// when there is nothing to say, so a span with no tool activity costs zero
// bytes.
func (ws workingState) render() string {
	var b strings.Builder
	b.WriteString("Working state (recorded mechanically from the discarded span; it is what happened, not why):\n")
	wrote := false
	writeList(&b, &wrote, "files edited", ws.edited, workingStateMaxFiles)
	writeList(&b, &wrote, "files read", ws.read, workingStateMaxFiles)
	writeList(&b, &wrote, "commands run", ws.commands, workingStateMaxCommands)
	writeList(&b, &wrote, "questions asked", ws.questions, workingStateMaxCommands)
	writeList(&b, &wrote, "tool failures", ws.failures, workingStateMaxFailures)
	if !wrote {
		return ""
	}
	out := b.String()
	if len(out) > workingStateMaxChars {
		out = out[:workingStateMaxChars] + "\n[working state truncated]\n"
	}
	return out
}

// writeList renders one capped list. The count in the header is the TRUE
// total, never the number shown: "files read (400)" followed by 25 rows tells
// the resumed session both what it is looking at and what it is not seeing.
func writeList(b *strings.Builder, wrote *bool, label string, items []string, limit int) {
	if len(items) == 0 {
		return
	}
	*wrote = true
	fmt.Fprintf(b, "- %s (%d)", label, len(items))
	shown := items
	if len(shown) > limit {
		shown = shown[:limit]
	}
	for _, it := range shown {
		fmt.Fprintf(b, "\n    %s", it)
	}
	if len(shown) < len(items) {
		fmt.Fprintf(b, "\n    … and %d more", len(items)-len(shown))
	}
}

// withWorkingState prepends the extracted block to a rung's summary.
//
// It runs at the ladder's single choke point rather than inside each member,
// so a new method gets the facts for free instead of having to remember.
// A rung whose summary is not text (snapcompact's PNG) is left alone: there is
// nowhere to put prose next to an image, and mangling one would be worse than
// losing the block.
func withWorkingState(summary ai.Message, ws workingState) ai.Message {
	block := ws.render()
	if block == "" {
		return summary
	}
	for _, b := range summary.Content {
		if _, isImage := b.(ai.ImageBlock); isImage {
			return summary
		}
	}
	merged := make([]ai.Block, 0, len(summary.Content)+1)
	merged = append(merged, ai.TextBlock{Text: block})
	merged = append(merged, summary.Content...)
	summary.Content = merged
	return summary
}

// stringArg reads a string field from a tool call's raw arguments.
func stringArg(raw json.RawMessage, keys ...string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// pathFromPatch pulls the target path out of a hashline patch: line 1 is a
// "[path]" or "[path#tag]" header.
func pathFromPatch(raw json.RawMessage) string {
	s := stringArg(raw, "input")
	if s == "" {
		return ""
	}
	line := s
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		line = s[:i]
	}
	if !strings.HasPrefix(line, "[") {
		return ""
	}
	end := strings.IndexByte(line, ']')
	if end <= 1 {
		return ""
	}
	p := line[1:end]
	if i := strings.IndexByte(p, '#'); i >= 0 {
		p = p[:i]
	}
	return strings.TrimSpace(p)
}

// oneline collapses whitespace and truncates, so one pathological tool output
// cannot own the block.
func oneline(s string, max int) string {
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
