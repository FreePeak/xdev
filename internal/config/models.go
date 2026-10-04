// Package config loads xdev configuration: providers/models (models.yml),
// settings, and env layering. The models.yml schema matches omp's
// (baseUrl/apiKey/api/models/discovery) so existing provider files port over.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/FreePeak/xdev/internal/ai"
)

// DiscoveryConfig selects dynamic model listing.
type DiscoveryConfig struct {
	Type     string `yaml:"type"` // "openai-models-list" is the MVP value
	InjectV1 bool   `yaml:"injectV1,omitempty"`
}

// ModelPricing is a model's per-million-token rates in USD. It exists because
// a provider is not required to report cost: a gateway, a proxy or a local
// server reports tokens and nothing else, so a store read by one shows $0.00
// for work that was genuinely paid for. Declaring the rates locally turns that
// $0.00 into an estimate, and only for the requests the provider left
// unpriced — a provider that reports its own cost is always believed
// (internal/stats reads usage.cost.total first and this only fills the gap).
//
// Cache read and cache write are separate fields because they are separate
// rates, not multipliers of input: Anthropic's cache read is a tenth of input
// while DeepSeek's is a fiftieth, so a shared multiplier cannot express both.
type ModelPricing struct {
	Input      float64 `yaml:"input,omitempty"`
	Output     float64 `yaml:"output,omitempty"`
	CacheRead  float64 `yaml:"cacheRead,omitempty"`
	CacheWrite float64 `yaml:"cacheWrite,omitempty"`
}

// Zero reports whether no rate is declared: a pricing block that names no
// number prices nothing, so an empty `pricing:` in models.yml is inert rather
// than a $0 estimate.
func (p ModelPricing) Zero() bool {
	return p == ModelPricing{}
}

// USD prices one billed request. A request with no total of its own is priced
// as the sum of its buckets, which is the same normalization every wire
// adapter already applies.
func (p ModelPricing) USD(input, output, cacheRead, cacheWrite, total int64) float64 {
	if total <= 0 {
		total = input + output + cacheRead + cacheWrite
	}
	return (float64(input)/1e6)*p.Input +
		(float64(output)/1e6)*p.Output +
		(float64(cacheRead)/1e6)*p.CacheRead +
		(float64(cacheWrite)/1e6)*p.CacheWrite
}

// ModelConfig is one statically pinned model entry.
type ModelConfig struct {
	ID        string `yaml:"id"`
	Name      string `yaml:"name,omitempty"`
	Reasoning bool   `yaml:"reasoning,omitempty"`
	// Pricing is the model's per-million-token rates in USD, used to estimate
	// the cost of a request the provider reported no cost for. Absent = the
	// model is priced by whatever the provider says, and nothing else.
	Pricing *ModelPricing `yaml:"pricing,omitempty"`
	// Vision marks a model that accepts image input. snapcompact's bitmap is
	// only useful to such a model (#83); without the flag the dropped text
	// would ride along as bytes nothing can read.
	Vision        bool `yaml:"vision,omitempty"`
	ContextWindow int  `yaml:"contextWindow,omitempty"`
	MaxTokens     int  `yaml:"maxTokens,omitempty"`
	// BaseURL/APIKey/Headers override the provider-level values per model.
	BaseURL string            `yaml:"baseUrl,omitempty"`
	APIKey  string            `yaml:"apiKey,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty"`
}

