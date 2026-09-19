package typesafe

import "time"

// Settings is the typesafe config block: API key, model and
// request timeout. It is owned here (not in internal/config)
// for the same reason WebSearchSettings lives in
// internal/websearch — the registry in internal/tool hands it
// to the tool, so the struct must sit in a package neither
// config nor tool can import (config → agent → tool, config →
// websearch, so typesafe must be config's peer).
type Settings struct {
	// APIKey holds the bearer token for POST /v1/systemone.
	// It is expanded (Resolve) at request time, so ${VAR}
	// works in config and env vars can rotate without a
	// restart. Empty means the tool returns an error
	// instead of a request.
	APIKey string `yaml:"apiKey"`
	// Model is the System One model that handles the request
	// ("jev-latest" is the default).
	Model string `yaml:"model"`
	// Timeout bounds one request.
	Timeout time.Duration `yaml:"timeout"`
}

// Config returns the settings with model defaulted and
// the timeout bounded. Empty APIKey is kept (the
// caller reports it as a missing credential).
func (s Settings) Config() Settings {
	if s.Model == "" {
		s.Model = DefaultModel
	}
	if s.Timeout <= 0 {
		s.Timeout = DefaultTimeout
	}
	return s
}

// DefaultModel is the model the tool sends when the caller omits one.
const DefaultModel = "jev-latest"

// DefaultTimeout bounds a single request to the API.
const DefaultTimeout = 10 * time.Second
