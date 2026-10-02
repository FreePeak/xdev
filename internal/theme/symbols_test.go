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

// TestHUDIconIsAlwaysOneCharacter pins the overlap fix. The status row is a
// fixed-cell grid: the icon is written into ONE cell, so a glyph the terminal
// paints two cells wide spills its own second column over the first digit of
// the number beside it, and "⏱9s" reads as a clock sitting on the 9.
//
// Two parts, and both are needed. The shipped gauge is U+23F2 STOPWATCH rather
// than U+23F1 (East Asian Ambiguous, drawn double-width by most terminals), and
// the accessor cuts any override to a single character so a theme cannot put
// the collision back.
func TestHUDIconIsAlwaysOneCharacter(t *testing.T) {
	th := Load("groknight")
	for _, key := range []string{HUDIconGauge, HUDIconDatabase, HUDIconTool} {
		got := th.HUDIcon(key)
		if n := len([]rune(got)); n != 1 {
			t.Errorf("%s icon %q is %d characters, want exactly 1", key, got, n)
		}
	}
	// The gauge in particular must not be U+23F1: that is the glyph a
	// terminal draws two cells wide, and it is what this change removed.
	if g := th.HUDIcon(HUDIconGauge); g == "⏱" {
		t.Errorf("the gauge is U+23F1 again (%q) — that is the two-cell overlap", g)
	}

	// A theme override is a free string, so the accessor has to cut it.
	two, err := ParseTheme([]byte(validThemeJSON(t, func(doc map[string]any) {
		doc["symbols"] = map[string]any{"overrides": map[string]any{"hud.gauge": "⏱⏱"}}
	})), "custom")
	if err != nil {
		t.Fatal(err)
	}
	if got := two.HUDIcon(HUDIconGauge); len([]rune(got)) != 1 {
		t.Errorf("a two-character override reached the row: %q", got)
	}
}
