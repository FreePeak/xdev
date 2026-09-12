// Package imagegen implements xdev's generate_image engine (M15 #69,
// parity-tools-providers): a prompt over an ORDERED chain of cloud image
// providers with sequential fallback. Two adapters — OpenAI
// images/generations (gpt-image-1) and Gemini generateContent / Imagen
// predict — are the exercisable subset of omp's seven image backends; a new
// one is a small case in (*Generator).call plus its request/response pair in
// adapters.go.
//
// The engine lives in its own leaf package for the same reason websearch
// does: internal/config imports internal/tool (through internal/agent) and
// aliases this package's Settings, so this package must import neither.
// Credentials and blob storage are injected by the caller.
//
// Two invariants shape the code:
//   - every hop is bounded — per-attempt deadline, prompt cap, response cap,
//     and a capped download for URL-shaped responses — so a hanging or
//     oversized provider cannot eat the call or the RSS budget;
//   - a provider that cannot run (no credential, HTTP failure) is skipped
//     with a note and the next one is tried; the failure is reported, never
//     silently downgraded to an empty result.
package imagegen

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// DefaultTimeout bounds ONE provider attempt. Per-provider (not
	// per-call) is the point: the chain's tail must still get its turn when
	// the head hangs. omp allows three minutes; one image is one request,
	// so two is already generous.
	DefaultTimeout = 120 * time.Second
	// DefaultMaxBytes caps one provider response body (and one image
	// download). An image past this is a provider misconfiguration, not a
	// picture worth 100 MB of RSS.
	DefaultMaxBytes = 12 << 20
	// MaxPromptBytes caps the composed prompt; the composition fields are
	// model-authored, so the cap is a trust boundary, not a nicety.
	MaxPromptBytes = 16 << 10
	// maxErrBody caps how much of a failed response is quoted back.
	maxErrBody = 400
)

// Provider adapter names. A settings entry may override model/baseUrl but not
// the wire protocol: an unknown name is dropped from the chain.
const (
	OpenAI = "openai"
	Gemini = "gemini"
)

// DefaultProviders is the built-in order, used when the settings block lists
// no providers: every entry is skipped unless a credential resolves, so an
// env-only OPENAI_API_KEY or GEMINI_API_KEY is enough to be useful.
var DefaultProviders = []string{OpenAI, Gemini}

// Default models: the image model each adapter asks for when the settings
// entry names none.
const (
	DefaultOpenAIModel = "gpt-image-1"
	DefaultGeminiModel = "gemini-2.5-flash-image"
)

// envKeys maps a provider to the environment variable holding its key. These
// are the names the no-credential error tells the user to export.
var envKeys = map[string]string{
	OpenAI: "OPENAI_API_KEY",
	Gemini: "GEMINI_API_KEY",
}

// Settings is the `imageProviders` block (internal/config aliases this type
// so the registry builder can hand it over whole). The zero value is usable:
// the built-in provider order plus the environment credential names.
type Settings struct {
	// Providers is the ordered chain; empty means DefaultProviders.
	Providers []Provider `yaml:"providers,omitempty"`
	// Timeout is one provider attempt as a Go duration ("90s"); empty means
	// DefaultTimeout.
	Timeout string `yaml:"timeout,omitempty"`
	// MaxBytes caps one provider response; 0 means DefaultMaxBytes.
	MaxBytes int64 `yaml:"maxBytes,omitempty"`
}

// Provider is one entry in the image chain: an adapter name plus optional
// overrides. A gateway or Azure-style deployment is expressed by pointing
// BaseURL at it while keeping the OpenAI adapter.
type Provider struct {
	// Name selects the adapter: openai or gemini.
	Name string `yaml:"name"`
	// Model overrides the provider's default image model.
	Model string `yaml:"model,omitempty"`
	// BaseURL overrides the provider's API root (a gateway, a proxy, or a
	// test server); empty means the adapter's real endpoint.
	BaseURL string `yaml:"baseUrl,omitempty"`
	// APIKey is an inline credential; ${VAR} is expanded by the config
	// layer. Empty falls through to the harness chain and then the env.
	APIKey string `yaml:"apiKey,omitempty"`
}

// CredentialLookup resolves one provider's API key through the harness
// credential chain (models.yml → stored login → env), returning a short
// source label for the summary. nil means environment-only.
type CredentialLookup func(provider string) (key, source string, err error)

// Generator runs prompts over the configured provider chain.
type Generator struct {
	Providers []Provider
	Timeout   time.Duration
	MaxBytes  int64
	Creds     CredentialLookup
	// HTTPClient overrides the transport; nil → one client with Timeout
	// applied, so a body read cannot hang past the deadline either.
	HTTPClient *http.Client
}

