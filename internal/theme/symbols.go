package theme

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Symbol vocabulary (research F4): glyph presets, the box styles outlined
// chrome draws with, and the spinner frames the running indicator cycles.
//
// The renderer reaches all three through accessors rather than the raw
// Symbols struct, so built-in themes (which carry no Symbols) and custom
// themes answer identically. Override keys a theme sets for symbols xdev
// does not paint are ignored, not rejected: an imported theme that speaks a
// richer vocabulary still loads.

// BoxChars is the outline one boxed surface draws with.
type BoxChars struct {
	TopLeft, TopRight       string
	BottomLeft, BottomRight string
	Horizontal, Vertical    string
}

// boxStyle names the two outline sets themes can override.
const (
	boxStyleRound = "round"
	boxStyleSharp = "sharp"
)

// Box glyph sets per symbol preset. Round is today's chrome; sharp is the
// table/junction set. The ascii preset degenerates both to +-|.
var (
	unicodeRound = BoxChars{"╭", "╮", "╰", "╯", "─", "│"}
	unicodeSharp = BoxChars{"┌", "┐", "└", "┘", "─", "│"}
	asciiBox     = BoxChars{"+", "+", "+", "+", "-", "|"}
)

// defaultSpinnerFrames is the shipped braille spinner (unicode/nerd).
var defaultSpinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// asciiSpinnerFrames is the preset default for terminals without braille.
var asciiSpinnerFrames = []string{"|", "/", "-", "\\"}

// HUD icon families, borrowed from the deepseek harness's composer pills
// (deepseek-harness StatsPills.tsx): IconGaugeOutline leads the timing
// readings, IconDatabaseOutline the token ones, and dsh's tool rows carry
// the wrench a tool call is drawn with. dsh draws them as 16px SVG; a
// terminal draws one cell, so these are the closest monospace stand-ins.
// Themes override them under symbols.overrides as "hud.<key>", and the ascii
// preset substitutes a letter tag, because a terminal that cannot draw the
// glyph would rather read "hit 96%" than paint a tofu box.
//
// Every default here must be one cell in EVERY terminal, not one cell by
// runewidth's reckoning: the status row is a fixed-cell grid, so a glyph a
// terminal paints two cells wide is written into one cell, spills its own
// second column over the first digit of the number beside it, and the
// reading collides. U+23F1 STOPWATCH is that glyph — East Asian Ambiguous,
// drawn double-width by most terminals — so the gauge reads U+23F2, which is
// Unambiguous. ▤ and ✳ are already one cell everywhere.
const (
	HUDIconGauge    = "gauge"    // timing: work timer, decode rate, ttft
	HUDIconDatabase = "database" // tokens: the token split, the cache hit rate
	HUDIconTool     = "tool"     // the session's tool-call count
)

var (
	// The gauge is U+23F2 STOPWATCH, never U+23F1: the note above is the
	// whole reason. ▤ and ✳ are already one cell in every terminal.
	hudUnicodeIcons = map[string]string{
		HUDIconGauge:    "⏲",
		HUDIconDatabase: "▤",
		HUDIconTool:     "✳",
	}
	hudAsciiIcons = map[string]string{
		HUDIconGauge:    "t",
		HUDIconDatabase: "k",
		HUDIconTool:     "*",
	}
)

// HUDIcon returns the one-cell icon a HUD segment leads with, for the
// metric family it names. A theme that overrides an unknown key is ignored
// (the same rule box/spinner overrides follow); the ascii preset substitutes
// a letter tag rather than a glyph its terminal cannot draw.
//
// The result is cut to ONE character. A theme's "hud.<key>" override is a
// free string, and a two-cell glyph in that slot is the overlap this call
// exists to prevent: the status row is a fixed-cell grid, so the icon lands
// in one cell, the terminal paints the glyph's own second column there, and
// the first digit of the number beside it is overwritten. One character of
// icon, or a letter — never a collision the user reads as a wrong number. A
// theme that wants a two-cell icon is asking for a two-cell row.
func (t *Theme) HUDIcon(key string) string {
	icon := ""
	if t != nil {
		if v, ok := t.Symbols.Overrides["hud."+key]; ok && v != "" {
			icon = v
		} else if t.SymbolPreset() == "ascii" {
			icon = hudAsciiIcons[key]
		}
	}
	if icon == "" {
		icon = hudUnicodeIcons[key]
	}
	for _, r := range icon {
		return string(r)
	}
	return ""
}

// Box returns the outline glyphs for outlined chrome under the theme's box
// style (symbols.box: round — the default and today's glyphs — or sharp).
// Per-glyph overrides live under "box.round.<key>"/"box.sharp.<key>" with
// keys topLeft, topRight, bottomLeft, bottomRight, horizontal, vertical.
func (t *Theme) Box() BoxChars { return t.box(boxStyleFrom(t)) }

