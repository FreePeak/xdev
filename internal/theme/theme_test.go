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
	// The accent identity: magenta user, cyan tool.
	if u := th.Get(AccentUser); u != Hex("#7D4BC6") {
		t.Fatalf("accent_user = %v", u)
	}
	if tool := th.Get(AccentTool); tool != Hex("#0db9d7") {
		t.Fatalf("accent_tool = %v", tool)
	}
	if bg := th.Get(BgBase); bg != Hex("#0e0e0e") {
		t.Fatalf("bg_base = %v", bg)
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
