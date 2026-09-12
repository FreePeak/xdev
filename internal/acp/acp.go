// Package acp is the agent side of the Agent Client Protocol (M14 #60):
// JSON-RPC 2.0 over stdio with LSP-style Content-Length framing, so an editor
// (Zed, or anything else speaking ACP) can drive an xdev session.
//
// The method set is the baseline every agent must support — initialize,
// session/new, session/prompt, session/cancel — plus session/update
// notifications streaming the turn (message chunks, thought chunks, tool
// calls) and session/request_permission for tool approvals. Framing and
// JSON-RPC are hand-rolled on bufio + encoding/json: internal/lsp frames the
// same way but keeps that code private to its subprocess client, and xdev
// carries no protocol dependency.
package acp

import (
	"encoding/json"
	"strings"
)

// ProtocolVersion is the ACP revision this server implements.
const ProtocolVersion = 1

// Method names. MethodNewSessionPre10 / MethodCancelShort are the pre-1.0
// spellings of session/new and session/cancel, still sent by older clients.
const (
	MethodInitialize        = "initialize"
	MethodNewSession        = "session/new"
	MethodNewSessionPre10   = "newSession"
	MethodPrompt            = "session/prompt"
	MethodCancel            = "session/cancel"
	MethodCancelShort       = "cancel"
	MethodUpdate            = "session/update"
	MethodRequestPermission = "session/request_permission"
)

// Stop reasons returned by session/prompt.
const (
	StopEndTurn         = "end_turn"
	StopMaxTokens       = "max_tokens"
	StopMaxTurnRequests = "max_turn_requests"
	StopRefusal         = "refusal"
	StopCancelled       = "cancelled"
)

// session/update discriminators.
const (
	UpdateAgentMessageChunk = "agent_message_chunk"
	UpdateAgentThoughtChunk = "agent_thought_chunk"
	UpdateToolCall          = "tool_call"
	UpdateToolCallUpdate    = "tool_call_update"
)

// Tool kinds: ACP's icon/UI hint for a call.
const (
	KindRead    = "read"
	KindEdit    = "edit"
	KindDelete  = "delete"
	KindMove    = "move"
	KindSearch  = "search"
	KindExecute = "execute"
	KindThink   = "think"
	KindFetch   = "fetch"
	KindOther   = "other"
)

// Tool call statuses.
const (
	StatusPending    = "pending"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
	StatusFailed     = "failed"
)

// Permission option ids + kinds, and the outcomes of a permission request.
const (
	OptionAllowOnce    = "allow_once"
	OptionAllowAlways  = "allow_always"
	OptionRejectOnce   = "reject_once"
	OptionRejectAlways = "reject_always"

	OutcomeSelected  = "selected"
	OutcomeCancelled = "cancelled"
)

// JSON-RPC 2.0 error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// ContentBlock is one prompt / update content block. Only text blocks are
// produced or consumed: the handshake advertises no image, audio or
// embedded-context support.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// Implementation identifies a client or agent (name/version metadata).
type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version,omitempty"`
}

// AgentCapabilities is what this agent supports. xdev has no session loading.
type AgentCapabilities struct {
	LoadSession        bool               `json:"loadSession"`
	PromptCapabilities PromptCapabilities `json:"promptCapabilities"`
}

// PromptCapabilities lists the optional prompt content the agent accepts.
type PromptCapabilities struct {
	Image           bool `json:"image"`
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
}

// InitializeParams is the client's initialize request.
type InitializeParams struct {
	ProtocolVersion    int             `json:"protocolVersion"`
	ClientCapabilities map[string]any  `json:"clientCapabilities,omitempty"`
	ClientInfo         *Implementation `json:"clientInfo,omitempty"`
}

// InitializeResult is the agent's initialize response.
type InitializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities `json:"agentCapabilities"`
	AgentInfo         Implementation    `json:"agentInfo"`
}

// NewSessionParams is the client's session/new request. MCPServers is parsed
// but unused: xdev attaches its own configured MCP servers.
type NewSessionParams struct {
	Cwd        string `json:"cwd"`
	MCPServers []any  `json:"mcpServers,omitempty"`
}

// NewSessionResult is the session/new response.
type NewSessionResult struct {
	SessionID string `json:"sessionId"`
}

// PromptParams is the client's session/prompt request.
type PromptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

// PromptResult is the session/prompt response. The request resolves only when
// the turn ends (or is cancelled), after every session/update went out.
type PromptResult struct {
	StopReason string `json:"stopReason"`
}

