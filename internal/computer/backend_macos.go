package computer

import (
	"errors"
	"fmt"
	"strings"
)

// darwinBackend drives macOS with the two tools every Mac already has:
// screencapture for images and osascript for input. osascript carries both
// AppleScript (System Events: keystrokes, typing, frontmost window) and
// JXA, whose ObjC bridge reaches CoreGraphics for pointer events — so no
// third-party binary and no cgo is needed. Synthetic input also needs an
// Accessibility grant for the process running xdev, which Preflight checks
// rather than letting a click vanish silently.
type darwinBackend struct{}

// Name implements backend.
func (darwinBackend) Name() string { return "darwin" }

// SizeArgv asks AppKit for the main screen's frame in points — the units
// CGEvent pointer coordinates use. screencapture's PNG is in pixels (2x on
// Retina), so the screenshot's dimensions are deliberately twice this.
func (darwinBackend) SizeArgv() []string {
	return jxa(`ObjC.import("AppKit"); var f=$.NSScreen.mainScreen.frame; f.size.width+"x"+f.size.height`)
}

// ParseSize implements backend.
func (darwinBackend) ParseSize(out []byte) (size, error) { return parseWxH(out, "osascript") }

// Preflight implements backend: System Events reports whether this process
// may synthesize input. Without the grant, CGEventPost drops every event
// without an error, so a silent no-op is the failure mode to rule out.
func (darwinBackend) Preflight() ([]string, error) {
	return []string{"osascript", "-e", `tell application "System Events" to get UI elements enabled`}, nil
}

// ParsePreflight implements backend.
func (darwinBackend) ParsePreflight(out []byte) error {
	if strings.TrimSpace(string(out)) == "true" {
		return nil
	}
	return errors.New("computer: macOS has not granted Accessibility to the process running xdev, so synthetic clicks and keystrokes would be dropped silently: enable it under System Settings → Privacy & Security → Accessibility, then restart the terminal")
}

// Argv implements backend.
func (darwinBackend) Argv(req request) ([]string, error) {
	switch req.op {
	case opScreenshot:
		// -x: no capture sound (a headless/automation capture must be quiet).
		return []string{"screencapture", "-x", req.path}, nil
	case opClick:
		return jxa(fmt.Sprintf(`ObjC.import("CoreGraphics");
var p={x:%d,y:%d};
function post(t){$.CGEventPost($.kCGHIDEventTap, $.CGEventCreateMouseEvent($(), t, p, $.kCGMouseButtonLeft));}
post($.kCGEventMouseMoved); post($.kCGEventLeftMouseDown); post($.kCGEventLeftMouseUp);`, req.x, req.y)), nil
	case opMove:
		return jxa(fmt.Sprintf(`ObjC.import("CoreGraphics"); $.CGEventPost($.kCGHIDEventTap, $.CGEventCreateMouseEvent($(), $.kCGEventMouseMoved, {x:%d,y:%d}, $.kCGMouseButtonLeft));`, req.x, req.y)), nil
	case opScroll:
		return jxa(scrollScript(req.dx, req.dy)), nil
	case opType:
		return []string{"osascript", "-e", typeScript(req.text)}, nil
	case opKey:
		return []string{"osascript", "-e", keyScript(req.chord)}, nil
	case opWindow:
		return []string{"osascript", "-e", frontWindowScript}, nil
	}
	return nil, fmt.Errorf("computer: darwin: unsupported op %q", req.op)
}

// jxa wraps a JavaScript for Automation script.
func jxa(script string) []string {
	return []string{"osascript", "-l", "JavaScript", "-e", script}
}

// scrollScript posts one wheel event: the vertical component when dy is
// set (CoreGraphics counts a positive wheel as "up", so the sign flips),
// horizontal otherwise.
func scrollScript(dx, dy int) string {
	if dy != 0 {
		return fmt.Sprintf(`ObjC.import("CoreGraphics"); $.CGEventPost($.kCGHIDEventTap, $.CGEventCreateScrollWheelEvent($(), $.kCGScrollEventUnitLine, 1, %d));`, -dy)
	}
	return fmt.Sprintf(`ObjC.import("CoreGraphics"); $.CGEventPost($.kCGHIDEventTap, $.CGEventCreateScrollWheelEvent($(), $.kCGScrollEventUnitLine, 2, 0, %d));`, dx)
}

// macKeyCodes are the Carbon key codes for the keys `keystroke` cannot
// express, so "key":"enter" and "key":"down" press the real key.
var macKeyCodes = map[string]int{
	"enter": 36, "tab": 48, "space": 49, "escape": 53, "backspace": 51,
	"forwarddelete": 117, "up": 126, "down": 125, "left": 123, "right": 124,
	"home": 115, "end": 119, "pageup": 116, "pagedown": 121,
}

// keyScript builds the System Events statement for a chord: `key code` for
// a named key, `keystroke` for a character, plus the modifier downs.
func keyScript(c combo) string {
	stmt := "keystroke " + appleScriptQuote(c.key)
	if code, ok := macKeyCodes[c.key]; ok {
		stmt = fmt.Sprintf("key code %d", code)
	}
	if len(c.mods) > 0 {
		downs := make([]string, 0, len(c.mods))
		for _, m := range c.mods {
			downs = append(downs, m+" down")
		}
		stmt += " using {" + strings.Join(downs, ", ") + "}"
	}
	return `tell application "System Events" to ` + stmt
}

// typeScript builds the keystroke statements for text. A newline or tab is
// a real key press (keystroke cannot type them), everything else is one
// literal — so the text never has to survive a shell or an argument
// parser.
func typeScript(text string) string {
	var b strings.Builder
	b.WriteString(`tell application "System Events"`)
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			b.WriteString("\n\tkeystroke " + appleScriptQuote(lit.String()))
			lit.Reset()
		}
	}
	for _, r := range text {
		switch r {
		case '\n':
			flush()
			b.WriteString("\n\tkey code 36") // Return
		case '\t':
			flush()
			b.WriteString("\n\tkey code 48") // Tab
		case '\r':
			// CRLF from a model's text is one Return, not two.
		default:
			lit.WriteRune(r)
		}
	}
	flush()
	b.WriteString("\nend tell")
	return b.String()
}

// frontWindowScript reports the frontmost process and its front window as
// tab-separated app, window, x, y, w, h. A process without a window (the
// Finder desktop, a menubar app) reports its name alone.
const frontWindowScript = `tell application "System Events"
	set p to first application process whose frontmost is true
	set n to name of p
	try
		set fw to front window of p
		return n & "\t" & (name of fw) & "\t" & (item 1 of (position of fw)) & "\t" & (item 2 of (position of fw)) & "\t" & (item 1 of (size of fw)) & "\t" & (item 2 of (size of fw))
	on error
		return n
	end try
end tell`

// appleScriptQuote renders s as an AppleScript string literal (backslash
// and the double quote escape with a backslash).
func appleScriptQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// annotateError adds the actionable hint for the one screencapture failure
// worth naming: without a Screen Recording grant macOS answers "could not
// create image from display" (the other silent outcome is a wallpaper-only
// image, which cannot be told apart from a clean desktop).
func annotateError(name, stderr string) string {
	if name == "screencapture" && strings.Contains(stderr, "could not create image from display") {
		return "grant Screen Recording to the terminal or app running xdev (System Settings → Privacy & Security → Screen Recording)"
	}
	return ""
}
