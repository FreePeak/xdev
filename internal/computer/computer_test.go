package computer

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/tool"
)

// recorder is the runFn seam: it records every argv and answers through
// reply, so the tool's own behavior is testable without exec'ing a
// platform binary.
type recorder struct {
	calls [][]string
	reply func(argv []string) ([]byte, error)
}

func (r *recorder) run(_ context.Context, argv []string) ([]byte, error) {
	r.calls = append(r.calls, argv)
	if r.reply == nil {
		return nil, nil
	}
	return r.reply(argv)
}

// log is every recorded argv joined into one string, for substring checks.
func (r *recorder) log() string {
	var b strings.Builder
	for _, argv := range r.calls {
		b.WriteString(strings.Join(argv, " "))
		b.WriteString("\n")
	}
	return b.String()
}

// argvEqual reports whether two commands are identical.
func argvEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// newToolFor returns a tool with an injected backend and runner.
func newToolFor(cfg Config, b backend, r *recorder) *Tool {
	tl := NewTool(cfg)
	tl.backend = b
	if r != nil {
		tl.runFn = r.run
	}
	return tl
}

// replyFor answers the backend's gate and size queries (the two commands a
// tool issues on its own); every other command prints nothing.
func replyFor(b backend, sizeLine string) func([]string) ([]byte, error) {
	gate, _ := b.Preflight()
	return func(argv []string) ([]byte, error) {
		switch {
		case argvEqual(argv, b.SizeArgv()):
			return []byte(sizeLine), nil
		case len(gate) > 0 && argvEqual(argv, gate):
			return []byte("true\n"), nil
		}
		return nil, nil
	}
}

// displayEnv opens the X11 backend's preflight gate.
func displayEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DISPLAY", ":0")
	t.Setenv("WAYLAND_DISPLAY", "")
}

// call runs one op and fails on a harness error.
func call(t *testing.T, tl *Tool, raw string) tool.Result {
	t.Helper()
	res, err := tl.Execute(context.Background(), json.RawMessage(raw))
	if err != nil {
		t.Fatalf("Execute(%s): harness error: %v", raw, err)
	}
	return res
}

// enabledCfg is the gate-on config every op test uses.
func enabledCfg(dir string) Config { return Config{Enabled: true, Dir: dir} }

// mustCombo parses a chord or fails the test.
func mustCombo(t *testing.T, s string) combo {
	t.Helper()
	c, err := parseCombo(s)
	if err != nil {
		t.Fatalf("parseCombo(%q): %v", s, err)
	}
	return c
}

const (
	darwinSizeScript = `ObjC.import("AppKit"); var f=$.NSScreen.mainScreen.frame; f.size.width+"x"+f.size.height`
	darwinClick      = `ObjC.import("CoreGraphics");
var p={x:12,y:34};
function post(t){$.CGEventPost($.kCGHIDEventTap, $.CGEventCreateMouseEvent($(), t, p, $.kCGMouseButtonLeft));}
post($.kCGEventMouseMoved); post($.kCGEventLeftMouseDown); post($.kCGEventLeftMouseUp);`
	darwinMove = `ObjC.import("CoreGraphics"); $.CGEventPost($.kCGHIDEventTap, $.CGEventCreateMouseEvent($(), $.kCGEventMouseMoved, {x:12,y:34}, $.kCGMouseButtonLeft));`
)

