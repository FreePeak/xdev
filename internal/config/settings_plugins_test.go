package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestPluginsMarketplacesSetting pins the additive plugins.marketplaces key
// (M13 #53): absent by default, replaced by a layer that declares it.
func TestPluginsMarketplacesSetting(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir()) // keep the user's real config out
	cwd := t.TempDir()

	base, err := LoadSettings(cwd, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if base.Plugins.Marketplaces != nil {
		t.Fatalf("plugins.marketplaces = %v, want nil by default", base.Plugins.Marketplaces)
	}

	overlay := filepath.Join(t.TempDir(), "overlay.yml")
	if err := os.WriteFile(overlay, []byte(
		"plugins:\n  marketplaces:\n    - /opt/catalog\n    - https://example.com/catalog.git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings(cwd, []string{overlay})
	if err != nil {
		t.Fatalf("load overlay: %v", err)
	}
	want := []string{"/opt/catalog", "https://example.com/catalog.git"}
	if !reflect.DeepEqual(s.Plugins.Marketplaces, want) {
		t.Fatalf("plugins.marketplaces = %v, want %v", s.Plugins.Marketplaces, want)
	}
}
