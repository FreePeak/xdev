package config

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Runtime model discovery (M9 #10): providers like ollama and lm-studio
// publish their loaded models over an HTTP list endpoint, so xdev can offer
// models nobody wrote into models.yml. The parsed DiscoveryConfig was
// previously inert — nothing called it.

// DiscoveryTimeout bounds one list request; a dead local server must not
// stall startup.
var DiscoveryTimeout = 4 * time.Second

// DiscoveryOpenAIModels is the supported discovery scheme.
const DiscoveryOpenAIModels = "openai-models-list"

// truncateBody keeps an error body to one bounded line.
func truncateBody(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// discoveredModel is the subset of an OpenAI-style /models entry we use.
type discoveredModel struct {
	ID               string `json:"id"`
	Object           string `json:"object"`
	ContextLength    int    `json:"context_length"`
	MaxTokens        int    `json:"max_tokens"`
	SupportsThinking *bool  `json:"supports_thinking"`
}

// DiscoverModels lists a provider's models from its discovery endpoint.
//
// The endpoint is DiscoveryConfig.Endpoint if set, else the provider baseUrl
// with "/models" appended. A configured `models:` list is merged under the
// discovered set: pinned entries win on id collision, so a user's explicit
// contextWindow/reasoning overrides never get clobbered by a server that
// reports defaults.
//
// Errors are returned to the caller, which decides whether discovery matters
// for this provider: startup must not fail because a local server is down.
func DiscoverModels(ctx context.Context, pc *ProviderConfig) ([]ModelConfig, error) {
	// Discovery is opt-in by presence: no `discovery:` block means nothing
	// to list. The type selects the scheme; only the OpenAI-style model list
	// exists today, so an unknown type is inert rather than a guess.
	if pc == nil || pc.Discovery == nil || pc.Discovery.Type != DiscoveryOpenAIModels {
		return nil, nil
	}
	endpoint := strings.TrimSuffix(pc.BaseURL, "/") + "/models"
	if pc.Discovery.InjectV1 && !strings.Contains(strings.TrimSuffix(pc.BaseURL, "/"), "/v1") {
		endpoint = strings.TrimSuffix(pc.BaseURL, "/") + "/v1/models"
	}
	cctx, cancel := context.WithTimeout(ctx, DiscoveryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if key := strings.TrimSpace(Resolve(pc.APIKey)); key != "" {
		hdr := strings.TrimSpace(pc.AuthHeader)
		if hdr == "" || strings.EqualFold(hdr, "Authorization") {
			req.Header.Set("Authorization", "Bearer "+key)
		} else {
			req.Header.Set(hdr, key)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return nil, fmt.Errorf("config: discovery %s: HTTP %d: %s", endpoint, resp.StatusCode, truncateBody(string(body), 200))
	}
	// Bound the read: a misconfigured endpoint returning gigabytes must not
	// become an allocation.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Data []discoveredModel `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err == nil && len(parsed.Data) > 0 {
		return mergeDiscovered(pc.Models, parsed.Data), nil
	}
	// Some servers (and ollama's own shape) return {"models":[{...}]} or a
	// bare array instead of {"data":[...]}.
	var alt struct {
		Models []discoveredModel `json:"models"`
	}
	if err := json.Unmarshal(raw, &alt); err == nil && len(alt.Models) > 0 {
		return mergeDiscovered(pc.Models, alt.Models), nil
	}
	var bare []discoveredModel
	if err := json.Unmarshal(raw, &bare); err == nil && len(bare) > 0 {
		return mergeDiscovered(pc.Models, bare), nil
	}
	return nil, fmt.Errorf("config: discovery %s: unrecognized model list", endpoint)
}

// mergeDiscovered layers discovered models under the pinned ones.
func mergeDiscovered(pinned []ModelConfig, found []discoveredModel) []ModelConfig {
	out := make([]ModelConfig, 0, len(pinned)+len(found))
	seen := map[string]bool{}
	for _, m := range pinned {
		if m.ID != "" && !seen[m.ID] {
			seen[m.ID] = true
			out = append(out, m)
		}
	}
	for _, d := range found {
		id := strings.TrimSpace(d.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		m := ModelConfig{ID: id, Name: id, ContextWindow: d.ContextLength, MaxTokens: d.MaxTokens}
		if m.ContextWindow == 0 && d.MaxTokens > 0 {
			m.ContextWindow = d.MaxTokens
		}
		if d.SupportsThinking != nil {
			m.Reasoning = *d.SupportsThinking
		}
		out = append(out, m)
	}
	return out
}
