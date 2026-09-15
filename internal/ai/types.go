// Package ai defines the provider-independent message model, the unified
// AssistantMessageEvent stream contract, and the Provider interface.
//
// The JSON shapes mirror omp's session format (verified against real
// omp-generated session files) so sessions stay interoperable.
package ai

import (
	"encoding/json"
	"fmt"
)

// Role is a message role. omp persists three roles: user, assistant, toolResult.
type Role string

const (
	RoleUser       Role = "user"
	RoleAssistant  Role = "assistant"
	RoleToolResult Role = "toolResult"
)

// StopReason is the unified terminal reason for an assistant turn.
type StopReason string

const (
	StopReasonStop    StopReason = "stop"    // model ended its turn normally
	StopReasonLength  StopReason = "length"  // output token limit hit
	StopReasonAborted StopReason = "aborted" // user or watchdog aborted
	StopReasonError   StopReason = "error"   // terminal provider error
)

// Block is a content block inside a Message. Only these block types exist:
// text, thinking, toolCall (assistant side), image (either side).
// Use ParseBlock to decode a JSON object with a "type" discriminator.
type Block interface {
	blockType() string
}

// TextBlock is literal text. Wire shape: {"type":"text","text":"..."}.
type TextBlock struct {
	Text string `json:"text"`
}

// ThinkingBlock is a reasoning block. Wire shape:
// {"type":"thinking","thinking":"...","thinkingSignature":"reasoning_content"}.
type ThinkingBlock struct {
	Thinking          string `json:"thinking"`
	ThinkingSignature string `json:"thinkingSignature,omitempty"`
}

// ToolCallBlock is the assistant requesting a tool invocation. Wire shape:
// {"type":"toolCall","id":"...","name":"...","arguments":{...},
//
//	"partialArgs":"{...}","streamIndex":0,"intent":"..."}.
//
// Arguments is the parsed JSON object; PartialArgs is the raw accumulated
// partial-JSON string kept for streaming transparency.
type ToolCallBlock struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Arguments   json.RawMessage `json:"arguments,omitempty"`
	PartialArgs string          `json:"partialArgs,omitempty"`
	StreamIndex int             `json:"streamIndex,omitempty"`
	Intent      string          `json:"intent,omitempty"`
	// Signature carries a provider reasoning signature tied to this call
	// (Gemini's thoughtSignature). It persists in the session so a rebuilt
	// follow-up request can echo it back — Google rejects thinking-enabled
	// function calls whose signature is not replayed.
	Signature json.RawMessage `json:"signature,omitempty"`
}

// ImageSource is an image reference: either a data URL or a blob reference.
type ImageSource struct {
	Type      string `json:"type"` // "base64" or "url" or "blob"
	MediaType string `json:"mediaType,omitempty"`
	Data      string `json:"data,omitempty"` // base64 or URL or blob:<sha256>
}

// ImageBlock is an image attachment. Wire shape:
// {"type":"image","source":{...}}.
type ImageBlock struct {
	Source ImageSource `json:"source"`
}

func (TextBlock) blockType() string     { return "text" }
func (ThinkingBlock) blockType() string { return "thinking" }
func (ToolCallBlock) blockType() string { return "toolCall" }
func (ImageBlock) blockType() string    { return "image" }

// MarshalBlock serializes b with its "type" discriminator prepended.
func MarshalBlock(b Block) ([]byte, error) {
	switch t := b.(type) {
	case TextBlock:
		return json.Marshal(struct {
			Type string `json:"type"`
			TextBlock
		}{"text", t})
	case ThinkingBlock:
		return json.Marshal(struct {
			Type string `json:"type"`
			ThinkingBlock
		}{"thinking", t})
	case ToolCallBlock:
		return json.Marshal(struct {
			Type string `json:"type"`
			ToolCallBlock
		}{"toolCall", t})
	case ImageBlock:
		return json.Marshal(struct {
			Type string `json:"type"`
			ImageBlock
		}{"image", t})
	default:
		return nil, fmt.Errorf("ai: unknown block type %T", b)
	}
}

// ParseBlock decodes one JSON object with a "type" field into a Block.
func ParseBlock(data []byte) (Block, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("ai: block missing type: %w", err)
	}
	switch probe.Type {
	case "text":
		var b struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(data, &b); err != nil {
			return nil, err
		}
		return TextBlock{Text: b.Text}, nil
	case "thinking":
		var b struct {
			Thinking          string `json:"thinking"`
			ThinkingSignature string `json:"thinkingSignature,omitempty"`
		}
		if err := json.Unmarshal(data, &b); err != nil {
			return nil, err
		}
		return ThinkingBlock{Thinking: b.Thinking, ThinkingSignature: b.ThinkingSignature}, nil
	case "toolCall":
		var b ToolCallBlock
		if err := json.Unmarshal(data, &b); err != nil {
			return nil, err
		}
		return b, nil
	case "image":
		var b struct {
			Source ImageSource `json:"source"`
		}
		if err := json.Unmarshal(data, &b); err != nil {
			return nil, err
		}
		return ImageBlock{Source: b.Source}, nil
	default:
		return nil, fmt.Errorf("ai: unknown block type %q", probe.Type)
	}
}

