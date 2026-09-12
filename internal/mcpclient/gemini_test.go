package mcpclient

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"
)

// TestDiscoverExtensions pins the manifest scan: one level deep, name from
// the manifest or the directory, string-or-list command/skill entries
// resolved against the extension dir, and a malformed manifest skipped
// rather than fatal.
func TestDiscoverExtensions(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "extensions")
	writeFile(t, filepath.Join(root, "alpha", "gemini-extension.json"), `{
  "name": "alpha-ext",
  "version": "1.2.3",
  "mcpServers": {"echo": {"command": "/bin/echo", "args": ["hi"], "cwd": "/tmp"}},
  "commands": "cmds",
  "skills": ["skills-a", "skills-b"]
}`)
	// No name: the directory names the extension. A bare string skill entry.
	writeFile(t, filepath.Join(root, "beta", "gemini-extension.json"), `{"skills": "s"}`)
	// Truncated JSON is warned and skipped.
	writeFile(t, filepath.Join(root, "broken", "gemini-extension.json"), `{"name": "x",`)
	// A directory without a manifest, and a stray file: both skipped.
	if err := os.MkdirAll(filepath.Join(root, "noManifest"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "loose.txt"), "x")

	exts := DiscoverExtensions(dir)
	if len(exts) != 2 {
		t.Fatalf("extensions = %+v, want alpha and beta only", exts)
	}
	if exts[0].Name != "alpha-ext" || exts[1].Name != "beta" {
		t.Fatalf("names = %q, %q", exts[0].Name, exts[1].Name)
	}
	alpha := exts[0]
	if alpha.Version != "1.2.3" {
		t.Errorf("version = %q", alpha.Version)
	}
	if alpha.Dir != filepath.Join(root, "alpha") {
		t.Errorf("dir = %q", alpha.Dir)
	}
	if sc := alpha.MCPServers["echo"]; sc == nil || sc.Command != "/bin/echo" || sc.Cwd != "/tmp" {
		t.Errorf("mcpServers = %+v", alpha.MCPServers["echo"])
	}
	if want := []string{filepath.Join(root, "alpha", "cmds")}; !slices.Equal(alpha.Commands, want) {
		t.Errorf("commands = %v, want %v", alpha.Commands, want)
	}
	wantSkills := []string{filepath.Join(root, "alpha", "skills-a"), filepath.Join(root, "alpha", "skills-b")}
	if !slices.Equal(alpha.Skills, wantSkills) {
		t.Errorf("skills = %v, want %v", alpha.Skills, wantSkills)
	}
	if want := []string{filepath.Join(root, "beta", "s")}; !slices.Equal(exts[1].Skills, want) {
		t.Errorf("beta skills = %v, want %v", exts[1].Skills, want)
	}
	// A missing root is not an error.
	if got := DiscoverExtensions(filepath.Join(dir, "nope")); got != nil {
		t.Fatalf("missing root = %+v, want nil", got)
	}
}

// TestExtensionServersMergeLowestPriority: a manifest's servers join the
// config only where mcp.yml is silent, and its directories surface as the
// lowest-priority discovery roots.
func TestExtensionServersMergeLowestPriority(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "agent")
	writeFile(t, filepath.Join(extRoot, "extensions", "e1", "gemini-extension.json"), `{
  "mcpServers": {
    "shared": {"command": "/from-ext"},
    "extOnly": {"command": "/ext-only"}
  },
  "skills": "sk"
}`)
	base := writeFile(t, filepath.Join(dir, "mcp.yml"), "servers:\n  shared: {command: /from-mcpyml}\n")

	cfg, err := LoadConfigIn(base, extRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Servers["shared"].Command; got != "/from-mcpyml" {
		t.Errorf("shared command = %q, want the explicit mcp.yml entry", got)
	}
	if got := cfg.Servers["extOnly"].Command; got != "/ext-only" {
		t.Errorf("extOnly command = %q", got)
	}
	if got := cfg.Servers["extOnly"].Source; got != "gemini:e1" {
		t.Errorf("extOnly source = %q, want gemini:e1", got)
	}
	if len(cfg.SkillDirs) != 1 || !slices.Equal(cfg.SkillDirs, []string{filepath.Join(extRoot, "extensions", "e1", "sk")}) {
		t.Errorf("skill dirs = %v", cfg.SkillDirs)
	}
	if len(cfg.CommandDirs) != 0 {
		t.Errorf("command dirs = %v, want none", cfg.CommandDirs)
	}
	// The seam returns the same directories without loading a config.
	commands, skills := ExtensionRoots(extRoot)
	if len(commands) != 0 || !slices.Equal(skills, cfg.SkillDirs) {
		t.Errorf("ExtensionRoots = %v / %v", commands, skills)
	}
}

// TestExtensionServersReachClient is the end-to-end proof that a merged
// manifest server actually reaches the client builder: the fixture server
// is declared ONLY in the manifest, connects, lists its tool, and answers a
// call.
func TestExtensionServersReachClient(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	bin := buildFixtureServer(t)
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "agent")
	writeFile(t, filepath.Join(extRoot, "extensions", "demo", "gemini-extension.json"),
		`{"name":"demo","mcpServers":{"echo":{"command":`+strconv.Quote(bin)+`}}}`)
	base := writeFile(t, filepath.Join(dir, "mcp.yml"), "servers: {}\n")

	cfg, err := LoadConfigIn(base, extRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Servers["echo"]; !ok {
		t.Fatalf("servers = %v, want the manifest's echo", cfg.Servers)
	}

	mgr := NewManager()
	defer mgr.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	connected, errs := mgr.Connect(ctx, cfg)
	if connected != 1 {
		t.Fatalf("connected = %d (errs=%v), want 1", connected, errs)
	}
	tools := mgr.Tools()
	if len(tools) != 1 || tools[0].Name() != "echo_echo" {
		t.Fatalf("tools = %v", tools)
	}
	res, err := tools[0].Execute(ctx, json.RawMessage(`{"text":"from-manifest"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("res = %+v", res)
	}
}
