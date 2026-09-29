package theme

import "testing"

// TestHUDIconDefaults: the status row's icons are dsh's composer pills in
// one cell each (StatsPills.tsx's IconGaugeOutline / IconDatabaseOutline).
// A built-in theme carries no Symbols, so it must still answer — a missing
// icon would paint a tofu box on the row of every default install.
func TestHUDIconDefaults(t *testing.T) {
	th := Load("groknight")
	if th == nil {
		t.Fatal("groknight must load")
	}
	if got := th.HUDIcon(HUDIconGauge); got == "" {
		t.Error("gauge icon must never be empty")
	}
	if got := th.HUDIcon(HUDIconDatabase); got == "" {
		t.Error("database icon must never be empty")
	}
	if got := th.HUDIcon(HUDIconTool); got == "" {
		t.Error("tool icon must never be empty")
	}
	// An unknown key is not an error and not a glyph: the accessor is asked
	// only by the segments that own a key.
	if got := th.HUDIcon("nope"); got != "" {
		t.Errorf("unknown key = %q, want empty", got)
	}
}

// TestHUDIconOverrideAndASCII: a theme overrides the glyph under
// symbols.overrides["hud.<key>"], and the ascii preset substitutes a letter
// tag rather than a glyph its terminal cannot draw.
func TestHUDIconOverrideAndASCII(t *testing.T) {
	th, err := ParseTheme([]byte(validThemeJSON(t, func(doc map[string]any) {
		doc["symbols"] = map[string]any{"overrides": map[string]any{"hud.gauge": "G"}}
	})), "custom")
	if err != nil {
		t.Fatal(err)
	}
	if got := th.HUDIcon(HUDIconGauge); got != "G" {
		t.Errorf("override ignored: %q", got)
	}
	if got := th.HUDIcon(HUDIconDatabase); got == "" {
		t.Error("an override of one key must not blank the others")
	}

	ascii, err := ParseTheme([]byte(validThemeJSON(t, func(doc map[string]any) {
		doc["symbols"] = map[string]any{"preset": "ascii"}
	})), "custom")
	if err != nil {
		t.Fatal(err)
	}
	if got := ascii.HUDIcon(HUDIconGauge); len(got) != 1 || got[0] > 127 {
		t.Errorf("ascii preset must use a single ASCII tag, got %q", got)
	}
}
