package theme

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// ompColorKeys is omp's own theme vocabulary (theme-schema.json / the
// shipped groknight.json), verbatim and independent of RequiredSlots: a
// theme written for omp must load here, so the two lists have to agree.
var ompColorKeys = []string{
	"accent", "bashMode", "border", "borderAccent", "borderMuted",
	"customMessageBg", "customMessageLabel", "customMessageText", "dim", "error",
	"mdCode", "mdCodeBlock", "mdCodeBlockBorder", "mdHeading", "mdHr", "mdLink",
	"mdLinkUrl", "mdListBullet", "mdQuote", "mdQuoteBorder", "muted", "pythonMode",
	"selectedBg", "statusLineBg", "statusLineContext", "statusLineCost",
	"statusLineDirty", "statusLineGitClean", "statusLineGitDirty", "statusLineModel",
	"statusLineOutput", "statusLinePath", "statusLineSep", "statusLineSpend",
	"statusLineStaged", "statusLineSubagents", "statusLineUntracked", "success",
	"syntaxComment", "syntaxFunction", "syntaxKeyword", "syntaxNumber",
	"syntaxOperator", "syntaxPunctuation", "syntaxString", "syntaxType",
	"syntaxVariable", "text", "thinkingHigh", "thinkingLow", "thinkingMedium",
	"thinkingMinimal", "thinkingOff", "thinkingText", "thinkingXhigh",
	"toolDiffAdded", "toolDiffContext", "toolDiffRemoved", "toolErrorBg",
	"toolOutput", "toolPendingBg", "toolSuccessBg", "toolTitle", "userMessageBg",
	"userMessageText", "warning",
}

// TestRequiredSlotsIsTheOMPContract pins the token contract: 66 required
// tokens (thinking_max optional), spelled exactly as omp spells them.
func TestRequiredSlotsIsTheOMPContract(t *testing.T) {
	got := make([]string, 0, len(RequiredSlots()))
	for _, slot := range RequiredSlots() {
		got = append(got, toCamel(slot))
	}
	if len(ompColorKeys) != 66 {
		t.Fatalf("fixture drift: %d omp tokens", len(ompColorKeys))
	}
	sort.Strings(got)
	want := append([]string(nil), ompColorKeys...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("required tokens = %v\nwant %v", got, want)
	}
	if strings.Contains(strings.Join(got, ","), toCamel(ThinkingMax)) {
		t.Fatal("thinking_max must stay optional")
	}
}

