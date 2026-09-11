// Package theme implements xdev's theme system: the slot vocabulary from
// PRD §3.5 (Grok-CLI-derived names), RGB definition with startup
// quantization to the terminal's color capability, and the two launch
// themes (GrokNight, GrokDay).
//
// Hex provenance: GrokNight values are PROVISIONAL (binary string
// extraction, no slot attribution — PRD §3.5 "indicative, not verified").
// They render a coherent Grok-like identity today; issue #5's
// render-verification prerequisite may replace them via a single edit to
// groknightSlots/grokdaySlots.
package theme

import (
	"os"
	"strings"
)

// Color is an RGB theme color (slot value).
type Color struct{ R, G, B uint8 }

// Hex parses "#rrggbb" (or "rrggbb").
func Hex(s string) Color {
	s = strings.TrimPrefix(s, "#")
	if len(s) != 6 {
		return Color{}
	}
	var r, g, b uint8
	for i := 0; i < 6; i += 2 {
		n := hexVal(s[i])<<4 + hexVal(s[i+1])
		switch i {
		case 0:
			r = n
		case 2:
			g = n
		case 4:
			b = n
		}
	}
	return Color{r, g, b}
}

func hexVal(c byte) uint8 {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	}
	return 0
}

// RGBA implements color.Color.
func (c Color) RGBA() (uint32, uint32, uint32, uint32) {
	return uint32(c.R) | uint32(c.R)<<8, uint32(c.G) | uint32(c.G)<<8, uint32(c.B) | uint32(c.B)<<8, 0xffff
}

// Slot names (PRD §3.5 — the single M4/M12 vocabulary). Not every Grok slot
// exists yet; M4 defines what the MVP TUI draws. M12 generalizes.
const (
	BgBase             = "bg_base"
	BgHighlight        = "bg_highlight"
	BgTerminal         = "bg_terminal"
	AccentUser         = "accent_user"
	AccentAssistant    = "accent_assistant"
	AccentThinking     = "accent_thinking"
	AccentTool         = "accent_tool"
	AccentError        = "accent_error"
	AccentSuccess      = "accent_success"
	AccentRunning      = "accent_running"
	TextPrimary        = "text_primary"
	TextSecondary      = "text_secondary"
	GrayDim            = "gray_dim"
	Gray               = "gray"
	GrayBright         = "gray_bright"
	PromptBorder       = "prompt_border"
	PromptBorderActive = "prompt_border_active"
	MdHeading1         = "md_heading_h1"
	MdHeading2         = "md_heading_h2"
	MdHeading3         = "md_heading_h3"
	MdCode             = "md_code"    // inline code fg
	MdCodeBg           = "md_code_bg" // fenced code block background
	MdMuted            = "md_muted"   // bullets, rules, quotes
	LinkFg             = "link_fg"
)

// Theme is a named set of slot colors.
type Theme struct {
	Name  string
	Dark  bool
	Slots map[string]Color
	// Symbols carries the glyph preset and spinner frames from a custom
	// JSON theme ("" = unicode default).
	Symbols Symbols
}

// Get returns a slot color, falling back to text_primary, then white.
func (t *Theme) Get(slot string) Color {
	if c, ok := t.Slots[slot]; ok {
		return c
	}
	if c, ok := t.Slots[TextPrimary]; ok {
		return c
	}
	return Color{0xe5, 0xe5, 0xe5}
}

