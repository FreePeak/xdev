package browser

// Real-Chrome end-to-end, opt-in: with XDEV_BROWSER_E2E=1 it drives the tool
// against an endpoint nothing is listening on, so the auto-launch path runs
// against a real browser (skipped otherwise, because CI has no Chrome).
//
//	XDEV_BROWSER_E2E=1 go test ./internal/browser/ -run TestRealChrome -v
//
import (
	"context"
	"encoding/json"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/tool"
)

// e2eEndpoint is a port chosen for this test alone, so a stray Chrome from a
// previous run cannot be mistaken for a live endpoint someone else owns.
const e2eEndpoint = "http://127.0.0.1:9231"

func TestRealChromeAutolaunch(t *testing.T) {
	if os.Getenv("XDEV_BROWSER_E2E") != "1" {
		t.Skip("set XDEV_BROWSER_E2E=1 to launch a real browser")
	}
	installProbe(t, e2eEndpoint)
	t.Setenv(envEndpoint, e2eEndpoint)
	// Keep the launched browser's profile out of t.TempDir: the test cannot
	// remove a directory a live Chrome is still writing to.
	profile := filepath.Join(os.TempDir(), "xdev-browser-e2e-profile")
	tl := NewTool(Settings{}, session.NewBlobStore(t.TempDir()))
	tl.ProfileDir = profile
	defer tl.Close()

	call := func(in map[string]any) tool.Result {
		raw, _ := json.Marshal(in)
		res, err := tl.Execute(context.Background(), raw)
		if err != nil {
			t.Fatalf("execute %v: %v", in, err)
		}
		return res
	}

	res := call(map[string]any{"op": "open", "url": "data:text/html,<title>e2e</title><h1>hello</h1>"})
	if res.IsError {
		t.Fatalf("auto-launch open failed: %s", res.Text)
	}
	t.Logf("open: %s", res.Text)

	snap := call(map[string]any{"op": "snapshot"})
	if snap.IsError || !strings.Contains(snap.Text, "hello") {
		t.Fatalf("snapshot = %+v, want the page text", snap)
	}
	t.Logf("snapshot: %s", snap.Text)

	ev := call(map[string]any{"op": "eval", "expression": "document.title"})
	if ev.IsError || !strings.Contains(ev.Text, "e2e") {
		t.Fatalf("eval = %+v, want the title", ev)
	}
	t.Logf("eval: %s", ev.Text)

	if closeRes := call(map[string]any{"op": "close", "all": true}); closeRes.IsError {
		t.Fatalf("close = %+v", closeRes)
	}
}

// TestRealChromeAttachOnly pins the regression the other half of the fix is
// about: with autolaunch off, the same dead endpoint reports the start-Chrome
// error and no browser is started.
//
// The browser the test above launched is stopped first, so "nothing is
// listening" is a real precondition rather than a hope.
func TestRealChromeAttachOnly(t *testing.T) {
	if os.Getenv("XDEV_BROWSER_E2E") != "1" {
		t.Skip("set XDEV_BROWSER_E2E=1 to launch a real browser")
	}
	if installProbe(t, e2eEndpoint) {
		stopE2EBrowser(t)
	}
	t.Setenv(envEndpoint, e2eEndpoint)
	off := false
	tl := NewTool(Settings{Autolaunch: &off}, session.NewBlobStore(t.TempDir()))
	tl.ProfileDir = filepath.Join(t.TempDir(), "never-used")
	defer tl.Close()

	raw, _ := json.Marshal(map[string]any{"op": "open"})
	res, err := tl.Execute(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "start Chrome/Chromium with") {
		t.Fatalf("attach-only open = %+v, want the start-Chrome error", res)
	}
	t.Logf("attach-only: %s", res.Text)
}

// installProbe reports whether anything answers on the endpoint, making the
// connect call through the same path the tool uses.
func installProbe(t *testing.T, endpoint string) bool {
	t.Helper()
	if _, err := ListTargets(context.Background(), endpoint, false); err == nil {
		return true
	}
	return false
}

// stopE2EBrowser kills the browser a previous run of this test launched. It
// matches on the test's own profile dir, so no other Chrome is touched.
func stopE2EBrowser(t *testing.T) {
	t.Helper()
	profile := filepath.Join(os.TempDir(), "xdev-browser-e2e-profile")
	out, err := osexec.Command("pkill", "-f", "user-data-dir="+profile).CombinedOutput()
	if err != nil {
		t.Logf("pkill: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	for i := 0; i < 50; i++ {
		if _, err := ListTargets(context.Background(), e2eEndpoint, false); err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("a browser still answers on %s", e2eEndpoint)
}
