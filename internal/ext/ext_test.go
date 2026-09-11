package ext

import (
	"context"
	"encoding/json"
	"fmt"
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
	if len(caps.Renderers) != 1 || caps.Renderers[0].Kind != "table" {
		t.Fatalf("renderers = %+v", caps.Renderers)
	}
	if len(caps.Renderers[0].Spec) == 0 {
		t.Fatal("renderer spec must reach the host")
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
	// Self-diagnosing: a Load-skip and a consult-and-allow both produce
	// ToolCall→nil, so pin the branch explicitly. If this fires on CI, the
	// handshake failed on that platform (see Failures text), not the policy.
	if n := len(m.list()); n != 1 {
		t.Fatalf("crash fixture did not load (n=%d): %v", n, m.Failures())
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
	// The fixture answers with a structured payload the TUI's table
	// renderer consumes; the tool adapter surfaces it verbatim.
	if !strings.Contains(res.Text, "greeting") || !strings.Contains(res.Text, "linh") {
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

// TestDeadExtensionIsRetiredNotLatching pins the fail-closed boundary:
// an unavailable policy extension denies the call it was consulted for,
// but it must LEAVE the routing chain — otherwise one transient timeout
// would deny every tool call for the rest of the session, turning the
// killable-process design into an agent-wide hang.
func TestDeadExtensionIsRetiredNotLatching(t *testing.T) {
	dir := writeExtDir(t, "flaky", "hang")
	m := NewManager()
	m.EventTimeout = 200 * time.Millisecond
	t.Cleanup(m.Close)
	if err := m.Load(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if len(m.list()) != 1 {
		t.Fatal("fixture should load")
	}
	// First call: consulted, timed out, denied (fail-closed for that call).
	if _, err := m.ToolCall(context.Background(), "bash", json.RawMessage(`{}`)); err == nil {
		t.Fatal("a timed-out policy extension must deny the call it was asked about")
	}
	if n := len(m.list()); n != 0 {
		t.Fatalf("dead extension still in the chain (%d): denial would latch", n)
	}
	// Second call: the dead one can no longer deny anything.
	if _, err := m.ToolCall(context.Background(), "bash", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("after retirement the call must proceed, got %v", err)
	}
}

// TestErrorResponseKeepsTheExtensionAlive: a well-formed response carrying
// `error` is the extension ANSWERING about one call, not a broken
// boundary. Killing it there would discard every later capability.
func TestErrorResponseKeepsTheExtensionAlive(t *testing.T) {
	dir := writeExtDir(t, "errs", "errorreply")
	m := NewManager()
	t.Cleanup(m.Close)
	if err := m.Load(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ToolCall(context.Background(), "bash", json.RawMessage(`{}`)); err == nil {
		t.Fatal("an errored policy reply must deny that call")
	}
	if len(m.list()) != 1 {
		t.Fatalf("extension died for answering with error; chain=%d", len(m.list()))
	}
	// Still usable for the next call (the fixture then allows).
	if _, err := m.ToolCall(context.Background(), "bash", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("live extension must keep serving: %v", err)
	}
}

// TestConcurrentToolCallsSerializeCleanly pins the shared-reader race: the
// agent runs up to MaxToolWorkers tool calls at once, and every reply
// must carry its own request's correlation, with no healthy extension
// killed by a host-caused race.
func TestConcurrentToolCallsSerializeCleanly(t *testing.T) {
	dir := writeExtDir(t, "conc", "revise")
	m := NewManager()
	m.EventTimeout = 3 * time.Second
	t.Cleanup(m.Close)
	if err := m.Load(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	const n = 12 // > MaxToolWorkers
	var wg sync.WaitGroup
	got := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := m.ToolCall(context.Background(), "bash",
				json.RawMessage(`{"command":"echo `+fmt.Sprint(i)+`"}`))
			got[i], errs[i] = string(out), err
		}(i)
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("call %d: %v", i, errs[i])
		}
		// The fixture revises every call identically; a torn or
		// mis-correlated reply would surface as a different payload or a
		// killed extension.
		if !strings.Contains(got[i], "REVISED") {
			t.Fatalf("call %d got %q (torn/mismatched reply)", i, got[i])
		}
	}
	if len(m.list()) != 1 {
		t.Fatal("healthy extension was retired by a host-side race")
	}
}

// A policy-only extension (no tools, no commands — just the tool_call
// event) is the documented hook shape, so the manager must report it as a
// live policy hook. cmd's attachExtensions used to discard the manager
// whenever tools and commands were both empty, which silently disabled
// exactly this extension.
func TestPolicyOnlyExtensionIsCountedAndEnforces(t *testing.T) {
	m, _ := loadOne(t, "block")
	if got := m.PolicyHooks(); got != 1 {
		t.Fatalf("PolicyHooks = %d, want 1 for an events-only extension", got)
	}
	// And it really is in the enforcement path.
	_, err := m.ToolCall(context.Background(), "bash", json.RawMessage(`{"command":"rm -rf /"}`))
	if err == nil {
		t.Fatal("blocking extension did not deny the call")
	}
}

// A manager with no extensions at all reports zero hooks (so callers can
// keep the cheap "nothing loaded" path).
func TestPolicyHooksZeroWithoutExtensions(t *testing.T) {
	m := NewManager()
	t.Cleanup(m.Close)
	if got := m.PolicyHooks(); got != 0 {
		t.Fatalf("PolicyHooks = %d, want 0", got)
	}
	var nilMgr *Manager
	if got := nilMgr.PolicyHooks(); got != 0 {
		t.Fatalf("nil manager PolicyHooks = %d, want 0", got)
	}
}

// TestRunCommandErrors pins the command-error boundaries: a manager with
// zero extensions must name the drop-in directory (a setup problem, not a
// typo), while a loaded manager keeps the plain unknown-name error, and
// unqualified names stay a caller bug.
func TestRunCommandErrors(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(t *testing.T) (*Manager, string) // (manager, dir the error must mention, "" = none)
		call    string
		wantSub string
	}{
		{
			name: "zero extensions names the drop-in directory",
			setup: func(t *testing.T) (*Manager, string) {
				dir := t.TempDir()
				m := NewManager()
				t.Cleanup(m.Close)
				if err := m.Load(context.Background(), dir); err != nil {
					t.Fatal(err)
				}
				return m, dir
			},
			call:    "nope:cmd",
			wantSub: "no extensions loaded (drop an executable into ",
		},
		{
			name: "loaded manager keeps the unknown-name error",
			setup: func(t *testing.T) (*Manager, string) {
				m, _ := loadOne(t, "block")
				return m, ""
			},
			call:    "nope:cmd",
			wantSub: `no extension named "nope"`,
		},
		{
			name: "unqualified name stays a caller error",
			setup: func(t *testing.T) (*Manager, string) {
				m := NewManager()
				t.Cleanup(m.Close)
				return m, ""
			},
			call:    "nope",
			wantSub: "not extension-qualified",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, dir := tc.setup(t)
			_, err := m.RunCommand(context.Background(), tc.call, "")
			if err == nil {
				t.Fatalf("%s: expected an error", tc.call)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q: want substring %q", err, tc.wantSub)
			}
			if dir != "" && !strings.Contains(err.Error(), dir) {
				t.Fatalf("zero-extension error must name the extensions dir %s: %q", dir, err)
			}
		})
	}
}