// groknightSlots — exact GrokNight palette from grok-build
// (crates/codegen/xai-grok-pager-render/src/theme/groknight.rs, 2026-09-09 sync).
// xAI neutral-gray base + TokyoNight Night accents.
func groknightSlots() map[string]Color {
	return map[string]Color{
		BgBase:             Hex("#141414"), // BG_STORM, main canvas
		BgHighlight:        Hex("#242424"), // BG_HIGHLIGHT (user prompt band)
		BgTerminal:         Hex("#0a0a0a"), // BG, terminal bg
		AccentUser:         Hex("#c8c8c8"), // accent_user = FG_DARK (neutral, not colored)
		AccentAssistant:    Hex("#bb9af7"), // MAGENTA
		AccentThinking:     Hex("#bb9af7"), // MAGENTA
		AccentTool:         Hex("#787878"), // DARK5
		AccentError:        Hex("#f7768e"), // RED
		AccentSuccess:      Hex("#9ece6a"), // GREEN
		AccentRunning:      Hex("#bb9af7"), // MAGENTA
		TextPrimary:        Hex("#e1e1e1"), // FG
		TextSecondary:      Hex("#c8c8c8"), // FG_DARK
		GrayDim:            Hex("#585858"), // gray_dim
		Gray:               Hex("#6c6c6c"), // COMMENT
		GrayBright:         Hex("#787878"), // DARK5
		PromptBorder:       Hex("#323237"), // prompt_border
		PromptBorderActive: Hex("#505058"), // prompt_border_active
		MdHeading1:         Hex("#1abc9c"), // TEAL
		MdHeading2:         Hex("#7aa2f7"), // BLUE
		MdHeading3:         Hex("#9d7cd8"), // PURPLE
		MdCode:             Hex("#3A95AB"), // BLUE1
		MdCodeBg:           Hex("#1c1c1c"), // rgb(28,28,28)
		MdMuted:            Hex("#6c6c6c"), // COMMENT
		LinkFg:             Hex("#7aa6da"),
	}
}

// grokdaySlots — exact GrokDay palette from grok-build
// (crates/codegen/xai-grok-pager-render/src/theme/grokday.rs, 2026-09-09 sync).
func grokdaySlots() map[string]Color {
	return map[string]Color{
		BgBase:             Hex("#eeeeee"),
		BgHighlight:        Hex("#dedede"),
		BgTerminal:         Hex("#f5f5f5"),
		AccentUser:         Hex("#444444"), // FG_DARK
		AccentAssistant:    Hex("#7D4BC6"), // MAGENTA
		AccentThinking:     Hex("#7D4BC6"), // MAGENTA
		AccentTool:         Hex("#626262"), // DARK5
		AccentError:        Hex("#cd3048"), // RED
		AccentSuccess:      Hex("#378E23"), // GREEN
		AccentRunning:      Hex("#7D4BC6"), // MAGENTA
		TextPrimary:        Hex("#262626"), // FG
		TextSecondary:      Hex("#444444"), // FG_DARK
		GrayDim:            Hex("#a5a5a5"),
		Gray:               Hex("#767676"), // COMMENT
		GrayBright:         Hex("#626262"), // DARK5
		PromptBorder:       Hex("#c8c8cd"),
		PromptBorderActive: Hex("#a5a5af"),
		MdHeading1:         Hex("#0A8E70"), // TEAL
		MdHeading2:         Hex("#2F64D2"), // BLUE
		MdHeading3:         Hex("#6C3EB2"), // PURPLE
		MdCode:             Hex("#0F87A2"), // BLUE1
		MdCodeBg:           Hex("#e4e4e4"), // rgb(228,228,228)
		MdMuted:            Hex("#767676"), // COMMENT
		LinkFg:             Hex("#2F64D2"), // BLUE
	}
}

// Builtins returns the launch themes.
func Builtins() map[string]*Theme {
	return map[string]*Theme{
		"groknight": {Name: "groknight", Dark: true, Slots: groknightSlots()},
		"grokday":   {Name: "grokday", Dark: false, Slots: grokdaySlots()},
	}
}

// Load resolves the theme by name ("groknight"|"grokday"; "" → auto via env
// polarity). Only the env-based detection ships in M4 (OSC 11 query and OS
// appearance polling are M12).
func Load(name string) *Theme {
	b := Builtins()
	if name != "" {
		switch strings.ToLower(name) {
		case "groknight", "grok-night", "dark", "night":
			return b["groknight"]
		case "grokday", "grok-day", "light", "day":
			return b["grokday"]
		}
	}
	// Auto: XDEV_THEME > terminal polarity guess > dark (Grok's default).
	if t := strings.ToLower(os.Getenv("XDEV_THEME")); t != "" {
		if t == "light" || t == "day" {
			return b["grokday"]
		}
		return b["groknight"]
	}
	if lightTerminal() {
		return b["grokday"]
	}
	return b["groknight"]
}

// lightTerminal makes a crude day-guess from COLORFGBG ("fg;bg", light bg
// when bg index >= 7 and not 8/9). Good enough for M4 auto; OSC 11 lands M12.
func lightTerminal() bool {
	v := os.Getenv("COLORFGBG")
	i := strings.LastIndex(v, ";")
	if i < 0 || i == len(v)-1 {
		return false
	}
	bg := 0
	for _, ch := range v[i+1:] {
		if ch < '0' || ch > '9' {
			return false
		}
		bg = bg*10 + int(ch-'0')
	}
	return bg >= 7 && bg != 8 && bg != 9
}

