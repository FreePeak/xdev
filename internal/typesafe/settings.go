package typesafe

import (
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
	// Hosted Jev defaults to "jev-latest"; a custom Laya endpoint
	// auto-routes when this is empty.
	Model string `yaml:"model"`
	// Timeout bounds one request.
	Timeout time.Duration `yaml:"timeout"`
}

// Config returns settings with environment fallbacks and defaults. A custom
// endpoint uses LAYA_API_KEY, never the hosted TYPESAFE_API_KEY implicitly;
// this keeps a configured Laya gateway from receiving a TypeSafe secret.
func (s Settings) Config() Settings {
	if strings.TrimSpace(s.BaseURL) == "" {
		s.BaseURL = os.Getenv("TYPESAFE_BASE_URL")
	}
	hosted := hostedEndpoint(s.BaseURL)
	if strings.TrimSpace(s.APIKey) == "" {
		if hosted {
			s.APIKey = os.Getenv("TYPESAFE_API_KEY")
		} else {
			s.APIKey = os.Getenv("LAYA_API_KEY")
		}
	}
	if s.Model == "" && hosted {
		s.Model = DefaultModel
	}
	if s.Timeout <= 0 {
		s.Timeout = DefaultTimeout
	}
	return s
}

func hostedEndpoint(raw string) bool {
	base := strings.TrimRight(strings.TrimSpace(raw), "/")
	return base == "" || base == defaultBaseURL
}
