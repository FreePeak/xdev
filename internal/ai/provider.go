package ai

import (
	"context"
	"encoding/json"
	"strings"
)

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
	// Cache opts this request into prompt caching. Zero value = no caching:
	// a provider behaves exactly as it did before the field existed.
	Cache CacheOpts
}

// CacheOpts carries the prompt-cache identity of one request. Providers with no
// cache-control vocabulary on the wire (Bedrock, gRPC, local servers) ignore all
// of it, so callers never have to care which adapter they hit.
type CacheOpts struct {
	// Key routes cache affinity for providers with a request-level key field —
	// OpenAI's `prompt_cache_key`. It is the stable session id, NOT the
	// per-request id: a side request that shares the main chain's prefix
	// should read from the same entry (omp pi-ai forwards
	// promptCacheKey ?? sessionId). Empty = send no key.
	Key string
	// SideRequest marks a request that is NOT the main chain — a compaction or
	// handoff summarize call, a one-shot CLI ask. Its markers stop at the last
	// completed tool round instead of the newest message, so a throwaway
	// request never writes a cache entry nothing later reads.
	SideRequest bool
}

// cacheWanted reports whether one request opts into cache markers: a caller
// that set no identity is asking for the pre-cache request shape, byte for
// byte. Providers with no marker vocabulary ignore the field entirely.
func (c CacheOpts) cacheWanted() bool {
	return c.Key != "" && cacheEnabled()
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
	APIAnthropicMessages    = "anthropic-messages"
	APIOpenAICompletions    = "openai-completions"
	APIOpenAIResponses      = "openai-responses"
	APIGoogleGenerativeAI   = "google-generative-ai"
	APIAzureOpenAIResponses = "azure-openai-responses"
	APIOpenAICodexResponses = "openai-codex-responses"
	APIGoogleVertex         = "google-vertex"
	APIGeminiCLI            = "gemini-cli"
)

// SupportedAPIs is the wire-adapter catalog in declaration order; config
// validation and error messages read it so adding an adapter updates both.
func SupportedAPIs() []string {
	return []string{
		APIAnthropicMessages, APIOpenAICompletions, APIOpenAIResponses,
		APIAzureOpenAIResponses, APIOpenAICodexResponses,
		APIGoogleGenerativeAI, APIGoogleVertex, APIGeminiCLI,
	}
}

// Identity is the stable per-install identifier providers attach to requests
// (Claude's metadata.user_id device blob, Codex's installation id). The ai
// package cannot import internal/config — config imports ai, so a cycle — and
// minting an id in the wire layer would defeat the purpose: it must be the
// SAME value the credential store keys on. main() installs it once at startup
// (#102: install-id minted a file that no request ever attached).
var identityUserID string

// SetInstallIdentity records the per-install identifier for request metadata.
// "" leaves every request exactly as it was, so an unset identity can never
// send an empty device blob.
func SetInstallIdentity(id string) { identityUserID = strings.TrimSpace(id) }

// InstallID returns the attached identifier ("" when unset).
func InstallID() string { return identityUserID }

// installIdentity renders the Claude-side metadata.user_id value: omp sends a
// JSON object rather than a bare id, so the shape is kept.
func installIdentity() string {
	if identityUserID == "" {
		return ""
	}
	b, err := json.Marshal(map[string]string{"device_id": identityUserID})
	if err != nil { // unreachable: one string field
		return ""
	}
	return string(b)
}
