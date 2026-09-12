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
