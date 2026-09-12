package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/memory"
	"github.com/FreePeak/xdev/internal/tool"
)

// resetMnemopiCache drops the package-level backend cache so a test starts
// from a fresh store (the cache keys on DataDir, and each test gets its own).
func resetMnemopiCache(t *testing.T) {
	t.Helper()
	mnemopiMu.Lock()
	mnemopiCache = map[string]*memory.Mnemopi{}
	mnemopiMu.Unlock()
}

// TestMnemopiBackendWiring is the anti-inertness check for M12 #44: the
// settings value alone must select the SQLite backend, inject its memories,
// answer the memory:// seam through the read tool, and register the tools.
func TestMnemopiBackendWiring(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	resetMnemopiCache(t)
	cwd := t.TempDir()
	settings := &config.Settings{Memory: "mnemopi"}

	store := buildMemory(settings)
	mm, ok := store.(*memory.Mnemopi)
	if !ok {
		t.Fatalf("buildMemory(memory: mnemopi) = %T, want *memory.Mnemopi", store)
	}
	if mm.Off() {
		t.Fatal("the configured mnemopi backend must not be off")
	}
	// One backend per settings identity: the prompt path and the registry
	// path must share the connection.
	if again := buildMemory(settings); again != memory.Store(mm) {
		t.Error("buildMemory returned a second mnemopi backend for the same settings")
	}
	if err := store.SaveLesson("wired lesson: the gateway caps streams at 4", "wire test"); err != nil {
		t.Fatalf("SaveLesson: %v", err)
	}
	if block := store.GuidanceBlock(); !strings.Contains(block, "the gateway caps streams at 4") {
		t.Errorf("the retained lesson did not reach the prompt block: %q", block)
	}

	// The memory:// read seam is registered by the backend constructor.
	read := tool.NewReadTool()
	res, err := read.Execute(context.Background(), json.RawMessage(`{"path":"memory://root"}`))
	if err != nil {
		t.Fatalf("read memory://root: %v", err)
	}
	if res.IsError || !strings.Contains(res.Text, "the gateway caps streams at 4") {
		t.Errorf("read memory://root = %+v, want the injected memory", res)
	}

	reg := newToolRegistry(cwd, nil, "p", "m", settings, nil, nil)
	for _, name := range []string{"recall", "retain", "reflect", "memory_edit"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("tool %q is not registered for the mnemopi backend", name)
		}
	}
	// learn is not registered here: its Backend field is typed *memory.Backend
	// (internal/memory/learn.go). Widening that one field to memory.Store makes
	// it work over mnemopi too; until then retain{kind: lesson} records lessons.
	if _, ok := reg.Get("learn"); ok {
		t.Error("learn over mnemopi needs LearnTool.Backend widened to memory.Store first")
	}

	// The retained facts are recallable through the tool the model calls.
	recall, ok := reg.Get("recall")
	if !ok {
		t.Fatal("recall tool missing")
	}
	res, err = recall.Execute(context.Background(), json.RawMessage(`{"query":"gateway streams"}`))
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if res.IsError || !strings.Contains(res.Text, "gateway caps streams at 4") {
		t.Errorf("recall = %+v, want the retained lesson", res)
	}

	// And the backend disappears when it is not the configured one.
	if other := buildMemory(&config.Settings{Memory: "local"}); other == nil {
		t.Error("memory: local must still build the markdown backend")
	}
	if none := buildMemory(&config.Settings{Memory: "off"}); none != nil {
		t.Errorf("memory: off = %v, want nil", none)
	}
}
