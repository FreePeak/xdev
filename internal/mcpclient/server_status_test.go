package mcpclient

import (
	"context"
	"strings"
	"testing"
)

// TestProbeRecordsWhyARealServerIsDown pins the production probe path the
// TUI's detail rows are rendered from: an unreachable server must carry the
// reason AND the time, not only the state word. The renderer test stubs
// these fields in, which is exactly why this test exists against the real
// ServerHealthProbe — with only State filled, `error:` and `since:` print
// nothing for the case users actually hit.
func TestProbeRecordsWhyARealServerIsDown(t *testing.T) {
	// Port 1 on loopback refuses instantly; no server, no auto-start.
	cfg := &Config{Servers: map[string]*ServerConfig{
		"dead-http": {URL: "http://localhost:1/mcp"},
	}}
	statuses, err := ServerHealthProbe(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ServerHealthProbe: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("statuses = %d, want 1", len(statuses))
	}
	st := statuses[0]
	if st.State != "unreachable" {
		t.Fatalf("State = %q, want unreachable", st.State)
	}
	if st.ErrorDetail == "" {
		t.Error("unreachable server has no ErrorDetail — the /mcps error row will be empty")
	}
	if !strings.Contains(st.ErrorDetail, "connection refused") && !strings.Contains(st.ErrorDetail, "localhost:1") {
		t.Errorf("ErrorDetail = %q, want the refused-connection reason or the endpoint", st.ErrorDetail)
	}
	if st.ErrorTime == "" {
		t.Error("unreachable server has no ErrorTime — /mcps will print no `since:`")
	}

	// A healthy server grows neither field.
	cfgHealthy := &Config{Servers: map[string]*ServerConfig{}}
	if _, err := ServerHealthProbe(context.Background(), cfgHealthy); err != nil {
		t.Fatalf("ServerHealthProbe(empty): %v", err)
	}
}
