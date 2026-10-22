package theme

import "testing"

// TestStatusGlyphSet pins omp's status glyph set (theme__symbols.ts): the
// shapes a reader scans a transcript's left column for. The values are
// PINNED LITERALS — a glyph that drifts is a glyph a reader stops
// recognising, and the shape is the information, not the hue.
//
// The set is the whole of omp's: ✔ ok, ✘ error, ⏳ pending, ● enabled, ○
// shadowed. xdev's old ◈ / ✗ / ● are gone: ✗ is not a glyph omp draws, and a
// filled circle beside a tool's name read as "enabled" rather than "done".
func TestStatusGlyphSet(t *testing.T) {
	th := Load("groknight")
	want := map[string]string{
		StatusGlyphSuccess:  "✔",
		StatusGlyphError:    "✘",
		StatusGlyphPending:  "⏳",
		StatusGlyphEnabled:  "●",
		StatusGlyphShadowed: "○",
	}
	for key, glyph := range want {
		if got := th.StatusGlyph(key); got != glyph {
			t.Errorf("status glyph %q = %q, want %q", key, got, glyph)
		}
	}
	// An unknown key is not an error and not a glyph: the accessor is asked
	// only by the states that own a key.
	if got := th.StatusGlyph("nope"); got != "" {
		t.Errorf("unknown key = %q, want empty", got)
	}
}

// TestStatusGlyphAsciiPreset: the ascii preset substitutes bracketed forms,
// because a terminal that cannot draw the glyph would rather read "[x]" than
// paint a tofu box.
func TestStatusGlyphAsciiPreset(t *testing.T) {
	ascii, err := ParseTheme([]byte(validThemeJSON(t, func(doc map[string]any) {
		doc["symbols"] = map[string]any{"preset": "ascii"}
	})), "custom")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		StatusGlyphSuccess:  "[+]",
		StatusGlyphError:    "[x]",
		StatusGlyphPending:  "[~]",
		StatusGlyphEnabled:  "[*]",
		StatusGlyphShadowed: "[ ]",
	}
	for key, glyph := range want {
		if got := ascii.StatusGlyph(key); got != glyph {
			t.Errorf("ascii status glyph %q = %q, want %q", key, got, glyph)
		}
	}
}

// TestStatusSpinnerFrames pins the STATUS spinner to omp's circle-braille
// set, and keeps it apart from the activity spinner: a status says "this is
// still running" and an activity says "this is still moving", and omp keeps
// the two in two different frame sets.
func TestStatusSpinnerFrames(t *testing.T) {
	th := Load("groknight")
	got := th.StatusSpinnerFrames()
	want := []string{"⣾", "⣽", "⣻", "⢿", "⡿", "⣟", "⣯", "⣷"}
	if len(got) != len(want) {
		t.Fatalf("status frames = %v, want %v", got, want)
	}
	for i, f := range want {
		if got[i] != f {
			t.Fatalf("status frames = %v, want %v", got, want)
		}
	}
	// The activity spinner keeps the braille set it always had.
	act := th.SpinnerFrames()
	if len(act) == 0 || act[0] != "⠋" {
		t.Errorf("activity frames = %v, want the braille set", act)
	}
}

// TestStatusSpinnerFramesAsciiPreset: a terminal without braille falls back
// to the ascii frames for the status spinner too.
func TestStatusSpinnerFramesAsciiPreset(t *testing.T) {
	ascii, err := ParseTheme([]byte(validThemeJSON(t, func(doc map[string]any) {
		doc["symbols"] = map[string]any{"preset": "ascii"}
	})), "custom")
	if err != nil {
		t.Fatal(err)
	}
	got := ascii.StatusSpinnerFrames()
	if len(got) != 4 || got[0] != "|" {
		t.Errorf("ascii status frames = %v, want |/-\\", got)
	}
}

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
