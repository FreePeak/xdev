package main

import (
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/memory"
	"github.com/FreePeak/xdev/internal/tool"
)

// TestBuildMemorySelectsBackend pins the memory.backend selection (M12 #43):
// empty Memory stays off (the dispatch does not invent a backend), "local"
// builds the markdown store, "hindsight" builds the remote backend — and
// ONE shared Hindsight instance across the repeated buildMemory calls the
// prompt, pipeline, and registry paths make (the backend owns the retain
// queue and the recall cache). The schema default is "local"; see
// defaultSettings.
func TestBuildMemorySelectsBackend(t *testing.T) {
	if got := buildMemory(nil); got != nil {
		t.Fatalf("buildMemory(nil) = %v, want nil", got)
	}
	off := &config.Settings{Memory: ""}
	if got := buildMemory(off); got != nil {
		t.Fatalf("default memory backend = %T, want nil", got)
	}

	local := &config.Settings{Memory: "local"}
	if got := buildMemory(local); got == nil {
		t.Fatalf("memory local = %T, want *memory.Backend", got)
	} else if _, ok := got.(*memory.Backend); !ok {
		t.Fatalf("memory local = %T, want *memory.Backend", got)
	}

	h := &config.Settings{Memory: "hindsight"}
	first := buildMemory(h)
	hs, ok := first.(*memory.Hindsight)
	if !ok {
		t.Fatalf("memory hindsight = %T, want *memory.Hindsight", first)
	}
	if hs.Off() {
		t.Fatal("a selected hindsight backend must not be off")
	}
	// The recall/retain tools share the same instance the prompt uses.
	if second := buildMemory(h); second != first {
		t.Fatalf("buildMemory returned a second instance: %p vs %p", second, first)
	}
	bank, tag := hs.Scope()
	if bank != memory.DefaultHindsightBank || !strings.HasPrefix(tag, "project:") {
		t.Errorf("default scope = (%q,%q)", bank, tag)
	}
}

// newTestHindsightRegistry builds the registry the way newToolRegistry does
// with memory: hindsight, so the additive registrations can be asserted.
func newTestHindsightRegistry() (*tool.Registry, *memory.Hindsight) {
	reg := tool.NewRegistry()
	h := memory.NewHindsight(memory.HindsightConfig{URL: "http://127.0.0.1:1", ProjectRoot: "/nonexistent"})
	reg.Register(&memory.RecallTool{Backend: h})
	reg.Register(&memory.RetainTool{Backend: h})
	reg.Register(&memory.ReflectTool{Backend: h})
	return reg, h
}

// TestHindsightToolsRegistered pins the additive tool surface (M12 #43):
// selecting hindsight exposes recall/retain/reflect — and no memory_edit and
// no learn (the local backend's tool).
func TestHindsightToolsRegistered(t *testing.T) {
	reg, _ := newTestHindsightRegistry()
	for _, name := range []string{memory.RecallToolName, memory.RetainToolName, memory.ReflectToolName} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("registry missing the %q tool", name)
		}
	}
	for _, name := range []string{"memory_edit", memory.LearnToolName} {
		if _, ok := reg.Get(name); ok {
			t.Errorf("registry must not expose %q for the hindsight backend", name)
		}
	}
}

// TestNoteMemoryTurn pins the turn boundary (M12 #43): a history ending in a
// user turn feeds the autoRetain cadence; anything else (a resumed session)
// adds no turn, and the local backend is a no-op.
func TestNoteMemoryTurn(t *testing.T) {
	var logs []string
	h := memory.NewHindsight(memory.HindsightConfig{
		URL:               "http://127.0.0.1:1", // unreachable: the cadence queues locally
		RetainEveryNTurns: 1,
		ProjectRoot:       t.TempDir(),
		Logf:              func(f string, a ...any) { logs = append(logs, f) },
	})
	noteMemoryTurn(h, []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "  fix the flaky test  "}}}})
	if h.QueueLen() != 1 {
		t.Fatalf("queue = %d after one user turn at cadence 1, want 1", h.QueueLen())
	}

	// Non-user tails add nothing.
	before := h.QueueLen()
	noteMemoryTurn(h, []ai.Message{{Role: ai.RoleAssistant}})
	noteMemoryTurn(h, nil)
	if h.QueueLen() != before {
		t.Errorf("non-user tails changed the queue: %d -> %d", before, h.QueueLen())
	}

	// The local backend has no cadence — the call must not panic or block.
	noteMemoryTurn(&memory.Backend{Dir: "/tmp/unused-memory-dir"}, []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hello"}}}})
	var nilMem memoryBackend
	noteMemoryTurn(nilMem, nil)
}

// TestHindsightMemoryOpsVerbWiring is the anti-inertness check for the remote
// backend's /memory verbs (M12 #43): the ops the TUI installs must reach the
// server-facing surface (diagnose, enqueue with the cadence's pending turns),
// and the local queue verbs must report themselves unavailable.
func TestHindsightMemoryOpsVerbWiring(t *testing.T) {
	h := memory.NewHindsight(memory.HindsightConfig{
		URL:         "http://127.0.0.1:1", // unreachable: diagnose reports it, nothing blocks
		ProjectRoot: t.TempDir(),
	})
	ops := memoryOps(h)
	if ops == nil {
		t.Fatal("no /memory ops were built for the hindsight backend")
	}
	diag, err := ops.Dispatch("diagnose")
	if err != nil {
		t.Fatalf("Dispatch(diagnose) = %v", err)
	}
	for _, want := range []string{"hindsight backend", "unreachable"} {
		if !strings.Contains(diag, want) {
			t.Errorf("diagnose dump missing %q:\n%s", want, diag)
		}
	}
	// The turn cadence's pending turns must reach the enqueue action.
	h.NoteUserTurn("fix the flaky test")
	queued, err := ops.Dispatch("enqueue")
	if err != nil {
		t.Fatalf("Dispatch(enqueue) = %v", err)
	}
	if !strings.Contains(queued, "1 retain(s) queued") {
		t.Errorf("enqueue = %q, want the pending turn queued", queued)
	}
	if h.QueueLen() != 1 {
		t.Errorf("queue = %d, want the failed flush to keep the retain", h.QueueLen())
	}
	// sync is the local store's verb: hindsight consolidates server-side.
	if _, err := ops.Dispatch("sync"); err == nil {
		t.Error("hindsight has no local sync; the verb must report itself unavailable")
	}
}
