package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Both ask tests below assert the ask DEFAULTS, so they must not see the
// developer's own ~/.xdev/agent/config.yml — LoadSettings layers the user
// file in, and a user who set `ask.timeout: 300` or `ask.autoAnswer: true`
// (a perfectly legal config) failed two tests that only ever run on a
// developer machine. Every other settings test in this package isolates
// HOME for exactly this reason; these two were the only ones that did not,
// which is why `go test ./internal/config` failed here and passed in CI.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "config-test-home")
	if err != nil {
		panic(err)
	}
	// HOME is the lever (GlobalSettingsPath falls back to
	// $HOME/.xdev/agent), and a test that wants its own directory still
	// wins by calling t.Setenv — which restores whatever is set here.
	os.Setenv("HOME", dir)
	// XDEV_AGENT_DIR overrides HOME outright, so a developer who exports
	// one would otherwise keep reading their real config through it.
	os.Unsetenv("XDEV_AGENT_DIR")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// ask.timeout is a layered scalar: absent → the ask tool's default
// headless wait (60s), overlay → that many seconds. The strict decoder
// must accept the new key (an unknown key is an error), and the default
// is pinned by number: it is the wait a human's question buys, so moving
// the constant must fail here rather than in a stalled CI run.
func TestAskTimeoutLayersAndDecodes(t *testing.T) {
	cwd := t.TempDir()
	s, err := LoadSettings(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.AskTimeout(); got != 60*time.Second {
		t.Fatalf("default ask timeout = %v, want 60s", got)
	}

	overlay := filepath.Join(t.TempDir(), "ask.yml")
	if err := os.WriteFile(overlay, []byte("ask:\n  timeout: 5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err = LoadSettings(cwd, []string{overlay})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.AskTimeout(); got.Seconds() != 5 {
		t.Fatalf("overlay ask timeout = %v, want 5s", got)
	}
}

// ask.autoAnswer is the opt-in for the old behavior: off, an unanswered
// question waits for a human instead of being answered from its
// recommendation. ask.timeout alone must never turn it on — a user who
// raised the wait did not ask to be auto-answered.
func TestAskAutoAnswerDefaultsOffAndLayersOn(t *testing.T) {
	cwd := t.TempDir()
	s, err := LoadSettings(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.AskAutoAnswerOn() {
		t.Fatal("auto-answer must be off by default")
	}
	var nilSettings *Settings
	if nilSettings.AskAutoAnswerOn() {
		t.Fatal("nil settings must report auto-answer off")
	}

	// timeout without autoAnswer: the wait is configured, the policy is not.
	only := filepath.Join(t.TempDir(), "ask.yml")
	if err := os.WriteFile(only, []byte("ask:\n  timeout: 5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err = LoadSettings(cwd, []string{only})
	if err != nil {
		t.Fatal(err)
	}
	if s.AskAutoAnswerOn() {
		t.Fatal("ask.timeout must not enable auto-answer")
	}

	on := filepath.Join(t.TempDir(), "ask.yml")
	if err := os.WriteFile(on, []byte("ask:\n  autoAnswer: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err = LoadSettings(cwd, []string{on})
	if err != nil {
		t.Fatal(err)
	}
	if !s.AskAutoAnswerOn() {
		t.Fatal("ask.autoAnswer: true did not layer on")
	}
	if !strings.Contains(strings.Join(List(s, "x"), "\n"), "ask.autoAnswer true") {
		t.Fatal("config list must show the opt-in")
	}
}
