package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/FreePeak/xdev/internal/ai"
)

// Deterministic ladder members (M5 #24). Everything here runs locally, with
// no provider round trip: shake elides, soft prunes, and both render through
// renderTranscript so a reader of the session file sees the same shape the
// model was given.

// Bounds for the deterministic members: one tool result and the whole
// retained text. A deterministic context must not grow with the span it
// replaces.
const (
	elideResultChars = 600
	elideTotalChars  = 60000
)

// elideShake renders a mechanically elided transcript of msgs: the
// role/tool skeleton survives, argument bodies and thinking disappear,
// results keep a head, and repeated identical consecutive lines collapse to
// one (the boilerplate half of "drop tool-call argument bodies and repeated
// boilerplate"). ponytail: a repeated *line* is boilerplate by definition;
// near-identical lines with a counter inside are not detected — the upgrade
// path is a fuzzy key, which costs more than it saves here.
func elideShake(msgs []ai.Message) string {
	var out []string
	for _, line := range skeletonize(msgs) {
		if n := len(out); n > 0 && out[n-1] == line {
			continue // repeated consecutive line: one copy stays
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// pruneUseless applies soft's two structural prunes to msgs:
// supersedeReads drops a read result that a later read of the same path
// reached further into, and dropUseless drops messages carrying nothing at
// all. It returns the kept messages and the drop count.
func pruneUseless(msgs []ai.Message) (kept []ai.Message, dropped int) {
	calls := readCalls(msgs)
	furthest := map[string]int{}
	for _, r := range calls {
		if r.offset > furthest[r.path] {
			furthest[r.path] = r.offset
		}
	}
	for i := range msgs {
		m := msgs[i]
		if isEmptyNoop(m) {
			dropped++
			continue
		}
		if m.Role == ai.RoleToolResult {
			if r, ok := calls[m.ToolCallID]; ok && r.offset < furthest[r.path] {
				dropped++
				continue
			}
		}
		kept = append(kept, m)
	}
	return kept, dropped
}

// skeletonize reduces msgs to one "role: payload" line each: the shape the
// shake member keeps once the bodies are gone.
func skeletonize(msgs []ai.Message) []string {
	out := make([]string, 0, len(msgs))
	for i := range msgs {
		m := &msgs[i]
		switch m.Role {
		case ai.RoleAssistant:
			parts := []string{}
			if txt := firstNonEmptyLine(m.Text()); txt != "" {
				parts = append(parts, txt)
			}
			for _, c := range m.ToolCalls() {
				parts = append(parts, fmt.Sprintf("[tool %s(%s)]", c.Name, argKeys(c.Arguments)))
			}
			if len(parts) == 0 {
				parts = append(parts, "(empty turn)")
			}
			out = append(out, "assistant: "+strings.Join(parts, " "))
		case ai.RoleUser:
			out = append(out, "user: "+firstNonEmptyLine(m.Text()))
		case ai.RoleToolResult:
			head := firstNonEmptyLine(m.Text())
			if head == "" {
				head = "(no output)"
			}
			out = append(out, fmt.Sprintf("toolResult(%s): %s", m.ToolName, head))
		default:
			out = append(out, string(m.Role)+": "+firstNonEmptyLine(m.Text()))
		}
	}
	return out
}

// renderTranscript renders msgs as bounded "role: text" turns; resultCap
// clips one tool result, totalCap clips the whole rendering.
func renderTranscript(msgs []ai.Message, resultCap, totalCap int) string {
	var b strings.Builder
	for i := range msgs {
		m := &msgs[i]
		txt := m.Text()
		if m.Role == ai.RoleToolResult && len(txt) > resultCap {
			txt = txt[:resultCap] + "…"
		}
		if txt == "" {
			txt = "(empty)"
		}
		if calls := m.ToolCalls(); len(calls) > 0 {
			names := make([]string, len(calls))
			for j, c := range calls {
				names[j] = c.Name
			}
			txt += " [tools: " + strings.Join(names, ", ") + "]"
		}
		fmt.Fprintf(&b, "%s: %s\n", m.Role, txt)
		if b.Len() >= totalCap {
			return capText(b.String(), totalCap) + "…"
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// readRef is one read call's target.
type readRef struct {
	path   string
	offset int
}

// readCalls maps every read call id in msgs to the path and offset it asked
// for; the result messages carry only the id, so the arguments are the one
// place the path is written down.
func readCalls(msgs []ai.Message) map[string]readRef {
	out := map[string]readRef{}
	for i := range msgs {
		for _, c := range msgs[i].ToolCalls() {
			if !strings.EqualFold(c.Name, "read") {
				continue
			}
			var a struct {
				Path   string `json:"path"`
				Offset int    `json:"offset"`
			}
			if err := json.Unmarshal(c.Arguments, &a); err != nil || a.Path == "" {
				continue
			}
			out[c.ID] = readRef{path: a.Path, offset: a.Offset}
		}
	}
	return out
}

// isEmptyNoop reports a message that carries nothing forward: no text and no
// tool call, or an empty non-error tool result (dropUseless).
func isEmptyNoop(m ai.Message) bool {
	switch m.Role {
	case ai.RoleAssistant, ai.RoleUser:
		return m.Text() == "" && len(m.ToolCalls()) == 0
	case ai.RoleToolResult:
		if m.Details != nil || m.IsError {
			return false
		}
		// An empty result, or the placeholder ai.EnsureToolOutput substituted
		// for one so the wire always has an `output`: either way it carries
		// nothing forward.
		return strings.TrimSpace(m.Text()) == "" || m.Text() == ai.ToolOutputPlaceholder
	}
	return false
}

// capText cuts s to at most n bytes without splitting a UTF-8 rune (the caps
// here bound size, not layout, so a clean byte cut would do — except that the
// result is stored as JSON text, where a torn rune is corruption).
func capText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// firstNonEmptyLine returns s's first non-empty line.
func firstNonEmptyLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// argKeys lists an argument object's keys, sorted — never its values: the
// skeleton keeps the shape of a call, not its payload.
func argKeys(raw []byte) string {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || len(obj) == 0 {
		return ""
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}