// ProviderConfig is one provider block in models.yml.
type ProviderConfig struct {
	BaseURL string `yaml:"baseUrl"`
	APIKey  string `yaml:"apiKey,omitempty"`
	// APIKeys are sibling credentials for the same provider (M5 #25): a
	// usage limit on one account rotates to the next instead of leaving
	// the provider. The chain still starts at apiKey (or the first entry
	// when apiKey is absent) — apiKeys only supplies the rotation pool.
	APIKeys []string `yaml:"apiKeys,omitempty"`
	API     string   `yaml:"api"`
	// AuthHeader names where a bearer credential rides (e.g.
	// "x-api-key"); empty means "Authorization".
	AuthHeader string `yaml:"authHeader,omitempty"`
	// Auth names the credential style: api_key (default), oauth, or none
	// (a local server that needs no credential — ollama, lm-studio).
	Auth      string            `yaml:"auth,omitempty"`
	Headers   map[string]string `yaml:"headers,omitempty"`
	Discovery *DiscoveryConfig  `yaml:"discovery,omitempty"`
	Models    []ModelConfig     `yaml:"models,omitempty"`
	// ContextWindow is this provider's window for every model whose own
	// window nothing stated — the gateway's uniform answer, stated once
	// instead of per model. It is the rung below a per-model
	// `contextWindow` and above the compiled 200000 default, so a gateway
	// that serves one window behind a dozen model ids (onegw's opencode
	// lane, measured 2026-10-04: a live bisect served 1,048,349 prompt
	// tokens and refused 1,048,400) is configured once, and a model that
	// really differs keeps its own pin. Absent = a stated window wins,
	// else ResolveMaxContextTokens().
	ContextWindow int `yaml:"contextWindow,omitempty"`
	// OAuth configures the browser login flow for this provider (xdev login
	// <provider>). Absent = not an OAuth provider.
	OAuth *OAuthConfig `yaml:"oauth,omitempty"`
	// Project/Location are the GCP coordinates: google-vertex needs both
	// (Location defaults to "global"), gemini-cli uses Project when the Code
	// Assist account has one.
	Project  string `yaml:"project,omitempty"`
	Location string `yaml:"location,omitempty"`
	// Deployment/APIVersion configure azure-openai-responses: the Azure
	// deployment name (empty = the model id) and the api-version query value.
	Deployment string `yaml:"deployment,omitempty"`
	APIVersion string `yaml:"apiVersion,omitempty"`
	// ToolsFormat pins an in-band tool-call dialect for models that cannot
	// emit native structured calls (hermes, deepseek, glm, ...). Empty =
	// native: the provider's own structured tool calls.
	ToolsFormat string `yaml:"toolsFormat,omitempty"`
	// HealthCheckURL overrides the provider's default liveness probe (/v1/models). Empty = default.
	HealthCheckURL string `yaml:"healthCheckURL,omitempty"`
}

// OAuthConfig is one provider's browser-login flow.
type OAuthConfig struct {
	AuthorizeURL string   `yaml:"authorizeUrl"`
	TokenURL     string   `yaml:"tokenUrl"`
	ClientID     string   `yaml:"clientId"`
	Scopes       []string `yaml:"scopes,omitempty"`
	RedirectPort int      `yaml:"redirectPort,omitempty"`
}

// CredentialPool returns a provider's declared models.yml credentials in
// rotation order (M5 #25): the per-model apiKey first when model names an
// entry, then the provider's apiKey, then the apiKeys siblings. Blanks and
// duplicates are dropped, so pool[0] is exactly the credential the ordinary
// chain picks and every later index is a real sibling to rotate onto.
//
// An empty pool means "no models.yml credential at all" — the caller falls
// through to the oauth / login / env chain as before.
func CredentialPool(pc *ProviderConfig, model string) []string {
	if pc == nil {
		return nil
	}
	var out []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" || slices.Contains(out, v) {
			return
		}
		out = append(out, v)
	}
	if model != "" {
		for _, m := range pc.Models {
			if m.ID == model {
				add(m.APIKey)
			}
		}
	}
	add(pc.APIKey)
	for _, k := range pc.APIKeys {
		add(k)
	}
	return out
}

// CredentialKey returns the i-th credential of the pool ("" when the index
// is out of range — an exhausted rotation must not silently reuse the
// primary credential).
func CredentialKey(pc *ProviderConfig, model string, i int) string {
	pool := CredentialPool(pc, model)
	if i < 0 || i >= len(pool) {
		return ""
	}
	return pool[i]
}

// Config is the parsed models.yml.
type Config struct {
	Providers    map[string]*ProviderConfig `yaml:"providers"`
	DefaultModel string                     `yaml:"defaultModel,omitempty"` // "provider/model"
	// ignoredProject names what a repository's .xdev/models.yml tried to set
	// that the repo-trust boundary refused (#114). Not part of the schema: it
	// never decodes and never marshals, only the startup notice reads it.
	ignoredProject []string
}

