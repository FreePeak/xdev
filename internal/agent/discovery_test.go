package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/tool"
)

func writeAgentFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fileDefs drops the bundled defaults so a test can assert on what the
// temp cwd contributed: since #272 a stock discovery is never empty, and
// every "exactly one agent" assertion would otherwise read the bundled set.
func fileDefs(defs []AgentDefinition) []AgentDefinition {
	var out []AgentDefinition
	for _, d := range defs {
		if !strings.HasPrefix(d.path, "bundled/") {
			out = append(out, d)
		}
	}
	return out
}

func TestDiscoverAgentsParsesFrontmatter(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, filepath.Join(dir, ".xdev", "agents"), "scout.md", `---
name: scout
description: read-only research agent
tools: read,grep,glob
model: onegw/free
---
You are a read-only scout. Do not edit files.`)

	defs, warnings := DiscoverAgents(dir)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	file := fileDefs(defs)
	if len(file) != 1 {
		t.Fatalf("defs = %d", len(file))
	}
	d := file[0]
	if d.Name != "scout" || d.Description != "read-only research agent" {
		t.Fatalf("frontmatter not parsed: %+v", d)
	}
	if !strings.Contains(d.SystemPrompt, "read-only scout") {
		t.Fatalf("system prompt = %q", d.SystemPrompt)
	}
	// yield is auto-added.
	var hasYield bool
	for _, tool := range d.Tools {
		if tool == "yield" {
			hasYield = true
		}
	}
	if !hasYield {
		t.Errorf("yield not auto-added to tools: %v", d.Tools)
	}
	if d.Model != "onegw/free" {
		t.Errorf("model = %q", d.Model)
	}
}

func TestDiscoverAgentsFileWithoutNameDerivesName(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, filepath.Join(dir, ".xdev", "agents"), "reviewer.md", "---\ndescription: code reviewer\n---\nReview this code.")
	defs, _ := DiscoverAgents(dir)
	file := fileDefs(defs)
	if len(file) != 1 || file[0].Name != "reviewer" {
		t.Fatalf("name should derive from filename: %+v", file)
	}
	// The bundled set still ships alongside: a project file never removes it.
	if len(defs) <= len(file) {
		t.Fatalf("bundled defaults missing from discovery: %+v", defs)
	}
}

func TestDiscoverAgentsBadFileSkippedWithWarning(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, filepath.Join(dir, ".xdev", "agents"), "good.md", "---\nname: good\ndescription: works\n---\nbody")
	writeAgentFile(t, filepath.Join(dir, ".xdev", "agents"), "bad.md", "---\nname: bad\n---\nno description")
	defs, warnings := DiscoverAgents(dir)
	if file := fileDefs(defs); len(file) != 1 || file[0].Name != "good" {
		t.Fatalf("good agent lost: %+v", file)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "bad") {
		t.Fatalf("bad file warning missing: %v", warnings)
	}
}

func TestDiscoverAgentsUnterminatedFrontmatter(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, filepath.Join(dir, ".xdev", "agents"), "broken.md", "---\nname: broken\ndescription: x\nno close")
	_, warnings := DiscoverAgents(dir)
	if len(warnings) == 0 {
		t.Fatal("unterminated frontmatter must warn")
	}
}

func TestDiscoverAgentsFirstWinsAcrossRoots(t *testing.T) {
	proj := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	userRoot := filepath.Join(home, ".xdev", "agent", "agents")

	writeAgentFile(t, filepath.Join(proj, ".xdev", "agents"), "dup.md", "---\nname: dup\ndescription: project version\n---\np")
	writeAgentFile(t, userRoot, "dup.md", "---\nname: dup\ndescription: user version\n---\nu")

	defs, _ := DiscoverAgents(proj)
	if file := fileDefs(defs); len(file) != 1 || file[0].Description != "project version" {
		t.Fatalf("project root should win: %+v", file)
	}
}

