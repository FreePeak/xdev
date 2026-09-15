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

// colorBlindPairs are the {removed/error, added/success} inks per polarity:
// orange and blue, darker on light backgrounds to keep contrast.
func colorBlindPairs(dark bool) (errC, okC Color) {
	if dark {
		return Hex("#ff9e64"), Hex("#7aa2f7") // TokyoNight orange / blue
	}
	return Hex("#b45309"), Hex("#1d4ed8")
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
	// state that hides an explicit colour — so the two cannot both stand. The
	// launch themes mark their diff slots terminal-default (the renderer then
	// paints the change in the terminal's own palette); the mode exists
	// precisely because red and green are the pair this reader cannot
	// separate, so that mark has to give way here.
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
	return &Theme{Name: t.Name, Dark: t.Dark, Slots: slots, Defaults: defaults, Symbols: t.Symbols}
}