// TestArgvMapping is the op -> command mapping for all three backends: one
// request, one exact argv. It runs on every OS (the backends carry no GOOS
// suffix), which is what makes the cross-platform mapping provable here.
func TestArgvMapping(t *testing.T) {
	const shot = "/tmp/shot.png"
	cases := []struct {
		name string
		b    backend
		req  request
		want []string
	}{
		// macOS: screencapture for images, osascript (AppleScript + JXA) for input.
		{"macos/screenshot", darwinBackend{}, request{op: opScreenshot, path: shot},
			[]string{"screencapture", "-x", shot}},
		{"macos/click", darwinBackend{}, request{op: opClick, x: 12, y: 34},
			[]string{"osascript", "-l", "JavaScript", "-e", darwinClick}},
		{"macos/move", darwinBackend{}, request{op: opMove, x: 12, y: 34},
			[]string{"osascript", "-l", "JavaScript", "-e", darwinMove}},
		{"macos/scroll-right", darwinBackend{}, request{op: opScroll, dx: 2},
			[]string{"osascript", "-l", "JavaScript", "-e", `ObjC.import("CoreGraphics"); $.CGEventPost($.kCGHIDEventTap, $.CGEventCreateScrollWheelEvent($(), $.kCGScrollEventUnitLine, 2, 0, 2));`}},
		{"macos/type", darwinBackend{}, request{op: opType, text: "x"},
			[]string{"osascript", "-e", "tell application \"System Events\"\n\tkeystroke \"x\"\nend tell"}},
		{"macos/scroll-down", darwinBackend{}, request{op: opScroll, dy: 3},
			[]string{"osascript", "-l", "JavaScript", "-e", `ObjC.import("CoreGraphics"); $.CGEventPost($.kCGHIDEventTap, $.CGEventCreateScrollWheelEvent($(), $.kCGScrollEventUnitLine, 1, -3));`}},
		{"macos/key-named", darwinBackend{}, request{op: opKey, chord: mustCombo(t, "enter")},
			[]string{"osascript", "-e", `tell application "System Events" to key code 36`}},
		{"macos/key-chord", darwinBackend{}, request{op: opKey, chord: mustCombo(t, "cmd+shift+s")},
			[]string{"osascript", "-e", `tell application "System Events" to keystroke "s" using {shift down, command down}`}},
		{"macos/window", darwinBackend{}, request{op: opWindow},
			[]string{"osascript", "-e", frontWindowScript}},

		// Linux: xdotool for input, ImageMagick import for screenshots.
		{"x11/screenshot", linuxBackend{}, request{op: opScreenshot, path: shot},
			[]string{"import", "-window", "root", "-silent", shot}},
		{"x11/click", linuxBackend{}, request{op: opClick, x: 12, y: 34},
			[]string{"xdotool", "mousemove", "12", "34", "click", "1"}},
		{"x11/move", linuxBackend{}, request{op: opMove, x: 12, y: 34},
			[]string{"xdotool", "mousemove", "12", "34"}},
		{"x11/scroll-down", linuxBackend{}, request{op: opScroll, dy: 3},
			[]string{"xdotool", "click", "--repeat", "3", "5"}},
		{"x11/scroll-up", linuxBackend{}, request{op: opScroll, dy: -3},
			[]string{"xdotool", "click", "--repeat", "3", "4"}},
		{"x11/scroll-left", linuxBackend{}, request{op: opScroll, dx: -2},
			[]string{"xdotool", "click", "--repeat", "2", "6"}},
		{"x11/type", linuxBackend{}, request{op: opType, text: "-oops"},
			[]string{"xdotool", "type", "--", "-oops"}},
		{"x11/key", linuxBackend{}, request{op: opKey, chord: mustCombo(t, "ctrl+alt+delete")},
			[]string{"xdotool", "key", "--", "ctrl+alt+BackSpace"}},
		{"x11/window", linuxBackend{}, request{op: opWindow},
			[]string{"xdotool", "getactivewindow", "getwindowname"}},

		// Windows: PowerShell + user32 through Add-Type.
		{"windows/screenshot", windowsBackend{}, request{op: opScreenshot, path: shot},
			powershell(`Add-Type -AssemblyName System.Windows.Forms,System.Drawing; $v=[System.Windows.Forms.SystemInformation]::VirtualScreen; $b=[System.Drawing.Bitmap]::new($v.Width,$v.Height); $g=[System.Drawing.Graphics]::FromImage($b); $g.CopyFromScreen($v.X,$v.Y,0,0,$b.Size); $b.Save('/tmp/shot.png',[System.Drawing.Imaging.ImageFormat]::Png); $g.Dispose(); $b.Dispose()`)},
		{"windows/click", windowsBackend{}, request{op: opClick, x: 12, y: 34},
			powershell(`Add-Type -AssemblyName System.Windows.Forms; ` + psUser32 + ` [System.Windows.Forms.Cursor]::Position=[System.Drawing.Point]::new(12,34); [Xdev.User32]::mouse_event(0x0002,0,0,0,0); [Xdev.User32]::mouse_event(0x0004,0,0,0,0)`)},
		{"windows/move", windowsBackend{}, request{op: opMove, x: 12, y: 34},
			powershell(`Add-Type -AssemblyName System.Windows.Forms; [System.Windows.Forms.Cursor]::Position=[System.Drawing.Point]::new(12,34)`)},
		{"windows/scroll-down", windowsBackend{}, request{op: opScroll, dy: 3},
			powershell(`Add-Type -AssemblyName System.Windows.Forms; ` + psUser32 + ` [Xdev.User32]::mouse_event(0x0800,0,0,-360,0)`)},
		{"windows/scroll-right", windowsBackend{}, request{op: opScroll, dx: 2},
			powershell(`Add-Type -AssemblyName System.Windows.Forms; ` + psUser32 + ` [Xdev.User32]::mouse_event(0x1000,0,0,240,0)`)},
		{"windows/type", windowsBackend{}, request{op: opType, text: "a+b"},
			powershell(`Add-Type -AssemblyName System.Windows.Forms; [System.Windows.Forms.SendKeys]::SendWait('a{+}b')`)},
		{"windows/key", windowsBackend{}, request{op: opKey, chord: mustCombo(t, "ctrl+c")},
			powershell(`Add-Type -AssemblyName System.Windows.Forms; [System.Windows.Forms.SendKeys]::SendWait('^c')`)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.b.Argv(tc.req)
			if err != nil {
				t.Fatalf("Argv: %v", err)
			}
			if !argvEqual(got, tc.want) {
				t.Fatalf("argv mismatch\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
	// The size query is the clamp's source of truth, so its argv is part of
	// the mapping too.
	for _, tc := range []struct {
		b    backend
		want []string
	}{
		{darwinBackend{}, []string{"osascript", "-l", "JavaScript", "-e", darwinSizeScript}},
		{linuxBackend{}, []string{"xdotool", "getdisplaygeometry"}},
		{windowsBackend{}, powershell(`Add-Type -AssemblyName System.Windows.Forms; $v=[System.Windows.Forms.SystemInformation]::VirtualScreen; "$($v.Width)x$($v.Height)"`)},
	} {
		if got := tc.b.SizeArgv(); !argvEqual(got, tc.want) {
			t.Fatalf("%s size argv = %q, want %q", tc.b.Name(), got, tc.want)
		}
	}
}

// TestUnsupportedOpsAreErrors covers the two platform ceilings: a Windows
// chord with the cmd/Windows modifier, and an op a backend does not know.
func TestUnsupportedOpsAreErrors(t *testing.T) {
	if _, err := (windowsBackend{}).Argv(request{op: opKey, chord: mustCombo(t, "cmd+c")}); err == nil {
		t.Fatal("windows: cmd chord must be an error, SendKeys has no Windows key")
	}
	for _, b := range []backend{darwinBackend{}, linuxBackend{}, windowsBackend{}} {
		if _, err := b.Argv(request{op: op("nope")}); err == nil {
			t.Fatalf("%s: an unknown op must be an error", b.Name())
		}
	}
}

// TestDisabledByDefault is the settings gate: the shipped default refuses
// every op before anything reaches the platform.
func TestDisabledByDefault(t *testing.T) {
	rec := &recorder{reply: replyFor(darwinBackend{}, "1512x982")}
	tl := newToolFor(Config{}, darwinBackend{}, rec) // Config{} is the default: off
	res := call(t, tl, `{"op":"screenshot"}`)
	if !res.IsError {
		t.Fatalf("disabled tool ran an op: %+v", res)
	}
	for _, want := range []string{"computer is disabled", "computer.enabled", "xdev config set computer.enabled true"} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("refusal %q missing %q", res.Text, want)
		}
	}
	if len(rec.calls) != 0 {
		t.Fatalf("disabled tool executed %d commands: %v", len(rec.calls), rec.calls)
	}
}

// TestSettingsGate proves the computer.enabled / computer.timeout YAML keys
// reach the tool through the real layered loader, and that the default
// (no `computer` block) is off.
func TestSettingsGate(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir()) // no global layer, no user config

	plain := t.TempDir()
	cfg, err := config.LoadSettings(plain, nil)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if got := FromSettings(cfg); got.Enabled || got.Timeout != 0 {
		t.Fatalf("default settings enabled desktop control: %+v", got)
	}

	on := t.TempDir()
	if err := os.MkdirAll(filepath.Join(on, ".xdev"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "computer:\n  enabled: true\n  timeout: 5\n"
	if err := os.WriteFile(filepath.Join(on, ".xdev", "config.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.LoadSettings(on, nil)
	if err != nil {
		t.Fatalf("LoadSettings(project): %v", err)
	}
	got := FromSettings(cfg)
	if !got.Enabled {
		t.Fatalf("computer.enabled: true did not enable the tool: %+v", got)
	}
	if got.Timeout != 5*time.Second {
		t.Fatalf("computer.timeout = %v, want 5s", got.Timeout)
	}
	if tl := NewTool(got); tl.timeout() != 5*time.Second {
		t.Fatalf("tool timeout = %v, want 5s", tl.timeout())
	}

	bad := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bad, ".xdev"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, ".xdev", "config.yml"), []byte("computer:\n  timeout: -1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.LoadSettings(bad, nil); err == nil {
		t.Fatal("a negative computer.timeout must be reported")
	}
}

// TestCoordinateClampingAndNotes checks the clamp against the size the
// backend reports, on the command that actually runs.
func TestCoordinateClampingAndNotes(t *testing.T) {
	const size = "100x80"
	cases := []struct {
		name     string
		raw      string
		wantCmd  string
		wantText string
	}{
		{"over-the-edge", `{"op":"click","x":5000,"y":-5}`, "p={x:99,y:0}", "click 99,0 (screen 100x80) (clamped from 5000,-5)"},
		{"inside", `{"op":"move","x":7,"y":8}`, "{x:7,y:8}", "move 7,8 (screen 100x80)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{reply: replyFor(darwinBackend{}, size)}
			tl := newToolFor(enabledCfg(t.TempDir()), darwinBackend{}, rec)
			res := call(t, tl, tc.raw)
			if res.IsError {
				t.Fatalf("op failed: %s", res.Text)
			}
			if !strings.Contains(rec.log(), tc.wantCmd) {
				t.Fatalf("command %q does not contain %q", rec.log(), tc.wantCmd)
			}
			if res.Text != tc.wantText {
				t.Fatalf("text = %q, want %q", res.Text, tc.wantText)
			}
		})
	}

	// A failed size query fails the op: an unclamped coordinate would land
	// outside every display.
	rec := &recorder{reply: replyFor(darwinBackend{}, "not a size")}
	tl := newToolFor(enabledCfg(t.TempDir()), darwinBackend{}, rec)
	res := call(t, tl, `{"op":"click","x":1,"y":1}`)
	if !res.IsError || !strings.Contains(res.Text, "screen size") {
		t.Fatalf("a broken size query must fail the op: %+v", res)
	}
}

// TestScrollClampAndValidation bounds one scroll op.
func TestScrollClampAndValidation(t *testing.T) {
	displayEnv(t)
	rec := &recorder{reply: replyFor(linuxBackend{}, "1512 982")}
	tl := newToolFor(enabledCfg(t.TempDir()), linuxBackend{}, rec)
	res := call(t, tl, `{"op":"scroll","dy":5000}`)
	if res.IsError {
		t.Fatalf("scroll failed: %s", res.Text)
	}
	if !strings.Contains(rec.log(), "click --repeat 100 5") {
		t.Fatalf("scroll not clamped: %q", rec.log())
	}
	if !strings.Contains(res.Text, "(clamped from") {
		t.Fatalf("scroll clamp not reported: %q", res.Text)
	}
	if res := call(t, tl, `{"op":"scroll"}`); !res.IsError || !strings.Contains(res.Text, "needs a non-zero dx or dy") {
		t.Fatalf("empty scroll must fail: %+v", res)
	}
}

// TestArgValidation covers the argument contract per op.
func TestArgValidation(t *testing.T) {
	rec := &recorder{reply: replyFor(darwinBackend{}, "1512x982")}
	tl := newToolFor(enabledCfg(t.TempDir()), darwinBackend{}, rec)
	cases := []struct{ name, raw, want string }{
		{"click-without-coords", `{"op":"click"}`, "needs integer x and y"},
		{"move-without-y", `{"op":"move","x":3}`, "needs integer x and y"},
		{"type-without-text", `{"op":"type"}`, "needs text"},
		{"type-over-cap", `{"op":"type","text":"` + strings.Repeat("a", maxTextBytes+1) + `"}`, "over the 4096-byte cap"},
		{"key-empty", `{"op":"key"}`, "key needs a chord"},
		{"key-bad-modifier", `{"op":"key","key":"hyper+c"}`, "unknown modifier"},
		{"unknown-op", `{"op":"levitate"}`, "unknown op \"levitate\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := call(t, tl, tc.raw)
			if !res.IsError || !strings.Contains(res.Text, tc.want) {
				t.Fatalf("result %+v does not mention %q", res, tc.want)
			}
		})
	}
	// A malformed payload is a harness error, not a tool failure.
	if _, err := tl.Execute(context.Background(), json.RawMessage(`{"op":`)); err == nil {
		t.Fatal("malformed arguments must be a harness error")
	}
}

// TestScreenshotResultAndBounds checks the screenshot result path: real
// dimensions from the PNG header, and the two bounded-failure shapes.
func TestScreenshotResultAndBounds(t *testing.T) {
	dir := t.TempDir()
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 3, 2))); err != nil {
		t.Fatal(err)
	}
	// The runner stand-in plays screencapture: it writes a real PNG to the
	// path the tool chose.
	rec := &recorder{reply: func(argv []string) ([]byte, error) {
		if argvEqual(argv, darwinBackend{}.SizeArgv()) {
			return []byte("1512x982"), nil
		}
		if len(argv) == 3 && argv[0] == "screencapture" {
			return nil, os.WriteFile(argv[2], pngBytes.Bytes(), 0o644)
		}
		return nil, nil
	}}
	tl := newToolFor(enabledCfg(dir), darwinBackend{}, rec)
	res := call(t, tl, `{"op":"screenshot"}`)
	if res.IsError {
		t.Fatalf("screenshot failed: %s", res.Text)
	}
	if !strings.Contains(res.Text, "3x2,") {
		t.Fatalf("dimensions missing from %q", res.Text)
	}
	path, _ := res.Details.(map[string]any)["path"].(string)
	if filepath.Dir(path) != dir {
		t.Fatalf("screenshot path %q is not in config dir %q", path, dir)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("screenshot file: %v", err)
	}

	// The same failure through Execute: the model gets an error result, not
	// a cheerful path to a file that was never written.
	blank := &recorder{reply: func(argv []string) ([]byte, error) {
		if len(argv) == 3 && argv[0] == "screencapture" {
			return nil, os.WriteFile(argv[2], nil, 0o644) // capture denied
		}
		return nil, nil
	}}
	if res := call(t, newToolFor(enabledCfg(dir), darwinBackend{}, blank), `{"op":"screenshot"}`); !res.IsError {
		t.Fatalf("an empty capture must be an error result: %+v", res)
	}

	// An empty capture is what a denied Screen Recording grant yields, and
	// every failure branch must reach the model as an error.
	empty := filepath.Join(dir, "empty.png")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	text, _, failed := screenshotResult(empty, map[string]any{})
	if !strings.Contains(text, "Screen Recording") || !failed {
		t.Fatalf("empty capture = %q failed=%v", text, failed)
	}
	if _, err := os.Stat(empty); !os.IsNotExist(err) {
		t.Fatal("an empty capture should be removed")
	}

	// Oversized captures are refused rather than kept.
	big := filepath.Join(dir, "big.png")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxScreenshotBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if text, _, failed := screenshotResult(big, map[string]any{}); !strings.Contains(text, "over the") || !failed {
		t.Fatalf("oversized capture = %q failed=%v", text, failed)
	}
	if text, _, failed := screenshotResult(filepath.Join(dir, "gone.png"), map[string]any{}); !strings.Contains(text, "produced no file") || !failed {
		t.Fatalf("missing capture = %q failed=%v", text, failed)
	}
}

