// Package theme implements xdev's theme system: the slot vocabulary from
// PRD §3.5 (Grok-CLI-derived names), RGB definition with startup
// quantization to the terminal's color capability, and the two launch
// themes (GrokNight, GrokDay).
//
// Hex provenance: the launch DARK theme is omp's own groknight palette — the
// file omp ships at ~/.omp/agent/themes/groknight.json, vars expanded (see
// groknightSlots). The launch LIGHT theme is Rosé Pine Dawn's own groups
// (grokdaySlots), because omp ships no light twin of groknight; the two are
// no longer a pair, and grokdaySlots says so.
//
// The slot vocabulary is omp's 66-token contract (see RequiredSlots): the
// canonical names are omp's own, and xdev's legacy names are the aliases the
// TUI reads. A theme file may spell any slot either way.
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
	// StatusLineMode is the chrome ink for session effort's standard rung
	// (and the Vibe chip). Optional like the diff bands — NOT in the omp
	// 66-token contract — so a theme that omits it derives it from Accent
	// (extraChains) rather than failing to load.
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

// groknightSlots — omp's groknight palette, resolved.
//
// Provenance: the theme omp actually runs — the file it ships at
// ~/.omp/agent/themes/groknight.json, with its `vars` expanded. It is NOT the
// grok-build palette this table used to carry: that one had a magenta accent
// (#bb9af7) over xAI's neutral greys, and omp's groknight has a tan accent
// (#e0af68) over the same canvas. The name is the same; the palette is omp's.
// (The pre-2026-10-21 palette survives as the shipped `rose-pine` palette,
// which carries the same syntax and diff inks it did.)
//
// The canonical tokens come FIRST and are omp's own names: the file a user
// drops into ~/.xdev/agent/themes/ carries exactly these keys, so the built-in
// theme and an imported one answer the same slot the same way. xdev's legacy
// slot names — the vocabulary the TUI reads — are derived from them below.
//
// One token is deliberately absent: status_line_bg. omp's groknight fills the
// status band (#242424); xdev's launch themes keep the row transparent, and
// filling it is a shape change, not a palette one.
func groknightSlots() map[string]Color {
	slots := map[string]Color{
		// --- omp's tokens, verbatim (vars expanded) ---
		Accent:       Hex("#e0af68"), // the identity ink: rails, model, spinner
		Border:       Hex("#505058"),
		BorderAccent: Hex("#6c6c6c"),
		BorderMuted:  Hex("#333333"),
		Success:      Hex("#9ece6a"),
		Error:        Hex("#e15759"),
		Warning:      Hex("#e0af68"),
		Muted:        Hex("#6c6c6c"),
		Dim:          Hex("#585858"),
		Text:         Hex("#e1e1e1"),
		ThinkingText: Hex("#6c6c6c"),

		SelectedBg:      Hex("#242424"),
		UserMessageBg:   Hex("#191919"),
		CustomMessageBg: Hex("#1b1b1b"),
		ToolPendingBg:   Hex("#1b1b1b"),
		ToolSuccessBg:   Hex("#171717"),
		ToolErrorBg:     Hex("#212121"),
		// StatusLineBg stays unset: the launch status row is transparent.

		UserMessageText:    Hex("#e1e1e1"),
		CustomMessageText:  Hex("#e1e1e1"),
		CustomMessageLabel: Hex("#6c6c6c"),
		ToolTitle:          Hex("#e1e1e1"),
		ToolOutput:         Hex("#c8c8c8"),

		MdHeading:         Hex("#e1e1e1"),
		MdLink:            Hex("#e1e1e1"),
		MdLinkUrl:         Hex("#6c6c6c"),
		MdCode:            Hex("#e0af68"),
		MdCodeBlock:       Hex("#e1e1e1"),
		MdCodeBlockBorder: Hex("#505058"),
		MdQuote:           Hex("#6c6c6c"),
		MdQuoteBorder:     Hex("#505058"),
		MdHr:              Hex("#505058"),
		MdListBullet:      Hex("#6c6c6c"),

		ToolDiffAdded:   Hex("#9ece6a"),
		ToolDiffRemoved: Hex("#e15759"),
		ToolDiffContext: Hex("#6c6c6c"),

		SyntaxComment:     Hex("#6c6c6c"),
		SyntaxKeyword:     Hex("#e0af68"),
		SyntaxFunction:    Hex("#e1e1e1"),
		SyntaxVariable:    Hex("#c8c8c8"),
		SyntaxString:      Hex("#73daca"),
		SyntaxNumber:      Hex("#ff9e64"),
		SyntaxType:        Hex("#e1e1e1"),
		SyntaxOperator:    Hex("#6c6c6c"),
		SyntaxPunctuation: Hex("#6c6c6c"),

		ThinkingOff:     Hex("#333333"),
		ThinkingMinimal: Hex("#6c6c6c"),
		ThinkingLow:     Hex("#6c6c6c"),
		ThinkingMedium:  Hex("#c8c8c8"),
		ThinkingHigh:    Hex("#c8c8c8"),
		ThinkingXhigh:   Hex("#e0af68"),
		ThinkingMax:     Hex("#e0af68"),
		BashMode:        Hex("#e0af68"),
		PythonMode:      Hex("#e0af68"),

		StatusLineSep:       Hex("#505058"),
		StatusLineModel:     Hex("#6c6c6c"),
		StatusLinePath:      Hex("#6c6c6c"),
		StatusLineGitClean:  Hex("#9ece6a"),
		StatusLineGitDirty:  Hex("#e0af68"),
		StatusLineContext:   Hex("#585858"),
		StatusLineSpend:     Hex("#6c6c6c"),
		StatusLineStaged:    Hex("#bcbcbc"), // omp's 256-index 250
		StatusLineDirty:     Hex("#d7af87"), // omp's 256-index 180
		StatusLineUntracked: Hex("#949494"), // omp's 256-index 246
		StatusLineOutput:    Hex("#bcbcbc"), // omp's 256-index 250
		StatusLineCost:      Hex("#bcbcbc"), // omp's 256-index 250
		StatusLineSubagents: Hex("#6c6c6c"),
	}
	// xdev's legacy slot names, derived from the tokens above.
	//
	// Most of them ARE the token under another name — the assistant rail, the
	// reasoning rail, the running spinner and the session mode all wear the
	// accent, because in omp they are the accent. The ones that are NOT are
	// named outright below, each with the reason it differs: xdev's chrome
	// has slots omp's contract has no room for, and a slot that silently
	// inherited a token's ink would be a palette that lies about what it is.
	slots[AccentAssistant] = slots[Accent]
	slots[AccentThinking] = slots[Accent]
	slots[AccentRunning] = slots[Accent]
	slots[AccentError] = slots[Error]
	slots[AccentSuccess] = slots[Success]
	slots[TextPrimary] = slots[Text]
	slots[GrayDim] = slots[Dim]
	slots[Gray] = slots[Muted]
	slots[MdHeading1] = slots[MdHeading]
	slots[MdHeading2] = slots[MdHeading]
	slots[MdHeading3] = slots[MdHeading]
	slots[MdMuted] = slots[MdListBullet]
	slots[LinkFg] = slots[MdLink]
	slots[StatusLineMode] = slots[Accent]
	slots[PromptBorderActive] = slots[Border]
	slots[PromptBorder] = slots[BorderMuted]
	// The body ink. omp paints a tool body's rows in toolOutput (#c8c8c8) and
	// a message's prose in text (#e1e1e1); xdev's text_secondary is the BODY
	// of both, so it takes the toolOutput ink — muted (#6c6c6c) would put the
	// transcript's prose a full step below the dim chrome around it.
	slots[TextSecondary] = Hex("#c8c8c8")
	// The canvas and the raised surface: omp's export.pageBg and its
	// selectedBg, which is the ink xdev's highlight surfaces (the user band,
	// the selected rows) wear.
	slots[BgBase] = Hex("#141414")
	slots[BgHighlight] = slots[SelectedBg]
	// xdev's own: the terminal's own background, the gutter's neutral, the
	// tool card's frame (omp paints a card's BORDER in dim, and its title in
	// toolTitle — the title is a different slot), and the second grey.
	slots[BgTerminal] = Hex("#0a0a0a")
	slots[AccentUser] = Hex("#c8c8c8")
	slots[AccentTool] = Hex("#787878")
	slots[GrayBright] = Hex("#787878")
	// The inline-code band: omp's export.cardBg, one step above the canvas.
	slots[MdCodeBg] = Hex("#191919")
	// The diff bands are xdev's own slots (omp's contract has no room for
	// them) and keep Claude Code's near-black tints (addLine rgb(2,40,0),
	// addWord rgb(4,71,0) and the two red equivalents): the only ink that
	// ever sits on one is the row's own body text, so re-tinting them to a
	// palette hue would repaint the whole diff panel — the transcript box AND
	// the sidebar's popup, which share these four slots — to buy no
	// legibility.
	slots[ToolDiffAddedBg] = Hex("#022800")
	slots[ToolDiffRemovedBg] = Hex("#3d0100")
	slots[ToolDiffAddedWordBg] = Hex("#044700")
	slots[ToolDiffRemovedWordBg] = Hex("#5c0200")
	return slots
}

