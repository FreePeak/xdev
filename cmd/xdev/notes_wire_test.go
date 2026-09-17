package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/tool"
)

// TestNotesToolsWiredThroughRegistry is the cmd-seam smoke test for M12 #45:
// newToolRegistry registers context_notes + new_context and the history://
// resolver, wireTaskParent binds the notebook to the active session, and a
// notebook write is reachable through both the tool and the read seam.
func TestNotesToolsWiredThroughRegistry(t *testing.T) {
	// The tools are gated behind compaction.experimentalContextManagement
	// (#88, matching omp's opt-in), so the settings enable it here.
	reg := newToolRegistry(t.TempDir(), nil, "p", "m",
		&config.Settings{ExperimentalContextManagement: true}, nil, nil)
	nt, ok := reg.Get(agent.ContextNotesToolName)
	if !ok {
		t.Fatal("context_notes not registered")
	}
	nc, ok := reg.Get(agent.NewContextToolName)
	if !ok {
		t.Fatal("new_context not registered")
	}
	if got := agent.NotesStateOf(reg); got == nil {
		t.Fatal("notes state not reachable from the registry")
	}

	store := session.OpenMem("/proj", "cmd notes smoke")
	t.Cleanup(func() { _ = store.Close() })
	for i := 0; i < 6; i++ {
		if err := store.Append(&session.MessageEntry{Message: ai.Message{
			Role:    ai.RoleUser,
			Content: []ai.Block{ai.TextBlock{Text: "cmd smoke turn"}},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	wireTaskParent(reg, store)

	exec := func(tl tool.Tool, args string) tool.Result {
		t.Helper()
		res, err := tl.Execute(context.Background(), json.RawMessage(args))
		if err != nil {
			t.Fatalf("%s %s: %v", tl.Name(), args, err)
		}
		return res
	}
	if res := exec(nt, `{"text":"wired notebook"}`); res.IsError {
		t.Fatalf("context_notes write: %s", res.Text)
	}
	if res := exec(nt, `{}`); res.IsError || res.Text != "wired notebook" {
		t.Fatalf("context_notes read = %q (err=%v)", res.Text, res.IsError)
	}

	// The read seam resolves through the registered history:// scheme.
	read, _ := reg.Get("read")
	res := exec(read, `{"path":"history://current"}`)
	if res.IsError || !strings.Contains(res.Text, "cmd smoke turn") {
		t.Fatalf("read history://current = %q (err=%v)", res.Text, res.IsError)
	}

	// All four prerequisites are active in the real registry, so the
	// rollover commits here (no provider call: no recursive summary).
	res = exec(nc, `{}`)
	if res.IsError {
		t.Fatalf("new_context: %s", res.Text)
	}
	if committed, _ := res.Details.(map[string]any)["committed"].(bool); !committed {
		t.Fatalf("details = %+v, want committed=true", res.Details)
	}
	var boundary *session.CompactionEntry
	for _, e := range store.Entries() {
		if c, ok := e.(*session.CompactionEntry); ok {
			boundary = c
		}
	}
	if boundary == nil || !agent.IsRolloverBoundary(boundary) {
		t.Fatalf("boundary = %+v, want a new_context rollover entry", boundary)
	}
	if !strings.Contains(boundary.Summary.Text(), "wired notebook") {
		t.Fatalf("boundary summary must carry the notebook: %q", boundary.Summary.Text())
	}

	// Session switch: the notebook follows the active session.
	other := session.OpenMem("/proj", "other notes smoke")
	t.Cleanup(func() { _ = other.Close() })
	wireTaskParent(reg, other)
	if res := exec(nt, `{}`); res.IsError || !strings.Contains(res.Text, "No context notes") {
		t.Fatalf("session switch leaked the notebook: %q", res.Text)
	}
}

// #88: with the gate off (the shipped default) neither tool registers, and the
// notes-backed rollover is unreachable — registering them unconditionally was
// the bug.
func TestNotesToolsGatedOffByDefault(t *testing.T) {
	for name, s := range map[string]*config.Settings{
		"unset": {},
		"off":   {ExperimentalContextManagement: false},
	} {
		reg := newToolRegistry(t.TempDir(), nil, "p", "m", s, nil, nil)
		if _, ok := reg.Get(agent.ContextNotesToolName); ok {
			t.Errorf("%s: context_notes registered with the gate off", name)
		}
		if _, ok := reg.Get(agent.NewContextToolName); ok {
			t.Errorf("%s: new_context registered with the gate off", name)
		}
	}
}