// CancelParams is the session/cancel notification.
type CancelParams struct {
	SessionID string `json:"sessionId"`
}

// Update is one session/update payload: the discriminator plus the fields of
// whichever variant it names. Content is raw because chunks carry a single
// content block while tool calls carry an array of them.
type Update struct {
	SessionUpdate string          `json:"sessionUpdate"`
	Content       json.RawMessage `json:"content,omitempty"`
	ToolCallID    string          `json:"toolCallId,omitempty"`
	Title         string          `json:"title,omitempty"`
	Kind          string          `json:"kind,omitempty"`
	Status        string          `json:"status,omitempty"`
	RawInput      json.RawMessage `json:"rawInput,omitempty"`
	RawOutput     json.RawMessage `json:"rawOutput,omitempty"`
}

// PermissionToolCall identifies the call a permission request is about (ACP's
// ToolCallUpdate: id plus the fields being reported).
type PermissionToolCall struct {
	ToolCallID string          `json:"toolCallId"`
	Title      string          `json:"title,omitempty"`
	Kind       string          `json:"kind,omitempty"`
	Status     string          `json:"status,omitempty"`
	RawInput   json.RawMessage `json:"rawInput,omitempty"`
}

// PermissionRequest is the agent's session/request_permission request.
type PermissionRequest struct {
	SessionID string             `json:"sessionId"`
	ToolCall  PermissionToolCall `json:"toolCall"`
	Options   []PermissionOption `json:"options"`
}

// PermissionOption is one choice offered to the user.
type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// PermissionOutcome is the client's decision. The zero value means the request
// was cancelled before the user answered and must be treated as a refusal.
type PermissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

// Allowed reports whether the client picked one of the allow options.
func (o PermissionOutcome) Allowed() bool {
	return o.Outcome == OutcomeSelected && (o.OptionID == OptionAllowOnce || o.OptionID == OptionAllowAlways)
}

// ApprovalOptions is the option set offered for one tool call.
func ApprovalOptions() []PermissionOption {
	return []PermissionOption{
		{OptionID: OptionAllowOnce, Name: "Allow once", Kind: OptionAllowOnce},
		{OptionID: OptionAllowAlways, Name: "Allow always", Kind: OptionAllowAlways},
		{OptionID: OptionRejectOnce, Name: "Reject", Kind: OptionRejectOnce},
	}
}

// Chunk builds an agent_message_chunk / agent_thought_chunk update.
func Chunk(sessionUpdate, text string) Update {
	content, _ := json.Marshal(ContentBlock{Type: "text", Text: text})
	return Update{SessionUpdate: sessionUpdate, Content: content}
}

// ToolCallStarted builds the tool_call update announcing a call.
func ToolCallStarted(id, title, kind string, rawInput json.RawMessage) Update {
	if !json.Valid(rawInput) {
		rawInput = nil
	}
	return Update{
		SessionUpdate: UpdateToolCall,
		ToolCallID:    id,
		Title:         title,
		Kind:          kind,
		Status:        StatusInProgress,
		RawInput:      rawInput,
	}
}

// ToolCallUpdated builds the tool_call_update carrying a call's outcome. An
// empty text leaves the content array off (status-only update).
func ToolCallUpdated(id, status, text string) Update {
	u := Update{SessionUpdate: UpdateToolCallUpdate, ToolCallID: id, Status: status}
	if text != "" {
		content, _ := json.Marshal([]toolCallContent{{Type: "content", Content: ContentBlock{Type: "text", Text: text}}})
		u.Content = content
	}
	return u
}

// toolCallContent is one entry of a tool call's content array: ACP wraps
// content blocks in {"type":"content","content":…} inside that array.
type toolCallContent struct {
	Type    string       `json:"type"`
	Content ContentBlock `json:"content"`
}

// ToolKind maps an xdev tool name onto ACP's tool kind vocabulary.
func ToolKind(name string) string {
	switch name {
	case "read", "snapshot", "context_notes", "memory":
		return KindRead
	case "write", "edit", "multiedit":
		return KindEdit
	case "bash", "bash_bg", "eval":
		return KindExecute
	case "grep", "glob", "ast_grep", "query_file":
		return KindSearch
	case "web_search", "web_fetch", "github":
		return KindFetch
	case "task", "todo", "ask":
		return KindThink
	default:
		return KindOther
	}
}

// PromptText joins the text blocks of an ACP prompt.
func PromptText(blocks []ContentBlock) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}