// grokdaySlots — the launch LIGHT theme: Rosé Pine Dawn's own groups (MIT,
// github.com/rose-pine/rose-pine-theme) over GrokDay's neutral structure. The
// same split as the dark theme: the canvas, the borders and the muted greys
// stay GrokDay's because the terminal's own background already supplies the
// canvas, and every INK a reader reads text with is the palette's.
//
// It is NOT the dark theme's twin any more. The dark theme became omp's
// groknight (groknightSlots), and omp ships no light counterpart of it — its
// own light theme is a different palette under a different name — so there is
// nothing to mirror. What is left here is the light theme xdev has always
// shipped, and the honest reason it is still here: `theme: auto` only picks it
// for an explicit `light`/`day`, XDEV_THEME, or a terminal that answers the
// OSC 11 / COLORFGBG light-background probe, and a reader on a light terminal
// gets it by DEFAULT. The cost is one table; the mapping is Dawn's, slot for
// slot.
func grokdaySlots() map[string]Color {
	return map[string]Color{
		// The canonical name of the accent, the token omp's contract spells
		// `accent`: the surfaces that read omp's token (the dock's section
		// headings) find it here, and the legacy names below are the same
		// ink under xdev's own vocabulary.
		Accent:             Hex("#7D4BC6"),
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
		// Diff rows — the day twin of groknightSlots, same shape: Claude
		// Code's model keeps the +/-, the bands are CC's day tints (addLine
		// rgb(220,255,220), addWord rgb(178,255,178) and the two red
		// equivalents, near-white so the day canvas shows through), and the
		// inks are Dawn's own git groups.
		//
		// Pine for the added marker, where the dark twin uses foam — the one
		// place the two variants do NOT pick the same token, and a
		// measurement rather than a preference. On a near-white band the
		// darker ink is the readable one: pine reads 5.6:1 on the added band
		// and 5.3:1 on the day canvas, foam 3.2:1 and 3.0:1 — barely over the
		// 3:1 floor for non-body text. The dark twin's foam reads 9.4:1 on
		// ITS band, so each variant takes the end of the palette that carries
		// its own polarity. Love reads 3.3:1 on the removed band, the
		// weakest marker in the theme and still ahead of the pine it replaced
		// at 2.9:1.
		ToolDiffAdded:         Hex("#286983"), // pine, upstream git_add
		ToolDiffRemoved:       Hex("#b4637a"), // love, upstream git_delete
		ToolDiffContext:       Hex("#797593"), // subtle, the unchanged rows
		ToolDiffAddedBg:       Hex("#dcffdc"),
		ToolDiffRemovedBg:     Hex("#ffdcdc"),
		ToolDiffAddedWordBg:   Hex("#b2ffb2"),
		ToolDiffRemovedWordBg: Hex("#ffc7c7"),
		// Fenced-code and tool-output tokens (#501, #582) — the day twin of
		// groknightSlots, from Dawn's own syntax groups (vscode
		// themes/rose-pine-dawn-color-theme.json).
		//
		// Comment is `muted` (#9893a5), which VS Code gives
		// punctuation.definition.tag, rather than `subtle`: grokday paints a
		// tool body's flat rows in TextSecondary (#444444 here), and a comment
		// in that same ink is a comment that vanished — the same failure
		// TestFencedBlockInToolOutputIsNotColoured caught on the dark twin.
		// `muted` reads 3.3:1 against that body against the old comment's
		// 3.4:1, so the choice costs nothing.
		//
		// Number is `rose` (#d7827e) — VS Code's own constant.numeric for
		// Dawn — and distinct from String (`gold`), as the nine slots require.
		SyntaxComment:     Hex("#9893a5"), // muted
		SyntaxKeyword:     Hex("#286983"), // Keyword
		SyntaxString:      Hex("#ea9d34"), // String
		SyntaxNumber:      Hex("#d7827e"), // constant.numeric
		SyntaxType:        Hex("#56949f"), // Type
		SyntaxVariable:    Hex("#575279"), // Identifier (body text)
		SyntaxFunction:    Hex("#d7827e"), // Function
		SyntaxOperator:    Hex("#797593"), // Operator
		SyntaxPunctuation: Hex("#797593"), // @punctuation
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
// The two launch themes stay hand-written tables: the dark one is omp's
// groknight palette, token for token (groknightSlots), and the light one is
// Rosé Pine Dawn's groups (grokdaySlots) — each value's comment names where
// it came from, which is worth that verbosity. The ported palettes are data
// (palettes.go) — 37 upstream tokens each, all MIT/Apache, validated by the
// same ParseTheme as a ~/.xdev/agent/themes/*.json.
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