// TestWindowPassthrough: the focused-window op returns whatever the
// platform reported, and an empty report is a failure.
func TestWindowPassthrough(t *testing.T) {
	rec := &recorder{reply: func(argv []string) ([]byte, error) {
		return []byte("Ghostty\tzsh\t10\t20\t800\t600\n"), nil
	}}
	tl := newToolFor(enabledCfg(t.TempDir()), darwinBackend{}, rec)
	res := call(t, tl, `{"op":"window"}`)
	if res.IsError || res.Text != "Ghostty\tzsh\t10\t20\t800\t600" {
		t.Fatalf("window result = %+v", res)
	}
	quiet := newToolFor(enabledCfg(t.TempDir()), darwinBackend{}, &recorder{})
	if res := call(t, quiet, `{"op":"window"}`); !res.IsError {
		t.Fatalf("an empty window report must fail: %+v", res)
	}
}

// TestTypeAndKeyScripts pins the AppleScript generation: a literal that
// survives quotes and backslashes, Return/Tab as real key presses, the
// named keys, and the modifier clause.
func TestTypeAndKeyScripts(t *testing.T) {
	cases := []struct{ name, text, want string }{
		{"plain", "hi", "tell application \"System Events\"\n\tkeystroke \"hi\"\nend tell"},
		{"quotes", `say "hi"`, "tell application \"System Events\"\n\tkeystroke \"say \\\"hi\\\"\"\nend tell"},
		{"newline", "a\nb", "tell application \"System Events\"\n\tkeystroke \"a\"\n\tkey code 36\n\tkeystroke \"b\"\nend tell"},
		{"tab", "a\tb", "tell application \"System Events\"\n\tkeystroke \"a\"\n\tkey code 48\n\tkeystroke \"b\"\nend tell"},
		{"crlf-is-one-return", "a\r\nb", "tell application \"System Events\"\n\tkeystroke \"a\"\n\tkey code 36\n\tkeystroke \"b\"\nend tell"},
		{"trailing-newline", "a\n", "tell application \"System Events\"\n\tkeystroke \"a\"\n\tkey code 36\nend tell"},
	}
	for _, tc := range cases {
		t.Run("type/"+tc.name, func(t *testing.T) {
			if got := typeScript(tc.text); got != tc.want {
				t.Fatalf("typeScript\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
	key := []struct{ chord, want string }{
		{"enter", `tell application "System Events" to key code 36`},
		{"down", `tell application "System Events" to key code 125`},
		{"ctrl+c", `tell application "System Events" to keystroke "c" using {control down}`},
		{"cmd+alt+shift+esc", `tell application "System Events" to key code 53 using {option down, shift down, command down}`},
	}
	for _, tc := range key {
		t.Run("key/"+tc.chord, func(t *testing.T) {
			if got := keyScript(mustCombo(t, tc.chord)); got != tc.want {
				t.Fatalf("keyScript(%q)\n got: %q\nwant: %q", tc.chord, got, tc.want)
			}
		})
	}
}

// TestParseCombo normalizes the chord vocabulary (one spelling for every
// platform) and rejects what it cannot express.
func TestParseCombo(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ctrl+c", "ctrl+c"},
		{"control+C", "ctrl+c"},
		{"shift+cmd+s", "shift+cmd+s"},
		{"cmd+shift+s", "shift+cmd+s"},
		{"super+shift+s", "shift+cmd+s"},
		{"meta+s", "cmd+s"},
		{"alt+tab", "alt+tab"},
		{"option+return", "alt+enter"},
		{"ctrl++", "ctrl++"},
		{"ctrl+", "ctrl++"},
		{"f5", "f5"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			c, err := parseCombo(tc.in)
			if err != nil {
				t.Fatalf("parseCombo(%q): %v", tc.in, err)
			}
			if got := c.String(); got != tc.want {
				t.Fatalf("parseCombo(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	// A bare "ctrl+" reads as the plus key, like "ctrl++"; only an empty
	// chord or an unknown modifier is a rejection.
	for _, bad := range []string{"", "  ", "hyper+x"} {
		if _, err := parseCombo(bad); err == nil {
			t.Fatalf("parseCombo(%q) must fail", bad)
		}
	}
}

// TestParseSize covers each backend's own size output, including the
// shapes that must be refused rather than clamped against zero.
func TestParseSize(t *testing.T) {
	ok := []struct {
		b    backend
		out  string
		w, h int
	}{
		{darwinBackend{}, "1512x982\n", 1512, 982},
		{windowsBackend{}, "3840x2160", 3840, 2160},
		{linuxBackend{}, "1512 982\n", 1512, 982},
	}
	for _, tc := range ok {
		s, err := tc.b.ParseSize([]byte(tc.out))
		if err != nil || s.w != tc.w || s.h != tc.h {
			t.Fatalf("%s.ParseSize(%q) = %+v, %v", tc.b.Name(), tc.out, s, err)
		}
	}
	for _, tc := range []struct {
		b   backend
		out string
	}{{darwinBackend{}, ""}, {darwinBackend{}, "oops"}, {windowsBackend{}, "0x0"}, {linuxBackend{}, "1512"}, {linuxBackend{}, "a b"}} {
		if _, err := tc.b.ParseSize([]byte(tc.out)); err == nil {
			t.Fatalf("%s.ParseSize(%q) must fail", tc.b.Name(), tc.out)
		}
	}
}

// TestPSSendKeysEscapes covers the SendKeys metacharacters and newlines.
func TestPSSendKeysEscapes(t *testing.T) {
	got := psSendKeys("a+b^(c)\n{}")
	want := "a{+}b{^}{(}c{)}{ENTER}{{}{}}"
	if got != want {
		t.Fatalf("psSendKeys = %q, want %q", got, want)
	}
}

// TestAnnotateError names the Screen Recording grant for the one
// screencapture failure that has an actionable fix.
func TestAnnotateError(t *testing.T) {
	if got := annotateError("screencapture", "could not create image from display"); !strings.Contains(got, "Screen Recording") {
		t.Fatalf("annotateError = %q", got)
	}
	if got := annotateError("screencapture", "something else"); got != "" {
		t.Fatalf("annotateError leaked a hint: %q", got)
	}
	if got := annotateError("osascript", "could not create image from display"); got != "" {
		t.Fatalf("hint must be screencapture-specific: %q", got)
	}
}

// TestMissingToolErrorIsActionable: a missing platform binary names what to
// install instead of a bare exec error.
func TestMissingToolErrorIsActionable(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // nothing is findable
	displayEnv(t)                 // the X11 gate must be open so the missing binary is what fails

	tl := newToolFor(enabledCfg(t.TempDir()), linuxBackend{}, nil)
	res := call(t, tl, `{"op":"click","x":1,"y":1}`)
	if !res.IsError || !strings.Contains(res.Text, "xdotool not found in PATH") || !strings.Contains(res.Text, "install xdotool") {
		t.Fatalf("missing xdotool: %+v", res)
	}
	res = call(t, tl, `{"op":"screenshot"}`)
	if !res.IsError || !strings.Contains(res.Text, "import not found in PATH") || !strings.Contains(res.Text, "ImageMagick") {
		t.Fatalf("missing import: %+v", res)
	}
	res = call(t, newToolFor(enabledCfg(t.TempDir()), darwinBackend{}, nil), `{"op":"move","x":1,"y":1}`)
	if !res.IsError || !strings.Contains(res.Text, "osascript not found in PATH") {
		t.Fatalf("missing osascript: %+v", res)
	}
}

// TestTimeoutBoundsOneOp: the op deadline (computer.timeout) reaches the
// command runner.
func TestTimeoutBoundsOneOp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no portable slow command: POSIX sleep is what this test needs")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := runCmd(ctx, []string{"sleep", "5"})
	if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "computer.timeout") {
		t.Fatalf("runCmd deadline error = %v", err)
	}

	// Through the tool: a slow platform tool is cut off at Config.Timeout.
	dir := t.TempDir()
	writeStub(t, dir, "osascript", "#!/bin/sh\nsleep 5\n")
	t.Setenv("PATH", dir)
	tl := newToolFor(Config{Enabled: true, Dir: t.TempDir(), Timeout: 100 * time.Millisecond}, darwinBackend{}, nil)
	start := time.Now()
	res := call(t, tl, `{"op":"click","x":1,"y":1}`)
	if !res.IsError || !strings.Contains(res.Text, "timed out") {
		t.Fatalf("slow op: %+v", res)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timeout did not bound the op: %s", elapsed)
	}
}

// writeStub installs an executable stub for name in dir.
func writeStub(t *testing.T, dir, name, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("PATH stubs are POSIX shell scripts")
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestExecuteStubsOnPath drives the real exec path (LookPath + argv) with
// stub platform binaries on PATH: a macOS click and a Linux click, each
// asserting the command the backend actually ran.
func TestExecuteStubsOnPath(t *testing.T) {
	log := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("STUB_LOG", log)

	t.Run("macos", func(t *testing.T) {
		dir := t.TempDir()
		writeStub(t, dir, "osascript", "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$STUB_LOG\"\ncase \"$*\" in\n  *NSScreen*) echo 100x80;;\n  *\"UI elements enabled\"*) echo true;;\nesac\n")
		t.Setenv("PATH", dir)
		t.Run("preflight-is-wired", func(t *testing.T) {
			os.Remove(log)
			tl := newToolFor(enabledCfg(t.TempDir()), darwinBackend{}, nil)
			res := call(t, tl, `{"op":"click","x":5000,"y":-5}`)
			if res.IsError {
				t.Fatalf("click failed: %s", res.Text)
			}
			got := readLog(t, log)
			if !strings.Contains(got, "UI elements enabled") {
				t.Fatalf("accessibility preflight did not run: %q", got)
			}
			if !strings.Contains(got, "p={x:99,y:0}") {
				t.Fatalf("click was not clamped in the executed script: %q", got)
			}
			if !strings.Contains(res.Text, "(clamped from 5000,-5)") {
				t.Fatalf("clamp not reported: %q", res.Text)
			}
		})
		t.Run("accessibility-denied", func(t *testing.T) {
			writeStub(t, dir, "osascript", "#!/bin/sh\ncase \"$*\" in\n  *NSScreen*) echo 100x80;;\n  *) echo false;;\nesac\n")
			tl := newToolFor(enabledCfg(t.TempDir()), darwinBackend{}, nil)
			res := call(t, tl, `{"op":"move","x":1,"y":1}`)
			if !res.IsError || !strings.Contains(res.Text, "Accessibility") {
				t.Fatalf("denied grant must be actionable: %+v", res)
			}
		})
	})

	t.Run("linux", func(t *testing.T) {
		dir := t.TempDir()
		writeStub(t, dir, "xdotool", "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$STUB_LOG\"\ncase \"$*\" in\n  *getdisplaygeometry*) echo \"1512 982\";;\nesac\n")
		t.Setenv("PATH", dir)
		t.Setenv("DISPLAY", ":0")
		t.Setenv("WAYLAND_DISPLAY", "")
		os.Remove(log)
		tl := newToolFor(enabledCfg(t.TempDir()), linuxBackend{}, nil)
		res := call(t, tl, `{"op":"click","x":5000,"y":-5}`)
		if res.IsError {
			t.Fatalf("click failed: %s", res.Text)
		}
		got := readLog(t, log)
		if !strings.Contains(got, "mousemove 1511 0 click 1") {
			t.Fatalf("linux click argv not executed as mapped: %q", got)
		}
	})
}

// TestLinuxWithoutDisplay: the X11 preflight refuses before xdotool is
// asked to do anything.
func TestLinuxWithoutDisplay(t *testing.T) {
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	rec := &recorder{reply: replyFor(linuxBackend{}, "1512 982")}
	tl := newToolFor(enabledCfg(t.TempDir()), linuxBackend{}, rec)
	res := call(t, tl, `{"op":"move","x":1,"y":1}`)
	if !res.IsError || !strings.Contains(res.Text, "no display") {
		t.Fatalf("headless input must be refused: %+v", res)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("commands ran without a display: %v", rec.calls)
	}
}

// readLog returns the stub log's contents.
func readLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("stub log: %v", err)
	}
	return string(b)
}
