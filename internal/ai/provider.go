package ai

import "context"

// StreamRequest is one assistant-turn request to a provider.
type StreamRequest struct {
	// System is the system prompt (providers prepend it natively).
	System string
	// Messages is the conversation so far (context reconstruction output).
	Messages []Message
	// Tools are the tools the model may call.
	Tools []ToolDef
	// Model is the provider-specific model id.
	Model string
	// MaxTokens caps output; 0 means provider default.
	MaxTokens int
	// Thinking requests reasoning when non-nil (reasoning-capable models).
	Thinking *ThinkingBudget
}

// Provider streams one assistant turn onto a channel of unified events.
//
// Contract:
//   - Exactly one EventDone or one EventError terminates the stream; the
//     channel is closed immediately after.
//   - Events before the terminal event follow block lifecycle order:
//     start → (thinking_* | text_* | toolcall_*)... → done.
//   - The provider owns bounded retries for transient quirks; EventError is
//     terminal and non-retryable from the caller's perspective.
type Provider interface {
	// Stream initiates the request. It may return an error before any event
	// is produced (connection, auth, malformed request). The returned
	// channel must not be re-consumed after EventDone/EventError.
	Stream(ctx context.Context, req StreamRequest) (<-chan Event, error)
	// Name returns the provider key from models.yml (e.g. "onegw").
	Name() string
	// API returns the wire adapter name (e.g. "openai-completions").
	API() string
}

// APINames are the supported wire adapters.
const (
	APIAnthropicMessages  = "anthropic-messages"
	APIOpenAICompletions  = "openai-completions"
	APIOpenAIResponses    = "openai-responses"
	APIGoogleGenerativeAI = "google-generative-ai"
)
