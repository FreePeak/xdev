package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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
		{"unknown lists available", "nope", `unknown theme "nope" (available: auto, grokday, groknight, ocean)`},
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