// Quantize maps an RGB color onto the terminal's palette class:
//   - truecolor: unchanged
//   - 256: nearest xterm-256cube entry (6x6x6 color cube + grays)
//   - 16/8: nearest of the base ANSI colors
//
// tcell handles final emission; this picks the value tier for consistent
// rendering across terminals.
func Quantize(c Color, capability int) Color {
	switch capability {
	case 24:
		return c
	case 8: // 256-color
		return nearest256(c)
	default: // 16 / 8-color ANSI
		return nearestANSI(c)
	}
}

// nearest256 finds the closest entry in the xterm 256 palette.
func nearest256(c Color) Color {
	best, bestD := Color{}, 1<<32
	check := func(idx int, r, g, b uint8) {
		d := sq(int(c.R)-int(r)) + sq(int(c.G)-int(g)) + sq(int(c.B)-int(b))
		if d < bestD {
			bestD, best = d, Color{r, g, b}
		}
	}
	// Color cube: 16..231, levels {0,95,135,175,215,255}.
	levels := [6]uint8{0, 95, 135, 175, 215, 255}
	for r := range 6 {
		for g := range 6 {
			for b := range 6 {
				check(16+36*r+6*g+b, levels[r], levels[g], levels[b])
			}
		}
	}
	// Grays: 232..255 (8..238 step 10) plus cube corners cover the rest.
	for i := range 24 {
		v := uint8(8 + 10*i)
		check(232+i, v, v, v)
	}
	return best
}

// nearestANSI maps to the 16 base ANSI colors (indices 0-15).
func nearestANSI(c Color) Color {
	base := [16]Color{
		{0, 0, 0}, {128, 0, 0}, {0, 128, 0}, {128, 128, 0},
		{0, 0, 128}, {128, 0, 128}, {0, 128, 128}, {192, 192, 192},
		{128, 128, 128}, {255, 0, 0}, {0, 255, 0}, {255, 255, 0},
		{0, 0, 255}, {255, 0, 255}, {0, 255, 255}, {255, 255, 255},
	}
	best, bestD := base[7], 1<<32
	for _, b := range base {
		d := sq(int(c.R)-int(b.R)) + sq(int(c.G)-int(b.G)) + sq(int(c.B)-int(b.B))
		if d < bestD {
			bestD, best = d, b
		}
	}
	return best
}

func sq(x int) int { return x * x }

// CapabilityFromEnv guesses the color capability from the environment
// (tcell refines this itself at screen init; this drives Quantize's tier).
func CapabilityFromEnv() int {
	if os.Getenv("NO_COLOR") != "" {
		return 4 // monochrome
	}
	if strings.Contains(strings.ToLower(os.Getenv("COLORTERM")), "truecolor") ||
		os.Getenv("XDEV_TRUECOLOR") != "" {
		return 24
	}
	if os.Getenv("TERM") != "" {
		return 8 // assume 256-color baseline
	}
	return 4
}

// Xterm256 converts a 256-color palette index to RGB using the standard
// xterm palette (16 system colors, a 6x6x6 cube, then the gray ramp).
func Xterm256(i int) Color {
	if i < 0 {
		i = 0
	}
	if i > 255 {
		i = 255
	}
	system := [16]Color{
		{0x00, 0x00, 0x00}, {0x80, 0x00, 0x00}, {0x00, 0x80, 0x00}, {0x80, 0x80, 0x00},
		{0x00, 0x00, 0x80}, {0x80, 0x00, 0x80}, {0x00, 0x80, 0x80}, {0xc0, 0xc0, 0xc0},
		{0x80, 0x80, 0x80}, {0xff, 0x00, 0x00}, {0x00, 0xff, 0x00}, {0xff, 0xff, 0x00},
		{0x00, 0x00, 0xff}, {0xff, 0x00, 0xff}, {0x00, 0xff, 0xff}, {0xff, 0xff, 0xff},
	}
	if i < 16 {
		return system[i]
	}
	if i < 232 {
		n := i - 16
		lv := [6]uint8{0, 95, 135, 175, 215, 255}
		return Color{lv[n/36], lv[(n/6)%6], lv[n%6]}
	}
	g := uint8(8 + (i-232)*10)
	return Color{g, g, g}
}
