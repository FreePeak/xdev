package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/FreePeak/xdev/internal/tool"
)

// ask.timeout is a layered scalar: absent → the ask tool's default
// headless wait, overlay → that many seconds. The strict decoder must
// accept the new key (an unknown key is an error).
func TestAskTimeoutLayersAndDecodes(t *testing.T) {
	cwd := t.TempDir()
	s, err := LoadSettings(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.AskTimeout(); got != tool.DefaultAskTimeout {
		t.Fatalf("default ask timeout = %v, want %v", got, tool.DefaultAskTimeout)
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