// BoxSharp returns the sharp outline (markdown tables and junction chrome
// always draw sharp, whatever the chrome style is).
func (t *Theme) BoxSharp() BoxChars { return t.box(boxStyleSharp) }

func (t *Theme) box(style string) BoxChars {
	b := unicodeRound
	switch {
	case t != nil && t.SymbolPreset() == "ascii":
		b = asciiBox
	case style == boxStyleSharp:
		b = unicodeSharp
	}
	if t == nil || len(t.Symbols.Overrides) == 0 {
		return b
	}
	pick := func(key, fallback string) string {
		if v, ok := t.Symbols.Overrides["box."+style+"."+key]; ok && v != "" {
			return v
		}
		return fallback
	}
	return BoxChars{
		TopLeft:     pick("topLeft", b.TopLeft),
		TopRight:    pick("topRight", b.TopRight),
		BottomLeft:  pick("bottomLeft", b.BottomLeft),
		BottomRight: pick("bottomRight", b.BottomRight),
		Horizontal:  pick("horizontal", b.Horizontal),
		Vertical:    pick("vertical", b.Vertical),
	}
}

func boxStyleFrom(t *Theme) string {
	if t != nil && strings.EqualFold(t.Symbols.Box, boxStyleSharp) {
		return boxStyleSharp
	}
	return boxStyleRound
}

// SpinnerFrames returns the running-indicator frames: symbols.status (omp's
// status spinner), then the flat symbols.spinnerFrames list, then the
// preset default. Never empty.
func (t *Theme) SpinnerFrames() []string {
	if t != nil {
		for _, frames := range [][]string{t.Symbols.Status, t.Symbols.SpinnerFrames} {
			if clean := nonEmptyFrames(frames); len(clean) > 0 {
				return clean
			}
		}
		if t.SymbolPreset() == "ascii" {
			return asciiSpinnerFrames
		}
	}
	return defaultSpinnerFrames
}

// nonEmptyFrames drops blank entries so a stray "" never blanks the spinner.
func nonEmptyFrames(frames []string) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// normalizeSymbols validates the preset and box style and fills the preset
// default for a missing spinner list.
func normalizeSymbols(s Symbols) (Symbols, error) {
	switch strings.ToLower(strings.TrimSpace(s.Preset)) {
	case "":
		s.Preset = "unicode"
	case "unicode", "nerd", "ascii":
		s.Preset = strings.ToLower(strings.TrimSpace(s.Preset))
	default:
		return Symbols{}, fmt.Errorf("symbols.preset %q must be unicode, nerd, or ascii", s.Preset)
	}
	switch strings.ToLower(strings.TrimSpace(s.Box)) {
	case "":
		s.Box = boxStyleRound
	case boxStyleRound, boxStyleSharp:
		s.Box = strings.ToLower(strings.TrimSpace(s.Box))
	default:
		return Symbols{}, fmt.Errorf("symbols.box %q must be round or sharp", s.Box)
	}
	return s, nil
}

// UnmarshalJSON accepts both omp spellings of spinnerFrames: a flat frame
// list (applies to both spinners) or {"status":[…],"activity":[…]}. The
// top-level `status`/`activity` keys are the other shipped spelling.
func (s *Symbols) UnmarshalJSON(raw []byte) error {
	var aux struct {
		Preset        string            `json:"preset"`
		Box           string            `json:"box"`
		Overrides     map[string]string `json:"overrides"`
		SpinnerFrames json.RawMessage   `json:"spinnerFrames"`
		Status        []string          `json:"status"`
		Activity      []string          `json:"activity"`
	}
	if err := json.Unmarshal(raw, &aux); err != nil {
		return err
	}
	out := Symbols{Preset: aux.Preset, Box: aux.Box, Overrides: aux.Overrides, Status: aux.Status, Activity: aux.Activity}
	if len(aux.SpinnerFrames) > 0 {
		var flat []string
		if err := json.Unmarshal(aux.SpinnerFrames, &flat); err == nil {
			out.SpinnerFrames = flat
			if len(out.Status) == 0 {
				out.Status = flat
			}
			if len(out.Activity) == 0 {
				out.Activity = flat // a flat list drives both spinners
			}
		} else {
			var obj struct {
				Status   []string `json:"status"`
				Activity []string `json:"activity"`
			}
			if err := json.Unmarshal(aux.SpinnerFrames, &obj); err != nil {
				return fmt.Errorf("symbols.spinnerFrames must be a frame list or {\"status\":[…],\"activity\":[…]} (got %s): %w",
					strings.TrimSpace(string(aux.SpinnerFrames)), err)
			}
			if len(out.Status) == 0 {
				out.Status = obj.Status
			}
			if len(out.Activity) == 0 {
				out.Activity = obj.Activity
			}
			out.SpinnerFrames = obj.Status
		}
	}
	*s = out
	return nil
}
