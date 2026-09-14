package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStatusLineAndColorBlindSettings pins the M12 theme keys end to end
// through the layering: the list form, the comma-separated scalar `xdev
// config set` writes (which must never quarantine the user's config), and
// the colorBlindMode boolean.
func TestStatusLineAndColorBlindSettings(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir()) // keep the user's real config out
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	doc := "theme: groknight\ncolorBlindMode: true\nstatusLine:\n  segments: [model, tokens, theme]\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _, err := readSettingsFile(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if !s.ColorBlindMode {
		t.Fatal("colorBlindMode must decode")
	}
	if got := s.StatusLineSegments(); strings.Join(got, ",") != "model,tokens,theme" {
		t.Fatalf("segments = %v", got)
	}
	listed := strings.Join(List(s, path), "\n")
	for _, want := range []string{"colorBlindMode true", "statusLine.segments model,tokens,theme"} {
		if !strings.Contains(listed, want) {
			t.Fatalf("config list missing %q:\n%s", want, listed)
		}
	}

	// The scalar form `xdev config set statusLine.segments model,tokens`
	// writes (yamlScalar turns "a,b" into a string) must decode, not
	// quarantine the file as broken.
	scalar := filepath.Join(dir, "scalar.yml")
	if err := os.WriteFile(scalar, []byte("statusLine:\n  segments: model,tokens\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s2, _, err := readSettingsFile(scalar, true)
	if err != nil {
		t.Fatalf("scalar segments must decode: %v", err)
	}
	if got := s2.StatusLineSegments(); strings.Join(got, ",") != "model,tokens" {
		t.Fatalf("scalar segments = %v", got)
	}
	if _, err := os.Stat(scalar); err != nil {
		t.Fatalf("config file must not be quarantined: %v", err)
	}

	// Defaults: unset means the TUI's shipped layout (nil) and no remap.
	def, err := LoadSettings(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if def.StatusLineSegments() != nil {
		t.Fatalf("default segments = %v", def.StatusLineSegments())
	}
	if def.ColorBlindMode {
		t.Fatal("colorBlindMode must default off")
	}
}

// TestMergeStatusLineLayer: a later layer that says nothing about statusLine
// leaves the earlier list alone; one that lists segments replaces it.
func TestMergeStatusLineLayer(t *testing.T) {
	base := defaultSettings()
	base.StatusLine = &StatusLineSettings{Segments: StringList{"model", "tokens"}}
	if err := base.merge(&Settings{}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(base.StatusLineSegments(), ",") != "model,tokens" {
		t.Fatalf("silent layer dropped segments: %v", base.StatusLineSegments())
	}
	if err := base.merge(&Settings{StatusLine: &StatusLineSettings{Segments: StringList{"cost"}}, ColorBlindMode: true}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(base.StatusLineSegments(), ",") != "cost" {
		t.Fatalf("layer must replace the list: %v", base.StatusLineSegments())
	}
	if !base.ColorBlindMode {
		t.Fatal("layer must be able to switch colorBlindMode on")
	}
	if err := base.merge(&Settings{}); err != nil {
		t.Fatal(err)
	}
	if !base.ColorBlindMode {
		t.Fatal("a silent layer must not switch colorBlindMode off")
	}
}
