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

// darwinClipboardImage writes the pasteboard's PNG flavour to a temp file and
// reads it back.
//
// «class PNGf» is what screenshot.app puts on the pasteboard, and the file route
// is what works: `the clipboard as PNG` instead exports a Finder icon
// representation for a copied FILE — a picture of an icon, never what the user
// meant to attach. osascript errors when the clipboard holds text, which is the
// ordinary case, so the error path is the common one and stays quiet.
func darwinClipboardImage() ([]byte, error) {
	const script = `set f to (POSIX path of (path to temporary folder)) & "xdev-clipboard.png"
set d to (the clipboard as «class PNGf»)
set fh to open for access file f with write permission
set eof of fh to 0
write d to fh
close access fh
return f`
	out, err := exec.Command("osascript", "-e", script).Output()
	if err != nil {
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