func TestSpawnPolicyResolution(t *testing.T) {
	tests := []struct {
		name   string
		spawns any
		want   SpawnPolicy
	}{
		{"unrestricted", "*", SpawnPolicy{AllowAll: true}},
		{"absent = unrestricted", nil, SpawnPolicy{AllowAll: true}},
		{"false = none", false, SpawnPolicy{None: true}},
		{"csv", "a, b", SpawnPolicy{Allow: []string{"a", "b"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := AgentDefinition{Spawns: tc.spawns}
			got := d.ResolveSpawnPolicy()
			if got.AllowAll != tc.want.AllowAll {
				t.Fatalf("AllowAll = %v, want %v", got.AllowAll, tc.want.AllowAll)
			}
			if strings.Join(got.Allow, "|") != strings.Join(tc.want.Allow, "|") {
				t.Fatalf("Allow = %v, want %v", got.Allow, tc.want.Allow)
			}
		})
	}
}

func TestSpawnPolicyYAMLList(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, filepath.Join(dir, ".xdev", "agents"), "spawner.md", "---\nname: spawner\ndescription: x\nspawns:\n  - scout\n  - reviewer\n---\nbody")
	defs, _ := DiscoverAgents(dir)
	file := fileDefs(defs)
	if len(file) != 1 {
		t.Fatalf("defs = %v", file)
	}
	pol := file[0].ResolveSpawnPolicy()
	if pol.AllowAll || len(pol.Allow) != 2 {
		t.Fatalf("YAML list spawns: %+v", pol)
	}
}

func TestFindAgent(t *testing.T) {
	defs := []AgentDefinition{{Name: "scout"}, {Name: "reviewer"}}
	if d, ok := FindAgent(defs, "reviewer"); !ok || d.Name != "reviewer" {
		t.Fatal("reviewer not found")
	}
	if _, ok := FindAgent(defs, "missing"); ok {
		t.Fatal("missing agent reported as found")
	}
}

