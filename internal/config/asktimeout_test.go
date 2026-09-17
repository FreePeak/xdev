package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
