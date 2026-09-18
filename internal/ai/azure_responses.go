package ai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// AzureResponsesProvider speaks azure-openai-responses: the Responses API
// mounted per deployment —
//
//	POST {baseURL}/openai/deployments/{deployment}/responses?api-version={ver}
//
// with the key in Azure's `api-key` header. The deployment selects the model
// server-side (the body's model field is informational), the deployment name
// defaults to the requested model id, and tool schemas are sanitized for the
// Responses API but never strictified: Azure expects `strict: false`.
type AzureResponsesProvider struct {
	*OpenAIResponsesProvider
	deployment string
	apiVersion string
}

// DefaultAzureAPIVersion is the pinned Azure Responses API version; the
// `apiVersion` provider field in models.yml overrides it.
const DefaultAzureAPIVersion = "2025-04-01-preview"

// AzureResponsesOptions configures the Azure Responses adapter.
type AzureResponsesOptions struct {
	// Deployment is the Azure deployment name; empty uses the request model.
	Deployment string
	// APIVersion is the api-version query value; empty uses
	// DefaultAzureAPIVersion.
	APIVersion string
}

// NewAzureResponsesProvider builds an Azure Responses provider. A nil hc uses
// the shared transport.
func NewAzureResponsesProvider(name, baseURL, apiKey string, headers map[string]string, opts AzureResponsesOptions, hc *http.Client) *AzureResponsesProvider {
	inner := NewOpenAIResponsesProvider(name, baseURL, apiKey, headers, hc)
	inner.apiLabel = APIAzureOpenAIResponses
	inner.behavior = responsesBehavior{sanitize: true, requestKeyHeader: "api-key"}
	version := strings.TrimSpace(opts.APIVersion)
	if version == "" {
		version = DefaultAzureAPIVersion
	}
	return &AzureResponsesProvider{
		OpenAIResponsesProvider: inner,
		deployment:              strings.TrimSpace(opts.Deployment),
		apiVersion:              version,
	}
}

// HealthCheck implements ai.HealthChecker.
func (p *AzureResponsesProvider) HealthCheck(ctx context.Context) error {
	return healthCheckOneGet(ctx, p.httpClient, p.baseURL+"/v1/models", nil, APIAzureOpenAIResponses)
}

func (p *AzureResponsesProvider) Name() string { return p.name }
func (p *AzureResponsesProvider) API() string  { return APIAzureOpenAIResponses }

// Stream implements Provider.
func (p *AzureResponsesProvider) Stream(ctx context.Context, req StreamRequest) (<-chan Event, error) {
	deployment := p.deployment
	if deployment == "" {
		deployment = req.Model
	}
	if deployment == "" {
		deployment = p.model
	}
	if deployment == "" {
		return nil, errors.New("azure-openai-responses: deployment or model is required")
	}
	endpoint := fmt.Sprintf("%s/openai/deployments/%s/responses?api-version=%s",
		p.baseURL, url.PathEscape(deployment), url.QueryEscape(p.apiVersion))
	return p.streamAt(ctx, req, endpoint, p.headers())
}
