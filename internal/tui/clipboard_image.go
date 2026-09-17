package tui

// Reading an image off the system clipboard.
//
// This has to shell out: a terminal hands the app only what its own paste key
// produces as bytes, and on every platform we target that is text — or nothing
// at all — when the clipboard holds a bitmap. Cmd+V belongs to the terminal, so
// xdev never sees it. Hence a chord of its own (Ctrl+V, omp's
// app.clipboard.pasteImage) and a native read, mirroring what nativeCopy does on
// the write side.
//
// A clipboard with no image is an error with a message worth showing: a bound
// chord that does nothing reads as a broken build.

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// noImage is the answer for a clipboard holding something else. It names the
// alternative because this is the chord everyone reaches for first.
const noImage = "no image on the clipboard (Cmd+V is the terminal's paste): " +
	"screenshot to a file and paste its path, or drag the file into the terminal"

// clipboardImage returns the clipboard bitmap and its media type.
func clipboardImage() ([]byte, string, error) {
	var (
		data []byte
		err  error
	)
	switch runtime.GOOS {
	case "darwin":
		data, err = darwinClipboardImage()
	case "windows":
		data, err = windowsClipboardImage()
	default:
		data, err = linuxClipboardImage()
	}
	if err != nil {
		return nil, "", err
	}
	mime, ok := imageMediaType(data)
	if !ok {
		return nil, "", fmt.Errorf("the clipboard holds an image format xdev cannot send")
	}
	if len(data) > maxPasteImageBytes {
		return nil, "", fmt.Errorf("clipboard image is %.1f MB — over the %d MB paste limit",
			float64(len(data))/(1<<20), maxPasteImageBytes>>20)
	}
	return data, mime, nil
}

// darwinClipboardScript writes the pasteboard's PNG flavour to a temp file and
// hands back its path.
//
// «class PNGf» is what screenshot.app puts on the pasteboard, and the file route
// is what works: `the clipboard as PNG` instead exports a Finder icon
// representation for a copied FILE — a picture of an icon, never what the user
// meant to attach. A clipboard holding no image at all (text is the ordinary
// case) errors instead, and osascript also answers «class PNGf» for a JPEG or
// TIFF-only pasteboard by converting, so the error path stays the rare one.
//
// The exact spelling of two phrases is load-bearing, each pinned by the error
// osascript gives when it is wrong — both were wrong once, and both failures
// surfaced as the same "no image on the clipboard":
//
//   - `path to temporary items folder`, not `path to temporary folder`: the
//     latter is a syntax error (-2741) on current macOS, so the script never
//     even compiled.
//   - `open for access (POSIX file f)`, not `open for access file f`: the bare
//     form coerces the POSIX *string* and fails -61 (not open with write
//     permission) at the first write.
//
// TestDarwinClipboardScriptCompiles catches the first without a clipboard;
// TestDarwinClipboardScriptWritesImage exercises the four file verbs for real.
const darwinClipboardScript = `set f to (POSIX path of (path to temporary items folder)) & "xdev-clipboard.png"
set d to (the clipboard as «class PNGf»)
set fh to open for access (POSIX file f) with write permission
set eof of fh to 0
write d to fh
close access fh
return f`

// darwinClipboardImage runs the script and reads back what it wrote.
func darwinClipboardImage() ([]byte, error) {
	cmd := exec.Command("osascript", "-e", darwinClipboardScript)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// The script cannot write the file when the pasteboard holds no image,
		// which is the case almost every time this chord is pressed — so that
		// failure is expected, not a bug, and keeps its plain wording. Anything
		// else (a dialect change in a future macOS) is a real defect and must
		// not hide behind the same reassuring sentence.
		if !isNoImageStderr(stderr.String()) {
			return nil, fmt.Errorf("read clipboard image: %w: %s",
				err, strings.TrimSpace(stderr.String()))
		}
		return nil, fmt.Errorf("%s", noImage)
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return nil, fmt.Errorf("%s", noImage)
	}
	defer func() { _ = os.Remove(path) }()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read clipboard image: %w", err)
	}
	return data, nil
}

// isNoImageStderr reports whether osascript failed because the clipboard holds
// no image rather than because the script is broken. A script error must never
// take this path: it is the one failure the user cannot tell from an empty
// clipboard, which is exactly how two dead versions of this script shipped.
//
// The codes, measured by running the script against each clipboard in turn:
//
//   - -1700 "Can't make some data into the expected type" for every clipboard
//     that simply has no image: plain text, an empty pasteboard, a Finder copy
//     of a file (an alias), RTF.
//   - -25133, the pasteboard family's "flavour declared, no data behind it",
//     for a pasteboard that advertises an image type lazily and never fills it
//     — rarer, same meaning, same quiet handling; -25130 is its sibling.
//
// Anything else is surfaced as itself, in osascript's own words.
func isNoImageStderr(s string) bool {
	if strings.Contains(s, "syntax error") || strings.Contains(s, "compilation error") {
		return false
	}
	for _, code := range []string{"-1700", "-25130", "-25133"} {
		if strings.Contains(s, code) {
			return true
		}
	}
	return false
}

// windowsClipboardImage exports the clipboard bitmap through WinForms, which
// answers for whatever form the clipboard holds and encodes it as PNG.
func windowsClipboardImage() ([]byte, error) {
	const script = `Add-Type -AssemblyName System.Windows.Forms
$img = [System.Windows.Forms.Clipboard]::GetImage()
if ($img) {
  $ms = New-Object System.IO.MemoryStream
  $img.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png)
  [Convert]::ToBase64String($ms.ToArray())
}`
	out, err := exec.Command("powershell", "-NoProfile", "-Command", script).Output()
	txt := strings.TrimSpace(string(out))
	if err != nil || txt == "" {
		return nil, fmt.Errorf("%s", noImage)
	}
	data, err := base64.StdEncoding.DecodeString(txt)
	if err != nil {
		return nil, fmt.Errorf("decode clipboard image: %w", err)
	}
	return data, nil
}

// linuxClipboardImage tries the wayland and X11 tools in the order the desktop
// is likely to answer. The type filter matters: without it wl-paste returns the
// clipboard's plain-text entry, and a text selection would paste as a "photo".
func linuxClipboardImage() ([]byte, error) {
	for _, c := range [][]string{
		{"wl-paste", "-t", "image/png"},
		{"xclip", "-selection", "clipboard", "-t", "image/png", "-o"},
	} {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		data, err := exec.Command(c[0], c[1:]...).Output()
		if err != nil || len(data) == 0 {
			continue
		}
		if _, ok := imageMediaType(data); ok {
			return data, nil
		}
	}
	return nil, fmt.Errorf("%s (image paste on Linux needs wl-clipboard or xclip)", noImage)
}
