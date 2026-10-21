package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/FreePeak/xdev/internal/tool"
)

// The tree's edge: a child's events name the child that spawned it. Without
// it a chain is a flat list of names, and the user cannot tell which child
// spawned which — the one thing a chain is about.
func TestSpawnChildEventsCarryTheParent(t *testing.T) {
	var mu sync.Mutex
	var got []SubagentEvent
	p := &fakeProvider{calls: childScripts(t, `{"result":"done"}`)}
	_, err := SpawnChild(context.Background(), SubagentSpec{
		Name: "leaf", Prompt: "do the leaf work", Provider: p, Model: "m",
		Parent: "scout",
		OnEvent: func(ev SubagentEvent) {
			mu.Lock()
			got = append(got, ev)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 {
		t.Fatal("the child emitted no events")
	}
	for _, ev := range got {
		if ev.Parent != "scout" {
			t.Fatalf("event %+v does not name the parent", ev)
		}
	}
}

// A child the top-level session spawned has no parent: the link is empty, so
// the display draws it as a root rather than hanging it off nothing.
func TestSpawnChildOfTheSessionHasNoParent(t *testing.T) {
	var mu sync.Mutex
	var got []SubagentEvent
	p := &fakeProvider{calls: childScripts(t, `{"result":"done"}`)}
	_, err := SpawnChild(context.Background(), SubagentSpec{
		Name: "root", Prompt: "do the work", Provider: p, Model: "m",
		OnEvent: func(ev SubagentEvent) {
			mu.Lock()
			got = append(got, ev)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, ev := range got {
		if ev.Parent != "" {
			t.Fatalf("event %+v names a parent it does not have", ev)
		}
	}
}

// A chain's grandchild names the child that spawned it. Asserted end to end,
// through a real spawn: the link is set where the chain is BUILT (the nested
// task tool knows the child's own label), so a unit test of the field alone
// would not show that the chain actually carries it.
func TestTaskChainEventsCarryTheParentLink(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".xdev", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	def := "---\nname: chainer\ndescription: a child that may delegate\ntools: task\n---\ndelegate the work"
	if err := os.WriteFile(filepath.Join(dir, ".xdev", "agents", "chainer.md"), []byte(def), 0o644); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var events []SubagentEvent
	p := &fakeProvider{calls: []fakeScript{
		// The child (the chainer) calls the task tool.
		{events: toolCallEvents("task", `{"prompt":"go deeper","name":"leaf"}`)},
		// The grandchild answers and yields.
		{events: yieldEvents(`{"result":"deep"}`)},
		// The child reports its own result.
		{events: yieldEvents(`{"result":"child done"}`)},
	}}
	tt := &TaskTool{
		Provider: p, Model: "m", AgentRoots: dir, MaxDepth: 3,
		ChildTools: []tool.Tool{echoTool{}},
		OnEvent: func(ev SubagentEvent) {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		},
	}
	res, err := tt.Execute(context.Background(), json.RawMessage(`{"prompt":"go","agent":"chainer","name":"scout"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("the chain failed: %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	sawLeaf := false
	for _, ev := range events {
		if ev.Label != "leaf" {
			continue
		}
		sawLeaf = true
		if ev.Parent != "scout" {
			t.Fatalf("the grandchild's event = %+v, want parent \"scout\"", ev)
		}
	}
	if !sawLeaf {
		t.Fatalf("the chain produced no grandchild events: %+v", events)
	}
}
