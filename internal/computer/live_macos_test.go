package computer

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/tool"
)

// TestRealScreenshotMacOS runs the real macOS capture path: screencapture
// writing a PNG that the tool then reports with its true dimensions. It
// skips when there is no macOS, no screencapture, or no capture-capable
// session — a headless/CI mac or a terminal without the Screen Recording
// grant answers "could not create image from display", which is an
// environment fact, not a code failure.
func TestRealScreenshotMacOS(t *testing.T) {
	if _, err := exec.LookPath("screencapture"); err != nil {
		t.Skip("screencapture not on PATH: not a macOS desktop session")
	}
	dir := t.TempDir()
	tl := NewTool(Config{Enabled: true, Dir: dir})
	res, err := tl.Execute(context.Background(), json.RawMessage(`{"op":"screenshot"}`))
	if err != nil {
		t.Fatalf("harness error: %v", err)
	}
	if res.IsError {
		if strings.Contains(res.Text, "could not create image from display") || strings.Contains(res.Text, "timed out") {
			t.Skipf("no capturable display in this session: %s", res.Text)
		}
		t.Fatalf("screenshot failed: %s", res.Text)
	}
	details, ok := res.Details.(map[string]any)
	if !ok {
		t.Fatalf("details = %#v", res.Details)
	}
	path, _ := details["path"].(string)
	if path == "" || filepath.Dir(path) != dir {
		t.Fatalf("screenshot path = %q, want a file in %q", path, dir)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	if len(b) == 0 || string(b[1:4]) != "PNG" {
		t.Fatalf("capture is not a PNG: %d bytes", len(b))
	}
	if !strings.Contains(res.Text, "x") || !strings.Contains(res.Text, "MB)") {
		t.Fatalf("result text lacks dimensions/size: %q", res.Text)
	}
	if bytes, _ := details["bytes"].(int64); bytes != int64(len(b)) {
		t.Fatalf("details bytes = %v, file is %d bytes", details["bytes"], len(b))
	}
	t.Logf("real capture: %s", res.Text)
}

// TestRealWindowMacOS exercises the focused-window query when System Events
// is usable (it needs no Accessibility grant, only a GUI session).
func TestRealWindowMacOS(t *testing.T) {
	if _, err := exec.LookPath("osascript"); err != nil {
		t.Skip("osascript not on PATH: not macOS")
	}
	tl := NewTool(Config{Enabled: true, Dir: t.TempDir()})
	res, err := tl.Execute(context.Background(), json.RawMessage(`{"op":"window"}`))
	if err != nil {
		t.Fatalf("harness error: %v", err)
	}
	if res.IsError {
		if strings.Contains(res.Text, "timed out") || strings.Contains(res.Text, "failed") {
			t.Skipf("no GUI session for System Events: %s", res.Text)
		}
		t.Fatalf("window failed: %s", res.Text)
	}
	var _ tool.Result = res
	t.Logf("focused window: %q", res.Text)
}
