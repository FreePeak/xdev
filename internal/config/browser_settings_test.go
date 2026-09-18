package config

// Browser settings block (M13 #50): the browser tool's endpoint/timeout keys
// must layer like every other group and a typo'd endpoint must fail at load.

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	// autolaunch defaults on and an explicit false survives the load; the
	// profile dir is always filled in, so the tool can launch when asked.
	if !got.AutolaunchOn() {
		t.Error("browser.autolaunch must default on")
	}
	if got.IdleTimeoutOn() != 300*time.Second {
		t.Errorf("browser.idleExit default = %s, want 5m", got.IdleTimeoutOn())
	}
	if got.ProfileDir == "" {
		t.Error("BrowserConfig must resolve a launch profile dir")
	}
	var absent Settings
	if absent.BrowserConfig().CDPURL != "" {
		t.Error("an absent browser block must stay empty (tool default applies)")
	}
	if !absent.BrowserConfig().AutolaunchOn() {
		t.Error("an absent browser block must autolaunch")
	}

	attachOnly := writeFile(t, filepath.Join(t.TempDir(), "attach.yml"), "browser:\n  autolaunch: false\n")
	s, err = LoadSettings(cwd, []string{attachOnly})
	if err != nil {
		t.Fatal(err)
	}

	// idleExit layers like the rest: a value sets it, 0 means "keep the
	// browser for the session".
	idle := writeFile(t, filepath.Join(t.TempDir(), "idle.yml"), "browser:\n  idleExit: 30\n")
	s, err = LoadSettings(cwd, []string{idle})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.BrowserConfig().IdleTimeoutOn(); got != 30*time.Second {
		t.Errorf("browser.idleExit: 30 gives %s, want 30s", got)
	}
	never := writeFile(t, filepath.Join(t.TempDir(), "never.yml"), "browser:\n  idleExit: 0\n")
	s, err = LoadSettings(cwd, []string{never})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.BrowserConfig().IdleTimeoutOn(); got != 0 {
		t.Errorf("browser.idleExit: 0 gives %s, want the idle exit off", got)
	}
	if !s.BrowserConfig().AutolaunchOn() {
		t.Error("idleExit must not disturb autolaunch")
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
