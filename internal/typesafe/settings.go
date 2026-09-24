package typesafe

import (
	"net"
	"net/url"
	"os"
	"strings"
	"time"
)

// Settings is the typesafe config block: API root, optional API key,
// model, and request timeout. It is owned here (not in internal/config)
// for the same reason WebSearchSettings lives in internal/websearch.
type Settings struct {
	// BaseURL optionally overrides the API root. Empty uses
	// TYPESAFE_BASE_URL when set, otherwise the hosted TypeSafe API.
	BaseURL string `yaml:"baseUrl"`
	// APIKey optionally holds the bearer token for POST /v1/systemone.
	// Empty is valid for a local Laya sidecar that does not require auth.
	APIKey string `yaml:"apiKey"`
	// Model is the System One model that handles the request.
	// "jev-latest" is the hosted default; Laya accepts "english",
	// "multilingual", or "typed-decisions".
	Model string `yaml:"model"`
	// Timeout bounds one request.
	Timeout time.Duration `yaml:"timeout"`
}

// Config returns settings with environment fallbacks and defaults. Local
// endpoints use LAYA_API_KEY when present and never inherit the hosted
// TYPESAFE_API_KEY implicitly. Non-local custom endpoints retain the historic
// TYPESAFE_API_KEY fallback, which also supports TypeSafe staging gateways.
func (s Settings) Config() Settings {
	if strings.TrimSpace(s.BaseURL) == "" {
		s.BaseURL = os.Getenv("TYPESAFE_BASE_URL")
	}
	if strings.TrimSpace(s.APIKey) == "" {
		if localEndpoint(s.BaseURL) {
			s.APIKey = os.Getenv("LAYA_API_KEY")
		} else {
			s.APIKey = os.Getenv("TYPESAFE_API_KEY")
		}
	}
	if s.Model == "" {
		s.Model = DefaultModel
	}
	if s.Timeout <= 0 {
		s.Timeout = DefaultTimeout
	}
	return s
}

func localEndpoint(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
