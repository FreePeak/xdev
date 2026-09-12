package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/tool"
)

// captureStderr runs f with os.Stderr redirected to a pipe and returns what
// was written (printHooks streams reasoning there).
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = old }()
	f()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// The reasoning display is the observable half of --print-thoughts /
// --hide-thinking: printHooks writes thinking deltas to stderr only when the
// resolved display state is on.
func TestThinkingDisplayFlagsDrivePrintOutput(t *testing.T) {
	delta := ai.Event{Type: ai.EventThinkingDelta, Delta: "weighing options"}
	off := false
	settings := &config.Settings{ShowThinking: &off}

	emit := func() string {
		return captureStderr(t, func() {
			(&printHooks{showThinking: showThinkingOn(settings)}).OnEvent(delta)
		})
	}

	withLaunch(t, launchFlags{})
	if out := emit(); out != "" {
		t.Fatalf("showThinking off must stay silent, got %q", out)
	}
	withLaunch(t, launchFlags{ThinkingDisplay: &off})
	if out := emit(); out != "" {
		t.Fatalf("--hide-thinking must stay silent, got %q", out)
	}
	on := true
	withLaunch(t, launchFlags{ThinkingDisplay: &on})
	if out := emit(); !strings.Contains(out, "weighing options") {
		t.Fatalf("--print-thoughts must stream reasoning to stderr, got %q", out)
	}
}

// --- --advisor -----------------------------------------------------------

// --advisor must build the reviewer even with settings.advisor off (the TUI
// is the mode that attaches it; the flag is its switch there).
func TestAdvisorFlagForcesTheReviewer(t *testing.T) {
	cfg := &config.Config{Providers: map[string]*config.ProviderConfig{
		"adv-p": {
			API:     ai.APIAnthropicMessages,
			BaseURL: "http://127.0.0.1:9",
			Auth:    "none",
			Models:  []config.ModelConfig{{ID: "m"}},
		},
	}}
	settings := &config.Settings{ModelRoles: map[string]string{"advisor": "adv-p/m"}}

	withLaunch(t, launchFlags{})
	if adv := buildAdvisor(cfg, settings); adv != nil {
		t.Fatal("advisor must stay off while settings.advisor is off")
	}
	withLaunch(t, launchFlags{Advisor: true})
	if adv := buildAdvisor(cfg, settings); adv == nil {
		t.Fatal("--advisor must force the reviewer on")
	}
}

// withLaunch installs launch flags for one test and restores the previous
// value (the seams are process-wide by design).
func withLaunch(t *testing.T, f launchFlags) {
	t.Helper()
	prev := launch
	launch = f
	t.Cleanup(func() { launch = prev })
}

// sandbox points the install data dir at a temp dir so session storage and
// extension discovery stay inside the test.
func sandbox(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dir)
	return dir
}

func assistantEntry() *session.MessageEntry {
	return &session.MessageEntry{Message: ai.Message{
		Role:    ai.RoleAssistant,
		Content: []ai.Block{ai.TextBlock{Text: "hi"}},
	}}
}

func TestApplyThinkingFlag(t *testing.T) {
	for _, tc := range []struct {
		flag, role, want string
		wantErr          bool
	}{
		{"", "medium", "medium", false},       // no flag: the role's effort stands
		{"auto", "medium", "medium", false},   // auto: provider default = resolved effort
		{"off", "medium", "", false},          // off: nothing requested
		{"minimal", "high", "minimal", false}, // explicit rung overrides the role
		{"low", "", "low", false},
		{"medium", "", "medium", false},
		{"high", "", "high", false},
		{"xhigh", "", "high", false}, // clamped: the ladder tops out at high
		{"max", "", "high", false},
		{"bogus", "", "", true},
	} {
		got, err := applyThinkingFlag(tc.flag, tc.role)
		if tc.wantErr {
			if err == nil {
				t.Errorf("applyThinkingFlag(%q) must fail", tc.flag)
			}
			continue
		}
		if err != nil {
			t.Errorf("applyThinkingFlag(%q): %v", tc.flag, err)
			continue
		}
		if got != tc.want {
			t.Errorf("applyThinkingFlag(%q, role=%q) = %q, want %q", tc.flag, tc.role, got, tc.want)
		}
	}
}

