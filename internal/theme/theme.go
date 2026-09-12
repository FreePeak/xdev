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

// Slot names. The second block is xdev's own vocabulary (PRD §3.5), the
// first block is the omp token contract xdev adopted in M12 (research F4):
// every token omp requires is required here too, so an imported theme is
// complete by construction. A theme file may spell any slot either way —
// canonical (`statusLineSep`), canonical snake (`status_line_sep`), or the
// legacy xdev name (`gray_dim`) — and the loader mirrors both vocabularies
// into Theme.Slots (custom.go is the single source for the mapping).
const (
	// Core text/borders (omp group of 11).
	Accent       = "accent"
	Border       = "border"
	BorderAccent = "border_accent"
	BorderMuted  = "border_muted"
	Success      = "success"
	Error        = "error"
	Warning      = "warning"
	Muted        = "muted"
	Dim          = "dim"
	Text         = "text"
	ThinkingText = "thinking_text"

	// Backgrounds (7).
	SelectedBg      = "selected_bg"
	UserMessageBg   = "user_message_bg"
	CustomMessageBg = "custom_message_bg"
	ToolPendingBg   = "tool_pending_bg"
	ToolSuccessBg   = "tool_success_bg"
	ToolErrorBg     = "tool_error_bg"
	StatusLineBg    = "status_line_bg"

	// Message / tool text (5).
	UserMessageText    = "user_message_text"
	CustomMessageText  = "custom_message_text"
	CustomMessageLabel = "custom_message_label"
	ToolTitle          = "tool_title"
	ToolOutput         = "tool_output"

	// Markdown (10).
	MdHeading         = "md_heading"
	MdLink            = "md_link"
	MdLinkUrl         = "md_link_url"
	MdCodeBlock       = "md_code_block"
	MdCodeBlockBorder = "md_code_block_border"
	MdQuote           = "md_quote"
	MdQuoteBorder     = "md_quote_border"
	MdHr              = "md_hr"
	MdListBullet      = "md_list_bullet"

	// Diff + syntax (12).
	ToolDiffAdded     = "tool_diff_added"
	ToolDiffRemoved   = "tool_diff_removed"
	ToolDiffContext   = "tool_diff_context"
	SyntaxComment     = "syntax_comment"
	SyntaxKeyword     = "syntax_keyword"
	SyntaxFunction    = "syntax_function"
	SyntaxVariable    = "syntax_variable"
	SyntaxString      = "syntax_string"
	SyntaxNumber      = "syntax_number"
	SyntaxType        = "syntax_type"
	SyntaxOperator    = "syntax_operator"
	SyntaxPunctuation = "syntax_punctuation"

	// Thinking-mode rails (8 + 1 optional: ThinkingMax falls back to
	// ThinkingXhigh when a theme omits it).
	ThinkingOff     = "thinking_off"
	ThinkingMinimal = "thinking_minimal"
	ThinkingLow     = "thinking_low"
	ThinkingMedium  = "thinking_medium"
	ThinkingHigh    = "thinking_high"
	ThinkingXhigh   = "thinking_xhigh"
	ThinkingMax     = "thinking_max"
	BashMode        = "bash_mode"
	PythonMode      = "python_mode"

	// Status line / HUD (13). The renderer consumes Sep, Model, Spend,
	// Context and Cost today; the git/staged/untracked/subagents/output
	// hues are required for contract completeness and reserved for the HUD
	// segments that will carry them.
	StatusLineSep       = "status_line_sep"
	StatusLineModel     = "status_line_model"
	StatusLinePath      = "status_line_path"
	StatusLineGitClean  = "status_line_git_clean"
	StatusLineGitDirty  = "status_line_git_dirty"
	StatusLineContext   = "status_line_context"
	StatusLineSpend     = "status_line_spend"
	StatusLineStaged    = "status_line_staged"
	StatusLineDirty     = "status_line_dirty"
	StatusLineUntracked = "status_line_untracked"
	StatusLineOutput    = "status_line_output"
	StatusLineCost      = "status_line_cost"
	StatusLineSubagents = "status_line_subagents"

	// Legacy xdev slot names (PRD §3.5): still what the TUI passes to Get,
	// mirrored from the canonical tokens at parse time.
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

// Theme is a named set of slot colors. Slots carries both vocabularies
// (canonical omp tokens and the legacy xdev names the TUI reads), so Get
// answers for either spelling.
type Theme struct {
	Name  string
	Dark  bool
	Slots map[string]Color
	// Defaults marks slots the theme set to "" — the terminal default
	// (`\x1b[39m`/`\x1b[49m`). Chrome that would otherwise paint a
	// background checks this to stay transparent instead of painting black.
	Defaults map[string]bool
	// Symbols carries the glyph preset, box style and spinner frames from a
	// custom JSON theme ("" = unicode round).
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
	if c, ok := t.Slots[Text]; ok {
		return c
	}
	return Color{0xe5, 0xe5, 0xe5}
}

// TerminalDefault reports whether slot resolved to the terminal default
// ("" in a theme file) rather than an explicit color.
func (t *Theme) TerminalDefault(slot string) bool {
	return t != nil && t.Defaults[slot]
}

// Slot returns a slot's explicit color. ok is false when the theme leaves
// the slot to the terminal default ("" in a theme file) or omits it — the
// distinction Get cannot make, because Get has to answer with something.
func (t *Theme) Slot(slot string) (Color, bool) {
	if t == nil || t.Defaults[slot] {
		return Color{}, false
	}
	c, ok := t.Slots[slot]
	return c, ok
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
		// Status-line hues: they keep the pre-segment rendering (model
		// divider in gray_dim, counters in gray). StatusLineBg stays unset
		// so the launch themes keep today's transparent status row.
		StatusLineSep:       Hex("#585858"),
		StatusLineModel:     Hex("#585858"),
		StatusLinePath:      Hex("#6c6c6c"),
		StatusLineGitClean:  Hex("#9ece6a"),
		StatusLineGitDirty:  Hex("#f7768e"),
		StatusLineContext:   Hex("#6c6c6c"),
		StatusLineSpend:     Hex("#6c6c6c"),
		StatusLineStaged:    Hex("#9ece6a"),
		StatusLineDirty:     Hex("#f7768e"),
		StatusLineUntracked: Hex("#6c6c6c"),
		StatusLineOutput:    Hex("#6c6c6c"),
		StatusLineCost:      Hex("#6c6c6c"),
		StatusLineSubagents: Hex("#bb9af7"),
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
		// Status-line hues (see groknightSlots); StatusLineBg unset.
		StatusLineSep:       Hex("#a5a5a5"),
		StatusLineModel:     Hex("#a5a5a5"),
		StatusLinePath:      Hex("#767676"),
		StatusLineGitClean:  Hex("#378E23"),
		StatusLineGitDirty:  Hex("#cd3048"),
		StatusLineContext:   Hex("#767676"),
		StatusLineSpend:     Hex("#767676"),
		StatusLineStaged:    Hex("#378E23"),
		StatusLineDirty:     Hex("#cd3048"),
		StatusLineUntracked: Hex("#767676"),
		StatusLineOutput:    Hex("#767676"),
		StatusLineCost:      Hex("#767676"),
		StatusLineSubagents: Hex("#7D4BC6"),
	}
}

// Builtins returns the launch themes.
func Builtins() map[string]*Theme {
	return map[string]*Theme{
		"groknight": {Name: "groknight", Dark: true, Slots: groknightSlots()},
		"grokday":   {Name: "grokday", Dark: false, Slots: grokdaySlots()},
	}
}

// Load resolves the theme by name ("groknight"|"grokday"; "" → auto). Auto
// polarity is XDEV_THEME, then the terminal's own answer to an OSC 11
// background query, then COLORFGBG, then dark (Grok's default). OS
// appearance polling is not implemented.
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
	if light, ok := DetectBackground(backgroundQueryTimeout); ok {
		if light {
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
