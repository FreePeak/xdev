package computer

import (
	"errors"
	"fmt"
	"strings"
)

// combo is a normalized key chord: canonical modifiers plus the key.
type combo struct {
	// mods are canonical: "control", "option", "shift", "command", in
	// that fixed order (so the generated command is deterministic).
	mods []string
	// key is one character, or a canonical named key ("enter", "pageup").
	key string
}

// modOrder is the canonical modifier order.
var modOrder = []string{"control", "option", "shift", "command"}

// modDisplay is the chord spelling the model sees back.
var modDisplay = map[string]string{"control": "ctrl", "option": "alt", "shift": "shift", "command": "cmd"}

// namedKeys maps the aliases a model is likely to write onto the canonical
// name the backends switch on.
var namedKeys = map[string]string{
	"enter": "enter", "return": "enter", "cr": "enter",
	"tab": "tab",
	"esc": "escape", "escape": "escape",
	"space": "space", "spacebar": "space",
	"backspace": "backspace", "delete": "backspace", "del": "backspace",
	"forwarddelete": "forwarddelete", "fdel": "forwarddelete",
	"up": "up", "down": "down", "left": "left", "right": "right",
	"home": "home", "end": "end",
	"pageup": "pageup", "pgup": "pageup",
	"pagedown": "pagedown", "pgdn": "pagedown",
}

// parseCombo normalizes a "ctrl+shift+s" style chord. Modifier aliases
// (ctrl/control, cmd/command/meta/super/win, alt/option, shift) all work,
// so one chord reads the same on every platform. A single ASCII letter
// lowercases — "ctrl+S" is the S key with control, not an implicit shift;
// write "ctrl+shift+s" for the capital. A trailing "+" is the plus key.
func parseCombo(s string) (combo, error) {
	var c combo
	raw := strings.TrimSpace(s)
	if raw == "" {
		return c, errors.New(`computer: key needs a chord like "ctrl+c"`)
	}
	parts := strings.Split(raw, "+")
	key := strings.TrimSpace(parts[len(parts)-1])
	if key == "" && len(parts) > 1 {
		key = "+" // "ctrl++" presses the plus key
	}
	if key == "" {
		return c, fmt.Errorf("computer: %q has no key after the modifiers", s)
	}
	for _, m := range parts[:len(parts)-1] {
		switch strings.ToLower(strings.TrimSpace(m)) {
		case "": // the empty slot of "ctrl++"
		case "ctrl", "control":
			c.mods = addMod(c.mods, "control")
		case "cmd", "command", "meta", "super", "win":
			c.mods = addMod(c.mods, "command")
		case "alt", "option", "opt":
			c.mods = addMod(c.mods, "option")
		case "shift":
			c.mods = addMod(c.mods, "shift")
		default:
			return c, fmt.Errorf("computer: unknown modifier %q in %q (want ctrl|cmd|alt|shift)", m, s)
		}
	}
	c.key = canonicalKey(key)
	return c, nil
}

// canonicalKey resolves a key token to the canonical name both backends
// and the result text use.
func canonicalKey(key string) string {
	if named, ok := namedKeys[strings.ToLower(key)]; ok {
		return named
	}
	if len(key) == 1 && key[0] >= 'A' && key[0] <= 'Z' {
		return strings.ToLower(key)
	}
	return key
}

// addMod appends one modifier in canonical order, ignoring duplicates.
func addMod(mods []string, m string) []string {
	for _, have := range mods {
		if have == m {
			return mods
		}
	}
	mods = append(mods, m)
	ordered := make([]string, 0, len(mods))
	for _, want := range modOrder {
		for _, have := range mods {
			if have == want {
				ordered = append(ordered, want)
			}
		}
	}
	return ordered
}

// String renders the chord back in the model's spelling.
func (c combo) String() string {
	if len(c.mods) == 0 {
		return c.key
	}
	parts := make([]string, 0, len(c.mods)+1)
	for _, m := range c.mods {
		parts = append(parts, modDisplay[m])
	}
	return strings.Join(append(parts, c.key), "+")
}
