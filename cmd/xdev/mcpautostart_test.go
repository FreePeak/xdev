package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/tool"
)

// The autoStart health probe and launch wait used to run inline in
// attachMCP, before the TUI drew its first frame. A local server that was
// down cost a probe plus a poll loop of up to healthTimeoutSec — 30s on the
// leankg recipe — of an undrawn screen before the user could type a word.
// The connect was already async; the probe that gates it was not.
func TestAttachMCPDoesNotHealthProbeOnTheCallersGoroutine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dir)

	// A server nothing answers on, whose autoStart never makes it healthy:
	// the inline path polled until the 8s budget ran out. A real recipe
	// points at a binary that binds eventually; the test only needs the
	// probe to stay slow.
	bin := filepath.Join(t.TempDir(), "never-listens")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := "servers:\n  slow:\n    url: http://localhost:19998/mcp\n    autoStart:\n      command: " + bin +
		"\n      healthTimeoutSec: 8\n"
	if err := os.WriteFile(filepath.Join(dir, "mcp.yml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	// wait=false is the TUI/rpc/acp path: the connect is deferred, so the
	// only work left on this goroutine is loading mcp.yml.
	if mgr := attachMCP(context.Background(), tool.NewRegistry(), false, nil); mgr != nil {
		defer mgr.Close()
	}
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("attachMCP blocked %s on the caller's goroutine; the autoStart health probe must run inside finishMCP", elapsed)
	}
}