// TestTaskToolDepthGuard pins the recursion limit: a task tool at the
// depth cap refuses to spawn further children.
func TestTaskToolDepthGuard(t *testing.T) {
	tt := &TaskTool{
		Provider: &fakeProvider{},
		Depth:    2,
	}
	res, err := tt.Execute(context.Background(), json.RawMessage(`{"prompt":"go"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "depth limit") {
		t.Fatalf("depth cap must refuse: %q", res.Text)
	}
}

// TestTaskToolNamedAgentResolution pins the full flow: named agent
// resolves from the discovered set, uses its system prompt, and rejects
// unknown agents with the available names listed.
func TestTaskToolNamedAgentResolution(t *testing.T) {
	agentDefs := []AgentDefinition{
		{Name: "scout", Description: "read-only scout", SystemPrompt: "You are a scout.", Tools: stringList{"read", "yield"}},
	}
	p := &fakeProvider{calls: []fakeScript{
		{events: yieldEvents(`{"result":"scouted"}`)},
	}}
	reg := tool.NewRegistry()
	for _, t2 := range []tool.Tool{echoTool{}} {
		reg.Register(t2)
	}
	tt := &TaskTool{
		Provider: p, Model: "m", Agents: agentDefs,
		ChildTools: []tool.Tool{echoTool{}},
		MaxTurns:   5,
	}
	// Unknown agent surfaces the list.
	res, _ := tt.Execute(context.Background(), json.RawMessage(`{"prompt":"x","agent":"bogus"}`))
	if !res.IsError || !strings.Contains(res.Text, "scout") {
		t.Fatalf("unknown agent must list available: %q", res.Text)
	}
}

// TestTaskToolSpawnPolicyEnforced pins that an agent with a restricted
// spawn policy cannot spawn agents outside its allowlist.
func TestTaskToolSpawnPolicyEnforced(t *testing.T) {
	agentDefs := []AgentDefinition{
		{Name: "director", Description: "director", Spawns: "scout,reviewer"},
		{Name: "scout", Description: "scout"},
		{Name: "rogue", Description: "rogue agent"},
	}
	// The scout spawn succeeds (provider script yields a result). The rogue
	// check must reject the request before the provider is ever consulted,
	// so one script is enough.
	provider := &fakeProvider{calls: []fakeScript{
		{events: yieldEvents(`{"result":"scouted"}`)},
	}}
	tt := &TaskTool{
		Provider: provider, Model: "m", Agents: agentDefs,
		AgentName: "director", // the director is spawning
		Depth:     0,
	}
	// director can spawn scout.
	res, err := tt.Execute(context.Background(), json.RawMessage(`{"prompt":"x","agent":"scout"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("director should spawn scout: %q", res.Text)
	}
	// director cannot spawn rogue — rejected before provider sees it.
	res, _ = tt.Execute(context.Background(), json.RawMessage(`{"prompt":"x","agent":"rogue"}`))
	if !res.IsError || !strings.Contains(res.Text, "not allowed") {
		t.Fatalf("spawn policy must block rogue: %q", res.Text)
	}
}

// --- Execute-level spawn policy matrix (research §1 guards) ---

func matrixAgents() []AgentDefinition {
	return []AgentDefinition{
		{Name: "root-open", Description: "d", Spawns: "*"},
		{Name: "root-csv", Description: "d", Spawns: "worker"},
		{Name: "root-none", Description: "d", Spawns: false},
		{Name: "worker", Description: "d", Tools: stringList{"read", "task"}},
		{Name: "leaf", Description: "d", Tools: stringList{"read"}},
	}
}

func newMatrixTool(parent string, depth int) *TaskTool {
	p := &fakeProvider{}
	return &TaskTool{
		Provider:   p,
		Model:      "m",
		Agents:     matrixAgents(),
		AgentName:  parent,
		Depth:      depth,
		ChildTools: []tool.Tool{&tool.ReadTool{}},
		MaxTurns:   1,
	}
}

func TestSpawnPolicyMatrix(t *testing.T) {
	tests := []struct {
		name    string
		parent  string
		child   string
		wantErr string // "" = allowed
	}{
		{"unrestricted", "root-open", "worker", ""},
		{"csv hit", "root-csv", "worker", ""},
		{"csv miss", "root-csv", "leaf", "not allowed"},
		{"empty spawns", "root-none", "worker", "not allowed"},
		{"no parent name = free", "", "worker", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tt := newMatrixTool(tc.parent, 0)
			res, err := tt.Execute(context.Background(),
				json.RawMessage(`{"prompt":"x","agent":"`+tc.child+`"}`))
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantErr == "" {
				// Allowed: the child ran (fake provider exhausts), so the
				// failure must NOT be the policy error.
				if strings.Contains(res.Text, "not allowed") {
					t.Fatalf("policy wrongly blocked: %q", res.Text)
				}
				return
			}
			if !res.IsError || !strings.Contains(res.Text, tc.wantErr) {
				t.Fatalf("want %q, got %q", tc.wantErr, res.Text)
			}
		})
	}
}

func TestDepthCapBlocksAtExecute(t *testing.T) {
	// A child at the cap (depth 2) cannot spawn, regardless of policy.
	tt := newMatrixTool("root-open", 2)
	res, _ := tt.Execute(context.Background(), json.RawMessage(`{"prompt":"x","agent":"worker"}`))
	if !res.IsError || !strings.Contains(res.Text, "depth limit") {
		t.Fatalf("cap must block: %q", res.Text)
	}
	// One below the cap is allowed to try (provider exhausted = not a
	// depth error).
	tt2 := newMatrixTool("root-open", 1)
	res2, _ := tt2.Execute(context.Background(), json.RawMessage(`{"prompt":"x","agent":"worker"}`))
	if strings.Contains(res2.Text, "depth limit") {
		t.Fatalf("depth 1 must be allowed to spawn: %q", res2.Text)
	}
}

func TestNamedAgentBelowCapGetsChildTaskTool(t *testing.T) {
	// worker declares tools: read,task. Spawning it at depth 0 must inject
	// a child TaskTool (research §1: recursion is live, not structural).
	// The child is scripted to call task, so the grandchild actually runs
	// and its yield result must surface in the root result — proving the
	// child really has a task tool.
	// Shared provider across worker and leaf. Stream orders: worker calls
	// task (0), the leaf runs and yields (1), then the worker yields (2).
	child := fakeProvider{calls: []fakeScript{{
		events: toolCallEvents("task", `{"prompt":"deep compute","agent":"leaf"}`),
	}, {
		events: yieldEvents(`{"result":"recursion-proven"}`),
	}, {
		events: yieldEvents(`{"result":"worker-done"}`),
	}}}
	tt := &TaskTool{
		Provider: &child, Model: "m",
		Agents:     matrixAgents(),
		ChildTools: []tool.Tool{&tool.ReadTool{}},
		MaxTurns:   2,
	}
	res, err := tt.Execute(context.Background(), json.RawMessage(`{"prompt":"spawn","agent":"worker"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("worker recursion failed: %q", res.Text)
	}
	// The root result must be the WORKER's own yield (calls[2]), which is
	// only reachable if the leaf consumed calls[1] — i.e. the child ran a
	// real task tool. Without recursion the worker would yield calls[1]
	// ("recursion-proven"), not calls[2] ("worker-done").
	if !strings.Contains(res.Text, "worker-done") {
		t.Fatalf("child did not run a task tool (worker yield script not consumed): %q", res.Text)
	}
}

// --- #272: the bundled set is the floor of discovery -------------------

// TestBundledAgentsDiscoverOnAStockCheckout is the issue's headline: a fresh
// install with no agent files must not answer `available: none`.
func TestBundledAgentsDiscoverOnAStockCheckout(t *testing.T) {
	defs, warnings := DiscoverAgents(t.TempDir())
	if len(warnings) != 0 {
		t.Fatalf("bundled definitions must parse clean: %v", warnings)
	}
	have := map[string]AgentDefinition{}
	for _, d := range defs {
		have[d.Name] = d
	}
	for _, name := range []string{"scout", "reviewer", "security-reviewer", "sonic", "task"} {
		d, ok := have[name]
		if !ok {
			t.Fatalf("stock discovery lost bundled %q: %v", name, agentNames(defs))
		}
		if d.Description == "" || d.SystemPrompt == "" {
			t.Fatalf("bundled %q is a stub: %+v", name, d)
		}
	}
}

// A bundled definition is usable, not decorative: the frontmatter it declares
// must be the shape the task tool consumes.
func TestBundledAgentsDeclareConsumableFrontmatter(t *testing.T) {
	defs, _ := discoverBundledAgents()
	for _, d := range defs {
		if d.Model != "" && !strings.Contains(d.Model, "/") {
			t.Errorf("%s: model %q is not a provider/model ref", d.Name, d.Model)
		}
		if d.ThinkingLevel != "" && !slices.Contains(config.EffortLevels, d.ThinkingLevel) {
			t.Errorf("%s: thinkingLevel %q not an effort level", d.Name, d.ThinkingLevel)
		}
		if len(d.Unsupported) > 0 {
			t.Errorf("%s: unsupported keys %v", d.Name, d.Unsupported)
		}
		if !slices.Contains(d.Tools, "yield") {
			t.Errorf("%s: yield missing from tools %v", d.Name, d.Tools)
		}
	}
}

func TestBundledAgentsAreOverriddenByName(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, filepath.Join(dir, ".xdev", "agents"), "scout.md", "---\nname: scout\ndescription: mine\n---\nmine")
	defs, _ := DiscoverAgents(dir)
	scout, ok := FindAgent(defs, "scout")
	if !ok {
		t.Fatal("scout vanished")
	}
	if scout.Description != "mine" || !strings.HasPrefix(scout.path, dir) {
		t.Fatalf("project file must beat the bundled def: %+v", scout)
	}
	// Its siblings stay: overriding one name never removes the set.
	if _, ok := FindAgent(defs, "reviewer"); !ok {
		t.Fatal("reviewer lost when scout was overridden")
	}
}

// A stale key must be named, not swallowed: `output:` advertised a config
// surface that did nothing.
func TestDiscoverAgentsReportsUnsupportedKeys(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, filepath.Join(dir, ".xdev", "agents"), "stale.md", "---\nname: stale\ndescription: x\noutput: json\nreadSummarize: true\n---\nbody")
	defs, warnings := DiscoverAgents(dir)
	if len(warnings) != 2 {
		t.Fatalf("one warning per stale key: %v", warnings)
	}
	if !strings.Contains(warnings[0]+warnings[1], "output") || !strings.Contains(warnings[0]+warnings[1], "readSummarize") {
		t.Fatalf("warnings must name the keys: %v", warnings)
	}
	// The agent still loads: a stale key never costs the definition.
	if _, ok := FindAgent(fileDefs(defs), "stale"); !ok {
		t.Fatal("stale key dropped the agent")
	}
}

// The old form `model: @role` is a reserved YAML indicator and is still what
// settled agent files carry; discovery promised it, so it must parse (the
// alias is then reported at spawn, never expanded).
func TestDiscoverAgentsBareRoleModelParses(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, filepath.Join(dir, ".xdev", "agents"), "roled.md", "---\nname: roled\ndescription: x\nmodel: @smol\n---\nbody")
	defs, warnings := DiscoverAgents(dir)
	if len(warnings) != 0 {
		t.Fatalf("bare @role must not be a syntax error: %v", warnings)
	}
	d, _ := FindAgent(fileDefs(defs), "roled")
	if d.Model != "@smol" {
		t.Fatalf("model = %q", d.Model)
	}
}

// A stale key next to `model: @smol` must still be reported: the alias is
// quoted before parsing, and the key scan reads the same text.
func TestDiscoverAgentsReportsStaleKeyAlongsideRoleModel(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, filepath.Join(dir, ".xdev", "agents"), "mix.md", "---\nname: mix\ndescription: x\nmodel: @smol\noutput: json\n---\nbody")
	_, warnings := DiscoverAgents(dir)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "output") {
		t.Fatalf("stale key beside @role: %v", warnings)
	}
}
