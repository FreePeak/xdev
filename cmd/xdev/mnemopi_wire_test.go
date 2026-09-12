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
	// learn rides the Store seam — LearnTool.Backend is typed memory.Store —
	// so the SQLite store gets the lesson recorder too; retain{kind: lesson}
	// is the tool-side equivalent.
	if _, ok := reg.Get("learn"); !ok {
		t.Error("learn must be registered for the mnemopi backend (LearnTool.Backend is memory.Store)")
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

	// The lesson recorder itself must work over the store: a lesson written
	// through the registered tool has to come back out of the injected block.
	learn, ok := reg.Get("learn")
	if !ok {
		t.Fatal("learn tool missing")
	}
	res, err = learn.Execute(context.Background(), json.RawMessage(`{"memory":"mnemopi stores lessons as facts","context":"wire test"}`))
	if err != nil {
		t.Fatalf("learn: %v", err)
	}
	if res.IsError {
		t.Fatalf("learn = %+v, want a stored lesson", res)
	}
	if block := store.GuidanceBlock(); !strings.Contains(block, "mnemopi stores lessons as facts") {
		t.Errorf("the learned lesson did not reach the prompt block: %q", block)
	}

	// And the backend disappears when it is not the configured one.
	if other := buildMemory(&config.Settings{Memory: "local"}); other == nil {
		t.Error("memory: local must still build the markdown backend")
	}
	if none := buildMemory(&config.Settings{Memory: "off"}); none != nil {
		t.Errorf("memory: off = %v, want nil", none)
	}
}

// TestMnemopiMemoryOpsVerbWiring is the anti-inertness check for the /memory
// verbs (M12 #44): the ops the TUI installs for a configured store must reach
// that store — queue, enqueue (which queues the retain and applies it) and
// sync — instead of leaving a green grammar over nil funcs.
func TestMnemopiMemoryOpsVerbWiring(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	resetMnemopiCache(t)
	store := buildMemory(&config.Settings{Memory: "mnemopi"})
	mm, ok := store.(*memory.Mnemopi)
	if !ok {
		t.Fatalf("buildMemory(memory: mnemopi) = %T, want *memory.Mnemopi", store)
	}
	// buildMnemopiMemory wires the synthesis role lazily; this test pins the
	// queue path, so the model seam stays out of it (no API call, no flake).
	mm.Complete = nil

	ops := memoryOps(store)
	if ops == nil {
		t.Fatal("no /memory ops were built for the mnemopi backend")
	}
	if queue, err := ops.Dispatch("queue"); err != nil || !strings.Contains(queue, "retain queue") {
		t.Fatalf("Dispatch(queue) = (%q, %v), want the queue report", queue, err)
	}
	out, err := ops.Dispatch("enqueue the deploy script lives in scripts/ci.sh")
	if err != nil {
		t.Fatalf("Dispatch(enqueue) = %v", err)
	}
	if !strings.Contains(out, "applied 1") {
		t.Errorf("enqueue did not apply the queued retain: %q", out)
	}
	// The enqueued text is a stored, recallable fact, not just a queue row.
	hits, err := mm.Recall(context.Background(), "deploy script", 5)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(hits) == 0 || !strings.Contains(hits[0].Fact.Text, "scripts/ci.sh") {
		t.Errorf("recall after enqueue = %+v, want the queued fact", hits)
	}
	if _, err := ops.Dispatch("sync"); err != nil {
		t.Errorf("Dispatch(sync) = %v, want no error", err)
	}
	// diagnose belongs to the remote backend: the verb must say so instead of
	// silently answering something else.
	if _, err := ops.Dispatch("diagnose"); err == nil {
		t.Error("the mnemopi backend has no diagnose; the verb must report itself unavailable")
	}
	// No backend at all leaves /memory unwired (Dispatch then explains how to
	// turn memory on rather than panicking).
	if none := memoryOps(nil); none != nil {
		t.Errorf("memoryOps(no backend) = %+v, want nil", none)
	}
}
