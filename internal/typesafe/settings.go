package typesafe

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Settings is the typesafe config block: API root, optional API key,
// model, and request timeout. It is owned here (not in internal/config)
// for the same reason WebSearchSettings lives in internal/websearch.
type Settings struct {
	// BaseURL optionally overrides the API root. Empty uses
	// DECISION_BASE_URL when set, otherwise the decision service.
	BaseURL string `yaml:"baseUrl"`
	// APIKey optionally holds the bearer token for POST /v1/systemone.
	// Empty is valid for an endpoint that does not require auth.
	APIKey string `yaml:"apiKey"`
	// Model is the System One model that handles the request.
	// The decision service serves the checkpoint it has loaded and ignores
	// this, so it is empty by default.
	Model string `yaml:"model"`
	// Timeout bounds one request.
	Timeout time.Duration `yaml:"timeout"`
	// Fanout adds parallel-subagent guidance to the system prompt
	// (see agent.BuildFanoutGuidance): the LLM itself decides per turn
	// whether the work is independent enough to fan out. Off by
	// default — a turn that serializes anyway pays the extra prompt
	// tokens and gains nothing.
	Fanout bool `yaml:"fanout"`
}

// Config returns settings with environment fallbacks and defaults.
//
// The only backend is the decision service, so there is no hosted endpoint to
// fall back to and no second key to keep straight: the bearer comes from
// DECISION_API_KEY, or from the key file DECISION_KEY_FILE points at.
func (s Settings) Config() Settings {
	if strings.TrimSpace(s.BaseURL) == "" {
		s.BaseURL = os.Getenv("DECISION_BASE_URL")
	}
	if strings.TrimSpace(s.APIKey) == "" {
		s.APIKey = os.Getenv("DECISION_API_KEY")
	}
	if strings.TrimSpace(s.APIKey) == "" {
		s.APIKey = decisionKeyFile()
	}
	if s.Timeout <= 0 {
		s.Timeout = DefaultTimeout
	}
	return s
}

// decisionKeyFile reads the bearer token from the key file. The token is a
// secret, so its normal home is a file with owner-only permissions rather
// than the environment of a shell.
func decisionKeyFile() string {
	path := strings.TrimSpace(os.Getenv("DECISION_KEY_FILE"))
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		path = filepath.Join(home, ".config", "decision", "api_key")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}
