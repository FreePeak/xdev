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

// e2eLaunchBudget is how long this test waits for a real browser to come and
// go; a cold Chrome start is a second or two, an idle exit a couple more.
const e2eLaunchBudget = 20 * time.Second

// e2eWaitForPort blocks until the endpoint answers (up) or stops answering
// (down), and fails the test on timeout.
func e2eWaitForPort(t *testing.T, up bool, what string) {
	t.Helper()
	deadline := time.Now().Add(e2eLaunchBudget)
	for {
		_, err := ListTargets(context.Background(), e2eEndpoint, false)
		if (err == nil) == up {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the launched browser did not %s within %s", what, e2eLaunchBudget)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestRealChromeAutolaunch(t *testing.T) {
	if os.Getenv("XDEV_BROWSER_E2E") != "1" {
		t.Skip("set XDEV_BROWSER_E2E=1 to launch a real browser")
	}
	if installProbe(t, e2eEndpoint) {
		stopE2EBrowser(t)
	}
	t.Setenv(envEndpoint, e2eEndpoint)
	// Keep the launched browser's profile out of t.TempDir: the test cannot
	// remove a directory a live Chrome is still writing to.
	profile := filepath.Join(os.TempDir(), "xdev-browser-e2e-profile")
	// idleExit: 2 so the run also proves the browser xdev launched goes away
	// by itself, without waiting the default five minutes.
	idle := 2
	tl := NewTool(Settings{IdleExit: &idle}, session.NewBlobStore(t.TempDir()))
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

	// Nothing touches the browser from here: after idleExit the browser xdev
	// launched must be gone, and the next op must say so.
	e2eWaitForPort(t, false, "close itself after the idle timeout")
	// A fresh endpoint is live again, but a new tab has to be opened for it:
	// snapshot alone would be looking for the page the old browser had.
	reopened := call(map[string]any{"op": "open", "url": "data:text/html,<title>again</title><h1>hi</h1>"})
	if reopened.IsError {
		t.Fatalf("open after idle exit failed: %s", reopened.Text)
	}
	e2eWaitForPort(t, true, "come back for the next op")
	if !strings.Contains(reopened.Text, "was closed after") {
		t.Fatalf("open after idle exit = %q, want the idle-exit notice", reopened.Text)
	}
	t.Logf("after idle exit: %s", reopened.Text)
	// The notice is one-shot: the following op is clean.
	if again := call(map[string]any{"op": "eval", "expression": "document.title"}); strings.Contains(again.Text, "was closed after") {
		t.Fatalf("the idle-exit notice repeated: %q", again.Text)
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
