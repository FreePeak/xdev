package theme

// Color-blind mode (research F4, settings `colorBlindMode`): the palette's
// red/green-only distinctions move to a blue/orange pair, which survives
// protanopia and deuteranopia. Deliberately small — only the slots whose
// meaning is carried by red/green alone are remapped:
//
//	error / success            (status accents)
//	tool_diff_removed / added  (diff lines)
//	status_line_git_dirty / clean, status_line_staged
//
// Everything else is untouched, so a theme keeps its identity.
//
// The diff BANDS move with the markers. A band is the claim on a changed row,
// so leaving a green band under a blue marker would leave the red/green pair
// standing in the one place it is hardest to read: a reader who cannot
// separate the two inks still has to tell an addition from a removal, and
// two bands of near-identical lightness do not tell them. Each band is a tint
// of the pair ink instead, so the polarity is carried by hue AND lightness.

// colorBlindPairs are the {removed/error, added/success} inks per polarity:
// orange and blue, darker on light backgrounds to keep contrast.
func colorBlindPairs(dark bool) (errC, okC Color) {
	if dark {
		return Hex("#ff9e64"), Hex("#7aa2f7") // TokyoNight orange / blue
	}
	return Hex("#b45309"), Hex("#1d4ed8")
}

// bands tints an ink into the two backgrounds a diff row of that polarity is
// painted on. On a dark canvas the band is the ink scaled down to a whisper; on
// a light one it is the ink mixed most of the way to white. word is the same
// tint one step louder — the band the changed words ride on. Two mixes rather
// than a table of eight hexes, so a pair that changes moves its bands with it.
func bands(c Color, dark bool) (row, word Color) {
	mix := func(k float64) Color { // k = how far toward the canvas it goes
		out := func(v uint8) uint8 {
			if dark {
				return uint8(float64(v) * (1 - k))
			}
			return uint8(float64(v) + (255-float64(v))*k)
		}
		return Color{out(c.R), out(c.G), out(c.B)}
	}
	if dark {
		return mix(0.86), mix(0.74)
	}
	return mix(0.85), mix(0.72)
}

// ApplyColorBlindMode returns a copy of t with the color-blind remap applied
// (nil-safe). The receiver is never mutated: built-in palettes are shared
// between calls.
func ApplyColorBlindMode(t *Theme) *Theme {
	if t == nil {
		return nil
	}
	errC, okC := colorBlindPairs(t.Dark)
	slots := make(map[string]Color, len(t.Slots))
	for k, v := range t.Slots {
		slots[k] = v
	}
	// A remap is an explicit colour, and "leave it to the terminal" is the one
	// state that hides an explicit colour — so the two cannot both stand. A
	// theme that marked a diff slot terminal-default (the renderer then paints
	// the change in the terminal's own palette) loses the mark here: the mode
	// exists precisely because red and green are the pair this reader cannot
	// separate, and the terminal's own pair is not a different one.
	defaults := make(map[string]bool, len(t.Defaults))
	for k, v := range t.Defaults {
		defaults[k] = v
	}
	remap := func(c Color, in ...string) {
		for _, slot := range in {
			if _, ok := slots[slot]; !ok {
				continue // a slot this theme never carried stays unfilled
			}
			slots[slot] = c
			delete(defaults, slot)
		}
	}
	remap(errC, Error, AccentError, ToolDiffRemoved, StatusLineGitDirty, StatusLineDirty)
	remap(okC, Success, AccentSuccess, ToolDiffAdded, StatusLineGitClean, StatusLineStaged)
	errRow, errWord := bands(errC, t.Dark)
	okRow, okWord := bands(okC, t.Dark)
	remap(errRow, ToolDiffRemovedBg)
	remap(errWord, ToolDiffRemovedWordBg)
	remap(okRow, ToolDiffAddedBg)
	remap(okWord, ToolDiffAddedWordBg)
	return &Theme{Name: t.Name, Dark: t.Dark, Slots: slots, Defaults: defaults, Symbols: t.Symbols}
}
