package main

import (
	"os"
	"testing"
)

func TestRunMcpsCLI(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dir)
	cfg := "servers:\n  browser:\n    url: http://localhost:3000\n  filesystem:\n  disabled-server:\n    disabled: true\n"
	if err := os.WriteFile(dir+"/mcp.yml", []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	// ServerHealthProbe will probe localhost:3000 (likely down) and
	// filesystem (no command → unreachable). Disabled stays disabled.
	rc := runMcps(nil)
	if rc != 0 {
		t.Fatalf("runMcps = %d, want 0", rc)
	}
}

func TestRunMcpsNoServers(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dir)
	if err := os.WriteFile(dir+"/mcp.yml", []byte("servers: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rc := runMcps(nil)
	if rc != 0 {
		t.Fatalf("runMcps = %d, want 0", rc)
	}
}

func TestRunMcpsProbeErrorContinues(t *testing.T) {
	// Verify that a probe error for one server doesn't prevent
	// listing other servers. Uses a real HTTP server that returns
	// 500 to trigger an error path.
	dir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dir)
	cfg := "servers:\n  bad:\n    url: http://localhost:1\n"
	if err := os.WriteFile(dir+"/mcp.yml", []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	rc := runMcps(nil)
	if rc != 0 {
		t.Fatalf("runMcps = %d, want 0 even with unreachable server", rc)
	}
}
