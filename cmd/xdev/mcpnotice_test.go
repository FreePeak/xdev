package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/tool"
)

// A failed MCP server used to be written to stderr, which the alt screen
// paints over the composer: the user read an error in the middle of their own
// draft, and then again after quitting. The report sink is where a mode with a
// UI of its own takes it instead, and the message must fit the one-line slot
// it lands in (the divider drops a hint wider than the room beside the model
// name, and the full error carries a whole fork/exec path).
func TestFailedMCPServerReportsToTheSinkNotStderr(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dir)
	cfg := "servers:\n  broken:\n    command: /nonexistent/mcp-server-that-does-not-exist\n"
	if err := os.WriteFile(filepath.Join(dir, "mcp.yml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	var got []string
	stderr := captureStderr(t, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// wait=true runs the connect inline, so the sink has fired by the time
		// attachMCP returns and the test needs no polling.
		if mgr := attachMCP(ctx, tool.NewRegistry(), true, func(msg string) { got = append(got, msg) }); mgr != nil {
			defer mgr.Close()
		}
	})

	if len(got) != 1 {
		t.Fatalf("sink got %v, want one notice for the broken server", got)
	}
	if want := "mcp: broken unavailable — see `xdev mcps` for configured URLs"; got[0] != want {
		t.Errorf("notice = %q, want %q", got[0], want)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want it empty: the sink owns the report now", stderr)
	}
}
