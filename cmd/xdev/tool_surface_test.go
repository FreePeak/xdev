package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/FreePeak/xdev/internal/tool"
)

// remoteStub is a stand-in for an MCP tool: a namespaced name, a description
// and a schema. The registry does not care where a tool came from.
type remoteStub struct{ name, desc string }

func (s remoteStub) Name() string                { return s.name }
func (s remoteStub) Description() string         { return s.desc }
func (s remoteStub) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (s remoteStub) Execute(context.Context, json.RawMessage) (tool.Result, error) {
	return tool.Result{}, nil
}

// The eager tool surface is the tax every request pays: the schema of every
// eager tool is re-sent on every turn, whether or not the task needs it.
//
// This measures the surface the effort rungs produce, with one wide remote
// server present — the shape a developer with a database MCP configured
// actually has. The remote surface is registered the way cmd/xdev registers
// it (mcpclient.Register defers a server past the threshold) and the ladder
// is then applied, which is the order the wiring uses.
//
// The number that matters is the DROP from medium to high. Before the
// sticky-deferral fix, the ladder's rebuild put every remote tool back in
// the eager schema, so a rung change moved the surface the WRONG way for
// the tools a session never calls.
func TestMeasureTheEagerToolSurfacePerRung(t *testing.T) {
	reg := newToolRegistry(t.TempDir(), nil, "p", "m", nil, nil, nil, false)

	// One wide remote server, the shape the threshold targets.
	var remote []tool.Tool
	for i := 0; i < 27; i++ {
		remote = append(remote, remoteStub{
			name: "db_query_thing" + string(rune('a'+i)),
			desc: "a database operation that is nothing to do with the repository being worked on",
		})
	}
	for _, t2 := range remote {
		reg.Register(t2)
	}
	// mcpclient.Register defers a server past the threshold. The registry
	// cannot import mcpclient (mcpclient imports the registry), so the
	// deferral it performs is performed here: the same sticky call.
	for _, t2 := range remote {
		reg.DeferSticky(t2.Name(), "a database operation", "mcp")
	}

	for _, rung := range []string{"low", "medium", "high", "xhigh", "max"} {
		tool.ApplySessionEffort(reg, rung)
		eager := len(reg.Defs())
		deferred := len(reg.Deferred())
		remoteEager := 0
		for _, d := range reg.Defs() {
			if len(d.Name) > 3 && d.Name[:3] == "db_" {
				remoteEager++
			}
		}
		t.Logf("rung %-6s eager=%-4d deferred=%-4d remote-eager=%d", rung, eager, deferred, remoteEager)
		if remoteEager != 0 {
			t.Errorf("rung %q: %d remote tools are in the eager schema", rung, remoteEager)
		}
	}
}

// The BEFORE picture, measured the same way: a plain Defer is what
// mcpclient.Register used to do, and the ladder's rebuild wipes it. The
// remote surface lands back in the eager schema at every rung above low.
//
// The two numbers side by side are the whole fix: 27 database tools, out of
// the request, on every turn, in a session that has nothing to do with
// databases.
func TestMeasureTheEagerToolSurfaceWithoutTheStickyDeferral(t *testing.T) {
	reg := newToolRegistry(t.TempDir(), nil, "p", "m", nil, nil, nil, false)
	var remote []tool.Tool
	for i := 0; i < 27; i++ {
		remote = append(remote, remoteStub{
			name: "db_query_thing" + string(rune('a'+i)),
			desc: "a database operation that is nothing to do with the repository being worked on",
		})
	}
	for _, t2 := range remote {
		reg.Register(t2)
	}
	for _, t2 := range remote {
		reg.Defer(t2.Name(), "a database operation", "mcp") // the OLD deferral
	}
	for _, rung := range []string{"low", "medium", "high", "xhigh", "max"} {
		tool.ApplySessionEffort(reg, rung)
		remoteEager := 0
		for _, d := range reg.Defs() {
			if len(d.Name) > 3 && d.Name[:3] == "db_" {
				remoteEager++
			}
		}
		t.Logf("rung %-6s eager=%-4d remote-eager=%d", rung, len(reg.Defs()), remoteEager)
	}
}

// The order the TUI actually uses, which is the order that matters.
//
// newToolRegistry applies the effort ladder; THEN the MCP servers connect
// and register their tools (attachMCP → finishMCP → mcpclient.Register),
// and a wide server is deferred. So at STARTUP the remote surface's
// deferral held — the ladder had already run.
//
// The break is the ladder's SECOND run. /effort and Shift-Tab re-apply it
// on the LIVE registry (cmd/xdev/tui.go's EffortOps.Set →
// tool.ApplySessionEffort), and that rebuild knew nothing about the remote
// surface — so every one of the 27 tools came back to the eager set on the
// very next turn, with no restart and nothing in the transcript to show
// why.
//
// This test walks that exact order, and it FAILS with the old non-sticky
// deferral:
//
//	after a second /effort change, 27 remote tools are back in the eager schema
func TestTheTUILadderRerunKeepsTheRemoteSurfaceDeferred(t *testing.T) {
	reg := newToolRegistry(t.TempDir(), nil, "p", "m", nil, nil, nil, false)

	// The MCP servers connect and register their tools.
	var remote []tool.Tool
	for i := 0; i < 27; i++ {
		remote = append(remote, remoteStub{
			name: "db_query_thing" + string(rune('a'+i)),
			desc: "a database operation that is nothing to do with the repository being worked on",
		})
	}
	for _, t2 := range remote {
		reg.Register(t2)
		reg.DeferSticky(t2.Name(), "a database operation", "mcp")
	}

	// /effort low → the ladder re-runs on the live registry.
	tool.ApplySessionEffort(reg, "low")

	remoteEager := 0
	for _, d := range reg.Defs() {
		if len(d.Name) > 3 && d.Name[:3] == "db_" {
			remoteEager++
		}
	}
	if remoteEager != 0 {
		t.Fatalf("after an /effort change, %d remote tools are back in the eager schema", remoteEager)
	}

	// Shift-Tab again, up the ladder: still deferred.
	tool.ApplySessionEffort(reg, "high")
	remoteEager = 0
	for _, d := range reg.Defs() {
		if len(d.Name) > 3 && d.Name[:3] == "db_" {
			remoteEager++
		}
	}
	if remoteEager != 0 {
		t.Fatalf("after a second /effort change, %d remote tools are back in the eager schema", remoteEager)
	}
}
