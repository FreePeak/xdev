package ai

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// GoogleVertexProvider speaks google-vertex: the Gemini GenerateContent shape
// on Vertex AI's project/location endpoint —
//
//	POST {baseURL}/projects/{project}/locations/{location}/publishers/google/models/{model}:streamGenerateContent?alt=sse
//
// Vertex authenticates with a Bearer token (the key is not a query parameter,
// unlike google-generative-ai) and its schema surface is Gemini's, so tool
// schemas run NormalizeSchemaForGoogle before they are sent.
type GoogleVertexProvider struct {
	*GoogleGenAIProvider
	project  string
	location string
}

// GoogleVertexOptions configures the Vertex adapter. Project is required;
// Location defaults to "global".
type GoogleVertexOptions struct {
	Project  string
	Location string
}

// NewGoogleVertexProvider builds a Vertex provider. A nil hc uses the shared
// transport.
func NewGoogleVertexProvider(name, baseURL, apiKey string, headers map[string]string, opts GoogleVertexOptions, hc *http.Client) *GoogleVertexProvider {
	inner := NewGoogleGenAIProvider(name, baseURL, apiKey, headers, hc)
	inner.apiLabel = APIGoogleVertex
	inner.normalizeTools = true
	location := strings.TrimSpace(opts.Location)
	if location == "" {
		location = "global"
	}
	return &GoogleVertexProvider{
		GoogleGenAIProvider: inner,
		project:             strings.TrimSpace(opts.Project),
		location:            location,
	}
}

func (p *GoogleVertexProvider) Name() string { return p.name }

// HealthCheck implements ai.HealthChecker.
func (p *GoogleVertexProvider) HealthCheck(ctx context.Context) error {
	return healthCheckOneGet(ctx, p.httpClient, p.baseURL+"/v1/models", nil, APIGoogleVertex)
}


// Stream implements Provider.
func (p *GoogleVertexProvider) Stream(ctx context.Context, req StreamRequest) (<-chan Event, error) {
	if p.project == "" {
		return nil, fmt.Errorf("%s: project is required", APIGoogleVertex)
	}
	model, err := p.resolveModel(req)
	if err != nil {
		return nil, err
	}
	body, err := p.buildRequest(req)
	if err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("%s/projects/%s/locations/%s/publishers/google/models/%s:streamGenerateContent?alt=sse",
		p.baseURL, url.PathEscape(p.project), url.PathEscape(p.location), url.PathEscape(model))
	return p.streamAt(ctx, req, endpoint, p.bearerHeaders(), body)
}

// bearerHeaders is the Vertex auth shape: Content-Type plus a Bearer token
// (extra headers still pass through for gateways).
func (p *GoogleGenAIProvider) bearerHeaders() map[string]string {
	h := map[string]string{"Content-Type": "application/json"}
	if p.apiKey != "" {
		h["Authorization"] = "Bearer " + p.apiKey
	}
	for k, v := range p.extraHeaders {
		h[k] = v
	}
	return h
}
