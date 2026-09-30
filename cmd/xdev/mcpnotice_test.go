package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/tool"
)

// A failed MCP server used to be written to stderr, which the alt screen
// paints over the composer: the user read an error in the middle of their own
// draft, and then again after quitting. The report sink is where a mode with a
// UI of its own takes it instead — the TUI shows it as a toast in the corner
// (tui.Toast) — and the message must fit the one row it lands in, since the
// full error carries a whole fork/exec path.
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
		if mgr := attachMCP(ctx, tool.NewRegistry(), true, func(msg string) { got = append(got, msg) }, nil); mgr != nil {
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

// The reported bug: this notice had its own two-minute grace, so at launch it
// sat in the corner long past the point anyone reads a toast — it looked like
// chrome that never clears rather than a notice that did its job. runTUI needs
// a tty, so the grace cannot be observed by driving the TUI here; what is
// assertable is that the mode no longer owns a grace of its own and takes the
// toast stack's error default (d == 0), which is where a per-mode lifetime can
// creep back in.
func TestMCPReportTakesTheToastStacksOwnGrace(t *testing.T) {
	src, err := os.ReadFile("tui.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	if strings.Contains(s, "mcpNoticeGrace") {
		t.Error("cmd/xdev owns an MCP notice grace again; the toast stack's error default is the one grace a corner notice should have")
	}
	if !strings.Contains(s, "app.Toast(tui.ToastError, msg, 0)") {
		t.Error("the MCP report no longer asks the toast stack for its own error grace (d == 0)")
	}
}
