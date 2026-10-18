package opencode

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// This is the whole of R1: opencode content becomes xdev's existing blocks and
// entries, so blocks.go renders it unchanged. No second renderer, no parallel
// transcript model.
//
// The mapping is asymmetric, which is the one thing worth knowing before
// reading the code:
//
//   - assistant messages carry a `content` union of text | reasoning | tool,
//     and `{"type":"text","text":…}` is the same wire shape xdev's TextBlock
//     already uses. Text and reasoning are a field rename each.
//   - a user message carries a flat `text` string, not a content list, so it
//     folds into exactly one TextBlock.
//
// opencode v2 replaced v1's flat parts array with a type-discriminated message
// union; this file is written against the v2 shape (docs/research/opencode-v2-spec.md).

// opencode message header, discriminated by `type`.
type ocMessage struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Time struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Agent string   `json:"agent"`
	Text  string   `json:"text"`
	Model ModelRef `json:"model"`
	State *ocState `json:"state"`
}

// ocState is opencode's tool state union, discriminated by `status`.
type ocState struct {
	Status  string          `json:"status"`
	Input   json.RawMessage `json:"input"`
	Content []ocToolContent `json:"content"`
	Error   json.RawMessage `json:"error"`
	Meta    map[string]any  `json:"metadata"`
}

// ocToolContent is Tool.Content: text or file.
type ocToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
	MIME string `json:"mime"`
	URL  string `json:"url"`
}

// ocContent is one item of an assistant message's content union.
type ocContent struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	State json.RawMessage `json:"state"`
}

// AdaptMessages folds an opencode message list into xdev session entries, in
// order. A message that cannot be understood becomes a CustomEntry rather
// than disappearing (F3).
func AdaptMessages(raws []string) ([]session.Entry, error) {
	var out []session.Entry
	for i, raw := range raws {
		entries, err := AdaptMessage(raw)
		if err != nil {
			// One bad message must not cost the whole history: keep it
			// verbatim and carry on.
			out = append(out, customEntry("opencode_message_undecodable", map[string]any{
				"index": i,
				"raw":   raw,
				"error": err.Error(),
			}))
			continue
		}
		out = append(out, entries...)
	}
	return out, nil
}

// AdaptMessage folds one opencode message into xdev entries. An assistant
// message with tool calls yields two entries per call — the call and then its
// result — because that is how xdev's store already models a tool turn.
func AdaptMessage(raw string) ([]session.Entry, error) {
	var m ocMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("opencode: message: %w", err)
	}

	switch m.Type {
	case "user":
		// v2 user text is a plain string, not a content list.
		return []session.Entry{
			messageEntry(m, ai.RoleUser, []ai.Block{ai.TextBlock{Text: m.Text}}),
		}, nil

	case "assistant":
		return adaptAssistant(m, raw)

	case "compaction":
		// xdev already models this; do not invent a parallel shape.
		return []session.Entry{&session.CompactionEntry{
			Env: session.Envelope{
				Type:      session.TypeCompaction,
				ID:        m.ID,
				Timestamp: msTime(m.Time.Created),
			},
		}}, nil

	default:
		// Unknown message type. opencode adds these over time, so this is
		// an expected path, not a bug (F3).
		return []session.Entry{customEntry("opencode_message", map[string]any{
			"opencodeType": m.Type,
			"raw":          raw,
		})}, nil
	}
}

