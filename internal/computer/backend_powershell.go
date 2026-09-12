package computer

import (
	"errors"
	"fmt"
	"strings"
)

// windowsBackend drives Windows through Windows PowerShell, which ships
// with the OS: System.Windows.Forms for the screen size, the cursor and
// SendKeys, plus user32 entry points through Add-Type for clicks, wheel
// scrolling and the foreground window. No cgo, no extra install.
type windowsBackend struct{}

// Name implements backend.
func (windowsBackend) Name() string { return "windows" }

// SizeArgv implements backend: the virtual screen spans every monitor, so
// coordinates are clamped against the full desktop.
func (windowsBackend) SizeArgv() []string {
	return powershell(`Add-Type -AssemblyName System.Windows.Forms; $v=[System.Windows.Forms.SystemInformation]::VirtualScreen; "$($v.Width)x$($v.Height)"`)
}

// ParseSize implements backend.
func (windowsBackend) ParseSize(out []byte) (size, error) { return parseWxH(out, "powershell") }

// Preflight implements backend: PowerShell reports a rejected SendInput
// itself, and a session without an interactive desktop has no screen size,
// so there is nothing to check ahead of the op.
func (windowsBackend) Preflight() ([]string, error) { return nil, nil }

// ParsePreflight implements backend (unreachable: Preflight returns no
// command on Windows).
func (windowsBackend) ParsePreflight(out []byte) error { return nil }

// Argv implements backend.
func (windowsBackend) Argv(req request) ([]string, error) {
	switch req.op {
	case opScreenshot:
		return powershell(fmt.Sprintf(`Add-Type -AssemblyName System.Windows.Forms,System.Drawing; $v=[System.Windows.Forms.SystemInformation]::VirtualScreen; $b=[System.Drawing.Bitmap]::new($v.Width,$v.Height); $g=[System.Drawing.Graphics]::FromImage($b); $g.CopyFromScreen($v.X,$v.Y,0,0,$b.Size); $b.Save(%s,[System.Drawing.Imaging.ImageFormat]::Png); $g.Dispose(); $b.Dispose()`, psQuote(req.path))), nil
	case opClick:
		return powershell(fmt.Sprintf(`Add-Type -AssemblyName System.Windows.Forms; %s [System.Windows.Forms.Cursor]::Position=[System.Drawing.Point]::new(%d,%d); [Xdev.User32]::mouse_event(0x0002,0,0,0,0); [Xdev.User32]::mouse_event(0x0004,0,0,0,0)`, psUser32, req.x, req.y)), nil
	case opMove:
		return powershell(fmt.Sprintf(`Add-Type -AssemblyName System.Windows.Forms; [System.Windows.Forms.Cursor]::Position=[System.Drawing.Point]::new(%d,%d)`, req.x, req.y)), nil
	case opScroll:
		// WHEEL (0x0800) scrolls vertically, HWHEEL (0x1000) horizontally;
		// one notch is WHEEL_DELTA = 120, positive means up/left.
		if req.dy != 0 {
			return powershell(fmt.Sprintf(`Add-Type -AssemblyName System.Windows.Forms; %s [Xdev.User32]::mouse_event(0x0800,0,0,%d,0)`, psUser32, -120*req.dy)), nil
		}
		return powershell(fmt.Sprintf(`Add-Type -AssemblyName System.Windows.Forms; %s [Xdev.User32]::mouse_event(0x1000,0,0,%d,0)`, psUser32, 120*req.dx)), nil
	case opType:
		return powershell(`Add-Type -AssemblyName System.Windows.Forms; [System.Windows.Forms.SendKeys]::SendWait(` + psQuote(psSendKeys(req.text)) + `)`), nil
	case opKey:
		chord, err := sendKeysChord(req.chord)
		if err != nil {
			return nil, err
		}
		return powershell(`Add-Type -AssemblyName System.Windows.Forms; [System.Windows.Forms.SendKeys]::SendWait(` + psQuote(chord) + `)`), nil
	case opWindow:
		return powershell(psUser32 + ` $h=[Xdev.User32]::GetForegroundWindow(); $s=[System.Text.StringBuilder]::new(512); [void][Xdev.User32]::GetWindowText($h,$s,512); "$($s.ToString()) (hwnd $h)"`), nil
	}
	return nil, fmt.Errorf("computer: windows: unsupported op %q", req.op)
}

// psUser32 declares the user32 entry points Windows needs beyond
// System.Windows.Forms.
const psUser32 = `Add-Type -Namespace Xdev -Name User32 -MemberDefinition '[DllImport("user32.dll")] public static extern void mouse_event(uint f, uint dx, uint dy, int d, int e); [DllImport("user32.dll")] public static extern IntPtr GetForegroundWindow(); [DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern int GetWindowText(IntPtr h, System.Text.StringBuilder s, int n);';`

// powershell wraps a script for the Windows PowerShell that ships with
// Windows.
func powershell(script string) []string {
	return []string{"powershell", "-NoProfile", "-NonInteractive", "-Command", script}
}

// psQuote renders s as a PowerShell single-quoted literal.
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// sendKeysChord renders a chord in SendKeys syntax: ^ (control), % (alt),
// + (shift). SendKeys has no Windows-key modifier, so that chord is an
// honest error rather than a wrong keypress.
func sendKeysChord(c combo) (string, error) {
	var prefix strings.Builder
	for _, m := range c.mods {
		switch m {
		case "control":
			prefix.WriteByte('^')
		case "option":
			prefix.WriteByte('%')
		case "shift":
			prefix.WriteByte('+')
		case "command":
			return "", errors.New("computer: windows: SendKeys cannot press the Windows/cmd modifier — use a shortcut the app itself exposes")
		}
	}
	if named, ok := winKeyNames[c.key]; ok {
		return prefix.String() + named, nil
	}
	return prefix.String() + psSendKeys(c.key), nil
}

// winKeyNames are the SendKeys names for the keys that are not characters.
var winKeyNames = map[string]string{
	"enter": "{ENTER}", "tab": "{TAB}", "space": " ", "escape": "{ESC}",
	"backspace": "{BACKSPACE}", "forwarddelete": "{DELETE}",
	"up": "{UP}", "down": "{DOWN}", "left": "{LEFT}", "right": "{RIGHT}",
	"home": "{HOME}", "end": "{END}", "pageup": "{PGUP}", "pagedown": "{PGDN}",
}

// psSendKeys escapes the SendKeys metacharacters by brace-wrapping them,
// and types an embedded newline as Return.
func psSendKeys(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString("{ENTER}")
		case r == '\t':
			b.WriteString("{TAB}")
		case strings.ContainsRune("+^%~(){}[]", r):
			b.WriteString("{")
			b.WriteRune(r)
			b.WriteString("}")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
