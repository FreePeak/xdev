// Claude Code transcript parser (issue #28). Source layout:
// ~/.claude/projects/<encoded-cwd>/<session-uuid>.jsonl, one JSON object
// per line. Shape verified against real 2026-09 transcripts:
//
//	{"type":"user","message":{"role":"user","content":"…"|[blocks]},
//	 "timestamp":"…","parentUuid":"…","isSidechain":false}
//	{"type":"assistant","message":{"role":"assistant",
//	 "content":[{"type":"text"|"thinking"|"tool_use",…}],"model":"…"},
//	 "timestamp":"…"}
//	{"type":"ai-title","aiTitle":"…"}                (generated title)
//	other types (attachment, mode, system, file-history-snapshot, …) are
//	harness metadata and skipped.
//
// user content arrays carry tool_result blocks paired with the preceding
// assistant tool_use. isSidechain=true lines are subagent side-branches and
// are skipped. The file is only ever read.
package session

import (
	"encoding/json"
	"io"
	"strings"

	"github.com/FreePeak/xdev/internal/ai"
)

// claudeLine is the subset of a Claude Code transcript line xdev imports.
type claudeLine struct {
	Type        string `json:"type"`
	Timestamp   string `json:"timestamp"`
	IsSidechain bool   `json:"isSidechain"`
	Message     *struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	AITitle string `json:"aiTitle"`
}

// claudeBlock is one content block of a Claude message.
type claudeBlock struct {
	Type      string          `json:"type"` // text | thinking | tool_use | tool_result
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`   // tool_use id
	Name      string          `json:"name"` // tool_use name
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"` // tool_result payload: string | blocks
	IsError   bool            `json:"is_error"`
}

// ImportClaude imports a Claude Code JSONL transcript as a NEW xdev session.
func ImportClaude(path, dataDir, cwd string) (*ImportResult, error) {
	return importForeign("claude", path, dataDir, cwd, scanClaude)
}

func scanClaude(r io.Reader) (importScan, error) {
	sc := importScan{}
	toolNames := map[string]string{} // tool_use id → name, for toolResult rows

	eachLine(r, func(line []byte) bool {
		var cl claudeLine
		if json.Unmarshal(line, &cl) != nil {
			sc.dropped++
			return true
		}
		if cl.Type == "ai-title" && cl.AITitle != "" {
			sc.title = cl.AITitle // last write wins: titles are regenerated
			return true
		}
		if cl.Message == nil || (cl.Type != "user" && cl.Type != "assistant") {
			if cl.Type == "user" || cl.Type == "assistant" {
				sc.dropped++ // conversation line with no message payload
			}
			return true
		}
		if cl.IsSidechain {
			sc.dropped++ // subagent side-branch, not part of the main thread
			return true
		}
		stamp := readTimestamp(cl.Timestamp)
		blocks := decodeClaudeBlocks(cl.Message.Content)

		if cl.Type == "assistant" {
			msg := ai.Message{Role: ai.RoleAssistant, Model: assistantModel(line)}
			for _, b := range blocks {
				switch b.Type {
				case "text":
					msg.Content = append(msg.Content, ai.TextBlock{Text: b.Text})
				case "thinking":
					msg.Content = append(msg.Content, ai.ThinkingBlock{Thinking: b.Thinking})
				case "tool_use":
					if len(b.Input) == 0 {
						b.Input = json.RawMessage("{}")
					}
					toolNames[b.ID] = b.Name
					msg.Content = append(msg.Content, ai.ToolCallBlock{ID: b.ID, Name: b.Name, Arguments: b.Input})
				default:
					sc.dropped++ // image and other opaque blocks
				}
			}
			if len(msg.Content) > 0 {
				sc.turns = append(sc.turns, importTurn{stamp: stamp, msg: msg})
			}
			return true
		}

		// user: plain text blocks append to the thread; tool_result blocks
		// become one toolResult message each (ai.Message has one tool id).
		var userText []ai.TextBlock
		for _, b := range blocks {
			switch b.Type {
			case "text":
				userText = append(userText, ai.TextBlock{Text: b.Text})
			case "tool_result":
				msg := ai.Message{
					Role:       ai.RoleToolResult,
					ToolCallID: b.ToolUseID,
					ToolName:   toolNames[b.ToolUseID],
					IsError:    b.IsError,
					Content:    []ai.Block{ai.TextBlock{Text: decodeClaudeToolResult(b.Content)}},
				}
				sc.turns = append(sc.turns, importTurn{stamp: stamp, msg: msg})
			default:
				sc.dropped++
			}
		}
		if len(userText) > 0 {
			sc.turns = append(sc.turns, importTurn{stamp: stamp, msg: ai.Message{
				Role: ai.RoleUser, Content: textBlocks(userText),
			}})
		}
		return true
	})
	return sc, nil
}

// eachLine feeds complete JSONL lines to fn until EOF (fn returning false
// stops the scan).
func eachLine(r io.Reader, fn func(line []byte) bool) {
	data, _ := io.ReadAll(r)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !fn([]byte(line)) {
			return
		}
	}
}

func decodeClaudeBlocks(raw json.RawMessage) []claudeBlock {
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '"' { // plain string content
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil
		}
		return []claudeBlock{{Type: "text", Text: s}}
	}
	var blocks []claudeBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	return blocks
}

// decodeClaudeToolResult renders a tool_result payload as readable text:
// the payload is either a string or an array of {type:"text"} blocks.
func decodeClaudeToolResult(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		return ""
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range blocks {
		b.WriteString(blk.Text)
	}
	return b.String()
}

// assistantModel extracts message.model without re-decoding the line.
func assistantModel(line []byte) string {
	var probe struct {
		Message struct {
			Model string `json:"model"`
		} `json:"message"`
	}
	_ = json.Unmarshal(line, &probe)
	return probe.Message.Model
}

func textBlocks(ts []ai.TextBlock) []ai.Block {
	out := make([]ai.Block, len(ts))
	for i, t := range ts {
		out[i] = t
	}
	return out
}
