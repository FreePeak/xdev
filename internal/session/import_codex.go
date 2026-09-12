// Codex transcript parser (issue #28). Source layout:
// ~/.codex/sessions/YYYY/MM/DD/rollout-<stamp>-<thread-uuid>.jsonl, one
// JSON object per line. Shape verified against real 2026-04..08 transcripts:
//
//	{"timestamp":"…","type":"session_meta","payload":{"id":"…","cwd":"…",…}}
//	{"timestamp":"…","type":"response_item","payload":{"type":"message",
//	 "role":"user"|"assistant","content":[{"type":"input_text"|"output_text",
//	 "text":"…"}]}}
//	{"timestamp":"…","type":"response_item","payload":{"type":"reasoning",
//	 "summary":[{"type":"summary_text","text":"…"}]}}
//	{"timestamp":"…","type":"response_item","payload":{"type":"function_call",
//	 "name":"…","arguments":"{…}","call_id":"call_…"}}
//	{"timestamp":"…","type":"response_item","payload":{"type":
//	 "function_call_output","call_id":"call_…","output":"…"}}
//	other types (event_msg, turn_context, custom_tool_call, …) are harness
//	metadata and skipped.
//
// The file is only ever read.
package session

import (
	"encoding/json"
	"io"
	"path/filepath"
	"strings"

	"github.com/FreePeak/xdev/internal/ai"
)

