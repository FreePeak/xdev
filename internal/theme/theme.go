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

// Lerp blends a→b by t, clamping t to [0,1]. It is the one primitive every
// xdev animation is built from: a focus border that eases toward its active
// ink, a status line that breathes, a toast that fades into the canvas. t is
// a float because the ease is the point — callers pass a sine or a
// per-frame fraction, not a step count.
//
// Interpolation is linear in sRGB, not gamma-correct: for a 6-step or
// 20-step tween between two inks a terminal already quantizes, the
// difference is not visible and the gamma form is 4 lines longer.
func Lerp(a, b Color, t float64) Color {
	if t <= 0 {
		return a
	}
	if t >= 1 {
		return b
	}
	// Round rather than truncate: truncating biases every mid-blend toward
	// the darker end, which over a fade reads as the fade going muddy.
	return Color{
		R: uint8(float64(a.R) + (float64(b.R)-float64(a.R))*t + 0.5),
		G: uint8(float64(a.G) + (float64(b.G)-float64(a.G))*t + 0.5),
		B: uint8(float64(a.B) + (float64(b.B)-float64(a.B))*t + 0.5),
	}
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

	// Diff bands (4, xdev's own — NOT in omp's 66-token contract, so they are
	// optional and a theme that omits them paints no band at all: the change
	// reads from the tool_diff_added/removed ink alone, the way it always
	// has). A row band and the stronger band the changed words ride on, per
	// polarity. The rule that retired coloured bands was coloured TEXT on a
	// band tinted from the same ink — the row's own words drown in it. These
	// are the opposite: the ink paints the marker and nothing else, and the
	// band is a flat tint of its own (Claude Code's addLine/addWord).
	ToolDiffAddedBg       = "tool_diff_added_bg"
	ToolDiffRemovedBg     = "tool_diff_removed_bg"
	ToolDiffAddedWordBg   = "tool_diff_added_word_bg"
	ToolDiffRemovedWordBg = "tool_diff_removed_word_bg"

	// Thinking-mode rails (8 + 1 optional: ThinkingMax falls back to
	// ThinkingXhigh when a theme omits it). The ladder is the shipped
	// Accent → Muted ramp, one step per rung, so a launch theme paints a
	// visible gradient from off to max without carrying eight colours of its
	// own; a theme that names them (the omp contract requires them) still
	// wins.
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
	// StatusLineMode is the FALLBACK ink for the session mode (mode.go: /mode,
	// the Shift-Tab cycle) — what App.modeToken uses for a mode name the
	// per-mode map does not carry. The four modes xdev ships each wear their
	// own semantic ink instead (Claude Code's mode map: default/plan/accept/
	// bypass). Optional like the diff bands — NOT in the omp 66-token
	// contract, because omp has no mode surface — so a theme that omits it
	// derives it from Accent (extraChains) rather than failing to load.
	StatusLineMode = "status_line_mode"

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
		// Status-line hues: StatusLineBg stays unset so the launch themes
		// keep today's transparent status row.
		//
		// The model wears the ACCENT, not gray_dim (the comment above this
		// block used to say otherwise): omp paints the model name through
		// accentFg, and 43 of the 98 themes it ships resolve statusLineModel
		// to `accent` — the name is the session's identity, and identity is
		// what the accent is for. Gray_dim is right for the separators, which
		// only have to divide the row's parts.
		StatusLineSep:       Hex("#585858"),
		StatusLineModel:     Hex("#bb9af7"), // MAGENTA, accent_assistant
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
		// The reasoning rails (thinking_off..thinking_max). The launcher
		// themes never named them — the contract requires them of an
		// imported omp theme, not of a built-in — so they were unset and
		// Get fell back to the body colour, which is why the reasoning level
		// on the composer divider read as part of the model name. The ramp
		// is one step per rung from the muted grey (off) to the accent
		// (max): the effort is legible as an amount.
		ThinkingOff:         Hex("#6c6c6c"),
		ThinkingMinimal:     Hex("#7d7da8"),
		ThinkingLow:         Hex("#8f8fb9"),
		ThinkingMedium:      Hex("#a1a1ca"),
		ThinkingHigh:        Hex("#b3b3db"),
		ThinkingXhigh:       Hex("#c6a6e8"),
		ThinkingMax:         Hex("#bb9af7"),
		StatusLineSubagents: Hex("#bb9af7"),
		// The session mode (mode.go). The mode now wears a PER-MODE ink
		// (App.modeToken), so this slot is only what an unrecognised mode
		// name falls back to; it keeps the accent for that case.
		StatusLineMode: Hex("#bb9af7"),
		// Diff rows. Claude Code's model (v2.1.287, the diff renderer) keeps
		// the SHAPE — the +/- marker is the only coloured text on a changed
		// row, and the change itself is read from a flat band — while the INKS
		// are Rosé Pine's own git groups, the two upstream hands its diff
		// signs (nvim lua/rose-pine/config.lua: git_add = "foam",
		// git_delete = "love"). Foam rather than pine for the added marker:
		// pine reads 4.0:1 on this canvas and 3.1:1 on the added band, so a
		// pine "+" is a marker the reader has to squint at, which defeats the
		// claim the band is making. Love reads 5.5:1 on the removed band.
		ToolDiffAdded:   Hex("#9ccfd8"), // foam, upstream git_add
		ToolDiffRemoved: Hex("#eb6f92"), // love, upstream git_delete
		ToolDiffContext: Hex("#908caa"), // subtle, the unchanged rows
		// The bands stay Claude Code's own near-black tints (addLine
		// rgb(2,40,0), addWord rgb(4,71,0) and the two red equivalents). They
		// are a near-black STRIPE, not a palette identity: the only ink that
		// ever sits on one is the row's own body text, so re-tinting them to a
		// palette hue would repaint the whole diff panel — the transcript box
		// AND the sidebar's popup, which share these four slots — to buy no
		// legibility. (The shipped rose-pine.json palette does use its own
		// pine/foam tints here; a launch theme deliberately does not.)
		ToolDiffAddedBg:       Hex("#022800"), // CC addLine
		ToolDiffRemovedBg:     Hex("#3d0100"), // CC deleteLine
		ToolDiffAddedWordBg:   Hex("#044700"), // CC addWord
		ToolDiffRemovedWordBg: Hex("#5c0200"), // CC deleteWord
		// Tool output and fenced-code tokens (#501, #582) wear Rosé Pine's own
		// syntax groups, mapped role-for-role onto its token table (vscode
		// themes/rose-pine-color-theme.json). A `read` preview, an `edit`
		// result and a bash command then read as ONE palette with the diff
		// above them, instead of a Grok-coloured frame around a One-Dark
		// block.
		//
		// Two of the nine are NOT upstream's own assignment, both forced by
		// slots xdev has that Rosé Pine has no counterpart for:
		//
		//   Comment is `muted`, not nvim's `subtle`. xdev paints a tool body's
		//   flat rows in TextSecondary (#c8c8c8 here), and a comment in the
		//   same ink as the body is a comment that vanished — that is exactly
		//   the regression TestFencedBlockInToolOutputIsNotColoured caught.
		//   `muted` is one step quieter than `subtle`, still a Rosé Pine token,
		//   and reads 3.3:1 on the code band against the old comment's 3.5:1.
		//
		//   Number is `rose`, not nvim's `gold`: upstream paints String and
		//   Number the same ink, and two classes a reader tells apart wearing
		//   one ink is the ambiguity these nine slots exist to remove
		//   (TestFencedGoBlockColorsItsTokens). VS Code's own rose-pine gives
		//   constant.numeric #ebbcba — rose — which stays in the palette.
		SyntaxComment:     Hex("#6e6a86"), // muted, one step below subtle
		SyntaxKeyword:     Hex("#31748f"), // Keyword
		SyntaxString:      Hex("#f6c177"), // String
		SyntaxNumber:      Hex("#ebbcba"), // constant.numeric, not nvim's gold
		SyntaxType:        Hex("#9ccfd8"), // Type
		SyntaxVariable:    Hex("#e0def4"), // Identifier (the brightest token)
		SyntaxFunction:    Hex("#ebbcba"), // Function
		SyntaxOperator:    Hex("#908caa"), // Operator
		SyntaxPunctuation: Hex("#908caa"), // @punctuation
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
		// Status-line hues (see groknightSlots); StatusLineBg unset. The
		// model takes the day theme's MAGENTA, for the same reason the dark
		// twin takes its accent.
		StatusLineSep:       Hex("#a5a5a5"),
		StatusLineModel:     Hex("#7D4BC6"), // MAGENTA, accent_assistant
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
		StatusLineMode:      Hex("#7D4BC6"), // see groknightSlots
		// The reasoning rails — see groknightSlots. The day twin of the dark
		// ramp: grey at off, the accent at max, so the level reads as an
		// amount on a light canvas too.
		ThinkingOff:     Hex("#767676"),
		ThinkingMinimal: Hex("#82829e"),
		ThinkingLow:     Hex("#8e8eb0"),
		ThinkingMedium:  Hex("#9a9ac2"),
		ThinkingHigh:    Hex("#a6a6d4"),
		ThinkingXhigh:   Hex("#b0a2dc"),
		ThinkingMax:     Hex("#7D4BC6"),
		// Diff rows — see groknightSlots. The bands are CC's day tints
		// (addLine rgb(220,255,220), addWord rgb(178,255,178) and the two
		// red equivalents): near-white, so the day canvas shows through.
		ToolDiffAdded:         Hex("#378E23"),
		ToolDiffRemoved:       Hex("#cd3048"),
		ToolDiffContext:       Hex("#767676"),
		ToolDiffAddedBg:       Hex("#dcffdc"),
		ToolDiffRemovedBg:     Hex("#ffdcdc"),
		ToolDiffAddedWordBg:   Hex("#b2ffb2"),
		ToolDiffRemovedWordBg: Hex("#ffc7c7"),
		// Fenced-code tokens (#501) — see groknightSlots: the same 9 roles on
		// Claude Code's light scope map (v2.1.287 — One Light), whose inks
		// hold contrast on the light code band.
		SyntaxComment:     Hex("#969896"), // comment/meta
		SyntaxKeyword:     Hex("#a71d5d"), // keyword/operator
		SyntaxString:      Hex("#183691"), // string/regexp
		SyntaxNumber:      Hex("#0086b3"), // literal/number
		SyntaxType:        Hex("#0086b3"), // built_in/type
		SyntaxVariable:    Hex("#333333"), // variable/property (body text)
		SyntaxFunction:    Hex("#795da3"), // title.function
		SyntaxOperator:    Hex("#a71d5d"), // operator
		SyntaxPunctuation: Hex("#333333"), // punctuation
	}
}

