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
)

// Theme is a named set of slot colors.
type Theme struct {
	Name  string
	Dark  bool
	Slots map[string]Color
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

// groknightSlots — PROVISIONAL GrokNight palette (see package comment).
func groknightSlots() map[string]Color {
	return map[string]Color{
		BgBase:             Hex("#0e0e0e"),
		BgHighlight:        Hex("#1a1a1a"),
		BgTerminal:         Hex("#0a0a0a"),
		AccentUser:         Hex("#7D4BC6"), // magenta (user/cursor)
		AccentAssistant:    Hex("#c8c8c8"),
		AccentThinking:     Hex("#909090"),
		AccentTool:         Hex("#0db9d7"), // cyan
		AccentError:        Hex("#de5971"),
		AccentSuccess:      Hex("#8fd3a7"),
		AccentRunning:      Hex("#0db9d7"),
		TextPrimary:        Hex("#e5e5e5"),
		TextSecondary:      Hex("#c8c8c8"),
		GrayDim:            Hex("#444444"),
		Gray:               Hex("#909090"),
		GrayBright:         Hex("#c8c8c8"),
		PromptBorder:       Hex("#444444"),
		PromptBorderActive: Hex("#7D4BC6"),
	}
}

// grokdaySlots — light counterpart (provisional).
func grokdaySlots() map[string]Color {
	return map[string]Color{
		BgBase:             Hex("#fafafa"),
		BgHighlight:        Hex("#ececec"),
		BgTerminal:         Hex("#ffffff"),
		AccentUser:         Hex("#6C3EB2"),
		AccentAssistant:    Hex("#171717"),
		AccentThinking:     Hex("#525252"),
		AccentTool:         Hex("#0F87A2"),
		AccentError:        Hex("#CD3048"),
		AccentSuccess:      Hex("#0A8E70"),
		AccentRunning:      Hex("#0F87A2"),
		TextPrimary:        Hex("#171717"),
		TextSecondary:      Hex("#404040"),
		GrayDim:            Hex("#a3a3a3"),
		Gray:               Hex("#737373"),
		GrayBright:         Hex("#404040"),
		PromptBorder:       Hex("#d4d4d4"),
		PromptBorderActive: Hex("#6C3EB2"),
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
