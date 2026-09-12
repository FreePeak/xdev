package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestSecurityScanWiredThroughRegistry is the cmd-seam smoke test for M15 #67:
// newToolRegistry registers security_scan, and the tool answers through the
// same Execute path the agent loop calls even when the host has nothing
// installed — a bare temp directory has no go.mod and a PATH with no scanners
// in it, so every scanner must report a skip instead of failing the tool.
func TestSecurityScanWiredThroughRegistry(t *testing.T) {
	dir := t.TempDir()
	reg := newToolRegistry(dir, nil, "p", "m", nil, nil, nil)
	scan, ok := reg.Get("security_scan")
	if !ok {
		t.Fatal("security_scan not registered in the tool registry")
	}
	t.Setenv("PATH", t.TempDir())
	res, err := scan.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("security_scan: %v", err)
	}
	if res.IsError {
		t.Fatalf("a host without scanners must not fail the tool: %s", res.Text)
	}
	for _, want := range []string{
		"security_scan: no findings in " + dir,
		"vet skipped: no go.mod above ",
		"semgrep skipped: semgrep is not installed",
		"gitleaks skipped: gitleaks is not installed",
	} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("result missing %q:\n%s", want, res.Text)
		}
	}
}
