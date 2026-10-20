package tool

import (
	"testing"
)

// A sticky deferral must survive the effort ladder's rebuild. The ladder
// calls ResetDeferred and re-applies its OWN tables, so a plain Defer was
// wiped and every tool of a wide remote surface landed back in the eager
// schema — re-sent on every request. A 27-tool database server has no
// business in the tool list of a session working on a Go repository.
func TestStickyDeferralSurvivesTheEffortRebuild(t *testing.T) {
	var ran []string
	reg := NewRegistry()
	reg.Register(&catalogStub{name: "remote_query_thing", desc: "query the remote thing", ran: &ran})
	reg.DeferSticky("remote_query_thing", "query the remote thing", "mcp")

	for _, rung := range []string{"low", "medium", "high", "xhigh", "max"} {
		ApplySessionEffort(reg, rung)
		if !reg.deferred("remote_query_thing") {
			t.Fatalf("rung %q: the sticky deferral was wiped — the remote tool is eager again", rung)
		}
		for _, d := range reg.Defs() {
			if d.Name == "remote_query_thing" {
				t.Fatalf("rung %q: the remote tool is in the eager schema", rung)
			}
		}
	}
}

// A sticky entry keeps its own index line. The low rung's generic index
// would otherwise replace a remote tool's server-authored summary with a
// truncated first line.
func TestStickyDeferralKeepsItsIndex(t *testing.T) {
	var ran []string
	reg := NewRegistry()
	reg.Register(&catalogStub{name: "remote_query_thing", desc: "query the remote thing", ran: &ran})
	reg.DeferSticky("remote_query_thing", "query the remote thing", "mcp")

	ApplySessionEffort(reg, "low")
	found := false
	for _, e := range reg.Deferred() {
		if e.Name == "remote_query_thing" {
			found = true
			if e.Index != "query the remote thing" {
				t.Fatalf("index = %q, want the sticky one", e.Index)
			}
		}
	}
	if !found {
		t.Fatal("the sticky tool is not in the deferred set")
	}
}

// A removed tool must not come back through the sticky set: ResetDeferred
// re-applies it, so a name left behind would reappear in the catalog with
// no tool behind it.
func TestRemovingAStickyToolDropsTheStickyEntry(t *testing.T) {
	var ran []string
	reg := NewRegistry()
	reg.Register(&catalogStub{name: "remote_query_thing", desc: "query the remote thing", ran: &ran})
	reg.DeferSticky("remote_query_thing", "query the remote thing", "mcp")

	reg.Remove("remote_query_thing")
	ApplySessionEffort(reg, "high")
	for _, e := range reg.Deferred() {
		if e.Name == "remote_query_thing" {
			t.Fatal("a removed tool came back through the sticky set")
		}
	}
}

// The built-in set stays the ladder's to decide: a sticky entry is a floor,
// never a ceiling, and the ladder still rebuilds its own tables from scratch
// with the sticky entries intact.
func TestStickyDeferralDoesNotBlockTheBuiltInSet(t *testing.T) {
	var ran []string
	reg := NewRegistry()
	reg.Register(&catalogStub{name: "read", desc: "read a file", ran: &ran})
	reg.Register(&catalogStub{name: "remote_query_thing", desc: "query the remote thing", ran: &ran})
	reg.DeferSticky("remote_query_thing", "query the remote thing", "mcp")

	ApplySessionEffort(reg, "medium")
	if !reg.deferred("remote_query_thing") {
		t.Fatal("the sticky deferral was lost")
	}
	// A built-in long-tail name the ladder defers on its own.
	reg.Register(&catalogStub{name: "ast_grep", desc: "structural code search", ran: &ran})
	ApplySessionEffort(reg, "medium")
	if !reg.deferred("remote_query_thing") {
		t.Fatal("the sticky deferral was lost on the second rebuild")
	}
	if !reg.deferred("ast_grep") {
		t.Fatal("the ladder's own deferred set was lost")
	}
}

// A sticky deferral is still a real deferral: the tool leaves the eager
// schema, stays registered, and stays runnable through the bridge.
func TestStickyDeferredToolStaysRunnable(t *testing.T) {
	var ran []string
	reg := NewRegistry()
	reg.Register(&catalogStub{name: "remote_query_thing", desc: "query the remote thing", ran: &ran})
	reg.DeferSticky("remote_query_thing", "query the remote thing", "mcp")
	ApplySessionEffort(reg, "high")

	if _, ok := reg.Get("remote_query_thing"); !ok {
		t.Fatal("the deferred tool left the registry")
	}
	for _, d := range reg.Defs() {
		if d.Name == "remote_query_thing" {
			t.Fatal("the deferred tool is in the eager schema")
		}
	}
}