// New builds a Generator from a settings block and a credential lookup.
func New(s Settings, creds CredentialLookup) *Generator {
	g := &Generator{Providers: s.Providers, MaxBytes: s.MaxBytes, Creds: creds}
	// A malformed duration cannot reach here through config (merge
	// validates it); on any other path the default is the safe fallback.
	if d, err := time.ParseDuration(strings.TrimSpace(s.Timeout)); err == nil && d > 0 {
		g.Timeout = d
	}
	return g
}

// Request is one generation call; the tool folds the compositional schema
// into Prompt before handing it over.
type Request struct {
	// Prompt is the composed text prompt.
	Prompt string
	// Size is the requested output size ("1024x1024", "1536x1024",
	// "1024x1536"); empty means the provider's default. Adapters that do
	// not take a size ignore it.
	Size string
	// Quality is "low" | "medium" | "high"; empty means the provider's
	// default. OpenAI only.
	Quality string
	// Aspect is the requested aspect ratio ("1:1", "3:4", "4:3", "9:16",
	// "16:9"); empty means the provider's default. Gemini only.
	Aspect string
	// Provider is a per-call preference. A concrete name is tried first;
	// an unknown one is ignored (the configured order still runs).
	Provider string
}

// Image is one decoded image.
type Image struct {
	Data []byte
	MIME string
	// URL is the provider URL the bytes came from; empty for inline base64.
	URL string
	// RevisedPrompt is the provider's rewritten prompt when it returns one.
	RevisedPrompt string
}

// Result is one completed generation.
type Result struct {
	Provider string
	Model    string
	// CredSource labels where the winning key came from (models.yml, login,
	// env:OPENAI_API_KEY, imageProviders.apiKey).
	CredSource string
	Images     []Image
}

// Generate runs the prompt through the chain and returns the first
// provider's images. The error names every provider tried and why each one
// was skipped, so a failure is actionable without a debug flag.
func (g *Generator) Generate(ctx context.Context, req Request) (Result, error) {
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return Result{}, errors.New("prompt is required")
	}
	if len(prompt) > MaxPromptBytes {
		return Result{}, fmt.Errorf("prompt is %d bytes; the cap is %d", len(prompt), MaxPromptBytes)
	}
	chain, err := g.chain(req.Provider)
	if err != nil {
		return Result{}, err
	}
	notes := make([]string, 0, len(chain))
	for _, p := range chain {
		key, source, err := g.key(p)
		if err != nil {
			notes = append(notes, p.Name+": "+err.Error())
			continue
		}
		res, err := g.call(ctx, p, key, prompt, req)
		if err != nil {
			notes = append(notes, p.Name+": "+err.Error())
			continue
		}
		res.CredSource = source
		if len(res.Images) == 0 {
			notes = append(notes, p.Name+": the response carried no image")
			continue
		}
		return res, nil
	}
	return Result{}, fmt.Errorf("no image provider answered: %s", strings.Join(notes, "; "))
}

// chain resolves the provider order: an explicit per-call preference is
// tried first, then the configured order (DefaultProviders when the block
// lists none). Unknown adapter names are dropped — a typo must not disable
// the whole chain.
func (g *Generator) chain(pref string) ([]Provider, error) {
	configured := g.Providers
	if len(configured) == 0 {
		for _, name := range DefaultProviders {
			configured = append(configured, Provider{Name: name})
		}
	}
	out := make([]Provider, 0, len(configured)+1)
	seen := map[string]bool{}
	add := func(p Provider) {
		name := strings.ToLower(strings.TrimSpace(p.Name))
		if _, ok := envKeys[name]; !ok || seen[name] {
			return
		}
		p.Name = name
		seen[name] = true
		out = append(out, p)
	}
	for _, p := range configured {
		if strings.EqualFold(strings.TrimSpace(p.Name), strings.TrimSpace(pref)) {
			add(p)
		}
	}
	for _, p := range configured {
		add(p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no usable image provider configured (known adapters: %s)", strings.Join(DefaultProviders, "|"))
	}
	return out, nil
}

// key resolves one provider's credential. The order mirrors buildProvider's:
// models.yml → stored login → env (the injected chain), then the settings
// block's inline key, then the provider's own environment variable. The
// error names the environment variable, because that is the one fix the user
// can apply without editing files.
func (g *Generator) key(p Provider) (key, source string, err error) {
	if g.Creds != nil {
		if k, src, cErr := g.Creds(p.Name); cErr == nil && strings.TrimSpace(k) != "" {
			return k, src, nil
		}
	}
	if k := strings.TrimSpace(p.APIKey); k != "" {
		return k, "imageProviders.apiKey", nil
	}
	if v := strings.TrimSpace(os.Getenv(envKeys[p.Name])); v != "" {
		return v, "env:" + envKeys[p.Name], nil
	}
	return "", "", fmt.Errorf("no credential: export %s, set imageProviders.providers[].apiKey, or configure the %s provider in models.yml",
		envKeys[p.Name], p.Name)
}

// call dispatches one attempt to its adapter.
func (g *Generator) call(ctx context.Context, p Provider, key, prompt string, req Request) (Result, error) {
	base := strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	switch p.Name {
	case OpenAI:
		if base == "" {
			base = openAIBaseURL
		}
		return g.openai(ctx, p, base, key, prompt, req)
	case Gemini:
		if base == "" {
			base = geminiBaseURL
		}
		return g.gemini(ctx, p, base, key, prompt, req)
	default:
		return Result{}, fmt.Errorf("unknown image adapter %q", p.Name)
	}
}

// model resolves the model the adapter should ask for.
func (p Provider) model(fallback string) string {
	if m := strings.TrimSpace(p.Model); m != "" {
		return m
	}
	return fallback
}

func (g *Generator) timeout() time.Duration {
	if g.Timeout > 0 {
		return g.Timeout
	}
	return DefaultTimeout
}

func (g *Generator) maxBytes() int64 {
	if g.MaxBytes > 0 {
		return g.MaxBytes
	}
	return DefaultMaxBytes
}

func (g *Generator) client() *http.Client {
	if g.HTTPClient != nil {
		return g.HTTPClient
	}
	return &http.Client{Timeout: g.timeout()}
}

// post sends one JSON request and returns the bounded response body. A
// non-2xx status is an error carrying the provider's own message.
func (g *Generator) post(ctx context.Context, url string, headers map[string]string, body any) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encoding the request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, g.timeout())
	defer cancel()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("content-type", "application/json")
	for k, v := range headers {
		hreq.Header.Set(k, v)
	}
	resp, err := g.client().Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := readCapped(resp.Body, g.maxBytes())
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%s: %s", resp.Status, apiMessage(data))
	}
	return data, nil
}

