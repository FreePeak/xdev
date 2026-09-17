package lsp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/config"
)

// starter is the launch seam used by the tests: it counts launches and hands
// back one in-process fake server.
type starter struct {
	mu     sync.Mutex
	calls  []string
	client *Client
	err    error
}

func (s *starter) start(ctx context.Context, name string, spec ServerSpec, root string) (*Client, error) {
	s.mu.Lock()
	s.calls = append(s.calls, name+"@"+root)
	s.mu.Unlock()
	return s.client, s.err
}

func (s *starter) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLazyLaunchOnFirstUse is the #52 contract: nothing is launched at
// construction, a file type without a server never launches anything, and the
// first matching query starts one server that later queries reuse.
func TestLazyLaunchOnFirstUse(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
	writeFile(t, filepath.Join(dir, "a.go"), "package x\n")
	writeFile(t, filepath.Join(dir, "pyproject.toml"), "[project]\n")
	writeFile(t, filepath.Join(dir, "b.py"), "x = 1\n")

	f := newFakeLSP(t, echoResponder(nil))
	st := &starter{client: f.client}
	mgr := NewManager(Config{Lazy: true, IdleTimeout: time.Minute, Servers: DefaultServers()}, dir)
	mgr.start = st.start
	defer mgr.Close()

	ctx := context.Background()
	if st.count() != 0 {
		t.Fatalf("construction launched %d server(s), want 0", st.count())
	}

	// No server claims .zzz: an error, and still nothing launched.
	if _, _, err := mgr.ClientFor(ctx, filepath.Join(dir, "notes.zzz")); err == nil {
		t.Fatal("unknown extension should error")
	} else if !strings.Contains(err.Error(), "no language server for .zzz") {
		t.Fatalf("err = %v", err)
	}
	if st.count() != 0 {
		t.Fatalf("failed resolve launched a server (%d calls)", st.count())
	}

	c1, lang1, err := mgr.ClientFor(ctx, filepath.Join(dir, "a.go"))
	if err != nil {
		t.Fatalf("ClientFor(a.go): %v", err)
	}
	if st.count() != 1 {
		t.Fatalf("first use launched %d servers, want 1", st.count())
	}
	if lang1 != "go" {
		t.Fatalf("language id = %q, want go", lang1)
	}
	if _, _, err := mgr.ClientFor(ctx, filepath.Join(dir, "a.go")); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if st.count() != 1 {
		t.Fatalf("reusing the server relaunched it (%d calls)", st.count())
	}

	c2, _, err := mgr.ClientFor(ctx, filepath.Join(dir, "b.py"))
	if err != nil {
		t.Fatalf("ClientFor(b.py): %v", err)
	}
	if st.count() != 2 {
		t.Fatalf("a second language should start its own server, calls = %d", st.count())
	}
	if c1 != f.client || c2 != f.client {
		t.Fatal("test seam should hand back the fake client")
	}
}

func TestRootDetectionUsesMarkers(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
	writeFile(t, filepath.Join(dir, "sub", "pkg", "a.go"), "package pkg\n")

	f := newFakeLSP(t, echoResponder(nil))
	st := &starter{client: f.client}
	mgr := NewManager(Config{Lazy: true, IdleTimeout: time.Minute, Servers: DefaultServers()}, dir)
	mgr.start = st.start
	defer mgr.Close()

	if _, _, err := mgr.ClientFor(context.Background(), filepath.Join(dir, "sub", "pkg", "a.go")); err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if want := "go@" + dir; st.calls[0] != want {
		t.Fatalf("launch root = %q, want %q (go.mod marks the root)", st.calls[0], want)
	}
}

// TestMissingBinaryIsActionableError exercises the real launcher: an absent
// binary must produce an installation hint, not a panic or a hang.
func TestMissingBinaryIsActionableError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.go"), "package x\n")
	mgr := NewManager(Config{
		Lazy:        true,
		IdleTimeout: time.Minute,
		Servers:     map[string]ServerSpec{"go": {Command: "xdev-lsp-binary-that-does-not-exist", FileTypes: []string{"go"}}},
	}, dir)
	defer mgr.Close()

	_, _, err := mgr.ClientFor(context.Background(), filepath.Join(dir, "a.go"))
	if err == nil {
		t.Fatal("missing binary should error")
	}
	for _, want := range []string{"not found on PATH", "lsp.servers.go.command"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to contain %q", err, want)
		}
	}
}

