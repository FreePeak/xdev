package config

import (
	"strings"
	"testing"
)

// TestSettingsWebSearchKeys pins the webSearch layer contract (M13 #48): the
// block survives KnownFields decoding, providers replace wholesale, keys
// merge per provider, and untouched keys survive a later layer.
func TestSettingsWebSearchKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	writeFile(t, GlobalSettingsPath(), `
webSearch:
  providers: [brave]
  timeout: 5s
  maxResults: 5
  apiKeys:
    brave: key-one
    tavily: tvly-one
`)
	s, err := LoadSettings(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.WebSearch.Providers) != 1 || s.WebSearch.Providers[0] != "brave" {
		t.Fatalf("providers = %v", s.WebSearch.Providers)
	}
	if s.WebSearch.Timeout != "5s" {
		t.Fatalf("timeout = %q", s.WebSearch.Timeout)
	}
	if s.WebSearch.MaxResults != 5 {
		t.Fatalf("maxResults = %d", s.WebSearch.MaxResults)
	}
	if s.WebSearch.APIKeys["brave"] != "key-one" || s.WebSearch.APIKeys["tavily"] != "tvly-one" {
		t.Fatalf("apiKeys = %v", s.WebSearch.APIKeys)
	}

	// A later layer replaces the chain wholesale (the arrays-replace rule)
	// while keys and the timeout it never mentions must survive.
	writeFile(t, projectSettingsPath(cwd), `
webSearch:
  providers: [duckduckgo]
`)
	s, err = LoadSettings(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.WebSearch.Providers) != 1 || s.WebSearch.Providers[0] != "duckduckgo" {
		t.Fatalf("providers after project layer = %v", s.WebSearch.Providers)
	}
	if s.WebSearch.Timeout != "5s" || s.WebSearch.APIKeys["tavily"] != "tvly-one" {
		t.Fatalf("project layer lost untouched webSearch keys: %+v", s.WebSearch)
	}

	// The nil-tolerant accessor feeds the registry builder.
	empty := (*Settings)(nil).WebSearchConfig()
	if empty.Timeout != "" || len(empty.Providers) != 0 || len(empty.APIKeys) != 0 {
		t.Fatalf("nil settings must yield the zero block, got %+v", empty)
	}
}

// TestSettingsWebSearchBadTimeoutIsRejected: a typo'd duration must fail the
// load loudly, not silently downgrade the per-provider timeout to the default.
func TestSettingsWebSearchBadTimeoutIsRejected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFile(t, GlobalSettingsPath(), `
webSearch:
  timeout: ten seconds
`)
	_, err := LoadSettings(t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "webSearch.timeout") {
		t.Fatalf("malformed duration accepted: %v", err)
	}
}
