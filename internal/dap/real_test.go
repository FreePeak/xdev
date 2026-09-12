package dap

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// TestRealDlvInitialize proves the socket transport, the framing and the
// initialize exchange against the real adapter when it is installed.
//
// A full session is deliberately not part of the suite: launching a debuggee
// needs local debug permissions (macOS taskgated) that test runners do not
// have, and dlv hangs inside the launch without them — no client-side timeout
// can fix that. The scripted adapters cover the post-handshake flow.
func TestRealDlvInitialize(t *testing.T) {
	if _, err := exec.LookPath("dlv"); err != nil {
		t.Skip("dlv is not installed")
	}
	spec := DefaultAdapters()["dlv"]
	if !spec.Socket {
		t.Fatal("the built-in dlv adapter must use the socket transport: `dlv dap` does not speak DAP on stdio")
	}
	c, err := startAdapter("dlv", spec, t.TempDir())
	if err != nil {
		t.Fatalf("startAdapter: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	caps, err := c.Initialize(ctx, "dlv")
	if err != nil {
		t.Fatalf("dlv initialize: %v", err)
	}
	// The capability that decides whether the handshake sends
	// configurationDone — without it the debuggee never starts.
	if !caps.SupportsConfigurationDoneRequest {
		t.Fatalf("dlv capabilities = %+v, want supportsConfigurationDoneRequest", caps)
	}
}