func TestIdleShutdown(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
	writeFile(t, filepath.Join(dir, "a.go"), "package x\n")

	f := newFakeLSP(t, echoResponder(nil))
	st := &starter{client: f.client}
	mgr := NewManager(Config{Lazy: true, IdleTimeout: 80 * time.Millisecond, Servers: DefaultServers()}, dir)
	mgr.start = st.start
	defer mgr.Close()

	if _, _, err := mgr.ClientFor(context.Background(), filepath.Join(dir, "a.go")); err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	waitFor(t, "idle shutdown", func() bool {
		mgr.mu.Lock()
		running := len(mgr.servers)
		mgr.mu.Unlock()
		select {
		case <-f.client.Done():
			return running == 0
		default:
			return false
		}
	})
}

// TestConfigFromSettingsLayersUserOverDefaults covers the merge rules: an
// override keeps the default file types, a new language is added, and the
// rootPatterns alias feeds rootMarkers.
func TestConfigFromSettingsLayersUserOverDefaults(t *testing.T) {
	lazy := false
	s := &config.Settings{LSP: &config.LSPConfig{
		Lazy:        &lazy,
		IdleTimeout: "90s",
		Servers: map[string]config.LSPServer{
			"go":  {Command: "/opt/custom/gopls"},
			"lua": {Command: "lua-language-server", FileTypes: []string{"lua"}, RootPatterns: []string{".luarc.json"}},
		},
	}}
	cfg := ConfigFromSettings(s)
	if cfg.Lazy {
		t.Fatal("lazy should follow the settings")
	}
	if cfg.IdleTimeout != 90*time.Second {
		t.Fatalf("idle timeout = %s, want 90s", cfg.IdleTimeout)
	}
	goSpec := cfg.Servers["go"]
	if goSpec.Command != "/opt/custom/gopls" {
		t.Fatalf("go command = %q", goSpec.Command)
	}
	if len(goSpec.FileTypes) != 1 || goSpec.FileTypes[0] != "go" {
		t.Fatalf("go file types = %v, want the built-in ones kept", goSpec.FileTypes)
	}
	if len(goSpec.RootMarkers) == 0 {
		t.Fatal("go root markers should keep the built-in defaults")
	}
	lua := cfg.Servers["lua"]
	if lua.Command != "lua-language-server" || len(lua.RootMarkers) != 1 || lua.RootMarkers[0] != ".luarc.json" {
		t.Fatalf("lua spec = %+v (rootPatterns should alias rootMarkers)", lua)
	}
}

// TestSettingsYAMLDecodesLSPBlock proves the new keys survive the strict
// layered loader (a typo'd key is rejected, so the schema has to match).
func TestSettingsYAMLDecodesLSPBlock(t *testing.T) {
	dir := t.TempDir()
	overlay := filepath.Join(dir, "overlay.yml")
	writeFile(t, overlay, `lsp:
  lazy: false
  idleTimeout: 2m
  servers:
    go:
      command: /usr/local/bin/gopls
      rootMarkers: [go.mod]
`)
	s, err := config.LoadSettings(dir, []string{overlay})
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if s.LSP == nil || s.LSP.Servers["go"].Command != "/usr/local/bin/gopls" {
		t.Fatalf("lsp block not decoded: %+v", s.LSP)
	}
	cfg := ConfigFromSettings(s)
	if cfg.Lazy || cfg.IdleTimeout != 2*time.Minute {
		t.Fatalf("cfg = %+v", cfg)
	}
	if got := cfg.Servers["go"].RootMarkers; len(got) != 1 || got[0] != "go.mod" {
		t.Fatalf("root markers = %v", got)
	}

	// An unknown key inside lsp is a typo, and the loader must say so.
	bad := filepath.Join(dir, "bad.yml")
	writeFile(t, bad, "lsp:\n  servers:\n    go:\n      commnad: gopls\n")
	if _, err := config.LoadSettings(dir, []string{bad}); err == nil {
		t.Fatal("a typo'd lsp key should be rejected")
	}
}