// RegisterProvider installs one provider block for the life of this process.
// Extensions call it at runtime (ext action `register_provider`, PRD M7), so
// the payload carries the same shape as a models.yml provider entry and the
// validation happens here — before any request — instead of surfacing as an
// opaque wire failure. Nothing is persisted: the next run re-registers, and
// use-time gating (Settings.CheckProvider, disabledProviders) still applies
// when the provider is built.
func (c *Config) RegisterProvider(name string, pc *ProviderConfig) error {
	if c == nil {
		return fmt.Errorf("config: register_provider: no model registry")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("config: register_provider: name is required")
	}
	if strings.ContainsAny(name, "/ \t") {
		return fmt.Errorf("config: register_provider: name %q must be a bare provider key (no \"/\" or spaces)", name)
	}
	if pc == nil {
		return fmt.Errorf("config: register_provider %q: provider block is required", name)
	}
	if strings.TrimSpace(pc.BaseURL) == "" {
		return fmt.Errorf("config: register_provider %q: baseUrl is required", name)
	}
	switch pc.API {
	case ai.APIOpenAICompletions, ai.APIOpenAIResponses, ai.APIAzureOpenAIResponses,
		ai.APIOpenAICodexResponses, ai.APIAnthropicMessages, ai.APIGoogleGenerativeAI,
		ai.APIGoogleVertex, ai.APIGeminiCLI:
	default:
		return fmt.Errorf("config: register_provider %q: unsupported api %q (want %s)",
			name, pc.API, strings.Join(ai.SupportedAPIs(), "|"))
	}
	// toolsFormat is validated here so a typo fails before any request instead
	// of silently falling back to native tool calls.
	if _, err := ai.ParseToolFormat(pc.ToolsFormat); err != nil {
		return fmt.Errorf("config: register_provider %q: %w", name, err)
	}
	models := 0
	for _, m := range pc.Models {
		if strings.TrimSpace(m.ID) != "" {
			models++
		}
	}
	if models == 0 {
		return fmt.Errorf("config: register_provider %q: at least one model with an id is required", name)
	}
	if c.Providers == nil {
		c.Providers = map[string]*ProviderConfig{}
	}
	c.Providers[name] = pc
	return nil
}

// Resolve expands ${VAR} references in s against the process environment.
// A missing variable expands to the empty string.
func Resolve(s string) string {
	if s == "" || !strings.Contains(s, "$") {
		return s
	}
	return os.Expand(s, func(k string) string {
		if v, ok := os.LookupEnv(k); ok {
			return v
		}
		return ""
	})
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// LoadModels parses one models.yml (the trusted shape: every key honored)
// with ${VAR} references expanded against the process environment.
func LoadModels(path string) (*Config, error) {
	cfg, _, err := loadModelsFile(path, true)
	return cfg, err
}

// loadModelsFile parses one layer. An untrusted layer (a repository's
// .xdev/models.yml, #114) is pruned to the repo-safe subset, and ${VAR}
// expansion is skipped for it: interpolating the environment through a file
// that arrived with a clone is a read of the user's secrets.
func loadModelsFile(path string, trusted bool) (*Config, []string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var cfg Config
	var ignored []string
	if trusted {
		err = parseYAMLLayer([]byte(expandEnvYAML(string(raw))), &cfg)
	} else {
		ignored, err = pruneTo(raw, &cfg, pruneProjectModels)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.Providers == nil {
		cfg.Providers = map[string]*ProviderConfig{}
	}
	return &cfg, ignored, nil
}

// LoadModelsLayered loads the profile models.yml plus the repository's, with
// the repository's held to the subset a stranger may choose (#114): model
// metadata and the transport dialect, never an endpoint, a credential or the
// default model. On a provider both name, the profile wins outright — a
// clone that ships .xdev/models.yml must not be able to move the user's
// traffic anywhere. Missing files are skipped silently.
func LoadModelsLayered() (*Config, error) {
	cfg := &Config{Providers: map[string]*ProviderConfig{}}
	// Project first, profile second, so the profile's entry is the last word.
	proj, ignored, err := loadModelsFile(projectModelsName, false)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
	} else {
		for k, v := range proj.Providers {
			cfg.Providers[k] = v
		}
	}
	global, _, err := loadModelsFile(filepath.Join(DataDir(), "models.yml"), true)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
	} else {
		for k, v := range global.Providers {
			cfg.Providers[k] = v
		}
		cfg.DefaultModel = global.DefaultModel
	}
	cfg.ignoredProject = ignored
	return cfg, nil
}

