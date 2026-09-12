package computer

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// linuxBackend drives X11 through xdotool (pointer, keyboard, active
// window) and ImageMagick's import (screenshots). Both are ordinary HID
// tools — no cgo. A Wayland session works when XWayland is present;
// without a display at all Preflight says so instead of letting xdotool
// fail opaquely.
type linuxBackend struct{}

// Name implements backend.
func (linuxBackend) Name() string { return "linux" }

// SizeArgv implements backend.
func (linuxBackend) SizeArgv() []string { return []string{"xdotool", "getdisplaygeometry"} }

// ParseSize implements backend: xdotool prints "WIDTH HEIGHT".
func (linuxBackend) ParseSize(out []byte) (size, error) {
	return parseTwoInts(out, "xdotool")
}

// Preflight implements backend: xdotool needs an X server (X11, or a
// Wayland session's XWayland), and a missing DISPLAY is knowable up front.
func (linuxBackend) Preflight() ([]string, error) {
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return nil, errors.New("computer: no display: DISPLAY (X11) and WAYLAND_DISPLAY are both unset — xdotool drives X11 or a Wayland session's XWayland, so synthetic input has nowhere to go")
	}
	return nil, nil
}

// ParsePreflight implements backend (the X11 gate is the environment, not
// a command's answer).
func (linuxBackend) ParsePreflight(out []byte) error { return nil }

// Argv implements backend.
func (linuxBackend) Argv(req request) ([]string, error) {
	switch req.op {
	case opScreenshot:
		// -silent drops ImageMagick's bell; the root window is the whole
		// (virtual) screen xdotool reports.
		return []string{"import", "-window", "root", "-silent", req.path}, nil
	case opClick:
		return []string{"xdotool", "mousemove", itoa(req.x), itoa(req.y), "click", "1"}, nil
	case opMove:
		return []string{"xdotool", "mousemove", itoa(req.x), itoa(req.y)}, nil
	case opScroll:
		// X11 wheels are buttons 4/5 (vertical) and 6/7 (horizontal).
		button, lines := "5", req.dy
		if req.dy == 0 {
			button, lines = "7", req.dx
			if req.dx < 0 {
				button = "6"
			}
		} else if req.dy < 0 {
			button = "4"
		}
		return []string{"xdotool", "click", "--repeat", itoa(abs(lines)), button}, nil
	case opType:
		// xdotool type types \n as Return; -- ends the option list so
		// text starting with "-" is text.
		return []string{"xdotool", "type", "--", req.text}, nil
	case opKey:
		return []string{"xdotool", "key", "--", xdotoolChord(req.chord)}, nil
	case opWindow:
		// ponytail: name only — xdotool's chained commands cannot return
		// the window name and its geometry in one invocation (the
		// geometry call consumes the id, getwindowname consumes it too).
		// Ceiling: no window geometry on Linux. Upgrade path: a second
		// argv (`xdotool getactivewindow getwindowgeometry --shell`) or an
		// argv list on the backend interface.
		return []string{"xdotool", "getactivewindow", "getwindowname"}, nil
	}
	return nil, fmt.Errorf("computer: linux: unsupported op %q", req.op)
}

// xdotoolChord renders a chord in xdotool's vocabulary: it spells the
// modifiers ctrl/alt/super/shift and uses X keysym names for named keys.
func xdotoolChord(c combo) string {
	parts := make([]string, 0, len(c.mods)+1)
	for _, m := range c.mods {
		switch m {
		case "control":
			parts = append(parts, "ctrl")
		case "option":
			parts = append(parts, "alt")
		case "command":
			parts = append(parts, "super")
		default:
			parts = append(parts, m)
		}
	}
	return strings.Join(append(parts, xKeysym(c.key)), "+")
}

// xKeysym maps a canonical key name onto its X keysym name.
func xKeysym(key string) string {
	if sym, ok := xKeysyms[key]; ok {
		return sym
	}
	return key
}

var xKeysyms = map[string]string{
	"enter": "Return", "tab": "Tab", "space": "space", "escape": "Escape",
	"backspace": "BackSpace", "forwarddelete": "Delete",
	"up": "Up", "down": "Down", "left": "Left", "right": "Right",
	"home": "Home", "end": "End", "pageup": "Prior", "pagedown": "Next",
}

func itoa(v int) string { return strconv.Itoa(v) }

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
