// Package protocol defines the RPC wire contract (M6 #7): JSONL frames
// over stdio, one JSON object per line, correlated by id. v1 keeps the
// frame set minimal — ready, prompt/steer/follow_up/abort/new_session/
// state/set_model in, responses + stream events out. Dialogs and host-tool
// calls ride the same envelope in v2.
package protocol

import (
	"encoding/json"
	"fmt"

	"github.com/FreePeak/xdev/internal/ai"
)

// ProtocolVersion is the v1 wire version advertised in the ready frame.
const ProtocolVersion = 1

// FrameLimit caps one JSONL frame in bytes (v1; v2 adds chunked
// reassembly up to 64 MiB).
const FrameLimit = 1 << 20 // 1 MiB

// Frame type names.
const (
	TypeReady      = "ready"
	TypePrompt     = "prompt"
	TypeSteer      = "steer"
	TypeFollowUp   = "follow_up"
	TypeAbort      = "abort"
	TypeNewSession = "new_session"
	TypeState      = "state"
	TypeSetModel   = "set_model"
	TypeResponse   = "response"
	TypeEvent      = "event"
)

// Ready advertises the protocol version + frame limit. First frame on
// stream up, always.
type Ready struct {
	Protocol int `json:"protocol"`
	// FrameLimit in bytes.
	FrameLimit int `json:"frameLimit"`
}

// State is the agent snapshot returned by the state command.
type State struct {
	SessionID string `json:"sessionId"`
	Title     string `json:"title,omitempty"`
	Model     string `json:"model,omitempty"`
	CWD       string `json:"cwd,omitempty"`
	Running   bool   `json:"running"`
}

// Response answers one command by id. Ok=false carries Err.
type Response struct {
	Ok         bool   `json:"ok"`
	Err        string `json:"error,omitempty"`
	SessionID  string `json:"sessionId,omitempty"`
	Model      string `json:"model,omitempty"`
	State      *State `json:"state,omitempty"`
	Text       string `json:"text,omitempty"`
	StopReason string `json:"stopReason,omitempty"`
}

// Event is the wire form of one stream event. ai.Event is deliberately
// not serialized directly (no JSON tags; Err is not a string).
type Event struct {
	Type string `json:"type"`

	Provider string `json:"provider,omitempty"`
	API      string `json:"api,omitempty"`
	Model    string `json:"model,omitempty"`

	Delta    string `json:"delta,omitempty"`
	Snapshot string `json:"snapshot,omitempty"`

	ToolCallID  string          `json:"toolCallId,omitempty"`
	ToolName    string          `json:"toolName,omitempty"`
	StreamIndex int             `json:"streamIndex,omitempty"`
	PartialJSON string          `json:"partialJson,omitempty"`
	Arguments   json.RawMessage `json:"arguments,omitempty"`

	StopReason string      `json:"stopReason,omitempty"`
	Message    *ai.Message `json:"message,omitempty"`
	MessageErr string      `json:"error,omitempty"`
}

// EventFromAI maps a stream event onto the wire form.
func EventFromAI(ev ai.Event) Event {
	out := Event{Type: string(ev.Type)}
	switch ev.Type {
	case ai.EventStart:
		out.Provider, out.API, out.Model = ev.Provider, ev.API, ev.Model
	case ai.EventTextDelta, ai.EventThinkingDelta, ai.EventTextStart, ai.EventThinkingStart:
		out.Delta, out.Snapshot = ev.Delta, ev.Snapshot
	case ai.EventToolcallStart:
		out.ToolCallID, out.ToolName, out.StreamIndex = ev.ToolCallID, ev.ToolName, ev.StreamIndex
	case ai.EventToolcallDelta:
		out.ToolCallID, out.StreamIndex, out.PartialJSON = ev.ToolCallID, ev.StreamIndex, ev.PartialJSON
	case ai.EventToolcallEnd:
		out.ToolCallID, out.StreamIndex, out.PartialJSON = ev.ToolCallID, ev.StreamIndex, ev.PartialJSON
	case ai.EventDone:
		out.StopReason = string(ev.StopReason)
		out.Message = ev.Message
	case ai.EventError:
		out.MessageErr = fmt.Sprintf("%v", ev.Err)
	}
	return out
}