// Builtins returns the launch themes plus the ported palettes.
//
// The diff slots are NOT terminal-default any more. They were, because the
// renderer used to colour the row's own WORDS with them, and a fixed RGB it
// picked for itself was free to land on the user's red or green. The claim
// now lives on the marker's single cell and on a flat band (Claude Code's
// model), so the theme's own green/red owns both the marker and the status
// line's clean/dirty tree, and the band is a tint of its own rather than of
// that ink. A theme that still leaves a slot "" keeps the terminal's answer.
//
// The two launch themes stay hand-written tables: each value was read out of
// a Grok source file and the comment above it names where, which is worth
// that verbosity. The ported palettes are data (palettes.go) — 37 upstream
// tokens each, all MIT/Apache, validated by the same ParseTheme as a
// ~/.xdev/agent/themes/*.json.
func Builtins() map[string]*Theme {
	out := map[string]*Theme{
		"groknight": {Name: "groknight", Dark: true, Slots: groknightSlots(), Defaults: map[string]bool{}},
		"grokday":   {Name: "grokday", Dark: false, Slots: grokdaySlots(), Defaults: map[string]bool{}},
	}
	for name, t := range palettes() {
		out[name] = t
	}
	return out
}

// Load resolves the theme by name ("" → auto). "dark"/"light"/"night"/"day"
// are aliases of the two launch themes; every other name is looked up
// case-insensitively among the built-ins, so a ported palette is settable by
// the same path as a hand-written one. Auto
// polarity is XDEV_THEME, then the terminal's own answer to an OSC 11
// background query, then COLORFGBG, then dark (Grok's default). OS
// appearance polling is not implemented.
func Load(name string) *Theme {
	b := Builtins()
	if n := strings.ToLower(name); n != "" {
		switch n {
		case "dark", "night":
			n = "groknight"
		case "light", "day":
			n = "grokday"
		}
		if t, ok := b[n]; ok {
			return t
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

// maxColorDistance is the "nothing compared yet" sentinel. It is a literal
// rather than 1<<32 because int is 32 bits on linux/386, where that constant
// overflows and the package stops compiling; the largest squared RGB distance
// a Color can produce is 3*255*255 = 195075.
const maxColorDistance = 1 << 30

// nearest256 finds the closest entry in the xterm 256 palette.
func nearest256(c Color) Color {
	best, bestD := Color{}, maxColorDistance
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
	best, bestD := base[7], maxColorDistance
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