func adaptAssistant(m ocMessage, raw string) ([]session.Entry, error) {
	var withContent struct {
		Content []ocContent `json:"content"`
	}
	if err := json.Unmarshal([]byte(raw), &withContent); err != nil {
		return nil, fmt.Errorf("opencode: assistant content: %w", err)
	}
	items := withContent.Content

	var (
		blocks  []ai.Block
		results []session.Entry
	)
	for _, it := range items {
		switch it.Type {
		case "text":
			blocks = append(blocks, ai.TextBlock{Text: it.Text})

		case "reasoning":
			// Same payload, different field name.
			blocks = append(blocks, ai.ThinkingBlock{Thinking: it.Text})

		case "tool":
			call, result, ok := adaptTool(it)
			if !ok {
				results = append(results, customEntry("opencode_part", map[string]any{
					"raw":      string(it.State),
					"toolName": it.Name,
					"note":     "tool state could not be read",
				}))
				continue
			}
			blocks = append(blocks, call)
			// A tool that has not finished has no result to report yet.
			if result != nil {
				results = append(results, result)
			}

		default:
			// F3: keep the part, do not invent a block for it and do not
			// flatten it into text opencode never wrote.
			results = append(results, customEntry("opencode_part", map[string]any{
				"opencodeType": it.Type,
				"raw":          string(it.State),
			}))
		}
	}

	// An assistant message whose every part was unrecognised contributes
	// nothing to the transcript — the custom entries below already carry
	// it. Emitting an empty assistant message here would render a blank
	// turn that never happened.
	if len(blocks) == 0 {
		return results, nil
	}
	out := []session.Entry{messageEntry(m, ai.RoleAssistant, blocks)}
	return append(out, results...), nil
}

// adaptTool turns one tool content item into an xdev ToolCallBlock and,
// when the call has finished, a tool-result entry.
//
// The streaming split matters: opencode's streaming input is a partial JSON
// *string*, and putting it in Arguments would hand a model half an object
// that looks complete. It goes to PartialArgs, which xdev's block already
// exists for.
func adaptTool(it ocContent) (ai.ToolCallBlock, session.Entry, bool) {
	call := ai.ToolCallBlock{ID: it.ID, Name: it.Name}
	var st ocState
	if len(it.State) == 0 || json.Unmarshal(it.State, &st) != nil {
		return call, nil, false
	}
	switch st.Status {
	case "streaming":
		// A partial JSON string, not an object.
		var partial string
		if json.Unmarshal(st.Input, &partial) == nil {
			call.PartialArgs = partial
		}
		return call, nil, true
	case "running", "completed", "error":
		call.Arguments = st.Input
	default:
		return call, nil, false
	}

	if st.Status == "running" {
		return call, nil, true
	}

	text := toolContentText(st.Content)
	msg := &session.MessageEntry{
		Env: session.Envelope{
			Type:      session.TypeMessage,
			ID:        it.ID + ":result",
			Timestamp: msTime(0),
		},
		Message: ai.Message{
			Role:    ai.RoleToolResult,
			Content: []ai.Block{ai.TextBlock{Text: text}},
		},
	}
	if st.Status == "error" {
		msg.Message.StopReason = ai.StopReasonError
		if len(st.Error) > 0 {
			msg.Message.Content = append(msg.Message.Content, ai.TextBlock{Text: string(st.Error)})
		}
	}
	return call, msg, true
}

// toolContentText flattens Tool.Content into displayable text. File content
// becomes its path or URL rather than its bytes: a transcript that inlined an
// image would be unreadable and enormous.
func toolContentText(items []ocToolContent) string {
	var out string
	for _, c := range items {
		switch c.Type {
		case "text":
			out += c.Text
		case "file":
			if c.URL != "" {
				out += c.URL
			}
		}
	}
	return out
}

// --- entry construction ----------------------------------------------------

func messageEntry(m ocMessage, role ai.Role, blocks []ai.Block) session.Entry {
	return &session.MessageEntry{
		Env: session.Envelope{
			Type:      session.TypeMessage,
			ID:        m.ID,
			Timestamp: msTime(m.Time.Created),
		},
		Message: ai.Message{
			Role:    role,
			Content: blocks,
		},
	}
}

func customEntry(kind string, data map[string]any) session.Entry {
	return &session.CustomEntry{
		Env:        session.Envelope{Type: session.TypeCustom},
		CustomType: kind,
		Data:       data,
	}
}

// msTime converts opencode's ms epoch to a time. A zero stays the zero time
// rather than becoming 1970, which would sort ahead of every real entry.
func msTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// adaptMessage is the single-entry convenience used by the tests and by any
// caller that wants the message without its tool results.
func adaptMessage(raw string) (*ai.Message, error) {
	entries, err := AdaptMessage(raw)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if me, ok := e.(*session.MessageEntry); ok && me.Message.Role != ai.RoleToolResult {
			return &me.Message, nil
		}
	}
	return nil, fmt.Errorf("opencode: no message entry in %s", raw)
}
