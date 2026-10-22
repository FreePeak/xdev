package theme

import (
	"testing"
)

func TestHexParse(t *testing.T) {
	c := Hex("#7D4BC6")
	if c.R != 0x7d || c.G != 0x4b || c.B != 0xc6 {
		t.Fatalf("Hex(#7D4BC6) = %v", c)
	}
	if Hex("bad") != (Color{}) {
		t.Fatal("short hex should be zero color")
	}
}

// ompGroknightTokens is omp's groknight palette, resolved: the file omp ships
// at ~/.omp/agent/themes/groknight.json with its `vars` expanded, token for
// token. The values are PINNED LITERALS, never read back out of the theme — a
// test that asks the theme what colour it should have painted passes against
// the palette it replaced.
//
// The 256-index tokens are written as the RGB they resolve to: omp's file
// spells them 250/180/246, and xterm-256's own ramp is what a terminal puts
// on the screen for them.
var ompGroknightTokens = map[string]string{
	Accent:       "#e0af68",
	Border:       "#505058",
	BorderAccent: "#6c6c6c",
	BorderMuted:  "#333333",
	Success:      "#9ece6a",
	Error:        "#e15759",
	Warning:      "#e0af68",
	Muted:        "#6c6c6c",
	Dim:          "#585858",
	Text:         "#e1e1e1",
	ThinkingText: "#6c6c6c",

	SelectedBg:      "#242424",
	UserMessageBg:   "#191919",
	CustomMessageBg: "#1b1b1b",
	ToolPendingBg:   "#1b1b1b",
	ToolSuccessBg:   "#171717",
	ToolErrorBg:     "#212121",

	UserMessageText:    "#e1e1e1",
	CustomMessageText:  "#e1e1e1",
	CustomMessageLabel: "#6c6c6c",
	ToolTitle:          "#e1e1e1",
	ToolOutput:         "#c8c8c8",

	MdHeading:         "#e1e1e1",
	MdLink:            "#e1e1e1",
	MdLinkUrl:         "#6c6c6c",
	MdCode:            "#e0af68",
	MdCodeBlock:       "#e1e1e1",
	MdCodeBlockBorder: "#505058",
	MdQuote:           "#6c6c6c",
	MdQuoteBorder:     "#505058",
	MdHr:              "#505058",
	MdListBullet:      "#6c6c6c",

	ToolDiffAdded:   "#9ece6a",
	ToolDiffRemoved: "#e15759",
	ToolDiffContext: "#6c6c6c",

	SyntaxComment:     "#6c6c6c",
	SyntaxKeyword:     "#e0af68",
	SyntaxFunction:    "#e1e1e1",
	SyntaxVariable:    "#c8c8c8",
	SyntaxString:      "#73daca",
	SyntaxNumber:      "#ff9e64",
	SyntaxType:        "#e1e1e1",
	SyntaxOperator:    "#6c6c6c",
	SyntaxPunctuation: "#6c6c6c",

	ThinkingOff:     "#333333",
	ThinkingMinimal: "#6c6c6c",
	ThinkingLow:     "#6c6c6c",
	ThinkingMedium:  "#c8c8c8",
	ThinkingHigh:    "#c8c8c8",
	ThinkingXhigh:   "#e0af68",
	ThinkingMax:     "#e0af68",
	BashMode:        "#e0af68",
	PythonMode:      "#e0af68",

	StatusLineSep:       "#505058",
	StatusLineModel:     "#6c6c6c",
	StatusLinePath:      "#6c6c6c",
	StatusLineGitClean:  "#9ece6a",
	StatusLineGitDirty:  "#e0af68",
	StatusLineContext:   "#585858",
	StatusLineSpend:     "#6c6c6c",
	StatusLineStaged:    "#bcbcbc", // omp's 250
	StatusLineDirty:     "#d7af87", // omp's 180
	StatusLineUntracked: "#949494", // omp's 246
	StatusLineOutput:    "#bcbcbc", // omp's 250
	StatusLineCost:      "#bcbcbc", // omp's 250
	StatusLineSubagents: "#6c6c6c",
}

