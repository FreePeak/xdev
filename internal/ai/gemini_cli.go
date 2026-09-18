package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// GeminiCLIProvider speaks the gemini-cli wire: Google's Cloud Code Assist
// endpoint the Gemini CLI drives —
//
//	POST {baseURL}/v1internal:streamGenerateContent?alt=sse
//	{"model": "...", "project": "...", "request": {<GenerateContent>}}
//
// with a Bearer token. Responses stream the GenerateContentResponse inside a
// `response` envelope (some deployments stream the bare chunk), and tool
// schemas run NormalizeSchemaForGoogle. The model id is carried in the body's
// model field, not the path.
type GeminiCLIProvider struct {
	*GoogleGenAIProvider
	project string
}

// GeminiCLIOptions configures the Code Assist adapter. Project is optional
// (personal Code Assist accounts have none).
type GeminiCLIOptions struct {
	Project string
}

// geminiCLIStreamEndpoint is the Code Assist streaming method on the base URL.
const geminiCLIStreamEndpoint = "/v1internal:streamGenerateContent?alt=sse"

// NewGeminiCLIProvider builds a Code Assist provider. A nil hc uses the shared
// transport.
func NewGeminiCLIProvider(name, baseURL, apiKey string, headers map[string]string, opts GeminiCLIOptions, hc *http.Client) *GeminiCLIProvider {
	inner := NewGoogleGenAIProvider(name, baseURL, apiKey, headers, hc)
	inner.apiLabel = APIGeminiCLI
	inner.normalizeTools = true
	inner.unwrapResponse = true
	return &GeminiCLIProvider{GoogleGenAIProvider: inner, project: strings.TrimSpace(opts.Project)}
}

func (p *GeminiCLIProvider) Name() string { return p.name }
func (p *GeminiCLIProvider) API() string  { return APIGeminiCLI }

// HealthCheck implements ai.HealthChecker.
func (p *GeminiCLIProvider) HealthCheck(ctx context.Context) error {
	return healthCheckOneGet(ctx, p.httpClient, modelsProbeURL(p.baseURL), nil, APIGeminiCLI)
}

// envelopeBody wraps the GenerateContent request in the CLI envelope.
func (p *GeminiCLIProvider) envelopeBody(req StreamRequest) ([]byte, error) {
	model, err := p.resolveModel(req)
	if err != nil {
		return nil, err
	}
	inner, err := p.GoogleGenAIProvider.buildRequest(req)
	if err != nil {
		return nil, err
	}
	envelope := map[string]any{
		"model":   model,
		"request": json.RawMessage(inner),
	}
	if p.project != "" {
		envelope["project"] = p.project
	}
	return json.Marshal(envelope)
}

// Stream implements Provider.
func (p *GeminiCLIProvider) Stream(ctx context.Context, req StreamRequest) (<-chan Event, error) {
	body, err := p.envelopeBody(req)
	if err != nil {
		return nil, err
	}
	return p.streamAt(ctx, req, p.baseURL+geminiCLIStreamEndpoint, p.bearerHeaders(), body)
}

// unwrapCodeAssistChunk reads the GenerateContentResponse out of the Code
// Assist envelope; a bare chunk is returned unchanged.
func unwrapCodeAssistChunk(data string) string {
	var envelope struct {
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal([]byte(data), &envelope); err != nil || len(envelope.Response) == 0 {
		return data
	}
	return string(envelope.Response)
}
