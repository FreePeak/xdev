package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ask.timeout is a layered scalar: absent → the ask tool's default
// headless wait (60s), overlay → that many seconds. The strict decoder
// must accept the new key (an unknown key is an error), and the default
// is pinned by number: it is the wait a human's question buys, so moving
// the constant must fail here rather than in a stalled CI run.
func TestAskTimeoutLayersAndDecodes(t *testing.T) {
	// These assert the SHIPPED DEFAULTS, so they must read the shipped
	// defaults — not the developer's own ~/.xdev/agent/config.yml, which
	// LoadSettings folds in as the global layer. Both tests failed on any
	// machine whose real config sets ask.* (autoAnswer: true,
	// timeout: 300 is exactly what those keys are for), which is a test
	// that measures the developer's machine, not the code. Every other
	// config test in this package already sets this.
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
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
	t.Setenv("XDEV_AGENT_DIR", t.TempDir()) // same reason as above: this asserts the default
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