// TestGroknightIsOmpGroknight pins the launch dark theme to omp's groknight
// palette, token for token: the built-in theme answers every token omp's
// file answers, with omp's value.
//
// The one token it does NOT carry is status_line_bg, and that is deliberate:
// omp fills the status band (#242424), xdev's launch themes keep the row
// transparent, and filling it is a shape change rather than a palette one.
func TestGroknightIsOmpGroknight(t *testing.T) {
	th := Load("groknight")
	if !th.Dark {
		t.Fatal("groknight must be dark")
	}
	for slot, hex := range ompGroknightTokens {
		got, ok := th.Slot(slot)
		if !ok {
			t.Errorf("%s is left to the terminal default; the launch theme must pin it", slot)
			continue
		}
		if want := Hex(hex); got != want {
			t.Errorf("%s = %+v, want omp's groknight %s", slot, got, hex)
		}
	}
	// The band stays transparent on purpose (see the comment above).
	if _, ok := th.Slot(StatusLineBg); ok {
		t.Errorf("%s must stay unset: the launch status row is transparent", StatusLineBg)
	}
	// The contract's own token list, so a token added to the palette and not
	// to the table above is caught here rather than in a screenshot.
	for _, slot := range RequiredSlots() {
		if slot == StatusLineBg {
			continue
		}
		if _, ok := ompGroknightTokens[slot]; !ok {
			t.Errorf("the pinned table does not cover %s", slot)
		}
	}
}

// TestGroknightLegacySlots pins the slots xdev reads under its OWN names.
// They are the ones a launch theme has and omp's contract does not, or the
// ones whose ink is not the token's — so each deviation is named here rather
// than inherited by accident.
func TestGroknightLegacySlots(t *testing.T) {
	th := Load("groknight")
	want := map[string]string{
		// The legacy names that ARE a token under another name.
		AccentAssistant:    "#e0af68",
		AccentThinking:     "#e0af68",
		AccentRunning:      "#e0af68",
		AccentError:        "#e15759",
		AccentSuccess:      "#9ece6a",
		TextPrimary:        "#e1e1e1",
		GrayDim:            "#585858",
		Gray:               "#6c6c6c",
		MdHeading1:         "#e1e1e1",
		MdHeading2:         "#e1e1e1",
		MdHeading3:         "#e1e1e1",
		MdMuted:            "#6c6c6c",
		LinkFg:             "#e1e1e1",
		StatusLineMode:     "#e0af68",
		PromptBorderActive: "#505058",
		PromptBorder:       "#333333",
		// The ones that are xdev's own, with the reason they differ.
		BgBase:                "#141414",
		BgHighlight:           "#242424",
		BgTerminal:            "#0a0a0a",
		AccentUser:            "#c8c8c8",
		AccentTool:            "#787878",
		TextSecondary:         "#c8c8c8",
		GrayBright:            "#787878",
		MdCodeBg:              "#191919",
		ToolDiffAddedBg:       "#022800",
		ToolDiffRemovedBg:     "#3d0100",
		ToolDiffAddedWordBg:   "#044700",
		ToolDiffRemovedWordBg: "#5c0200",
	}
	for slot, hex := range want {
		got, ok := th.Slot(slot)
		if !ok {
			t.Errorf("%s is left to the terminal default; the launch theme must pin it", slot)
			continue
		}
		if w := Hex(hex); got != w {
			t.Errorf("%s = %+v, want %s", slot, got, hex)
		}
	}
}

func TestLoadAutoByEnv(t *testing.T) {
	t.Setenv("XDEV_THEME", "")
	t.Setenv("COLORFGBG", "15;0") // dark bg
	if th := Load(""); !th.Dark {
		t.Fatal("COLORFGBG dark bg should pick groknight")
	}
	t.Setenv("COLORFGBG", "0;15") // light bg
	if th := Load(""); th.Dark {
		t.Fatal("COLORFGBG light bg should pick grokday")
	}
	t.Setenv("COLORFGBG", "")
	t.Setenv("XDEV_THEME", "light")
	if th := Load(""); th.Dark {
		t.Fatal("XDEV_THEME=light should pick grokday")
	}
}

func TestQuantizeTruecolorPassthrough(t *testing.T) {
	c := Hex("#7D4BC6")
	if got := Quantize(c, 24); got != c {
		t.Fatalf("truecolor must pass through: %v", got)
	}
}

func TestQuantize256StaysClose(t *testing.T) {
	cases := []Color{Hex("#0e0e0e"), Hex("#7D4BC6"), Hex("#0db9d7"), Hex("#e5e5e5"), Hex("#444444")}
	for _, c := range cases {
		q := Quantize(c, 8)
		d := sq(int(q.R)-int(c.R)) + sq(int(q.G)-int(c.G)) + sq(int(q.B)-int(c.B))
		// Cube steps are ~40 per channel; total distance should stay modest.
		if d > 3*40*40 {
			t.Fatalf("256 quantization too far for %v: %v", c, q)
		}
	}
	// Dark gray should quantize to the gray ramp (R==G==B).
	if q := Quantize(Hex("#0e0e0e"), 8); q.R != q.G || q.G != q.B {
		t.Fatalf("gray must stay gray: %v", q)
	}
}

