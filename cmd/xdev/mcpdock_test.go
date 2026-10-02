package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/mcpclient"
	"github.com/FreePeak/xdev/internal/tool"
)

// The sidebar's MCP section used to show a count of CONNECTED servers, so a
// server that was enabled in mcp.yml and failed to answer was invisible: the
// list said "3 servers" and the human's broken one was one of the two missing.
// The section now comes from the config — every enabled server, one row each —
// with ○ on the ones the manager has no session for, which is the case worth a
// look.
func TestMcpDockBlockListsEveryEnabledServer(t *testing.T) {
	off := false
	on := true
	cfg := &mcpclient.Config{Servers: map[string]*mcpclient.ServerConfig{
		"zeta":   {Command: "/bin/z"},
		"alpha":  {Command: "/bin/a", Enabled: &off}, // disabled: not listed
		"middle": {URL: "http://localhost:9699/mcp"},
		"be-kg":  {URL: "http://gw/mcp", Enabled: &on},
	}}
	mgr := mcpclient.NewManager()
	defer mgr.Close()

	// No sessions yet: the panel builds on the first paint, long before the
	// async connect lands, so "nothing connected" is the shape it starts in.
	got := strings.Split(mcpDockBlock(cfg, mgr), "\n")
	want := []string{"MCP · 3", "○ be-kg", "○ middle", "○ zeta"}
	if len(got) != len(want) {
		t.Fatalf("block = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// The other half of the promise: a nil manager (a mode that never attached
// MCP) and a config with nothing enabled both paint nothing, so the dock omits
// the section instead of painting an empty one.
func TestMcpDockBlockEmptyWhenNothingEnabled(t *testing.T) {
	if got := mcpDockBlock(nil, nil); got != "" {
		t.Errorf("nil config = %q, want empty", got)
	}
	off := false
	cfg := &mcpclient.Config{Servers: map[string]*mcpclient.ServerConfig{
		"off": {Command: "/bin/x", Enabled: &off},
	}}
	if got := mcpDockBlock(cfg, nil); got != "" {
		t.Errorf("all-disabled config = %q, want empty", got)
	}
}

// End to end through the real loader: attachMCP must hand the registry a
// block that names a server even though NOTHING connects. That is the whole
// feature — a failed server visible in the sidebar — and it only works if the
// field is assigned before/independently of the connect result.
func TestAttachMCPDockBlockSurvivesAFailedServer(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dir)
	cfg := "servers:\n  broken:\n    command: /nonexistent/mcp-server\n  off:\n" +
		"    command: /nonexistent/other\n    enabled: false\n"
	if err := os.WriteFile(filepath.Join(dir, "mcp.yml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := tool.NewRegistry()
	// wait=true: the connect is inline, so the row's mark is already settled
	// (nothing connected) when the assertion runs. The block itself is
	// assigned before the connect, which is what this covers.
	stderr := captureStderr(t, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if mgr := attachMCP(ctx, reg, true, func(string) {}, nil); mgr != nil {
			defer mgr.Close()
		}
	})
	if reg.MCPBlock == nil {
		t.Fatal("attachMCP left the dock without an MCP source")
	}
	got := reg.MCPBlock()
	if want := "MCP · 1\n○ broken"; got != want {
		t.Errorf("block = %q, want %q (stderr %q)", got, want, stderr)
	}
}
