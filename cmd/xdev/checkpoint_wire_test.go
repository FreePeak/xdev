package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/tool"
)

// TestCheckpointToolsWiredThroughRegistry is the cmd-seam smoke test for
// M13 #51: newToolRegistry registers checkpoint/rewind, wireTaskParent binds
// them to the active session, and a rewind through the registry moves that
// store's leaf — a session switch rebinds, so the old checkpoints are gone.
func TestCheckpointToolsWiredThroughRegistry(t *testing.T) {
	reg := newToolRegistry(t.TempDir(), nil, "p", "m", nil, nil, nil)
	cpt, ok := reg.Get(tool.CheckpointToolName)
	if !ok {
		t.Fatal("checkpoint tool not registered")
	}
	rwt, ok := reg.Get(tool.RewindToolName)
	if !ok {
		t.Fatal("rewind tool not registered")
	}

	store := session.OpenMem(t.TempDir(), "cmd checkpoint smoke")
	t.Cleanup(func() { _ = store.Close() })
	wireTaskParent(reg, store)

	sayUser := func(text string) {
		t.Helper()
		msg := ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: text}}}
		if err := store.Append(&session.MessageEntry{Message: msg}); err != nil {
			t.Fatal(err)
		}
	}
	exec := func(tl tool.Tool, args string) string {
		t.Helper()
		res, err := tl.Execute(context.Background(), json.RawMessage(args))
		if err != nil {
			t.Fatalf("%s %s: %v", tl.Name(), args, err)
		}
		if res.IsError {
			t.Fatalf("%s %s: %s", tl.Name(), args, res.Text)
		}
		return res.Text
	}

	sayUser("start")
	sayUser("state worth keeping")
	exec(cpt, `{"name":"keep","note":"registry smoke"}`)
	sayUser("exploration")
	exec(rwt, `{"name":"keep","report":"abandoned the exploration"}`)

	leaf, ok := store.Entry(store.LeafID()).(*session.BranchSummaryEntry)
	if !ok {
		t.Fatalf("leaf = %T, want *session.BranchSummaryEntry", store.Entry(store.LeafID()))
	}
	if !strings.Contains(leaf.Summary.Text(), "abandoned the exploration") {
		t.Fatalf("report = %q", leaf.Summary.Text())
	}

	// Session switch: the tools follow the active session.
	other := session.OpenMem(t.TempDir(), "other")
	t.Cleanup(func() { _ = other.Close() })
	wireTaskParent(reg, other)
	res, err := cpt.Execute(context.Background(), json.RawMessage(`{"op":"list"}`))
	if err != nil || !strings.Contains(res.Text, "No checkpoints") {
		t.Fatalf("list after session switch = %q (err=%v)", res.Text, err)
	}
}