// ompThemeJSON writes a theme the way omp ships one: camelCase tokens, bare
// var names (no "@"), an export block, and no xdev legacy slots at all.
func ompThemeJSON(t *testing.T, mutate func(colors map[string]string, doc map[string]any)) string {
	t.Helper()
	colors := map[string]string{}
	for _, k := range ompColorKeys {
		colors[k] = "#123456"
	}
	doc := map[string]any{
		"name":   "ported",
		"colors": colors,
		"vars": map[string]string{
			"text":   "#e1e1e1",
			"accent": "#ff9e64",
		},
		"export": map[string]string{"pageBg": "#141414", "cardBg": "#191919"},
	}
	if mutate != nil {
		mutate(colors, doc)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestParseOMPThemeLoadsUnchanged(t *testing.T) {
	raw := ompThemeJSON(t, func(colors map[string]string, _ map[string]any) {
		colors["text"] = "text" // bare var name, omp spelling
		colors["accent"] = "accent"
		colors["thinkingXhigh"] = "#bb9af7"
		colors["mdHeading"] = "#1abc9c"
		colors["muted"] = "#6c6c6c"
	})
	th, err := ParseTheme([]byte(raw), "ported")
	if err != nil {
		t.Fatal(err)
	}
	if got := th.Get(Text); got != Hex("#e1e1e1") {
		t.Fatalf("text = %+v", got)
	}
	// The legacy xdev names the TUI reads are mirrored from the canonical
	// tokens, so an omp theme paints xdev's chrome without editing.
	if got := th.Get(TextPrimary); got != Hex("#e1e1e1") {
		t.Fatalf("text_primary mirror = %+v", got)
	}
	if got := th.Get(AccentAssistant); got != Hex("#ff9e64") {
		t.Fatalf("accent_assistant mirror = %+v", got)
	}
	if got := th.Get(MdHeading1); got != Hex("#1abc9c") {
		t.Fatalf("md_heading_h1 mirror = %+v", got)
	}
	if got := th.Get(MdHeading2); got != Hex("#1abc9c") {
		t.Fatalf("md_heading_h2 default = %+v", got)
	}
	if got := th.Get(Gray); got != Hex("#6c6c6c") {
		t.Fatalf("gray default (from muted) = %+v", got)
	}
	// thinking_max falls back to thinking_xhigh.
	if th.Get(ThinkingMax) != Hex("#bb9af7") || th.Get(ThinkingXhigh) != Hex("#bb9af7") {
		t.Fatalf("thinking_max = %+v", th.Get(ThinkingMax))
	}
	// The canvas default comes from the export block; the band color is
	// userMessageBg (omp's token for it).
	if got := th.Get(BgBase); got != Hex("#141414") {
		t.Fatalf("bg_base from export.pageBg = %+v", got)
	}
	if got := th.Get(BgHighlight); got != Hex("#123456") {
		t.Fatalf("bg_highlight from userMessageBg = %+v", got)
	}
}

func TestParseThemeOMPSpinnerAndBox(t *testing.T) {
	raw := ompThemeJSON(t, func(_ map[string]string, doc map[string]any) {
		doc["symbols"] = map[string]any{
			"preset":        "nerd",
			"box":           "sharp",
			"overrides":     map[string]string{"box.sharp.topLeft": "*"},
			"spinnerFrames": map[string]any{"status": []string{"A", "B"}, "activity": []string{"x"}},
		}
	})
	th, err := ParseTheme([]byte(raw), "ported")
	if err != nil {
		t.Fatal(err)
	}
	if got := th.SpinnerFrames(); len(got) != 2 || got[0] != "A" {
		t.Fatalf("spinner = %v", got)
	}
	if b := th.Box(); b.TopLeft != "*" || b.TopRight != "┐" {
		t.Fatalf("box = %+v", b)
	}
	if th.BoxSharp().TopLeft != "*" {
		t.Fatalf("sharp box = %+v", th.BoxSharp())
	}
}

func TestBoxPresets(t *testing.T) {
	def := Load("groknight")
	if got := def.Box(); got != unicodeRound {
		t.Fatalf("default box = %+v", got)
	}
	if got := def.BoxSharp(); got.TopLeft != "┌" {
		t.Fatalf("sharp box = %+v", got)
	}
	if got := def.SymbolPreset(); got != "unicode" {
		t.Fatalf("preset = %q", got)
	}
	ascii := &Theme{Name: "ascii", Symbols: Symbols{Preset: "ascii"}}
	if got := ascii.Box(); got.Horizontal != "-" || got.Vertical != "|" || got.TopLeft != "+" {
		t.Fatalf("ascii box = %+v", got)
	}
	if got := ascii.SpinnerFrames(); got[0] != "|" {
		t.Fatalf("ascii spinner = %v", got)
	}
	sharp := &Theme{Name: "sharp", Symbols: Symbols{Box: "sharp"}}
	if got := sharp.Box().TopLeft; got != "┌" {
		t.Fatalf("sharp chrome = %q", got)
	}
	// Unknown override keys are kept, not rejected: another style keeps its
	// own glyphs.
	ovr := &Theme{Name: "ovr", Symbols: Symbols{Overrides: map[string]string{"box.round.topLeft": "o"}}}
	if got := ovr.Box(); got.TopLeft != "o" || got.BottomRight != "╯" {
		t.Fatalf("override box = %+v", got)
	}
	if _, err := ParseTheme([]byte(validThemeJSON(t, func(doc map[string]any) {
		doc["symbols"] = map[string]any{"box": "wobble"}
	})), "custom"); err == nil {
		t.Fatal("bad symbols.box must error")
	}
	if _, err := ParseTheme([]byte(validThemeJSON(t, func(doc map[string]any) {
		doc["symbols"] = map[string]any{"preset": "hologram"}
	})), "custom"); err == nil {
		t.Fatal("bad symbols.preset must error")
	}
}

func TestSpinnerFramesPrecedence(t *testing.T) {
	flat := ParseMust(t, validThemeJSON(t, func(doc map[string]any) {
		doc["symbols"] = map[string]any{"spinnerFrames": []string{"1", "2"}}
	}))
	if got := flat.SpinnerFrames(); len(got) != 2 || got[1] != "2" {
		t.Fatalf("flat spinner = %v", got)
	}
	// A flat list feeds both spinners (omp semantics).
	if len(flat.Symbols.Status) == 0 || len(flat.Symbols.Activity) == 0 {
		t.Fatalf("flat list must fill both spinners: %+v", flat.Symbols)
	}
	// An explicit status spinner wins over the flat list.
	both := ParseMust(t, validThemeJSON(t, func(doc map[string]any) {
		doc["symbols"] = map[string]any{"spinnerFrames": []string{"1", "2"}, "status": []string{"s"}}
	}))
	if got := both.SpinnerFrames(); len(got) != 1 || got[0] != "s" {
		t.Fatalf("status must win: %v", got)
	}
	if Load("groknight").SpinnerFrames()[0] != defaultSpinnerFrames[0] {
		t.Fatal("built-ins must keep the braille spinner")
	}
}

// ParseMust parses a theme fixture that is expected to be valid.
func ParseMust(t *testing.T, raw string) *Theme {
	t.Helper()
	th, err := ParseTheme([]byte(raw), "c")
	if err != nil {
		t.Fatal(err)
	}
	return th
}

func TestColorBlindModeRemapsRedGreen(t *testing.T) {
	base := ParseMust(t, ompThemeJSON(t, func(colors map[string]string, _ map[string]any) {
		colors["error"] = "#f7768e"
		colors["success"] = "#9ece6a"
		colors["toolDiffAdded"] = "#9ece6a"
		colors["toolDiffRemoved"] = "#f7768e"
		colors["text"] = "#e1e1e1"
	}))
	dark := ApplyColorBlindMode(base)
	if dark.Get(Error) == base.Get(Error) {
		t.Fatal("color-blind mode must remap error")
	}
	if dark.Get(Error) != dark.Get(ToolDiffRemoved) {
		t.Fatalf("error/removed must share the orange ink: %+v", dark.Get(Error))
	}
	if dark.Get(Success) == base.Get(Success) || dark.Get(Success) != dark.Get(ToolDiffAdded) {
		t.Fatalf("success/added must share the blue ink: %+v", dark.Get(Success))
	}
	if dark.Get(Text) != base.Get(Text) {
		t.Fatal("color-blind mode must leave unrelated slots alone")
	}
	// The source theme is never mutated (built-ins are shared).
	if base.Get(Error) != Hex("#f7768e") {
		t.Fatal("ApplyColorBlindMode must return a copy")
	}
	// The built-in palettes carry the legacy names the TUI reads.
	builtin := ApplyColorBlindMode(Load("groknight"))
	if builtin.Get(AccentError) == Load("groknight").Get(AccentError) {
		t.Fatal("built-in error accent must remap")
	}
	if builtin.Get(AccentError) == builtin.Get(AccentSuccess) {
		t.Fatal("the color-blind pair must stay distinct")
	}
	light := ApplyColorBlindMode(Load("grokday"))
	if light.Get(AccentError) == builtin.Get(AccentError) {
		t.Fatal("light and dark need different color-blind inks")
	}
	if ApplyColorBlindMode(nil) != nil {
		t.Fatal("nil-safe")
	}
}

func TestParseOSC11Reply(t *testing.T) {
	cases := map[string]struct {
		color Color
		ok    bool
	}{
		"\x1b]11;rgb:1e1e/1e1e/1e1e\x1b\\": {Hex("#1e1e1e"), true},
		"\x1b]11;rgb:ff/ff/ff\x07":         {Hex("#ffffff"), true},
		"\x1b]11;rgb:0/0/0\x07":            {Color{}, true},
		"\x1b]11;#f5f5f5\x1b\\":            {Hex("#f5f5f5"), true},
		"\x1b]11;rgb:zz/00/00\x07":         {Color{}, false},
		"":                                 {Color{}, false},
	}
	for reply, want := range cases {
		got, ok := parseOSC11(reply)
		if ok != want.ok || got != want.color {
			t.Errorf("parseOSC11(%q) = %+v/%v, want %+v/%v", reply, got, ok, want.color, want.ok)
		}
	}
	if !lightBackground(Hex("#f5f5f5")) || lightBackground(Hex("#0a0a0a")) {
		t.Fatal("luminance polarity is wrong")
	}
	// Tests run with stdout piped: detection must decline, not block.
	if _, ok := DetectBackground(backgroundQueryTimeout); ok {
		t.Fatal("DetectBackground must skip off-terminal")
	}
}

func TestTerminalDefaultIsMarked(t *testing.T) {
	raw := validThemeJSON(t, func(doc map[string]any) {
		doc["colors"].(map[string]any)[StatusLineBg] = ""
	})
	th, err := ParseTheme([]byte(raw), "custom")
	if err != nil {
		t.Fatal(err)
	}
	if !th.TerminalDefault(StatusLineBg) {
		t.Fatal("empty statusLineBg must be marked as terminal default")
	}
	if th.TerminalDefault(Text) {
		t.Fatal("explicit colors are not terminal defaults")
	}
}
