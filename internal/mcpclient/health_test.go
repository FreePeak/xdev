package mcpclient

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestProbeHTTPHappyPath: a health endpoint that returns 200
// makes IsHealthy report true.
func TestProbeHTTPHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sc := &ServerConfig{
		URL: srv.URL + "/mcp",
		AutoStart: &AutoStartConfig{
			HealthURL: srv.URL + "/health",
		},
	}
	healthy, err := sc.IsHealthy()
	if err != nil {
		t.Fatalf("IsHealthy: %v", err)
	}
	if !healthy {
		t.Fatal("expected healthy, got unhealthy")
	}
}

// TestProbeHTTPDown: a port with no server makes IsHealthy
// report false with no error.
func TestProbeHTTPDown(t *testing.T) {
	sc := &ServerConfig{
		URL: "http://localhost:19999/mcp",
		AutoStart: &AutoStartConfig{
			HealthURL: "http://localhost:19999/health",
		},
	}
	healthy, err := sc.IsHealthy()
	if err != nil {
		t.Fatalf("IsHealthy: unexpected err: %v", err)
	}
	if healthy {
		t.Fatal("expected unhealthy, got healthy")
	}
}

// TestHealthEndpointFromURL: when AutoStart.HealthURL is absent,
// HealthEndpoint derives from URL (path → /health, query dropped).
func TestHealthEndpointFromURL(t *testing.T) {
	sc := &ServerConfig{
		URL: "http://localhost:9699/mcp?project=/some/path",
	}
	got := sc.HealthEndpoint()
	want := "http://localhost:9699/health"
	if got != want {
		t.Fatalf("HealthEndpoint = %q, want %q", got, want)
	}
}

// TestHealthEndpointOverride: AutoStart.HealthURL takes
// precedence over the derived URL.
func TestHealthEndpointOverride(t *testing.T) {
	sc := &ServerConfig{
		URL: "http://localhost:9699/mcp",
		AutoStart: &AutoStartConfig{
			HealthURL: "http://localhost:9699/custom-health",
		},
	}
	got := sc.HealthEndpoint()
	want := "http://localhost:9699/custom-health"
	if got != want {
		t.Fatalf("HealthEndpoint = %q, want %q", got, want)
	}
}

// TestHealthEndpointNoURL: a stdio server (empty URL) yields an
// empty health endpoint; IsHealthy falls through to the stdio
// probe which checks the command path.
func TestHealthEndpointNoURL(t *testing.T) {
	sc := &ServerConfig{
		URL:     "http://localhost",
		Command: "leankg",
	}
	if got := sc.HealthEndpoint(); got != "http://localhost/health" {
		t.Fatalf("HealthEndpoint = %q, want empty", got)
	}
	// leankg is on PATH in CI / dev environments.
	healthy, err := sc.IsHealthy()
	if err != nil {
		t.Fatalf("IsHealthy: %v", err)
	}
	if !healthy {
		t.Log("leankg not on PATH — skipping healthy assertion")
	}
}

// TestIsHealthyNil: a nil ServerConfig is unhealthy, not a panic.
func TestIsHealthyNil(t *testing.T) {
	healthy, err := (*ServerConfig)(nil).IsHealthy()
	if err != nil {
		t.Fatalf("IsHealthy: %v", err)
	}
	if healthy {
		t.Fatal("expected unhealthy, got healthy")
	}
}

// TestStartAutoNoConfig: StartAuto without an AutoStart config
// fails closed rather than spawning anything.
func TestStartAutoNoConfig(t *testing.T) {
	sc := &ServerConfig{URL: "http://localhost:9699/mcp"}
	_, err := sc.StartAuto()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// TestStartAutoPassesEnv: autoStart.env reaches the launched process. The
// leankg recipe needs it (LEANKG_EMBED_SIDECAR_PORT) because the sidecar's
// default port 8080 is already taken by the gateway, and leankg exits
// during startup when the bind fails — which xdev then reports as
// "mcp: leankg unavailable".
func TestStartAutoPassesEnv(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "env.txt")
	sc := &ServerConfig{
		URL: srv.URL + "/mcp",
		AutoStart: &AutoStartConfig{
			Command:   "/bin/sh",
			Args:      []string{"-c", "printf %s \"$XDEV_AUTOSTART_TEST\" > " + out},
			Env:       map[string]string{"XDEV_AUTOSTART_TEST": "reached"},
			HealthURL: srv.URL,
		},
	}
	cmd, err := sc.StartAuto()
	if err != nil {
		t.Fatalf("StartAuto: %v", err)
	}
	wait := make(chan struct{})
	go func() { _ = cmd.Wait(); close(wait) }()
	select {
	case <-wait:
	case <-time.After(5 * time.Second):
		t.Fatal("auto-started process did not exit")
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read env dump: %v", err)
	}
	if string(got) != "reached" {
		t.Fatalf("autoStart.env did not reach the process: got %q", got)
	}
}

// TestLoadConfigAutoStartEnv: the autoStart block's env survives YAML
// parsing and secret resolution — this is the path the leankg recipe needs,
// and it was dropped on the floor before (the block had no Env field at all,
// so yaml silently discarded the key and StartAuto launched the server with
// whatever the parent environment happened to hold).
func TestLoadConfigAutoStartEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.yml")
	const body = `
servers:
    leankg:
        url: http://localhost:9699/mcp
        autoStart:
            command: /usr/bin/true
            args: ["serve"]
            cwd: /tmp
            healthUrl: http://localhost:9699/health
            healthTimeoutSec: 30
            env:
                LEANKG_EMBED_SIDECAR_PORT: "9101"
                LEANKG_EMBED_PROVIDER: local
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigIn(path, t.TempDir())
	if err != nil {
		t.Fatalf("LoadConfigIn: %v", err)
	}
	sc, ok := cfg.Servers["leankg"]
	if !ok {
		t.Fatal("leankg server missing")
	}
	if sc.AutoStart == nil {
		t.Fatal("autoStart missing")
	}
	if got := sc.AutoStart.Env["LEANKG_EMBED_SIDECAR_PORT"]; got != "9101" {
		t.Fatalf("autoStart.env[LEANKG_EMBED_SIDECAR_PORT] = %q, want 9101", got)
	}
	if got := sc.AutoStart.Env["LEANKG_EMBED_PROVIDER"]; got != "local" {
		t.Fatalf("autoStart.env[LEANKG_EMBED_PROVIDER] = %q, want local", got)
	}
	if sc.AutoStart.Cwd != "/tmp" {
		t.Fatalf("autoStart.cwd = %q, want /tmp", sc.AutoStart.Cwd)
	}
}
