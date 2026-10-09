package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/tool"
)

// TestGmailToolWiredThroughRegistry is the cmd-seam smoke test: newToolRegistry
// registers gmail, and a missing `gog` binary is reported actionably through
// the registry rather than panicking at call time.
func TestGmailToolWiredThroughRegistry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH isolation is POSIX")
	}
	dataDir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dataDir)
	// Empty PATH so lookPath("gog") fails inside the tool.
	t.Setenv("PATH", filepath.Join(t.TempDir(), "empty"))

	reg := newToolRegistry(t.TempDir(), nil, "p", "m", nil, nil, nil)
	it, ok := reg.Get("gmail")
	if !ok {
		t.Fatal("gmail not registered")
	}

	res, err := it.Execute(context.Background(), json.RawMessage(`{"op":"labels"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "gog") {
		t.Fatalf("missing gog not actionable through the registry: %+v", res)
	}

	// Medium effort defers gmail behind the catalog (like github).
	tool.ApplySessionEffort(reg, "medium")
	deferred := map[string]bool{}
	for _, e := range reg.Deferred() {
		deferred[e.Name] = true
	}
	if !deferred["gmail"] {
		t.Fatal("medium effort must defer gmail")
	}

	// Keep the empty PATH dir from looking like a leak if a later test
	// reuses the process (defense in depth; t.Setenv already restores).
	_ = os.Getenv("PATH")
}
