package theme

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validThemeJSON builds a complete theme document. Tests mutate the map
// to exercise validation.
func validThemeJSON(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	colors := map[string]any{}
	for _, slot := range RequiredSlots() {
		colors[slot] = "#123456"
	}
	doc := map[string]any{"name": "custom", "dark": true, "colors": colors}
	if mutate != nil {
		mutate(doc)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestParseThemeComplete(t *testing.T) {
	th, err := ParseTheme([]byte(validThemeJSON(t, nil)), "custom")
	if err != nil {
		t.Fatal(err)
	}
	if th.Name != "custom" || !th.Dark {
		t.Fatalf("theme = %+v", th)
	}
	if th.Get(TextPrimary).R != 0x12 {
		t.Fatalf("slot color = %+v", th.Get(TextPrimary))
	}
}

func TestParseThemeGroupedMissingError(t *testing.T) {
	raw := validThemeJSON(t, func(doc map[string]any) {
		colors := doc["colors"].(map[string]any)
		delete(colors, Text)
		delete(colors, SyntaxComment)
	})
	_, err := ParseTheme([]byte(raw), "custom")
	if err == nil {
		t.Fatal("missing slots must error")
	}
	// One error naming every missing slot (fix in one pass).
	for _, want := range []string{Text, SyntaxComment, "2 missing"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error must name %q: %v", want, err)
		}
	}
}

func TestParseThemeVarsAndCamelCase(t *testing.T) {
	raw := validThemeJSON(t, func(doc map[string]any) {
		doc["vars"] = map[string]any{"bg": "#0a0a0a"}
		colors := doc["colors"].(map[string]any)
		delete(colors, BgBase)
		colors["bgBase"] = "@bg" // camelCase spelling + var reference
	})
	th, err := ParseTheme([]byte(raw), "custom")
	if err != nil {
		t.Fatal(err)
	}
	if th.Get(BgBase).R != 0x0a {
		t.Fatalf("var resolution failed: %+v", th.Get(BgBase))
	}
}

func TestParseThemeColorForms(t *testing.T) {
	raw := validThemeJSON(t, func(doc map[string]any) {
		colors := doc["colors"].(map[string]any)
		colors[BgTerminal] = ""  // terminal default
		colors[GrayDim] = "238"  // 256 index
		colors[Gray] = "#abcdef" // hex
	})
	th, err := ParseTheme([]byte(raw), "custom")
	if err != nil {
		t.Fatal(err)
	}
	if got := th.Get(BgTerminal); got != (Color{}) {
		t.Fatalf("empty color must be the zero Color, got %+v", got)
	}
	if got := th.Get(GrayDim); got.R != got.G || got.G != got.B {
		t.Fatalf("index 238 must be a gray: %+v", got)
	}
}

func TestParseThemeRejectsBadValues(t *testing.T) {
	cases := map[string]string{
		"bad hex":     "#12345",
		"bad index":   "999",
		"unknown var": "@nope",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			raw := validThemeJSON(t, func(doc map[string]any) {
				doc["colors"].(map[string]any)[TextPrimary] = value
			})
			if _, err := ParseTheme([]byte(raw), "custom"); err == nil {
				t.Fatalf("%s must be rejected", name)
			}
		})
	}
}

func TestParseThemeCircularVars(t *testing.T) {
	raw := validThemeJSON(t, func(doc map[string]any) {
		doc["vars"] = map[string]any{"a": "@b", "b": "@a"}
		doc["colors"].(map[string]any)[BgBase] = "@a"
	})
	if _, err := ParseTheme([]byte(raw), "custom"); err == nil {
		t.Fatal("circular vars must error")
	}
}

func TestLoadCustomAndAvailableThemes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mine.json"), []byte(validThemeJSON(t, nil)), 0o644); err != nil {
		t.Fatal(err)
	}
	// A built-in name in the custom dir never shadows the built-in.
	if err := os.WriteFile(filepath.Join(dir, "groknight.json"), []byte(validThemeJSON(t, nil)), 0o644); err != nil {
		t.Fatal(err)
	}
	list := AvailableThemes(dir)
	joined := strings.Join(list, ",")
	if !strings.Contains(joined, "mine") {
		t.Fatalf("custom theme missing: %v", list)
	}
	if !strings.Contains(joined, "groknight") {
		t.Fatalf("built-in missing: %v", list)
	}
	// Exactly one groknight entry (dedup, built-in wins).
	if strings.Count(joined, "groknight") != 1 {
		t.Fatalf("built-in must win dedup: %v", list)
	}
	th, err := LoadCustom(dir, "mine")
	if err != nil || th == nil {
		t.Fatalf("LoadCustom = %v, %v", th, err)
	}
	// LoadNamed falls back to a built-in when the custom file is absent.
	if got := LoadNamed("nope", dir); got == nil || got.Name != "groknight" {
		t.Fatalf("fallback = %+v", got)
	}
}

func TestSymbolsPresetDefaultsToUnicode(t *testing.T) {
	th, err := ParseTheme([]byte(validThemeJSON(t, nil)), "custom")
	if err != nil {
		t.Fatal(err)
	}
	if th.SymbolPreset() != "unicode" {
		t.Fatalf("preset = %q", th.SymbolPreset())
	}
	raw := validThemeJSON(t, func(doc map[string]any) {
		doc["symbols"] = map[string]any{"preset": "nerd", "spinnerFrames": []string{"⠋", "⠙"}}
	})
	th2, err := ParseTheme([]byte(raw), "custom")
	if err != nil {
		t.Fatal(err)
	}
	if th2.SymbolPreset() != "nerd" || len(th2.Symbols.SpinnerFrames) != 2 {
		t.Fatalf("symbols = %+v", th2.Symbols)
	}
}

func TestXterm256Palette(t *testing.T) {
	if got := Xterm256(0); got != (Color{0, 0, 0}) {
		t.Fatalf("index 0 = %+v", got)
	}
	if got := Xterm256(15); got != (Color{0xff, 0xff, 0xff}) {
		t.Fatalf("index 15 = %+v", got)
	}
	if got := Xterm256(16); got != (Color{0, 0, 0}) {
		t.Fatalf("cube origin = %+v", got)
	}
	if got := Xterm256(231); got != (Color{0xff, 0xff, 0xff}) {
		t.Fatalf("cube max = %+v", got)
	}
	g := Xterm256(240)
	if g.R != g.G || g.G != g.B {
		t.Fatalf("gray ramp must be neutral: %+v", g)
	}
}
