package browser

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/FreePeak/xdev/internal/session"
)

func writeProfile(t *testing.T, dir string, port int, userEnabled *bool, includePort bool) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if includePort {
		content := fmt.Sprintf("%d\n/devtools/browser/test-guid\n", port)
		if err := os.WriteFile(filepath.Join(dir, devToolsPortFile), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if userEnabled != nil {
		doc := map[string]any{
			"devtools": map[string]any{
				"remote_debugging": map[string]any{
					"user-enabled": *userEnabled,
				},
			},
		}
		b, _ := json.Marshal(doc)
		if err := os.WriteFile(filepath.Join(dir, localStateFile), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadDevToolsActivePort(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := ReadDevToolsActivePort(dir); err == nil {
		t.Fatal("missing file should error")
	}
	writeProfile(t, dir, 9222, nil, true)
	port, path, err := ReadDevToolsActivePort(dir)
	if err != nil {
		t.Fatal(err)
	}
	if port != 9222 || path != "/devtools/browser/test-guid" {
		t.Fatalf("got %d %q", port, path)
	}
	// malformed port
	bad := t.TempDir()
	os.WriteFile(filepath.Join(bad, devToolsPortFile), []byte("nope\n/devtools/browser/x\n"), 0o600)
	if _, _, err := ReadDevToolsActivePort(bad); err == nil {
		t.Error("bad port should error")
	}
	// truncated file
	trunc := t.TempDir()
	os.WriteFile(filepath.Join(trunc, devToolsPortFile), []byte("9222\n"), 0o600)
	if _, _, err := ReadDevToolsActivePort(trunc); err == nil {
		t.Error("truncated file should error")
	}
}

func TestUserRemoteDebugging(t *testing.T) {
	dir := t.TempDir()
	if userRemoteDebugging(dir) != nil {
		t.Error("missing Local State should be nil")
	}
	enabled := true
	writeProfile(t, dir, 9222, &enabled, false)
	if v := userRemoteDebugging(dir); v == nil || !*v {
		t.Error("expected enabled true")
	}
	disabled := false
	dir2 := t.TempDir()
	writeProfile(t, dir2, 9222, &disabled, false)
	if v := userRemoteDebugging(dir2); v == nil || *v {
		t.Error("expected enabled false")
	}
}

func TestAutoConnectEndpointPrefersReachable(t *testing.T) {
	f := newFakeCDP(t, func(method string, _ json.RawMessage) (json.RawMessage, error) {
		return nil, nil
	})
	dir := t.TempDir()
	t.Setenv(envProfileDir, dir)
	writeProfile(t, dir, fPort(f), nil, true)
	if ep, ok := autoConnectEndpoint(""); ok {
		if ep != f.endpoint() {
			t.Fatalf("endpoint = %q, want %q", ep, f.endpoint())
		}
	} else {
		t.Fatal("expected reachable auto-connect endpoint")
	}
}

func fPort(f *fakeCDP) int {
	// derive port from the httptest server URL
	host := f.endpoint()
	// strip scheme
	i := 0
	if idx := indexOf(host, "://"); idx >= 0 {
		i = idx + 3
	}
	host = host[i:]
	// host:port
	j := indexOf(host, ":")
	if j < 0 {
		return 80
	}
	portStr := host[j+1:]
	// trim trailing slash/path
	if k := indexOf(portStr, "/"); k >= 0 {
		portStr = portStr[:k]
	}
	n := 0
	for _, c := range portStr {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestAutoConnectStalePortIsRejected(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envProfileDir, dir)
	// port that nothing listens on
	writeProfile(t, dir, 1, nil, true)
	if _, ok := autoConnectEndpoint(""); ok {
		t.Fatal("stale port must not be reported reachable")
	}
	info := ProbeAutoConnect("")
	if !info.Stale() {
		t.Fatalf("expected stale, got %+v", info)
	}
}

func TestProbeAutoConnectUserEnabled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envProfileDir, dir)
	enabled := false
	writeProfile(t, dir, 1, &enabled, false)
	info := ProbeAutoConnect("")
	if info.Enabled() {
		t.Error("expected not enabled")
	}
	if info.ProfileFound != true {
		t.Error("profile should be found")
	}
}

func TestResolveEndpointAutoConnect(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envProfileDir, dir)
	f := newFakeCDP(t, nil)
	writeProfile(t, dir, fPort(f), nil, true)
	defer t.Setenv(envProfileDir, "")
	// auto on + no explicit endpoint → auto endpoint
	if got := resolveEndpoint("", true, ""); got != f.endpoint() {
		t.Fatalf("auto endpoint = %q, want %q", got, f.endpoint())
	}
	// explicit config wins
	if got := resolveEndpoint("http://cfg:9222", true, ""); got != "http://cfg:9222" {
		t.Fatalf("config should win: %q", got)
	}
	// env wins
	t.Setenv(envEndpoint, "http://env:9222")
	if got := resolveEndpoint("", true, ""); got != "http://env:9222" {
		t.Fatalf("env should win: %q", got)
	}
	t.Setenv(envEndpoint, "")
	// auto off → default path
	if got := resolveEndpoint("", false, ""); got == f.endpoint() {
		t.Error("auto off should not use the profile endpoint")
	}
}

func TestToolAutoConnectAttaches(t *testing.T) {
	f := newFakeCDP(t, func(method string, _ json.RawMessage) (json.RawMessage, error) {
		if method == "Runtime.evaluate" {
			return json.RawMessage(`{"result":{"type":"object","value":{"url":"http://x","title":"T","readyState":"complete"}}}`), nil
		}
		return nil, nil
	})
	dir := t.TempDir()
	t.Setenv(envProfileDir, dir)
	enabled := true
	writeProfile(t, dir, fPort(f), &enabled, true)
	blobs := session.NewBlobStore(t.TempDir())
	on := true
	tl := NewTool(Settings{AutoConnect: &on}, blobs)
	t.Cleanup(func() { _ = tl.Close() })
	res := exec(t, tl, map[string]any{"op": "open"})
	if res.IsError {
		t.Fatalf("auto-connect open failed: %s", res.Text)
	}
}

func TestToolAutoConnectUnreachableNamesInspect(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envProfileDir, dir)
	writeProfile(t, dir, 1, nil, true) // stale port
	blobs := session.NewBlobStore(t.TempDir())
	on := true
	tl := NewTool(Settings{AutoConnect: &on}, blobs)
	t.Cleanup(func() { _ = tl.Close() })
	res := exec(t, tl, map[string]any{"op": "open"})
	if !res.IsError {
		t.Fatal("expected error")
	}
	if !contains(res.Text, InspectURL) {
		t.Fatalf("error should name %s: %s", InspectURL, res.Text)
	}
	if contains(res.Text, "--remote-debugging-port=9222") {
		// ensure we don't give the wrong remedy for auto mode
	}
}

func contains(s, sub string) bool {
	return len(s) > 0 && len(sub) > 0 && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
