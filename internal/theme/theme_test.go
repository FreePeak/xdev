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

// The built-in palettes must actually carry the diff slots: Theme.Get falls
// back through text_primary to a fixed gray, so an unpopulated palette would
// render every diff row the same colour as prose and no test of the renderer
// could tell the difference.
func TestBuiltinThemesPopulateDiffSlots(t *testing.T) {
	for _, name := range []string{"groknight", "grokday"} {
		th := Builtins()[name]
		for _, slot := range []string{ToolDiffAdded, ToolDiffRemoved, ToolDiffContext} {
			c, ok := th.Slot(slot)
			if !ok {
				t.Errorf("%s: %s unset", name, slot)
				continue
			}
			// Added and removed must not be the same colour, and neither may
			// collapse onto the context gray.
			if slot == ToolDiffAdded {
				if r, _ := th.Slot(ToolDiffRemoved); c == r {
					t.Errorf("%s: added == removed", name)
				}
			}
			if slot != ToolDiffContext {
				if ctx, _ := th.Slot(ToolDiffContext); c == ctx {
					t.Errorf("%s: %s equals the context color", name, slot)
				}
			}
		}
	}
}

// Color-blind mode remaps the diff inks along with the status accents — it can
// only do that once the palettes define the slots (the remap skips absent ones).
func TestColorBlindRemapsDiffSlots(t *testing.T) {
	for _, name := range []string{"groknight", "grokday"} {
		base := Builtins()[name]
		cb := ApplyColorBlindMode(base)
		add, _ := base.Slot(ToolDiffAdded)
		rem, _ := base.Slot(ToolDiffRemoved)
		cbAdd, _ := cb.Slot(ToolDiffAdded)
		cbRem, _ := cb.Slot(ToolDiffRemoved)
		if cbAdd == add || cbRem == rem {
			t.Errorf("%s: colorblind left the diff inks unchanged", name)
		}
		if cbAdd == cbRem {
			t.Errorf("%s: colorblind collapsed added and removed to one color", name)
		}
		ctx, _ := cb.Slot(ToolDiffContext)
		if ctx == cbAdd || ctx == cbRem {
			t.Errorf("%s: colorblind diff context collides with an ink", name)
		}
		if add == rem {
			t.Fatalf("%s: base palette has no added/removed distinction to test", name)
		}
	}
}