func TestResolveThinkingDisplay(t *testing.T) {
	if got := resolveThinkingDisplay(false, false); got != nil {
		t.Errorf("no flag must follow settings, got %v", *got)
	}
	if got := resolveThinkingDisplay(true, false); got == nil || *got {
		t.Error("--hide-thinking must force the display off")
	}
	if got := resolveThinkingDisplay(false, true); got == nil || !*got {
		t.Error("--print-thoughts must force the display on")
	}
	if got := resolveThinkingDisplay(true, true); got == nil || *got {
		t.Error("--hide-thinking wins when both are passed")
	}
}

func TestParseMaxTime(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"", 0, false},
		{"600", 600 * time.Second, false}, // bare number = seconds
		{"10m", 10 * time.Minute, false},
		{"1h30m", 90 * time.Minute, false},
		{"0", 0, true},
		{"-5", 0, true},
		{"soon", 0, true},
	} {
		got, err := parseMaxTime(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseMaxTime(%q) must fail", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseMaxTime(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseMaxTime(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestWithMaxTimeDeadlineExpires(t *testing.T) {
	ctx, cancel := withMaxTime(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("--max-time must install a deadline")
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("deadline never fired")
	}
	if ctx.Err() != context.DeadlineExceeded {
		t.Fatalf("ctx.Err() = %v, want DeadlineExceeded", ctx.Err())
	}
	// Uncapped: no deadline, still cancellable (the RPC turn path).
	ctx2, cancel2 := boundedCtx(context.Background(), 0)
	if _, ok := ctx2.Deadline(); ok {
		t.Fatal("max-time 0 must not install a deadline")
	}
	cancel2()
	if ctx2.Err() == nil {
		t.Fatal("boundedCtx cancel must release the turn")
	}
}

// --- --tools / --no-tools / --no-lsp -------------------------------------

func TestRegistryHonorsToolFlags(t *testing.T) {
	cwd := t.TempDir()

	withLaunch(t, launchFlags{})
	all := newToolRegistry(cwd, nil, "p", "m", nil, nil, nil)
	names := all.Names()
	if !contains(names, "lsp") || !contains(names, "bash") {
		t.Fatalf("baseline registry missing tools: %v", names)
	}

	withLaunch(t, launchFlags{NoLSP: true})
	if got := newToolRegistry(cwd, nil, "p", "m", nil, nil, nil).Names(); contains(got, "lsp") {
		t.Fatalf("--no-lsp must drop the lsp tool, got %v", got)
	}

	withLaunch(t, launchFlags{Tools: []string{"read", "bash"}})
	got := newToolRegistry(cwd, nil, "p", "m", nil, nil, nil).Names()
	if strings.Join(got, ",") != "bash,read" {
		t.Fatalf("--tools allowlist must leave exactly its names, got %v", got)
	}

	withLaunch(t, launchFlags{NoTools: true})
	if got := newToolRegistry(cwd, nil, "p", "m", nil, nil, nil).Names(); len(got) != 0 {
		t.Fatalf("--no-tools must leave no built-in tools, got %v", got)
	}
}

func TestApplyToolFilterReportsUnknownNames(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(tool.NewReadTool())
	reg.Register(&tool.GrepTool{CWD: "/tmp"})

	unknown := applyToolFilter(reg, []string{"read", "raed"}, false)
	if strings.Join(unknown, ",") != "raed" {
		t.Fatalf("unknown = %v, want [raed]", unknown)
	}
	if got := reg.Names(); strings.Join(got, ",") != "read" {
		t.Fatalf("registry = %v, want [read]", got)
	}
	// A deferred tool is filtered with the same call, catalog included.
	reg2 := tool.NewRegistry()
	reg2.Register(tool.NewReadTool())
	reg2.Register(&tool.CheckpointTool{})
	reg2.Defer(tool.CheckpointToolName, "bookmark the tree")
	applyToolFilter(reg2, nil, true)
	if len(reg2.Names()) != 0 || len(reg2.Deferred()) != 0 {
		t.Fatalf("--no-tools left %v / %v", reg2.Names(), reg2.Deferred())
	}
}

// --- --no-session / --session-dir / --no-title ---------------------------

func TestNoSessionLeavesNothingOnDisk(t *testing.T) {
	dataDir := sandbox(t)
	withLaunch(t, launchFlags{NoSession: true})

	store, err := openSession(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(assistantEntry()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if store.Path() != "" {
		t.Fatalf("--no-session must not materialize a file, got %s", store.Path())
	}
	if n := countSessionFiles(t, filepath.Join(dataDir, "sessions")); n != 0 {
		t.Fatalf("--no-session wrote %d session file(s)", n)
	}
}

func TestSessionDirOverrideStoresAndFinds(t *testing.T) {
	sandbox(t)
	override := filepath.Join(t.TempDir(), "sessions-override")
	cwd := t.TempDir()
	withLaunch(t, launchFlags{SessionDir: override})

	store, err := openSession(cwd, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(assistantEntry()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(store.Path(), override) {
		t.Fatalf("session landed outside --session-dir: %s", store.Path())
	}
	// Lookup honors the same root: --continue finds it.
	again, err := openSession(cwd, true, "")
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.ID() != store.ID() {
		t.Fatalf("--continue under --session-dir resumed %s, want %s", again.ID(), store.ID())
	}
}

func TestNoTitleSkipsTheStamp(t *testing.T) {
	sandbox(t)
	withLaunch(t, launchFlags{NoTitle: true})
	store, err := openSession(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if store.Title() != "" {
		t.Fatalf("--no-title must leave the title empty, got %q", store.Title())
	}

	withLaunch(t, launchFlags{})
	stamped, err := openSession(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer stamped.Close()
	if !strings.HasPrefix(stamped.Title(), "print ") {
		t.Fatalf("default title stamp missing: %q", stamped.Title())
	}
}

// --- --auto-approve ------------------------------------------------------

func TestAutoApproveForcesYoloButKeepsWrittenRules(t *testing.T) {
	pol := tool.ApprovalPolicy{
		Mode:         tool.AlwaysAsk,
		PerTool:      map[string]tool.Action{"bash": tool.ActionDeny},
		BashPatterns: []tool.PolicyRule{{Pattern: "rm -rf /*", Action: tool.ActionDeny}},
	}
	withLaunch(t, launchFlags{})
	if got := autoApprovePolicy(pol); got.Mode != tool.AlwaysAsk {
		t.Fatalf("no flag must keep the configured mode, got %v", got.Mode)
	}
	withLaunch(t, launchFlags{AutoApprove: true})
	got := autoApprovePolicy(pol)
	if got.Mode != tool.Yolo {
		t.Fatalf("--auto-approve mode = %v, want Yolo", got.Mode)
	}
	if got.PerTool["bash"] != tool.ActionDeny || len(got.BashPatterns) != 1 {
		t.Fatal("--auto-approve must not erase explicit deny rules")
	}
}

// --- --skills / --no-skills ---------------------------------------------

func TestSkillFlagsFilterTheAdvertisedSet(t *testing.T) {
	proj := t.TempDir()
	for name, desc := range map[string]string{
		"git-commit": "commit helper",
		"docker":     "container helper",
	} {
		dir := filepath.Join(proj, ".xdev", "skills", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "---\ndescription: " + desc + "\n---\nbody"
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	withLaunch(t, launchFlags{})
	if block := skillPromptBlock(proj); !strings.Contains(block, "docker: container helper") {
		t.Fatalf("baseline must advertise every skill: %q", block)
	}

	withLaunch(t, launchFlags{Skills: []string{"git-*"}})
	block := skillPromptBlock(proj)
	if !strings.Contains(block, "git-commit: commit helper") || strings.Contains(block, "docker") {
		t.Fatalf("--skills git-* must keep only the git skill: %q", block)
	}

	withLaunch(t, launchFlags{NoSkills: true})
	if block := skillPromptBlock(proj); block != "" {
		t.Fatalf("--no-skills must advertise nothing: %q", block)
	}
}

// --- --no-extensions ----------------------------------------------------

var (
	extFixtureOnce sync.Once
	extFixturePath string
	extFixtureErr  error
)

// extFixture builds the scriptable extension fixture (internal/ext/testdata)
// once and returns the binary path, so the discovery path is exercised for
// real instead of asserting that a gate function returns false.
func extFixture(t *testing.T) string {
	t.Helper()
	extFixtureOnce.Do(func() {
		dir, err := os.MkdirTemp("", "xdev-ext-fixture")
		if err != nil {
			extFixtureErr = err
			return
		}
		bin := filepath.Join(dir, "policy")
		if out, err := exec.Command("go", "build", "-o", bin, "../../internal/ext/testdata/extfixture").CombinedOutput(); err != nil {
			extFixtureErr = fmt.Errorf("%v: %s", err, out)
			return
		}
		extFixturePath = bin
	})
	if extFixtureErr != nil {
		t.Skipf("cannot build the extension fixture: %v", extFixtureErr)
	}
	return extFixturePath
}

func TestNoExtensionsSkipsDiscovery(t *testing.T) {
	dataDir := sandbox(t)
	raw, err := os.ReadFile(extFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "extensions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "extensions", "policy"), raw, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EXT_MODE", "ok")

	withLaunch(t, launchFlags{})
	reg := tool.NewRegistry()
	mgr := attachExtensions(context.Background(), reg, func(string) {}, func(string) {}, &config.Config{})
	if mgr == nil {
		t.Fatal("baseline: the fixture extension must load")
	}
	defer mgr.Close()
	if !hasToolContaining(reg.Names(), "greet") {
		t.Fatalf("baseline: extension tool missing from %v", reg.Names())
	}

	withLaunch(t, launchFlags{NoExtensions: true})
	reg2 := tool.NewRegistry()
	if mgr2 := attachExtensions(context.Background(), reg2, func(string) {}, func(string) {}, &config.Config{}); mgr2 != nil {
		mgr2.Close()
		t.Fatal("--no-extensions must not load any extension")
	}
	if len(reg2.Names()) != 0 {
		t.Fatalf("--no-extensions registered tools: %v", reg2.Names())
	}
}

// --- --models -----------------------------------------------------------

func TestPrintModelCatalogListsProvidersModelsAndRoles(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]*config.ProviderConfig{
			"cat-p": {
				API:     "openai-completions",
				BaseURL: "http://127.0.0.1:9/v1",
				Models: []config.ModelConfig{
					{ID: "small", ContextWindow: 32000},
					{ID: "big", ContextWindow: 200000, Reasoning: true},
				},
			},
		},
	}
	settings := &config.Settings{
		DefaultModel:     "cat-p/big",
		ModelRoles:       map[string]string{"smol": "cat-p/small"},
		ModelRolesEffort: map[string]string{"smol": "low"},
	}
	var buf bytes.Buffer
	printModelCatalog(&buf, cfg, settings)
	out := buf.String()
	for _, want := range []string{
		"default: cat-p/big",
		"cat-p (openai-completions, http://127.0.0.1:9/v1)",
		"small  ctx=32000",
		"big  ctx=200000 reasoning",
		"smol -> cat-p/small:low",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("catalog output missing %q:\n%s", want, out)
		}
	}
}

// --- --cwd --------------------------------------------------------------

func TestChdirToChangesResolutionRoot(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(t.TempDir())
	if err := chdirTo(dir); err != nil {
		t.Fatal(err)
	}
	got, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	want := dir
	if real, rerr := filepath.EvalSymlinks(dir); rerr == nil {
		want = real
	}
	if got != want {
		t.Fatalf("cwd = %s, want %s", got, want)
	}

	if err := chdirTo(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("--cwd must reject a missing directory")
	}
}

func countSessionFiles(t *testing.T, root string) int {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(root, "*", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return len(files)
}

func contains(list []string, name string) bool {
	for _, n := range list {
		if n == name {
			return true
		}
	}
	return false
}

func hasToolContaining(list []string, sub string) bool {
	for _, n := range list {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}
