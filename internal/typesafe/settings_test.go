package typesafe

import (
	"testing"
	"time"
)

func TestConfigIdempotent(t *testing.T) {
	t.Setenv("DECISION_BASE_URL", "")
	t.Setenv("DECISION_API_KEY", "")
	t.Setenv("DECISION_KEY_FILE", "")
	s := Settings{APIKey: "k"}
	if s.Config().APIKey != "k" {
		t.Fatalf("api key: got %q", s.Config().APIKey)
	}
	if s.Config().Model != DefaultModel {
		t.Fatalf("model: want %s, got %s", DefaultModel, s.Config().Model)
	}
}

func TestConfigEnvironmentFallbacks(t *testing.T) {
	t.Setenv("DECISION_BASE_URL", "")
	t.Setenv("DECISION_API_KEY", "service-key")
	t.Setenv("DECISION_KEY_FILE", "")
	c := Settings{}.Config()
	if c.BaseURL != "" {
		t.Fatalf("default base URL: got %q", c.BaseURL)
	}
	if c.APIKey != "service-key" {
		t.Fatalf("api key: got %q", c.APIKey)
	}
	if c.Model != DefaultModel {
		t.Fatalf("model: %q", c.Model)
	}

	t.Setenv("DECISION_BASE_URL", "http://127.0.0.1:8000")
	c = Settings{}.Config()
	if c.BaseURL != "http://127.0.0.1:8000" {
		t.Fatalf("base URL from the environment: %+v", c)
	}
	if got := (Settings{Timeout: time.Second}).Config().Timeout; got != time.Second {
		t.Fatalf("explicit timeout changed: %s", got)
	}
}
