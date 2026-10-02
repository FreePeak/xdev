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

func TestGrokNightIdentity(t *testing.T) {
	th := Load("groknight")
	if !th.Dark {
		t.Fatal("groknight must be dark")
	}
	// Identity per groknight.rs: neutral user (FG_DARK), gray tool (DARK5),
	// BG_STORM canvas. Magenta accents live on assistant/thinking/running.
	if u := th.Get(AccentUser); u != Hex("#c8c8c8") {
		t.Fatalf("accent_user = %v", u)
	}
	if tool := th.Get(AccentTool); tool != Hex("#787878") {
		t.Fatalf("accent_tool = %v", tool)
	}
	if bg := th.Get(BgBase); bg != Hex("#141414") {
		t.Fatalf("bg_base = %v", bg)
	}
	if asst := th.Get(AccentAssistant); asst != Hex("#bb9af7") {
		t.Fatalf("accent_assistant = %v", asst)
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
	for _, name := range []string{"groknight", "grokday"} {
		th := Builtins()[name]
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
	for _, name := range []string{"groknight", "grokday"} {
		base := Builtins()[name]
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