// TestDeadServerIsRelaunched: a server that exited must not poison the
// session — the next call starts a fresh one.
func TestDeadServerIsRelaunched(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
	writeFile(t, filepath.Join(dir, "a.go"), "package x\n")

	f := newFakeLSP(t, echoResponder(nil))
	st := &starter{client: f.client}
	mgr := NewManager(Config{Lazy: true, IdleTimeout: time.Minute, Servers: DefaultServers()}, dir)
	mgr.start = st.start
	defer mgr.Close()

	first, _, err := mgr.ClientFor(context.Background(), filepath.Join(dir, "a.go"))
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	_ = first.Close() // the server exits
	waitFor(t, "client death", func() bool {
		select {
		case <-first.Done():
			return true
		default:
			return false
		}
	})

	f2 := newFakeLSP(t, echoResponder(nil))
	st.mu.Lock()
	st.client = f2.client
	st.mu.Unlock()
	second, _, err := mgr.ClientFor(context.Background(), filepath.Join(dir, "a.go"))
	if err != nil {
		t.Fatalf("ClientFor after death: %v", err)
	}
	if second == first {
		t.Fatal("a dead client was reused")
	}
	if st.count() != 2 {
		t.Fatalf("launches = %d, want 2", st.count())
	}
}

// TestIdleDropRearmsWhenTouched: a timer that fires while a call is in flight
// must re-arm instead of closing the client under it.
func TestIdleDropRearmsWhenTouched(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
	writeFile(t, filepath.Join(dir, "a.go"), "package x\n")

	f := newFakeLSP(t, echoResponder(nil))
	st := &starter{client: f.client}
	mgr := NewManager(Config{Lazy: true, IdleTimeout: time.Minute, Servers: DefaultServers()}, dir)
	mgr.start = st.start
	defer mgr.Close()

	if _, _, err := mgr.ClientFor(context.Background(), filepath.Join(dir, "a.go")); err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	mgr.idleDrop("go\x00" + dir) // the stale timer firing right after the call

	mgr.mu.Lock()
	running := len(mgr.servers)
	mgr.mu.Unlock()
	if running != 1 {
		t.Fatalf("running servers = %d, want the touched one kept", running)
	}
	select {
	case <-f.client.Done():
		t.Fatal("client was closed although it was just used")
	default:
	}
}

// Concurrent first calls for the same (server, root) must ALL get the one
// launched client. The old shared-result channel handed the value to the
// first waiter and the zero value to everyone else — a nil *startResult the
// caller dereferenced, panicking the tool goroutine. That is exactly what
// happened when two lsp calls raced the first gopls launch.
func TestConcurrentFirstLaunchSharesResult(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
	writeFile(t, filepath.Join(dir, "a.go"), "package x\n")

	f := newFakeLSP(t, echoResponder(nil))
	// The seam holds every waiter inside the launch, so the race is forced
	// rather than incidental.
	gate := make(chan struct{})
	calls := 0
	var mu sync.Mutex
	mgr := NewManager(Config{Lazy: true, IdleTimeout: time.Minute, Servers: DefaultServers()}, dir)
	mgr.start = func(context.Context, string, ServerSpec, string) (*Client, error) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			<-gate // hold the launch until every waiter has registered
		}
		return f.client, nil
	}
	defer mgr.Close()

	const waiters = 16
	var wg sync.WaitGroup
	starting := make(chan struct{})
	results := make([]*Client, waiters)
	errs := make([]error, waiters)
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-starting
			results[i], _, errs[i] = mgr.ClientFor(context.Background(), filepath.Join(dir, "a.go"))
		}(i)
	}
	close(starting)
	time.Sleep(100 * time.Millisecond) // let all waiters pile into the launch
	close(gate)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("waiter %d: %v", i, err)
		}
	}
	for i, c := range results {
		if c == nil {
			t.Fatalf("waiter %d got a nil client (the shared-launch result was lost)", i)
		}
	}
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 1 {
		t.Fatalf("launches = %d, want 1 (concurrent first calls must share)", n)
	}
}
