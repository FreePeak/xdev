package mcpclient

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
		URL:      "http://localhost",
		Command:  "leankg",
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
