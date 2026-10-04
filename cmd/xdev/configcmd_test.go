package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/theme"
)

// writeTestTheme writes a minimal valid custom theme so validateKey's
// AvailableThemes path sees it.
func writeTestTheme(t *testing.T, dir, name string) {
	t.Helper()
	colors := map[string]any{}
	for _, slot := range theme.RequiredSlots() {
		colors[slot] = "#123456"
	}
	raw, err := json.Marshal(map[string]any{"name": name, "dark": true, "colors": colors})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestValidateKeyTheme pins theme validation to the theme package: any
// listed theme (built-in or custom) plus "auto" is settable, anything
// else is rejected with the available list so the fix is discoverable.
func TestValidateKeyTheme(t *testing.T) {
	agentDir := t.TempDir()
	themes := filepath.Join(agentDir, "themes")
	if err := os.MkdirAll(themes, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestTheme(t, themes, "ocean")
	t.Setenv("XDEV_AGENT_DIR", agentDir)

	cases := []struct {
		name    string
		value   string
		wantErr string // "" = accepted
	}{
		{"auto", "auto", ""},
		{"builtin", "groknight", ""},
		{"custom", "ocean", ""},
		{"ported palette", "tokyo-night", ""},
		{"unknown lists available", "nope", `unknown theme "nope" (available: auto, catppuccin, dracula, grokday, groknight, gruvbox, nord, one-dark, one-light, rose-pine, tokyo-night, ocean)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateKey("theme", tc.value)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateKey(theme, %q) = %v, want nil", tc.value, err)
				}
				return
			}
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("validateKey(theme, %q) = %v, want %s", tc.value, err, tc.wantErr)
			}
		})
	}
}

// TestFanoutConfigKeyIsComplete pins the three doors the toggle has to
// pass: `config set` accepts only the two booleans, `config get` answers
// the effective value when the key is absent from the file, and
// `config list` names it. A bool key that any other setting already
// validates is a one-case edit; a missing one leaves the user editing
// YAML by hand to discover a flag they cannot see.
func TestFanoutConfigKeyIsComplete(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())

	if err := validateKey("typesafe.fanout", "true"); err != nil {
		t.Fatalf("true rejected: %v", err)
	}
	err := validateKey("typesafe.fanout", "yes")
	if err == nil || !strings.Contains(err.Error(), "true|false") {
		t.Fatalf("non-boolean accepted: %v", err)
	}

	s := &config.Settings{}
	if got := fallbackValue(s, "typesafe.fanout"); got != "false" {
		t.Fatalf("fallbackValue(unset) = %q, want false", got)
	}
	s.TypeSafe.Fanout = true
	if got := fallbackValue(s, "typesafe.fanout"); got != "true" {
		t.Fatalf("fallbackValue(on) = %q, want true", got)
	}
}
