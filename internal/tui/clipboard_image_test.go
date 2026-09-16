package tui

import (
	"bytes"
	"encoding/hex"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// darwinScriptTool returns the path to an AppleScript tool, or skips. These
// tests run the real dialect: the bug they pin was a phrase AppleScript stopped
// accepting, which no amount of Go-side mocking can see.
func darwinScriptTool(t *testing.T, tool string) string {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	path, err := exec.LookPath(tool)
	if err != nil {
		t.Skipf("%s not on PATH: not a macOS desktop session", tool)
	}
	return path
}

// The reported bug: `path to temporary folder` is a syntax error on current
// macOS, so the pasteboard script never compiled, and Ctrl+V answered "no image
// on the clipboard" while a screenshot sat on the clipboard. osacompile rejects
// it without needing a clipboard, which is what makes this the cheap check.
func TestDarwinClipboardScriptCompiles(t *testing.T) {
	osacompile := darwinScriptTool(t, "osacompile")
	out, err := exec.Command(osacompile, "-e", darwinClipboardScript,
		"-o", filepath.Join(t.TempDir(), "clipboard.scpt")).CombinedOutput()
	if err != nil {
		t.Fatalf("the clipboard script does not compile:\n%s", out)
	}
}

// The second half of the same bug: `open for access file f` fails -61 (not open
// with write permission) at the first write, because the bare form coerces the
// POSIX *string*. Compilation cannot catch that, so this runs the script for
// real — with only its clipboard read swapped for a data literal, leaving the
// four file verbs under test byte-for-byte as shipped.
func TestDarwinClipboardScriptWritesImage(t *testing.T) {
	osascript := darwinScriptTool(t, "osascript")

	want := tinyPNG(t)
	const anchor = "(the clipboard as «class PNGf»)"
	if strings.Count(darwinClipboardScript, anchor) != 1 {
		t.Fatalf("clipboard read %q is not exactly once in the script: the test's "+
			"substitution has no home, so fix this test before changing the script", anchor)
	}
	script := strings.Replace(darwinClipboardScript, anchor,
		"«data PNGf"+strings.ToUpper(hex.EncodeToString(want))+"»", 1)

	out, err := exec.Command(osascript, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("the clipboard script failed to write an image: %v\n%s", err, out)
	}
	path := strings.TrimSpace(string(out))
	defer func() { _ = os.Remove(path) }()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read what the script wrote: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("script wrote %d bytes, want the %d bytes it was handed", len(got), len(want))
	}
}

// tinyPNG is a real PNG, encoded by the same stdlib reader the app uses.
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 3, 2))
	img.Set(1, 1, color.RGBA{R: 0xff, A: 0xff})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

// A broken script must not be reported as an empty clipboard: the failures
// arrive from osascript as numbers, and only the pasteboard's own "no image"
// numbers (measured against a text, an empty, an alias, an RTF and a
// data-less image pasteboard) may mean it.
func TestIsNoImageStderr(t *testing.T) {
	cases := []struct {
		stderr string
		want   bool
	}{
		// Measured: text, an empty pasteboard and a Finder file copy all say
		// this one; a lazily-declared image flavour with no data says -25133.
		{"execution error: Can't make some data into the expected type. (-1700)", true},
		{"execution error: An error of type -25133 has occurred. (-25133)", true},
		{"execution error: No image data. (-25130)", true},
		// A failure with no message at all is not diagnosed as an empty
		// clipboard: osascript always names its reason, so silence means the
		// assumption behind this script needs looking at.
		{"", false},
		{"execution error: File not open with write permission. (-61)", false},
		{"43:49: syntax error: Expected \",\" but found property. (-2741)", false},
		{"compilation error: Expected \",\" but found property. (-2741)", false},
	}
	for _, c := range cases {
		if got := isNoImageStderr(c.stderr); got != c.want {
			t.Errorf("isNoImageStderr(%q) = %v, want %v", c.stderr, got, c.want)
		}
	}
}