// codexLine is the subset of a Codex rollout line xdev imports.
type codexLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexItem struct {
	Type    string `json:"type"` // message | reasoning | function_call | function_call_output
	Role    string `json:"role"` // message only
	Content []struct {
		Type string `json:"type"` // input_text | output_text
		Text string `json:"text"`
	} `json:"content"`
	Summary []struct {
		Text string `json:"text"`
	} `json:"summary"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	CallID    string `json:"call_id"`
	Output    string `json:"output"`
}

type codexMeta struct {
	ID  string `json:"id"`
	CWD string `json:"cwd"`
}

// ImportCodex imports a Codex rollout JSONL transcript as a NEW xdev session.
func ImportCodex(path, dataDir, cwd string) (*ImportResult, error) {
	return importForeign("codex", path, dataDir, cwd, scanCodex)
}

func scanCodex(r io.Reader) (importScan, error) {
	sc := importScan{}
	toolNames := map[string]string{} // call_id → tool name, for toolResult rows

	add := func(stamp string, msg ai.Message) {
		sc.turns = append(sc.turns, importTurn{stamp: readTimestamp(stamp), msg: msg})
	}

	eachLine(r, func(line []byte) bool {
		var cl codexLine
		if json.Unmarshal(line, &cl) != nil {
			sc.dropped++
			return true
		}
		if cl.Type != "response_item" {
			if cl.Type == "session_meta" {
				var m codexMeta
				if json.Unmarshal(cl.Payload, &m) == nil {
					sc.cwd = m.CWD
				}
			}
			return true // event_msg / turn_context / …: harness metadata
		}
		var it codexItem
		if json.Unmarshal(cl.Payload, &it) != nil {
			sc.dropped++
			return true
		}
		switch it.Type {
		case "message":
			var blocks []ai.Block
			for _, c := range it.Content {
				if c.Type == "input_text" || c.Type == "output_text" {
					blocks = append(blocks, ai.TextBlock{Text: c.Text})
				} else {
					sc.dropped++
				}
			}
			if len(blocks) == 0 {
				return true
			}
			// Codex's developer/system roles carry harness instructions,
			// not conversation: drop them. The user preambles
			// (<permissions instructions>, <environment_context>, …)
			// interleaved before the first real user message are skipped
			// too, so the import starts at the conversation proper.
			var role ai.Role
			switch it.Role {
			case "assistant":
				role = ai.RoleAssistant
			case "user":
				role = ai.RoleUser
			default:
				sc.dropped++
				return true
			}
			if role == ai.RoleUser && isCodexPreamble(blocks) {
				sc.dropped++
				return true
			}
			add(cl.Timestamp, ai.Message{Role: role, Content: blocks})
		case "reasoning":
			var b strings.Builder
			for _, s := range it.Summary {
				b.WriteString(s.Text)
			}
			if b.Len() > 0 {
				add(cl.Timestamp, ai.Message{Role: ai.RoleAssistant,
					Content: []ai.Block{ai.ThinkingBlock{Thinking: b.String()}}})
			}
		case "function_call":
			args := json.RawMessage(it.Arguments)
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			toolNames[it.CallID] = it.Name
			add(cl.Timestamp, ai.Message{Role: ai.RoleAssistant,
				Content: []ai.Block{ai.ToolCallBlock{ID: it.CallID, Name: it.Name, Arguments: args}}})
		case "function_call_output":
			add(cl.Timestamp, ai.Message{
				Role: ai.RoleToolResult, ToolCallID: it.CallID,
				ToolName: toolNames[it.CallID],
				Content:  []ai.Block{ai.TextBlock{Text: it.Output}},
			})
		default:
			sc.dropped++ // web_search_call, custom_tool_call, …
		}
		return true
	})
	return sc, nil
}

// isCodexPreamble recognizes the harness-injected user preambles
// (<permissions instructions>, <environment_context>, <user_instructions>,
// and the "# AGENTS.md instructions for <dir>" rule block).
func isCodexPreamble(blocks []ai.Block) bool {
	for _, b := range blocks {
		t, ok := b.(ai.TextBlock)
		if !ok {
			return false
		}
		if !strings.Contains(t.Text, "<permissions instructions>") &&
			!strings.Contains(t.Text, "<environment_context>") &&
			!strings.Contains(t.Text, "<user_instructions>") &&
			!strings.Contains(t.Text, "<AGENTS-FILE-CONTEXT>") &&
			!strings.Contains(t.Text, "AGENTS.md instructions") && // "# AGENTS.md instructions for <dir>"
			!strings.Contains(t.Text, "<codex_internal_context") && // harness continuation context
			!strings.Contains(t.Text, "<INSTRUCTIONS>") {
			return false
		}
	}
	return len(blocks) > 0
}
func foreignID(kind, base string) string {
	switch kind {
	case "claude":
		return strings.TrimSuffix(base, ".jsonl") // <session-uuid>.jsonl
	case "codex":
		// rollout-<stamp>-<thread-uuid>.jsonl → the trailing uuid
		stem := strings.TrimSuffix(base, ".jsonl")
		if len(stem) >= 36 {
			if tail := stem[len(stem)-36:]; isUUID(tail) {
				return tail
			}
		}
		return stem
	}
	return ""
}

// isUUID reports whether s has the 8-4-4-4-12 hex shape.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
			continue
		}
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

// foreignCWD extracts the recorded cwd from a transcript. Codex records it
// in the session_meta payload (bounded to the file head — cheap enough for
// listing). Claude Code encodes it in the PROJECT DIRECTORY name instead,
// and the "-"↔"/" encoding is ambiguous to reverse, so Claude transcripts
// carry the encoded directory in CWD; callers filter via
// ClaudeProjectSlug(cwd).
func foreignCWD(kind, path string) string {
	switch kind {
	case "claude":
		return filepath.Base(filepath.Dir(path)) // e.g. "-Users-x-work-proj"
	case "codex":
		for _, line := range decodeHead(path, 8192) {
			var cl codexLine
			if json.Unmarshal([]byte(strings.TrimSpace(line)), &cl) != nil || cl.Type != "session_meta" {
				continue
			}
			var m codexMeta
			if json.Unmarshal(cl.Payload, &m) == nil {
				return m.CWD
			}
		}
		return ""
	}
	return ""
}

// ClaudeProjectSlug mirrors Claude Code's project-dir encoding of a cwd:
// "/" and "." become "-". Decoding is ambiguous (a cwd may contain "-"),
// so callers match the encoded form exactly, never a decode.
func ClaudeProjectSlug(cwd string) string {
	return strings.NewReplacer("/", "-", ".", "-").Replace(cwd)
}

// CodexSessionsRoot returns home/.codex/sessions (the default transcript root).
func CodexSessionsRoot(home string) string {
	return filepath.Join(home, ".codex", "sessions")
}

// ClaudeProjectsRoot returns home/.claude/projects (the default root).
func ClaudeProjectsRoot(home string) string {
	return filepath.Join(home, ".claude", "projects")
}
