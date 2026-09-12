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

	"gopkg.in/yaml.v3"

	"github.com/FreePeak/xdev/internal/ai"
)

// DataDir returns the xdev agent data directory (~/.xdev/agent).
func DataDir() string {
	if v := os.Getenv("XDEV_AGENT_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".xdev/agent"
	}
	return filepath.Join(home, ".xdev", "agent")
}

// DiscoveryConfig selects dynamic model listing.
type DiscoveryConfig struct {
	Type     string `yaml:"type"` // "openai-models-list" is the MVP value
	InjectV1 bool   `yaml:"injectV1,omitempty"`
}

// ModelConfig is one statically pinned model entry.
type ModelConfig struct {
	ID            string `yaml:"id"`
	Name          string `yaml:"name,omitempty"`
	Reasoning     bool   `yaml:"reasoning,omitempty"`
	ContextWindow int    `yaml:"contextWindow,omitempty"`
	MaxTokens     int    `yaml:"maxTokens,omitempty"`
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
	// OAuth configures the browser login flow for this provider (xdev login
	// <provider>). Absent = not an OAuth provider.
	OAuth *OAuthConfig `yaml:"oauth,omitempty"`
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
	case ai.APIOpenAICompletions, ai.APIOpenAIResponses, ai.APIAnthropicMessages, ai.APIGoogleGenerativeAI:
	default:
		return fmt.Errorf("config: register_provider %q: unsupported api %q (want %s|%s|%s|%s)",
			name, pc.API, ai.APIOpenAICompletions, ai.APIOpenAIResponses, ai.APIAnthropicMessages, ai.APIGoogleGenerativeAI)
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

// LoadModels parses one models.yml file with ${VAR} expansion applied to
// string values (baseUrl, apiKey, header values).
func LoadModels(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(expandEnvYAML(string(raw))))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.Providers == nil {
		cfg.Providers = map[string]*ProviderConfig{}
	}
	return &cfg, nil
}

// LoadModelsLayered loads global then project models.yml, later wins.
// Missing files are skipped silently.
func LoadModelsLayered() (*Config, error) {
	cfg := &Config{Providers: map[string]*ProviderConfig{}}
	paths := []string{
		filepath.Join(DataDir(), "models.yml"),
		".xdev/models.yml",
	}
	for _, p := range paths {
		c, err := LoadModels(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for k, v := range c.Providers {
			cfg.Providers[k] = v
		}
		if c.DefaultModel != "" {
			cfg.DefaultModel = c.DefaultModel
		}
	}
	return cfg, nil
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

func sortedKeys(m map[string]*ProviderConfig) []string {
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