// download fetches an image URL from a response. Only http(s) is followed —
// a provider answer is not an authority to read file:// or a cloud metadata
// endpoint — and the body is capped like any other provider payload.
func (g *Generator) download(ctx context.Context, rawURL string) (data []byte, mime string, err error) {
	u, perr := url.Parse(rawURL)
	if perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, "", fmt.Errorf("refusing to download non-http image URL %q", rawURL)
	}
	ctx, cancel := context.WithTimeout(ctx, g.timeout())
	defer cancel()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := g.client().Do(hreq)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	data, err = readCapped(resp.Body, g.maxBytes())
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, "", fmt.Errorf("image download %s: %s", resp.Status, apiMessage(data))
	}
	return data, strings.TrimSpace(strings.Split(resp.Header.Get("content-type"), ";")[0]), nil
}

// readCapped reads at most max bytes. A body past the cap is an ERROR rather
// than a silently truncated image: half a PNG is not a result.
func readCapped(r io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("the response exceeded the %d-byte cap (raise imageProviders.maxBytes to accept it)", max)
	}
	return data, nil
}

// apiMessage extracts the provider's own error text so a 4xx is actionable
// instead of an opaque status line. Both OpenAI and Gemini nest it under
// "error"; anything else is quoted raw, capped.
func apiMessage(body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	raw := strings.TrimSpace(string(body))
	if raw == "" {
		return "no error body"
	}
	if len(raw) > maxErrBody {
		raw = raw[:maxErrBody] + "…"
	}
	return raw
}

// decodeImage turns one response image — inline base64, or a URL to fetch —
// into bytes plus a MIME type. baseMIME is the provider's declared type.
func (g *Generator) decodeImage(ctx context.Context, b64, rawURL, baseMIME, revised string) (Image, error) {
	if strings.TrimSpace(b64) != "" {
		data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
		if err != nil {
			return Image{}, fmt.Errorf("decoding the inline image: %w", err)
		}
		if len(data) == 0 {
			return Image{}, errors.New("the provider returned an empty image")
		}
		if int64(len(data)) > g.maxBytes() {
			return Image{}, fmt.Errorf("the response exceeded the %d-byte cap (raise imageProviders.maxBytes to accept it)", g.maxBytes())
		}
		return Image{Data: data, MIME: sniffMIME(data, baseMIME), RevisedPrompt: revised}, nil
	}
	if strings.TrimSpace(rawURL) == "" {
		return Image{}, errors.New("the response carried neither image data nor a URL")
	}
	data, mime, err := g.download(ctx, rawURL)
	if err != nil {
		return Image{}, err
	}
	if len(data) == 0 {
		return Image{}, errors.New("the image download returned nothing")
	}
	return Image{Data: data, MIME: sniffMIME(data, mime), URL: rawURL, RevisedPrompt: revised}, nil
}

// sniffMIME prefers the magic bytes over the provider's claim: the blob's
// MIME is what a viewer will actually get.
func sniffMIME(data []byte, declared string) string {
	if m := http.DetectContentType(data[:min(len(data), 512)]); strings.HasPrefix(m, "image/") {
		return m
	}
	if declared != "" && strings.HasPrefix(declared, "image/") {
		return declared
	}
	return "image/png"
}
