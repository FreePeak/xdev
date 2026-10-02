package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/FreePeak/xdev/internal/tool"
)

// TestConnectListsAndCalls is the MCP exit criterion: a configured stdio
// server is connected, its tools are namespaced and callable, and a
// failing server only logs (never blocks the agent).
var (
	binOnce  sync.Once
	binPath  string
	binError error
)

// buildFixtureServer compiles the test MCP server once per binary run.
func buildFixtureServer(t *testing.T) string {
	binOnce.Do(func() {
		dir, derr := os.MkdirTemp("", "mcpfixture")
		if derr != nil {
			binError = derr
			return
		}
		binPath = filepath.Join(dir, "mcpserver")
		build := exec.Command("go", "build", "-o", binPath, "./testdata/mcpserver")
		if out, err := build.CombinedOutput(); err != nil {
			binError = fmt.Errorf("build: %v\n%s", err, out)
		}
	})
	if binError != nil {
		t.Skipf("cannot build test server: %v (%s)", binError, binPath)
	}
	return binPath
}

func TestConnectListsAndCalls(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	bin := buildFixtureServer(t)
	dir := t.TempDir()

	cfgYAML := `
servers:
  echo:
    command: ` + bin + `
    args: []
  broken:
    command: /nonexistent/binary
    initTimeoutSec: 1
`
	cfgPath := filepath.Join(dir, "mcp.yml")
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewManager()
	defer mgr.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	connected, errs := mgr.Connect(ctx, cfg)
	if connected != 1 {
		t.Fatalf("connected = %d (errs=%v), want 1", connected, errs)
	}
	if len(errs) != 1 || !strings.Contains(errs[0], "broken") {
		t.Fatalf("broken server must be reported, not fatal: %v", errs)
	}

	tools := mgr.Tools()
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(tools))
	}
	if got := tools[0].Name(); got != "echo_echo" {
		t.Fatalf("namespaced name = %q", got)
	}
	// The schema reaches the model verbatim.
	var schema map[string]any
	if err := json.Unmarshal(tools[0].Parameters(), &schema); err != nil {
		t.Fatalf("parameters: %v", err)
	}
	if schema["type"] != "object" {
		t.Fatalf("schema = %v", schema)
	}

	reg := tool.NewRegistry()
	Register(reg, tools)
	got, ok := reg.Get("echo_echo")
	if !ok {
		t.Fatal("registered remote tool not found")
	}
	res, err := got.Execute(ctx, json.RawMessage(`{"text":"round-trip"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError || !strings.Contains(res.Text, "round-trip") {
		t.Fatalf("res = %+v", res)
	}
	// The fixture answers with a text summary AND structuredContent (the
	// shape a modern MCP server returns, SEP-2106); both must reach the
	// model. Before the fix the model saw only "echo: round-trip".
	if !strings.Contains(res.Text, `"echoed":"round-trip"`) {
		t.Fatalf("structuredContent did not reach the tool result:\n%s", res.Text)
	}
}

// TestConfigFilters pins per-server enable/disable and tool filtering.
func TestConfigFilters(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "mcp.yml")
	if err := os.WriteFile(p, []byte(`
servers:
  off:
    command: /bin/true
    disabled: true
  on:
    url: http://127.0.0.1:1/mcp
    excludeTools: [noisy]
    includeTools: [wanted]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Servers["off"].Disabled {
		t.Fatal("disabled flag not parsed")
	}
	if len(cfg.Servers["on"].ExcludeTools) != 1 || len(cfg.Servers["on"].IncludeTools) != 1 {
		t.Fatalf("filters = %+v", cfg.Servers["on"])
	}
	// Missing file → MCP off, not an error.
	if c, err := LoadConfig(filepath.Join(dir, "absent.yml")); err != nil || len(c.Servers) != 0 {
		t.Fatalf("absent config = %+v err=%v", c, err)
	}
}

// TestRenderContentIsBounded pins PRD §3.6 for remote tools: a multi-MB
// MCP payload must arrive windowed (head+tail), never verbatim.
func TestRenderContentIsBounded(t *testing.T) {
	huge := strings.Repeat("x", 4<<20) // 4 MiB
	res := &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: huge}},
	}
	got := renderContent(res)
	if len(got) > 4*mcpOutputHeadTail+1024 {
		t.Fatalf("rendered %d bytes — sink did not bound the output", len(got))
	}
	if !strings.Contains(got, "truncated") {
		t.Fatal("truncation not reported to the model")
	}
	// A small result passes through untouched.
	small := renderContent(&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "tiny"}}})
	if small != "tiny" {
		t.Fatalf("small result = %q", small)
	}
}

// TestRenderContentKeepsStructuredContentBesideText pins the unit half of
// the same bug: a server answering with a human summary in Content AND the
// real payload in structuredContent had that payload silently dropped,
// because the old gate rendered it only when Content rendered empty.
func TestRenderContentKeepsStructuredContentBesideText(t *testing.T) {
	res := &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: "found 2 issues"}},
		StructuredContent: map[string]any{"issues": []any{map[string]any{"key": "FOOD-2"}}},
	}
	got := renderContent(res)
	if !strings.Contains(got, "found 2 issues") {
		t.Fatalf("text content lost: %q", got)
	}
	if !strings.Contains(got, `"FOOD-2"`) {
		t.Fatalf("structuredContent dropped alongside text content: %q", got)
	}

	// The compliant-server case: both channels carry the same JSON (the
	// SDK populates Content from the typed Out value). One copy, not two.
	dup := &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: `{"issues":["FOOD-1"]}`}},
		StructuredContent: map[string]any{"issues": []any{"FOOD-1"}},
	}
	if n := strings.Count(renderContent(dup), `"FOOD-1"`); n != 1 {
		t.Fatalf("identical structured output rendered %d times, want 1: %q", n, renderContent(dup))
	}
}