// IgnoredProjectKeys returns what the repository's models.yml named that the
// repo-trust boundary refused (#114); see Settings.IgnoredProjectKeys.
func (c *Config) IgnoredProjectKeys() []string {
	if c == nil {
		return nil
	}
	return c.ignoredProject
}

// Pricing returns the declared rates for one model id, matching pinned
// entries by suffix so "onegw/claude-sonnet-4-6" finds a "claude-sonnet-4-6"
// entry whatever the provider block calls it. The empty ModelPricing means
// "no local price", which is the zero value callers can test with Zero.
//
// Two rules decide which entry wins, in this order:
//
//  1. The provider named by the model's own prefix. "router/claude-x" is
//     priced by the `router` block even when another provider pins the same
//     model id at a different rate — a gateway's price and the vendor's are
//     both real and only one of them was billed.
//  2. Otherwise the longest matching id, so a pinned "gpt-5.4-mini" is not
//     shadowed by "gpt-5.4".
func (c *Config) Pricing(modelID string) ModelPricing {
	if c == nil || modelID == "" {
		return ModelPricing{}
	}
	keys := sortedKeys(c.Providers)
	if prefix, _, ok := strings.Cut(modelID, "/"); ok {
		if _, isProvider := c.Providers[prefix]; isProvider {
			keys = []string{prefix}
		}
	}
	var best *ModelConfig
	for _, key := range keys {
		for i := range c.Providers[key].Models {
			m := &c.Providers[key].Models[i]
			if m.Pricing == nil || !modelMatches(m.ID, modelID) {
				continue
			}
			if best == nil || len(m.ID) > len(best.ID) {
				best = m
			}
		}
	}
	if best == nil {
		return ModelPricing{}
	}
	return *best.Pricing
}

// modelMatches reports whether a request for modelID is served by the pinned
// entry id. Exact first, then a suffix match on a "/" boundary, because a
// gateway prefixes its own namespace ("openrouter/anthropic/claude-x").
func modelMatches(pinned, modelID string) bool {
	if pinned == "" {
		return false
	}
	if pinned == modelID {
		return true
	}
	return strings.HasSuffix(modelID, "/"+pinned)
}

// ParseModelRef splits "provider/model" into its two halves.
func ParseModelRef(ref string) (provider, model string, err error) {
	i := strings.Index(ref, "/")
	if i <= 0 || i == len(ref)-1 {
		return "", "", fmt.Errorf("invalid model reference %q: want \"provider/model\"", ref)
	}
	return ref[:i], ref[i+1:], nil
}

// DefaultModelRef returns cfg.DefaultModel if set, else the first provider's
// first pinned model, else "".
func (c *Config) DefaultModelRef() string {
	if c.DefaultModel != "" {
		return c.DefaultModel
	}
	// Deterministic: prefer common keys, else first sorted provider.
	for _, k := range []string{"onegw", "router", "anthropic", "openai"} {
		if p, ok := c.Providers[k]; ok && len(p.Models) > 0 {
			return k + "/" + p.Models[0].ID
		}
	}
	keys := sortedKeys(c.Providers)
	for _, k := range keys {
		if len(c.Providers[k].Models) > 0 {
			return k + "/" + c.Providers[k].Models[0].ID
		}
	}
	return ""
}

func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// expandEnvYAML replaces ${VAR} occurrences in scalar YAML values.
// It operates on the raw text; only quoted or plain scalars containing the
// pattern are touched, which is safe because keys never match the pattern.
func expandEnvYAML(s string) string {
	return envRef.ReplaceAllStringFunc(s, func(m string) string {
		k := m[2 : len(m)-1]
		if v, ok := os.LookupEnv(k); ok {
			return v
		}
		return ""
	})
}
