package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestEndpointPort asserts which endpoints may be auto-launched on: a
// loopback host with a usable port, in each spelling the config accepts.
func TestEndpointPort(t *testing.T) {
	cases := []struct {
		endpoint string
		want     int
		ok       bool
	}{
		{"http://127.0.0.1:9222", 9222, true},
		{"127.0.0.1:9222", 9222, true},
		{"http://localhost:9222/json/list", 9222, true},
		{"ws://127.0.0.1:9333", 9333, true},
		{"http://[::1]:9222", 9222, true},
		{"http://0.0.0.0:9222", 9222, true},
		{"http://10.0.0.5:9222", 0, false},
		{"http://example.com:9222", 0, false},
		{"http://127.0.0.1", 0, false},
		{"http://127.0.0.1:0", 0, false},
		{"http://127.0.0.1:70000", 0, false},
		{"http://127.0.0.1:http", 0, false},
	}
	for _, tc := range cases {
		got, ok := endpointPort(tc.endpoint)
		if ok != tc.ok || got != tc.want {
			t.Errorf("endpointPort(%q) = %d,%v want %d,%v", tc.endpoint, got, ok, tc.want, tc.ok)
		}
	}
}

// TestAutolaunchOn asserts the default: only an explicit autolaunch: false
// turns the launch off.
func TestAutolaunchOn(t *testing.T) {
	if !(Settings{}).AutolaunchOn() {
		t.Error("zero-value Settings must autolaunch")
	}
	off := false
	if (Settings{Autolaunch: &off}).AutolaunchOn() {
		t.Error("autolaunch: false must be attach-only")
	}
	on := true
	if !(Settings{Autolaunch: &on}).AutolaunchOn() {
		t.Error("autolaunch: true must autolaunch")
	}
	// attach-only also needs a profile dir to launch with
	tl := &Tool{Cfg: Settings{}}
	if tl.autolaunch() {
		t.Error("no profile dir means attach-only")
	}
	tl.ProfileDir = t.TempDir()
	if !tl.autolaunch() {
		t.Error("default settings with a profile dir must autolaunch")
	}
	tl.Cfg.Autolaunch = &off
	if tl.autolaunch() {
		t.Error("autolaunch: false must win over the profile dir")
	}
}

// TestIdleTimeoutOn asserts the idle exit default and that 0 (or less) turns
// it off rather than closing an instant after launch.
func TestIdleTimeoutOn(t *testing.T) {
	if got := (Settings{}).IdleTimeoutOn(); got != 5*time.Minute {
		t.Errorf("default idle timeout = %s, want 5m", got)
	}
	twenty := 20
	if got := (Settings{IdleExit: &twenty}).IdleTimeoutOn(); got != 20*time.Second {
		t.Errorf("idleExit: 20 gives %s, want 20s", got)
	}
	for _, secs := range []int{0, -1} {
		s := secs
		if got := (Settings{IdleExit: &s}).IdleTimeoutOn(); got != 0 {
			t.Errorf("idleExit: %d gives %s, want the idle exit off", secs, got)
		}
	}
}

// TestPendingIdleNotice asserts the one channel the tool has to explain an
// out-of-band event: the idle exit's notice rides on the next Result and is
// then cleared, so it is reported once and not repeated on every op.
func TestPendingIdleNotice(t *testing.T) {
	tl := &Tool{Cfg: Settings{CDPURL: "http://127.0.0.1:9"}, tabs: map[string]*tab{}}
	tl.stopped = "xdev's browser on http://127.0.0.1:9 was closed after 5m0s idle"
	if got := tl.pendingIdleNotice(); !strings.Contains(got, "was closed after") {
		t.Fatalf("notice = %q, want the idle-exit explanation", got)
	}
	if again := tl.pendingIdleNotice(); again != "" {
		t.Fatalf("notice repeated: %q", again)
	}
	// A live endpoint that the tool did not launch (nothing in bot) means
	// the notice describes a browser that is not the one answering.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	tl.stopped = "stale"
	tl.Cfg.CDPURL = srv.URL
	if got := tl.pendingIdleNotice(); got != "" {
		t.Fatalf("notice on a live endpoint = %q, want none", got)
	}
}

// TestUnreachableErrNamesTheRightFix asserts the two remedies do not get
// mixed up: autolaunch on points at the binary, attach-only points at
// starting Chrome by hand.
func TestUnreachableErrNamesTheRightFix(t *testing.T) {
	err := unreachableErr("http://127.0.0.1:9222", context.DeadlineExceeded, true)
	for _, want := range []string{"could not auto-launch", envBinary} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("autolaunch error lacks %q: %s", want, err)
		}
	}
	err = unreachableErr("http://127.0.0.1:9222", context.DeadlineExceeded, false)
	for _, want := range []string{"start Chrome/Chromium with", "--remote-debugging-port=9222", "browser.cdpUrl"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("attach-only error lacks %q: %s", want, err)
		}
	}
}

// TestLaunchBrowserRefusesRemoteEndpoint asserts a non-loopback endpoint is
// never launched on locally.
func TestLaunchBrowserRefusesRemoteEndpoint(t *testing.T) {
	_, err := launchBrowser("http://10.0.0.5:9222", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("err = %v, want a loopback refusal", err)
	}
}

// TestFindChromeEnvOverride asserts $XDEV_BROWSER_BIN is honored, including
// the failure when it names nothing executable — the one path that needs no
// installed browser.
func TestFindChromeEnvOverride(t *testing.T) {
	t.Setenv(envBinary, "/nope/definitely-not-here")
	if _, err := findChrome(); err == nil || !strings.Contains(err.Error(), envBinary) {
		t.Fatalf("err = %v, want an %s complaint", err, envBinary)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-chrome")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envBinary, bin)
	got, err := findChrome()
	if err != nil || got != bin {
		t.Fatalf("findChrome() = %q,%v want %q", got, err, bin)
	}
}

// TestEnsureLaunchedFailsFast asserts a browser that never opens its port is
// killed and reported, and that the wait follows the call's own deadline
// (MinTimeout = 1s) instead of the full launch budget.
func TestEnsureLaunchedFailsFast(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake browser is a shell script")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-chrome")
	// Ignore every flag and just sleep: the port never opens.
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envBinary, bin)
	// A port nothing is listening on; the endpoint just has to be loopback.
	endpoint := "http://127.0.0.1:1"

	ctx, cancel := context.WithTimeout(context.Background(), MinTimeout)
	defer cancel()
	start := time.Now()
	bot, err := ensureLaunched(ctx, endpoint, filepath.Join(dir, "profile"))
	if bot != nil {
		t.Fatal("a failed launch must not report a browser")
	}
	if err == nil {
		t.Fatal("ensureLaunched must fail when the port never opens")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("ensureLaunched waited %s, want it bounded by the 1s context", elapsed)
	}
}
