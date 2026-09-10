package ext

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/tool"
)

// buildFixture compiles the scriptable extension once per test binary run.
var (
	fixtureOnce sync.Once
	fixtureBin  string
	fixtureErr  error
)

func fixture(t *testing.T) string {
	t.Helper()
	fixtureOnce.Do(func() {
		dir, err := os.MkdirTemp("", "extfixture")
		if err != nil {
			fixtureErr = err
			return
		}
		fixtureBin = filepath.Join(dir, "ext")
		build := exec.Command("go", "build", "-o", fixtureBin, "./testdata/extfixture")
		if out, err := build.CombinedOutput(); err != nil {
			fixtureErr = err
			fixtureBin = "BUILD FAILED: " + string(out)
			return
		}
	})
	if fixtureErr != nil {
		t.Skipf("cannot build ext fixture: %v (%s)", fixtureErr, fixtureBin)
	}
	return fixtureBin
}

// writeExtDir installs the fixture into an extensions dir with mode set.
func writeExtDir(t *testing.T, name, mode string) string {
	t.Helper()
	dir := t.TempDir()
	bin := fixture(t)
	dst := filepath.Join(dir, name)
	raw, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EXT_MODE", mode)
	return dir
}

func loadOne(t *testing.T, mode string) (*Manager, string) {
	t.Helper()
	dir := writeExtDir(t, "policy", mode)
	m := NewManager()
	t.Cleanup(m.Close)
	if err := m.Load(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if len(m.list()) != 1 {
		t.Fatalf("expected 1 extension loaded, got %d", len(m.list()))
	}
	return m, dir
}

func TestHandshakeAnnouncesCapabilities(t *testing.T) {
	m, _ := loadOne(t, "ok")
	x := m.list()[0]
	caps := x.Capabilities()
	if len(caps.Tools) != 1 || caps.Tools[0].Name != "greet" {
		t.Fatalf("tools = %+v", caps.Tools)
	}
	if len(caps.Commands) != 1 || caps.Commands[0].Name != "ping" {
		t.Fatalf("commands = %+v", caps.Commands)
	}
	if len(caps.Renderers) != 1 || caps.Renderers[0].Kind != "card" {
		t.Fatalf("renderers = %+v", caps.Renderers)
	}
	// Declarative specs and commands surface through the manager.
	if _, ok := m.Renderers()["ext_policy_greet"]; !ok {
		t.Fatalf("renderers map = %v", m.Renderers())
	}
	if _, ok := m.Commands()["policy:ping"]; !ok {
		t.Fatalf("commands map = %v", m.Commands())
	}
}

func TestToolCallBlocksFailClosed(t *testing.T) {
	m, _ := loadOne(t, "block")
	_, err := m.ToolCall(context.Background(), "bash", json.RawMessage(`{"command":"rm -rf /"}`))
	if err == nil || !strings.Contains(err.Error(), "policy: no bash") {
		t.Fatalf("expected block with reason, got %v", err)
	}
}

func TestToolCallRevisesArguments(t *testing.T) {
	m, _ := loadOne(t, "revise")
	got, err := m.ToolCall(context.Background(), "bash", json.RawMessage(`{"command":"echo ORIGINAL"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "REVISED") {
		t.Fatalf("args not revised: %s", got)
	}
}

func TestToolResultPatches(t *testing.T) {
	m, _ := loadOne(t, "ok")
	original := json.RawMessage(`{"text":"raw output","isError":false}`)
	got := m.ToolResult(context.Background(), "read", json.RawMessage(`{"path":"x"}`), original)
	if !strings.Contains(string(got), "patched by ext") {
		t.Fatalf("result not patched: %s", got)
	}
}

// TestHungExtensionIsKilled pins the boundary promise: a child that never
// answers is timed out and SIGKILLed, and the policy verdict is DENY
// (fail-closed), not a silent allow.
func TestHungExtensionIsKilled(t *testing.T) {
	dir := writeExtDir(t, "hung", "hang")
	m := NewManager()
	m.EventTimeout = 150 * time.Millisecond
	t.Cleanup(m.Close)
	if err := m.Load(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if len(m.list()) != 1 {
		t.Fatal("hung extension should complete the handshake before hanging on events")
	}
	start := time.Now()
	_, err := m.ToolCall(context.Background(), "bash", json.RawMessage(`{"command":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout error, got %v", err)
	}
	if !strings.Contains(err.Error(), "blocked by extension") {
		t.Fatalf("policy failure must deny the call: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("kill was not prompt: %s", d)
	}
}

// TestCrashedExtensionFailsClosed: a child that exits is dead, so a
// subscribed policy event cannot be answered — the call is denied.
func TestCrashedExtensionFailsClosed(t *testing.T) {
	dir := writeExtDir(t, "crash", "crash")
	m := NewManager()
	m.EventTimeout = time.Second
	t.Cleanup(m.Close)
	if err := m.Load(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	_, err := m.ToolCall(context.Background(), "bash", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "blocked by extension") {
		t.Fatalf("dead policy extension must fail closed, got %v", err)
	}
}

// TestFailOpenOptIn is the documented escape hatch: an extension that
// declares failOpen lets calls through when it dies.
func TestFailOpenOptIn(t *testing.T) {
	m, _ := loadOne(t, "failopen")
	x := m.list()[0]
	if !x.Capabilities().FailOpen {
		t.Fatal("failOpen not parsed from capabilities")
	}
	// The fixture answers normally in failopen mode, so allow flows through.
	if _, err := m.ToolCall(context.Background(), "bash", json.RawMessage(`{"command":"x"}`)); err != nil {
		t.Fatalf("failopen allow failed: %v", err)
	}
}

// TestRuntimeActionFromExtension: an extension may steer the agent, and
// the action arrives out-of-band without derailing the reply.
func TestRuntimeActionFromExtension(t *testing.T) {
	dir := writeExtDir(t, "actor", "action")
	m := NewManager()
	t.Cleanup(m.Close)
	actions := make(chan Action, 4)
	m.BindHost(func(a Action) { actions <- a })
	if err := m.Load(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	m.Emit(context.Background(), EventSessionStart, map[string]any{"model": "x"})
	select {
	case a := <-actions:
		if a.Action != "steer" || a.Text != "extension steering" {
			t.Fatalf("action = %+v", a)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("extension action never reached the host")
	}
}

// TestExtensionToolIsCallableThroughTheRegistry covers the tool adapter.
func TestExtensionToolIsCallableThroughTheRegistry(t *testing.T) {
	m, _ := loadOne(t, "ok")
	tools := m.Tools()
	if len(tools) != 1 {
		t.Fatalf("tools = %d", len(tools))
	}
	if got := tools[0].Name(); got != "ext_policy_greet" {
		t.Fatalf("namespaced name = %q", got)
	}
	reg := tool.NewRegistry()
	for _, tt := range tools {
		reg.Register(tt)
	}
	got, ok := reg.Get("ext_policy_greet")
	if !ok {
		t.Fatal("extension tool not registered")
	}
	res, err := got.Execute(context.Background(), json.RawMessage(`{"who":"linh"}`))
	if err != nil || res.IsError {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if !strings.Contains(res.Text, "hello from the extension") {
		t.Fatalf("text = %q", res.Text)
	}
}

// TestNonExecutableIsSkipped: the directory holds arbitrary files; only
// executables load, and a broken extension never aborts Load.
func TestNonExecutableIsSkipped(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken"), []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := fixture(t)
	raw, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("EXT_MODE", "ok")
	if err := os.WriteFile(filepath.Join(dir, "good"), raw, 0o755); err != nil {
		t.Fatal(err)
	}
	m := NewManager()
	t.Cleanup(m.Close)
	if err := m.Load(context.Background(), dir); err != nil {
		t.Fatalf("Load must tolerate broken extensions: %v", err)
	}
	if len(m.list()) != 1 {
		t.Fatalf("loaded %d extensions, want 1 (good only)", len(m.list()))
	}
}

// TestAbsentDirIsNotAnError keeps MCP-style optionality: no extensions
// directory means no extensions, not a failed start.
func TestAbsentDirIsNotAnError(t *testing.T) {
	m := NewManager()
	if err := m.Load(context.Background(), filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Fatalf("absent dir: %v", err)
	}
	if len(m.list()) != 0 {
		t.Fatal("expected no extensions")
	}
}
