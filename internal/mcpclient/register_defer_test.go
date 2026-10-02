package mcpclient

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/tool"
)

// stubTool is a tool.Tool that also reports a server name, the way the real
// remoteTool does, so the search-tag path is exercised.
type stubTool struct {
	name, desc, server string
}

func (s stubTool) Name() string                { return s.server + "_" + s.name }
func (s stubTool) Description() string         { return s.desc }
func (s stubTool) ServerName() string          { return s.server }
func (s stubTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (s stubTool) Execute(context.Context, json.RawMessage) (tool.Result, error) {
	return tool.Result{}, nil
}

func stubTools(n int) []tool.Tool {
	out := make([]tool.Tool, 0, n)
	for i := range n {
		// Unique per index: Registry.Register panics on a duplicate name, so
		// a wrapping scheme would make the fixture itself the failure.
		suffix := strconv.Itoa(i)
		out = append(out, stubTool{
			name:   "call" + suffix,
			desc:   "remote capability " + suffix,
			server: "srv",
		})
	}
	return out
}

// TestRegisterKeepsSmallSurfacesEager pins the no-regression half of the
// threshold: under DeferThreshold a session's MCP tools stay in the eager
// schema, because hiding three tools would cost usability for no measured gain.
func TestRegisterKeepsSmallSurfacesEager(t *testing.T) {
	reg := tool.NewRegistry()
	Register(reg, stubTools(DeferThreshold))

	if got := len(reg.Defs()); got != DeferThreshold {
		t.Fatalf("eager defs = %d, want %d (small surfaces must stay direct)", got, DeferThreshold)
	}
	if d := reg.Deferred(); len(d) != 0 {
		t.Fatalf("deferred = %v, want none below the threshold", d)
	}
}

// TestRegisterDefersWideSurfaces is the fix: past the threshold the remote
// tools leave the eager schema that every request re-sends.
func TestRegisterDefersWideSurfaces(t *testing.T) {
	reg := tool.NewRegistry()
	n := DeferThreshold + 5
	tools := stubTools(n)
	Register(reg, tools)

	if got := len(reg.Defs()); got != 0 {
		t.Fatalf("eager defs = %d, want 0 — a wide MCP surface must leave the per-request schema", got)
	}
	deferred := reg.Deferred()
	if len(deferred) != n {
		t.Fatalf("deferred = %d, want %d", len(deferred), n)
	}
	// The catalog is a discovery seam, not a boundary: every tool must still
	// be registered and runnable under its real namespaced name.
	for _, tl := range tools {
		name := tl.Name()
		got, ok := reg.Get(name)
		if !ok {
			t.Fatalf("%q vanished from the registry — deferral must not remove it", name)
		}
		if _, err := got.Execute(context.Background(), json.RawMessage(`{}`)); err != nil {
			t.Fatalf("%q not executable after deferral: %v", name, err)
		}
	}
}

// TestDeferredToolsAreDiscoverableByServerAndToolName guards the reason the
// tags exist: a model searching "srv" or "calla" must be able to land on the
// tool, or deferring it would make it unreachable.
func TestDeferredToolsAreDiscoverableByServerAndToolName(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(tool.NewToolSearchTool(reg.Catalog()))
	reg.Register(tool.NewToolDescribeTool(reg.Catalog()))
	reg.Register(tool.NewToolCallTool(reg.Catalog()))
	Register(reg, stubTools(DeferThreshold+1))

	for _, q := range []string{"srv", "srv_call1", "call1", "remote capability", "mcp"} {
		hits := reg.Catalog().Search(q, 10)
		if len(hits) == 0 {
			t.Fatalf("tool_search(%q) found nothing; the deferred index is unreachable", q)
		}
	}
}

// TestRegisterSurvivesDegenerateInput covers the paths that would otherwise
// panic a run: a nil registry, and a foreign tool whose Description is empty
// (Defer panics on an empty index).
func TestRegisterSurvivesDegenerateInput(t *testing.T) {
	Register(nil, stubTools(3)) // must not panic

	reg := tool.NewRegistry()
	many := make([]tool.Tool, 0, DeferThreshold+1)
	many = append(many, stubTool{name: "ok", desc: "fine", server: "srv"})
	for i := range DeferThreshold {
		many = append(many, stubTool{name: "silent" + strconv.Itoa(i), desc: "", server: "srv"})
	}
	Register(reg, many) // must not panic on the empty-description tools

	// An empty-description tool is still registered and simply not deferred.
	if _, ok := reg.Get("srv_silent0"); !ok {
		t.Fatal("a tool with no description must still be registered")
	}
	for _, e := range reg.Deferred() {
		if e.Name == "srv_silent0" {
			t.Fatal("a tool with no description must not be deferred — Defer panics on an empty index")
		}
	}
	if len(reg.Deferred()) != 1 {
		t.Fatalf("deferred = %d, want 1 (only the described tool)", len(reg.Deferred()))
	}
}

// TestIndexLineIsCappedAndFlat pins the index contract: server-authored
// descriptions are unbounded (one ran past 900 chars) and the index is one
// capped line per tool.
func TestIndexLineIsCappedAndFlat(t *testing.T) {
	long := strings.Repeat("word ", 500)
	got := indexLine(long)
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("index line carries a line break: %q", got)
	}
	if len([]rune(got)) > deferredIndexChars+1 { // +1 for the ellipsis
		t.Fatalf("index line is %d runes, cap is %d", len([]rune(got)), deferredIndexChars)
	}
	if got := indexLine("  \n\t "); got != "" {
		t.Fatalf("blank description = %q, want empty", got)
	}
	if got := indexLine("a\n\nb"); got != "a b" {
		t.Fatalf("flattening = %q, want %q", got, "a b")
	}
}

// TestRegisterDefersPerServerNotPooled is the fix: a wide surface must not
// drag a small server's tools behind the catalog with it. db-mcp-server
// (28 tools) configured beside leankg (3) used to push leankg_query out of the
// eager schema, and a session needing the knowledge graph then re-derived it
// with bash instead.
func TestRegisterDefersPerServerNotPooled(t *testing.T) {
	reg := tool.NewRegistry()
	var tools []tool.Tool
	for i := range DeferThreshold + 5 {
		tools = append(tools, stubTool{name: "db" + strconv.Itoa(i), desc: "sql call", server: "db"})
	}
	for i := range 3 {
		tools = append(tools, stubTool{name: "kg" + strconv.Itoa(i), desc: "graph search", server: "leankg"})
	}
	Register(reg, tools)

	for _, e := range reg.Deferred() {
		if strings.HasPrefix(e.Name, "leankg_") {
			t.Fatalf("%q deferred: a 3-tool server must stay eager beside a 17-tool one", e.Name)
		}
	}
	for i := range 3 {
		name := "leankg_kg" + strconv.Itoa(i)
		if _, ok := reg.Get(name); !ok {
			t.Fatalf("%q missing from the registry", name)
		}
	}
	if got, want := len(reg.Deferred()), DeferThreshold+5; got != want {
		t.Fatalf("deferred = %d, want %d (only the fat server)", got, want)
	}
}