// Usage is token accounting. Cost values are USD.
type Usage struct {
	Input           int64      `json:"input"`
	Output          int64      `json:"output"`
	CacheRead       int64      `json:"cacheRead,omitempty"`
	CacheWrite      int64      `json:"cacheWrite,omitempty"`
	TotalTokens     int64      `json:"totalTokens"`
	ReasoningTokens int64      `json:"reasoningTokens,omitempty"`
	Cost            *UsageCost `json:"cost,omitempty"`
}

// UsageCost is per-surface cost breakdown.
type UsageCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead,omitempty"`
	CacheWrite float64 `json:"cacheWrite,omitempty"`
	Total      float64 `json:"total"`
}

// Message is the unified message model persisted in session entries
// under key "message".
type Message struct {
	Role    Role    `json:"role"`
	Content []Block `json:"content"`

	// user
	Attribution string `json:"attribution,omitempty"` // "user" for typed input
	UserTS      int64  `json:"timestamp,omitempty"`   // ms epoch (user messages)

	// assistant
	Provider    string     `json:"provider,omitempty"`
	API         string     `json:"api,omitempty"`
	Model       string     `json:"model,omitempty"`
	ResponseID  string     `json:"responseId,omitempty"`
	StopReason  StopReason `json:"stopReason,omitempty"`
	Usage       *Usage     `json:"usage,omitempty"`
	CompletedAt string     `json:"completedAt,omitempty"` // RFC3339 UTC
	DurationMS  int64      `json:"duration,omitempty"`
	TTFTMS      int64      `json:"ttft,omitempty"`

	// toolResult
	ToolCallID string `json:"toolCallId,omitempty"`
	ToolName   string `json:"toolName,omitempty"`
	IsError    bool   `json:"isError,omitempty"`
	Details    any    `json:"details,omitempty"` // tool-specific structured metadata
}

// MarshalJSON serializes Content blocks with their "type" discriminators.
func (m Message) MarshalJSON() ([]byte, error) {
	type alias Message
	raws := make([]json.RawMessage, 0, len(m.Content))
	for _, b := range m.Content {
		r, err := MarshalBlock(b)
		if err != nil {
			return nil, err
		}
		raws = append(raws, r)
	}
	return json.Marshal(struct {
		alias
		Content []json.RawMessage `json:"content"`
	}{alias(m), raws})
}

// UnmarshalJSON decodes Content via ParseBlock. Unknown block types fail
// loudly rather than silently dropping content.
func (m *Message) UnmarshalJSON(data []byte) error {
	type alias Message
	var aux struct {
		alias
		Content []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*m = Message(aux.alias)
	m.Content = make([]Block, 0, len(aux.Content))
	for _, r := range aux.Content {
		b, err := ParseBlock(r)
		if err != nil {
			return err
		}
		m.Content = append(m.Content, b)
	}
	return nil
}

// Text returns the concatenated text of all text blocks (streaming-friendly).
func (m *Message) Text() string {
	out := ""
	for _, b := range m.Content {
		if t, ok := b.(TextBlock); ok {
			out += t.Text
		}
	}
	return out
}

// ToolCalls returns all toolCall blocks.
//
// It is also the one guard every reader of a stored call passes through (the
// native wire encoders, the in-band dialects, ACP, tool execution): arguments
// that are not valid JSON — an imported or pre-fix log, a hand-edited file, a
// dialect decoder that could not parse its body — are presented as {} so a
// request is never poisoned and a tool never runs on arguments nobody wrote.
// The model's bytes survive in PartialArgs, which callers still fall back to.
// An empty blob is left alone: it means the arguments are only in PartialArgs.
func (m *Message) ToolCalls() []ToolCallBlock {
	var out []ToolCallBlock
	for _, b := range m.Content {
		t, ok := b.(ToolCallBlock)
		if !ok {
			continue
		}
		if len(t.Arguments) > 0 && !json.Valid(t.Arguments) {
			t.Arguments = parseToolArgs(m.API, t.ID, string(t.Arguments))
		}
		out = append(out, t)
	}
	return out
}

// ToolDef is a tool advertised to the model (JSON-schema parameters).
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ThinkingBudget requests reasoning from the model when non-nil.
type ThinkingBudget struct {
	Tokens int
}
