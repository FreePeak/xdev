package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTypeSafeSettings(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TYPESAFE_BASE_URL", "http://env.example")
	t.Setenv("TYPESAFE_API_KEY", "hosted-key")
	t.Setenv("LAYA_API_KEY", "")
	dir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dir)
	cwd := t.TempDir()
	writeFile(t, GlobalSettingsPath(), `typesafe:
  baseUrl: ${TYPESAFE_BASE_URL}
  apiKey: ${TYPESAFE_API_KEY}
  model: english
  timeout: 3s
`)

	s, err := LoadSettings(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.TypeSafeConfig()
	if cfg.BaseURL != "http://env.example" || cfg.APIKey != "hosted-key" || cfg.Model != "english" || cfg.Timeout != 3*time.Second {
		t.Fatalf("TypeSafeConfig = %+v", cfg)
	}
}

func TestTypeSafeLocalSettings(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	t.Setenv("TYPESAFE_BASE_URL", "")
	t.Setenv("TYPESAFE_API_KEY", "")
	t.Setenv("LAYA_API_KEY", "local-key")
	cwd := t.TempDir()
	writeFile(t, GlobalSettingsPath(), `typesafe:
  baseUrl: http://127.0.0.1:8000
  model: typed-decisions
`)

	s, err := LoadSettings(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.TypeSafeConfig()
	if cfg.BaseURL != "http://127.0.0.1:8000" || cfg.APIKey != "local-key" || cfg.Model != "typed-decisions" {
		t.Fatalf("local TypeSafeConfig = %+v", cfg)
	}
}

func TestTypeSafeOverlayKeepsUnsetFields(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	t.Setenv("TYPESAFE_BASE_URL", "")
	t.Setenv("TYPESAFE_API_KEY", "")
	t.Setenv("LAYA_API_KEY", "")
	cwd := t.TempDir()
	writeFile(t, GlobalSettingsPath(), `typesafe:
  baseUrl: http://127.0.0.1:8000
  model: english
  timeout: 3s
`)
	overlay := writeFile(t, filepath.Join(t.TempDir(), "overlay.yml"), `typesafe:
  model: typed-decisions
`)

	s, err := LoadSettings(cwd, []string{overlay})
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.TypeSafeConfig()
	if cfg.BaseURL != "http://127.0.0.1:8000" || cfg.Model != "typed-decisions" || cfg.Timeout != 3*time.Second {
		t.Fatalf("TypeSafeConfig after overlay = %+v", cfg)
	}
}

func TestTypeSafeEndpointOverrideDropsOldKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	t.Setenv("TYPESAFE_BASE_URL", "")
	t.Setenv("TYPESAFE_API_KEY", "")
	t.Setenv("LAYA_API_KEY", "")
	cwd := t.TempDir()
	writeFile(t, GlobalSettingsPath(), `typesafe:
  baseUrl: https://typesafe-staging.example
  apiKey: hosted-key
`)
	overlay := writeFile(t, filepath.Join(t.TempDir(), "local.yml"), `typesafe:
  baseUrl: http://127.0.0.1:8000
`)

	s, err := LoadSettings(cwd, []string{overlay})
	if err != nil {
		t.Fatal(err)
	}
	if cfg := s.TypeSafeConfig(); cfg.APIKey != "" {
		t.Fatalf("old hosted key crossed to local endpoint: %q", cfg.APIKey)
	}
}

func TestTypeSafeNegativeTimeoutRejected(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	writeFile(t, GlobalSettingsPath(), `typesafe:
  timeout: -1s
`)
	if _, err := LoadSettings(t.TempDir(), nil); err == nil {
		t.Fatal("negative typesafe timeout accepted")
	}
}

// TestTypeSafeFanoutOptIn pins the default: no fanout block unless the
// user asks for it, and the flag rides the same block as the endpoint
// (so `typesafe.fanout` is the whole surface, no second config group).
func TestTypeSafeFanoutOptIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	writeFile(t, GlobalSettingsPath(), `typesafe:
  fanout: true
`)

	s, err := LoadSettings(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !s.TypeSafe.Fanout {
		t.Fatal("typesafe.fanout: true did not read through to Settings")
	}

	writeFile(t, GlobalSettingsPath(), "typesafe: {}\n")
	s, err = LoadSettings(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.TypeSafe.Fanout {
		t.Fatal("fanout defaulted on with no setting")
	}
}

// TestTypeSafeFanoutListRow pins the discoverability path: `config list`
// has to name the opt-in, or the only way to find it is reading the PRD.
func TestTypeSafeFanoutListRow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	s, err := LoadSettings(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	row := fmt.Sprintf("typesafe.fanout %v", s.TypeSafe.Fanout)
	if !strings.Contains(strings.Join(List(s, "x"), "\n"), row) {
		t.Fatalf("config list is missing the %q row", row)
	}
}
