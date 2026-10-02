package typesafe

import (
	"testing"
	"time"
)

func TestConfigIdempotent(t *testing.T) {
	t.Setenv("TYPESAFE_BASE_URL", "")
	t.Setenv("TYPESAFE_API_KEY", "")
	t.Setenv("LAYA_API_KEY", "")
	s := Settings{APIKey: "k"}
	if s.Config().Model != DefaultModel {
		t.Fatalf("model: want %s, got %s", DefaultModel, s.Config().Model)
	}
}

func TestConfigEnvironmentFallbacks(t *testing.T) {
	t.Setenv("TYPESAFE_BASE_URL", "")
	t.Setenv("TYPESAFE_API_KEY", "hosted-key")
	t.Setenv("LAYA_API_KEY", "")
	c := Settings{}.Config()
	if c.BaseURL != "" {
		t.Fatalf("default base URL: got %q", c.BaseURL)
	}
	if c.APIKey != "hosted-key" {
		t.Fatalf("hosted API key: got %q", c.APIKey)
	}
	if c.Model != DefaultModel {
		t.Fatalf("hosted model: %q", c.Model)
	}

	t.Setenv("TYPESAFE_BASE_URL", "http://127.0.0.1:8000")
	c = Settings{}.Config()
	if c.BaseURL != "http://127.0.0.1:8000" || c.APIKey != "" {
		t.Fatalf("local fallback: %+v", c)
	}
	t.Setenv("LAYA_API_KEY", "local-key")
	if got := (Settings{}).Config().APIKey; got != "local-key" {
		t.Fatalf("local API key: got %q", got)
	}
	if got := (Settings{Timeout: time.Second}).Config().Timeout; got != time.Second {
		t.Fatalf("explicit timeout changed: %s", got)
	}

	// A custom non-hosted endpoint is treated as a local/Laya-compatible
	// router: it uses LAYA_API_KEY and leaves the model unset for routing.
	t.Setenv("TYPESAFE_BASE_URL", "https://typesafe-staging.example")
	t.Setenv("LAYA_API_KEY", "")
	c = Settings{}.Config()
	if c.APIKey != "" || c.Model != "" {
		t.Fatalf("custom endpoint fallback: %+v", c)
	}
}