func TestQuantizeANSI(t *testing.T) {
	q := Quantize(Hex("#ffffff"), 4)
	if q != (Color{255, 255, 255}) {
		t.Fatalf("white should map to white: %v", q)
	}
	q = Quantize(Hex("#000000"), 4)
	if q != (Color{0, 0, 0}) {
		t.Fatalf("black should map to black: %v", q)
	}
}

func TestCapabilityFromEnv(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("COLORTERM", "")
	if CapabilityFromEnv() != 4 {
		t.Fatal("NO_COLOR must force monochrome tier")
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("COLORTERM", "truecolor")
	if CapabilityFromEnv() != 24 {
		t.Fatal("COLORTERM=truecolor must be 24")
	}
	t.Setenv("COLORTERM", "")
	t.Setenv("TERM", "xterm-256color")
	if CapabilityFromEnv() != 8 {
		t.Fatal("TERM set must be 256 tier")
	}
}

// The built-in palettes own the diff inks, and a band beside them: the change
// is read from the band's colour and the marker's ink, so a built-in theme that
// named neither would fall back to the terminal's own green/red with no band at
// all — the look this retired. All four band slots are xdev's own vocabulary
// (not omp's 66-token contract), so they are optional for an imported theme and
// required of a built-in.
func TestBuiltinThemesPaintTheDiff(t *testing.T) {
	// Every built-in, not just the launch pair: a palette is a built-in, so a
	// band it failed to pin would paint a diff the user's own colours decide.
	for name, th := range Builtins() {
		for _, slot := range []string{ToolDiffAdded, ToolDiffRemoved, ToolDiffContext,
			ToolDiffAddedBg, ToolDiffRemovedBg, ToolDiffAddedWordBg, ToolDiffRemovedWordBg} {
			if c, ok := th.Slot(slot); !ok || c == (Color{}) {
				t.Errorf("%s: %s must pin a colour, got %+v (ok=%v)", name, slot, c, ok)
			}
		}
		// A band that equals its word band teaches nothing: the changed words
		// are the whole reason wordPair exists.
		add, _ := th.Slot(ToolDiffAddedBg)
		addW, _ := th.Slot(ToolDiffAddedWordBg)
		rem, _ := th.Slot(ToolDiffRemovedBg)
		remW, _ := th.Slot(ToolDiffRemovedWordBg)
		if add == addW || rem == remW {
			t.Errorf("%s: the word band must differ from the row band", name)
		}
		if add == rem {
			t.Errorf("%s: added and removed collapsed onto one band", name)
		}
	}
}

// Color-blind mode exists because the diff's whole meaning rides on a
// red/green pair some readers cannot separate, so it must override the
// built-ins' terminal-default mark, not skip over it.
func TestColorBlindRemapsDiffSlots(t *testing.T) {
	for name, base := range Builtins() {
		cb := ApplyColorBlindMode(base)
		cbAdd, ok := cb.Slot(ToolDiffAdded)
		cbRem, _ := cb.Slot(ToolDiffRemoved)
		if !ok {
			t.Fatalf("%s: colorblind must pin the added ink, not leave it to the terminal", name)
		}
		if cbAdd == cbRem {
			t.Errorf("%s: colorblind collapsed added and removed to one color", name)
		}
		if cbAdd != cb.Get(AccentSuccess) || cbRem != cb.Get(AccentError) {
			t.Errorf("%s: diff inks must join the success/error pair: %+v vs %+v", name, []Color{cbAdd, cbRem}, []Color{cb.Get(AccentSuccess), cb.Get(AccentError)})
		}
		if base.Get(ToolDiffAdded) == cbAdd {
			t.Errorf("%s: the mode must remap a copy, not the shared built-in (%+v)", name, base.Get(ToolDiffAdded))
		}
		// The bands move with the markers: a green band under a blue marker
		// leaves the red/green pair standing exactly where it is hardest to
		// read, so the mode tints them from the pair ink too.
		row, _ := cb.Slot(ToolDiffAddedBg)
		word, _ := cb.Slot(ToolDiffAddedWordBg)
		if row == cbAdd || word == cbAdd {
			t.Errorf("%s: diff bands must be tints, not the marker ink itself: %+v %+v", name, row, word)
		}
		if row == word {
			t.Errorf("%s: the color-blind word band collapsed onto the row band", name)
		}
	}
}
