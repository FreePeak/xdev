package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestDebugToolWiredThroughRegistry is the cmd-seam smoke test for M15 #66:
// newToolRegistry registers the debug tool, its schema reaches the model, and
// an op runs through the real registry (no adapter binary is needed for the
// status/adapter-resolution paths).
func TestDebugToolWiredThroughRegistry(t *testing.T) {
	reg := newToolRegistry(t.TempDir(), nil, "p", "m", nil, nil, nil)
	dt, ok := reg.Get("debug")
	if !ok {
		t.Fatal("debug tool not registered")
	}
	res, err := dt.Execute(context.Background(), json.RawMessage(`{"op":"status"}`))
	if err != nil {
		t.Fatalf("debug status: %v", err)
	}
	if res.IsError {
		t.Fatalf("debug status: %s", res.Text)
	}
	if !strings.Contains(res.Text, "no active debug session") || !strings.Contains(res.Text, "dlv: dlv dap") {
		t.Fatalf("status = %q, want the inactive session and the built-in adapters", res.Text)
	}
	// A session-bound op fails with guidance rather than an error, and an
	// unknown adapter is reported with the configured list.
	res, err = dt.Execute(context.Background(), json.RawMessage(`{"op":"threads"}`))
	if err != nil || !res.IsError || !strings.Contains(res.Text, "op=launch") {
		t.Fatalf("threads without a session = %+v, %v", res, err)
	}
	res, err = dt.Execute(context.Background(), json.RawMessage(`{"op":"launch","adapter":"ghost","program":"/prog"}`))
	if err != nil || !res.IsError || !strings.Contains(res.Text, "unknown adapter") {
		t.Fatalf("unknown adapter = %+v, %v", res, err)
	}
}
