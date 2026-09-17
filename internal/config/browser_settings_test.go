package config

// Browser settings block (M13 #50): the browser tool's endpoint/timeout keys
// must layer like every other group and a typo'd endpoint must fail at load.

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestBrowserSettingsLayer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()

	writeFile(t, GlobalSettingsPath(), "browser:\n  cdpUrl: http://127.0.0.1:9222\n")
	overlay := writeFile(t, filepath.Join(t.TempDir(), "extra.yml"), "browser:\n  cdpUrl: 127.0.0.1:9333\n  timeout: 45\n")

	s, err := LoadSettings(cwd, []string{overlay})
	if err != nil {
		t.Fatal(err)
	}
	got := s.BrowserConfig()
	if got.CDPURL != "127.0.0.1:9333" {
		t.Errorf("overlay should win cdpUrl: %q", got.CDPURL)
	}
	if got.Timeout != 45 {
		t.Errorf("browser.timeout = %d, want 45", got.Timeout)
	}
	var absent Settings
	if absent.BrowserConfig().CDPURL != "" {
		t.Error("an absent browser block must stay empty (tool default applies)")
	}
}

func TestBrowserSettingsRejectsBadEndpoint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	overlay := writeFile(t, filepath.Join(t.TempDir(), "extra.yml"), "browser:\n  cdpUrl: ftp://127.0.0.1:9222\n")
	if _, err := LoadSettings(t.TempDir(), []string{overlay}); err == nil || !strings.Contains(err.Error(), "browser.cdpUrl") {
		t.Fatalf("err = %v, want a browser.cdpUrl rejection", err)
	}
	overlay = writeFile(t, filepath.Join(t.TempDir(), "extra2.yml"), "browser:\n  cdpUrl: nonsense\n")
	if _, err := LoadSettings(t.TempDir(), []string{overlay}); err == nil || !strings.Contains(err.Error(), "browser.cdpUrl") {
		t.Fatalf("err = %v, want a browser.cdpUrl rejection", err)
	}
}
