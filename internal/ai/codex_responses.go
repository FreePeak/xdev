package ai

import (
	"context"
	"net/http"
	"strings"
)

// OpenAICodexResponsesProvider speaks openai-codex-responses, the ChatGPT
// Codex backend's Responses variant: POST {baseURL}/responses with
// `store: false` (Codex keeps no server-side conversation state, so history is
// replayed every turn) and an `instructions` block that is always non-empty —
// the request's system prompt, or CodexDefaultInstructions when the caller has
// none. Tool schemas run the Responses sanitizer before strict-mode adaptation.
type OpenAICodexResponsesProvider struct {
	*OpenAIResponsesProvider
}

// CodexDefaultInstructions stands in when a Codex request carries no system
// prompt: the backend rejects an empty instructions block.
const CodexDefaultInstructions = "You are a coding agent. Follow the user's instructions and use the provided tools when they are needed."

// NewOpenAICodexResponsesProvider builds a Codex Responses provider. A nil hc
// uses the shared transport.
func NewOpenAICodexResponsesProvider(name, baseURL, apiKey string, headers map[string]string, hc *http.Client) *OpenAICodexResponsesProvider {
	inner := NewOpenAIResponsesProvider(name, baseURL, apiKey, headers, hc)
	inner.apiLabel = APIOpenAICodexResponses
	// Codex carries the per-install identity on the wire (#102): omp sends
	// installation_id in the request metadata; the Responses API's documented
	// home for an opaque external id is `user`.
	inner.behavior = responsesBehavior{sanitize: true, strictTools: true, attachInstallID: true}
	storeDisabled := false // Codex is stateless; the flag must be present and false
	inner.store = &storeDisabled
	return &OpenAICodexResponsesProvider{OpenAIResponsesProvider: inner}
}

func (p *OpenAICodexResponsesProvider) Name() string { return p.name }
func (p *OpenAICodexResponsesProvider) API() string  { return APIOpenAICodexResponses }

// HealthCheck implements ai.HealthChecker.
func (p *OpenAICodexResponsesProvider) HealthCheck(ctx context.Context) error {
	return healthCheckOneGet(ctx, p.httpClient, p.baseURL+"/v1/models", nil, APIOpenAICodexResponses)
}

// Stream implements Provider: the Responses wire plus the Codex
// instructions/store mapping.
func (p *OpenAICodexResponsesProvider) Stream(ctx context.Context, req StreamRequest) (<-chan Event, error) {
	if strings.TrimSpace(req.System) == "" {
		req.System = CodexDefaultInstructions
	}
	return p.streamAt(ctx, req, p.baseURL+"/responses", p.headers())
}
