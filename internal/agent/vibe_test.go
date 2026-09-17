package agent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/tool"
)

// --- fixtures ---

// vibeParentRegistry is a stand-in for the session registry: the tools the
// director gives up plus the two it keeps.
func vibeParentRegistry() *tool.Registry {
	reg := tool.NewRegistry()
	reg.Register(tool.NewReadTool())
	reg.Register(tool.NewWriteTool())
	reg.Register(tool.NewEditTool())
	reg.Register(tool.NewTodoTool())
	reg.Register(echoTool{})
	return reg
}

func vibeNames(reg *tool.Registry) []string {
	out := make([]string, 0)
	for _, d := range reg.Defs() {
		out = append(out, d.Name)
	}
	slices.Sort(out)
	return out
}

// vibeCall runs one worker tool out of the director's own registry (the
// same lookup the agent loop uses).
func vibeCall(t *testing.T, v *VibeScope, name, args string) tool.Result {
	t.Helper()
	tl, ok := v.Registry().Get(name)
	if !ok {
		t.Fatalf("tool %s is not in the director's registry", name)
	}
	res, err := tl.Execute(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

// vibeLog records the persisted lifecycle entries (session custom entries).
type vibeLog struct {
	mu   sync.Mutex
	seen []string
}

func (l *vibeLog) persist(customType string, data map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if customType == VibeWorkerEntry {
		id, _ := data["id"].(string)
		status, _ := data["status"].(string)
		l.seen = append(l.seen, id+":"+status)
		return
	}
	on, _ := data["on"].(bool)
	if on {
		l.seen = append(l.seen, "mode:on")
	} else {
		l.seen = append(l.seen, "mode:off")
	}
}

func (l *vibeLog) has(want string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Contains(l.seen, want)
}

// newTestVibeScope wires a scope over a real hub with a scripted provider.
func newTestVibeScope(t *testing.T, p *fakeProvider, log *vibeLog, childTools []tool.Tool) *VibeScope {
	t.Helper()
	return NewVibeScope(VibeConfig{
		Hub:   NewHub(),
		Task:  &TaskTool{Provider: p, Model: "parent-model", CWD: "/tmp", ChildTools: childTools},
		Tools: vibeParentRegistry(),
		Resolve: func() (*VibeModel, error) {
			return &VibeModel{Provider: p, Model: "parent-model"}, nil
		},
		Persist:  log.persist,
		ParentID: func() string { return "sess-parent" },
	})
}

// --- tests ---

// Entering the mode routes the agent at a registry holding read, the
// parent's todo, and the five worker tools — and nothing else. The parent
// registry is untouched, which is what makes exit a restoration.
func TestVibeRegistryRestrictsToolset(t *testing.T) {
	parent := vibeParentRegistry()
	before := vibeNames(parent)

	v := NewVibeScope(VibeConfig{Tools: parent})
	got := vibeNames(v.Registry())
	want := []string{"read", "todo", "vibe_kill", "vibe_send", "vibe_spawn", "vibe_status", "vibe_wait"}
	if !slices.Equal(got, want) {
		t.Fatalf("director toolset = %v, want %v", got, want)
	}
	if after := vibeNames(parent); !slices.Equal(before, after) {
		t.Fatalf("the parent registry must never be mutated: %v → %v", before, after)
	}
	// The director's todo IS the session's todo: the parent owns the list.
	pt, _ := parent.Get("todo")
	vt, _ := v.Registry().Get("todo")
	if pt != vt {
		t.Fatal("the view must share the parent's todo tool, not clone it")
	}

	// A build without a todo tool (or a bare test registry) drops it: read
	// plus the worker tools remain the mandatory core.
	bare := NewVibeScope(VibeConfig{Tools: tool.NewRegistry()})
	got = vibeNames(bare.Registry())
	want = []string{"read", "vibe_kill", "vibe_send", "vibe_spawn", "vibe_status", "vibe_wait"}
	if !slices.Equal(got, want) {
		t.Fatalf("toolset without a parent todo = %v, want %v", got, want)
	}
}

// Vibe is mutually exclusive with plan and goal modes: entering refuses
// with the conflicting mode named, and a build with no hub cannot enter.
func TestVibeEnterRefusesConflictingModes(t *testing.T) {
	v := NewVibeScope(VibeConfig{
		Hub:       NewHub(),
		Task:      &TaskTool{},
		Tools:     tool.NewRegistry(),
		Conflicts: func() []string { return []string{"plan", "goal"} },
	})
	err := v.Enter()
	if err == nil || !strings.Contains(err.Error(), "plan") || !strings.Contains(err.Error(), "goal") {
		t.Fatalf("err = %v, want a conflict naming plan and goal", err)
	}
	if v.Active() {
		t.Fatal("a refused entry must not activate the mode")
	}

	// Resolving the conflict enters; the mode flip is persisted.
	log := &vibeLog{}
	v.cfg.Conflicts = func() []string { return nil }
	v.cfg.Persist = log.persist
	if err := v.Enter(); err != nil {
		t.Fatal(err)
	}
	if !v.Active() || !log.has("mode:on") {
		t.Fatalf("active = %v, log = %v", v.Active(), log.seen)
	}
	v.Exit()
	if v.Active() || !log.has("mode:off") {
		t.Fatalf("after exit: active = %v, log = %v", v.Active(), log.seen)
	}

	// No subagent machinery: refuse rather than half-enter.
	noHub := NewVibeScope(VibeConfig{Tools: tool.NewRegistry()})
	if err := noHub.Enter(); err == nil || !strings.Contains(err.Error(), "hub") {
		t.Fatalf("err = %v, want a missing-hub refusal", err)
	}
}

// Each tier selects its bundled agent and its role model, and the spec the
// worker runs under carries both (model, provider, tier prompt, tools).
func TestVibeTierMapping(t *testing.T) {
	cases := []struct{ in, cli, agent string }{
		{"fast", "fast", "sonic"},
		{"sonic", "fast", "sonic"},
		{"good", "good", "task"},
		{"task", "good", "task"},
		{" GOOD ", "good", "task"},
	}
	for _, tt := range cases {
		got, ok := VibeTierOf(tt.in)
		if !ok || got.CLI != tt.cli || got.Agent != tt.agent {
			t.Errorf("VibeTierOf(%q) = %+v, %v; want %s/%s", tt.in, got, ok, tt.cli, tt.agent)
		}
	}
	for _, bad := range []string{"", "slow", "medium"} {
		if _, ok := VibeTierOf(bad); ok {
			t.Errorf("VibeTierOf(%q) must not resolve", bad)
		}
	}

	p := &fakeProvider{}
	v := NewVibeScope(VibeConfig{
		Task: &TaskTool{
			Provider:   p,
			Model:      "parent-model",
			CWD:        "/tmp/work",
			ChildTools: []tool.Tool{echoTool{}},
		},
		Resolve: func() (*VibeModel, error) {
			return &VibeModel{Provider: p, Model: "session-model", Thinking: &ai.ThinkingBudget{Tokens: 7}}, nil
		},
	})

	fast, _ := VibeTierOf("fast")
	m, err := v.model(fast)
	if err != nil {
		t.Fatal(err)
	}
	spec := v.specFor(fast, VibeWorker{ID: "w1"}, m, "do the thing")
	if spec.Model != "session-model" || spec.Provider != ai.Provider(p) {
		t.Fatalf("fast spec model = %q/%v, want the session model", spec.Model, spec.Provider)
	}
	if !strings.Contains(spec.System, SubagentSystemPromptBase) || !strings.Contains(spec.System, "fast execution worker") {
		t.Fatalf("fast spec must carry the bundled sonic prompt: %q", spec.System)
	}
	if len(spec.Tools) != 1 || spec.Tools[0].Name() != "echo" {
		t.Fatalf("worker tools = %v, want the task tool's child tools", spec.Tools)
	}
	if spec.CWD != "/tmp/work" || spec.ParentSessionID != "" {
		t.Fatalf("spec = %+v", spec)
	}

	good, _ := VibeTierOf("good")
	m, _ = v.model(good)
	spec = v.specFor(good, VibeWorker{ID: "w2"}, m, "review it")
	if spec.Model != "session-model" || !strings.Contains(spec.System, "senior worker") {
		t.Fatalf("good spec = %+v (%q)", spec, spec.System)
	}
}

// spawn → wait → status → send → wait → kill against a scripted worker.
func TestVibeWorkerRoundTrip(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: yieldEvents(`{"result":"first-done"}`)},
		{events: yieldEvents(`{"result":"second-done"}`)},
	}}
	log := &vibeLog{}
	v := newTestVibeScope(t, p, log, []tool.Tool{})
	if err := v.Enter(); err != nil {
		t.Fatal(err)
	}

	// spawn: the name is sanitized into the worker id.
	res := vibeCall(t, v, VibeSpawnToolName, `{"cli":"fast","prompt":"fix the flaky test","name":"fix it!"}`)
	if res.IsError || !strings.Contains(res.Text, "fix-it") {
		t.Fatalf("spawn = %q", res.Text)
	}

	// wait returns the worker's final text.
	res = vibeCall(t, v, VibeWaitToolName, `{}`)
	if res.IsError || !strings.Contains(res.Text, "first-done") || !strings.Contains(res.Text, "done") {
		t.Fatalf("wait = %q", res.Text)
	}

	// status renders the registry, and the descriptor is complete.
	res = vibeCall(t, v, VibeStatusToolName, `{}`)
	for _, want := range []string{"fix-it", "fast", "done", "turn 1", "fake/"} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("status = %q, missing %q", res.Text, want)
		}
	}
	rows := v.Workers()
	if len(rows) != 1 {
		t.Fatalf("workers = %+v", rows)
	}
	if w := rows[0]; w.Status != VibeDone || w.Turn != 1 || w.Output != "first-done" || w.Session == "" {
		t.Fatalf("worker = %+v", w)
	}

	// send by unique prefix revives the worker for its next turn.
	res = vibeCall(t, v, VibeSendToolName, `{"session":"fix","message":"now update the docs"}`)
	if res.IsError || !strings.Contains(res.Text, "turn 2") {
		t.Fatalf("send = %q", res.Text)
	}
	res = vibeCall(t, v, VibeWaitToolName, `{"sessions":["fix-it"],"timeout":5}`)
	if res.IsError || !strings.Contains(res.Text, "second-done") {
		t.Fatalf("wait after send = %q", res.Text)
	}
	if w := v.Workers()[0]; w.Turn != 2 || w.Status != VibeDone || w.Output != "second-done" {
		t.Fatalf("worker after turn 2 = %+v", w)
	}

	// kill retires the worker; a killed worker takes no more work.
	if res = vibeCall(t, v, VibeKillToolName, `{"session":"fix-it"}`); res.IsError {
		t.Fatalf("kill = %q", res.Text)
	}
	if w := v.Workers()[0]; w.Status != VibeKilled {
		t.Fatalf("worker after kill = %+v", w)
	}
	if res = vibeCall(t, v, VibeSendToolName, `{"session":"fix-it","message":"more"}`); !res.IsError || !strings.Contains(res.Text, "killed") {
		t.Fatalf("send to a killed worker = %q", res.Text)
	}
	// An unknown selector names the registry so the director can recover.
	if res = vibeCall(t, v, VibeSendToolName, `{"session":"nope","message":"x"}`); !res.IsError || !strings.Contains(res.Text, "fix-it") {
		t.Fatalf("unknown worker = %q", res.Text)
	}
	// Required arguments are rejected, not guessed.
	if res = vibeCall(t, v, VibeSpawnToolName, `{"cli":"slow","prompt":"x"}`); !res.IsError {
		t.Fatalf("a bad tier must be refused: %q", res.Text)
	}
	if res = vibeCall(t, v, VibeSpawnToolName, `{"cli":"fast"}`); !res.IsError {
		t.Fatalf("a missing prompt must be refused: %q", res.Text)
	}

	// The lifecycle trail is persisted for resume.
	for _, want := range []string{"mode:on", "fix-it:running", "fix-it:done", "fix-it:killed"} {
		if !log.has(want) {
			t.Fatalf("persisted %v, missing %q", log.seen, want)
		}
	}
}

// Exiting kills every worker (including one mid-turn) and leaves the
// parent toolset usable — the mode only ever selected the restricted view.
func TestVibeExitKillsWorkers(t *testing.T) {
	blocker := &blockingTool{release: make(chan struct{})}
	defer blocker.unblock()
	p := &fakeProvider{calls: []fakeScript{
		{events: toolCallEvents("block", `{}`)},
		{events: yieldEvents(`{"result":"never"}`)},
	}}
	log := &vibeLog{}
	v := newTestVibeScope(t, p, log, []tool.Tool{blocker})
	parent := v.cfg.Tools
	before := vibeNames(parent)

	if err := v.Enter(); err != nil {
		t.Fatal(err)
	}
	if res := vibeCall(t, v, VibeSpawnToolName, `{"cli":"good","prompt":"hold"}`); res.IsError {
		t.Fatalf("spawn = %q", res.Text)
	}
	if w := v.Workers()[0]; w.Status != VibeRunning {
		t.Fatalf("worker must be mid-turn: %+v", w)
	}

	v.Exit()
	if v.Active() {
		t.Fatal("exit must leave the mode off")
	}
	rows := v.Workers()
	if len(rows) != 1 || rows[0].Status != VibeKilled {
		t.Fatalf("workers after exit = %+v", rows)
	}
	if !log.has("mode:off") {
		t.Fatalf("exit must persist the mode flip: %v", log.seen)
	}
	if got := vibeNames(parent); !slices.Equal(before, got) {
		t.Fatalf("parent toolset = %v, want %v", got, before)
	}
	// The agent routes at the parent registry again once the mode is off.
	if _, ok := v.Registry().Get("write"); ok {
		t.Fatal("the director view must not hold write")
	}
	if _, ok := parent.Get("write"); !ok {
		t.Fatal("the parent toolset must still hold write after exit")
	}
}

// Resume replays the persisted descriptors: completed workers come back
// idle (nothing runs in a fresh process), killed ones stay terminal, and a
// send to a rehydrated worker starts a new turn carrying its last result.
func TestVibeResumeRehydratesWorkers(t *testing.T) {
	worker := func(id, tier, status string) *session.CustomEntry {
		return &session.CustomEntry{CustomType: VibeWorkerEntry, Data: VibeWorker{
			ID: id, Tier: tier, Status: status, Model: "onegw/dev", Job: "hub-9",
			Session: "child-1", Output: status + "-output", Turn: 2,
		}.data()}
	}
	entries := []session.Entry{
		&session.CustomEntry{CustomType: VibeModeEntry, Data: map[string]any{"on": true}},
		worker("alpha", "fast", VibeDone),
		worker("beta", "good", VibeRunning),
		worker("gamma", "fast", VibeKilled),
	}
	workers, active := LoadVibe(entries)
	if !active || len(workers) != 3 {
		t.Fatalf("LoadVibe = %+v, %v", workers, active)
	}

	p := &fakeProvider{calls: []fakeScript{{events: yieldEvents(`{"result":"revived-done"}`)}}}
	log := &vibeLog{}
	v := newTestVibeScope(t, p, log, []tool.Tool{})
	v.Restore(workers, active)
	if !v.Active() {
		t.Fatal("a session whose mode was on must come back active")
	}
	byID := map[string]VibeWorker{}
	for _, w := range v.Workers() {
		byID[w.ID] = w
	}
	if w := byID["beta"]; w.Status != VibeIdle {
		t.Fatalf("an interrupted worker must come back idle, got %+v", w)
	}
	if w := byID["gamma"]; w.Status != VibeKilled {
		t.Fatalf("a killed worker stays terminal, got %+v", w)
	}
	if w := byID["alpha"]; w.Status != VibeDone || w.Output != "done-output" || w.Session != "child-1" {
		t.Fatalf("descriptor lost on resume: %+v", w)
	}
	if w := byID["alpha"]; w.Job != "" {
		t.Fatalf("a process-local hub id must not survive a resume: %+v", w)
	}

	// The retired worker refuses; the rehydrated one starts a fresh turn
	// with its prior result as context.
	if res := vibeCall(t, v, VibeSendToolName, `{"session":"gamma","message":"again"}`); !res.IsError {
		t.Fatalf("a killed worker must stay retired: %q", res.Text)
	}
	if res := vibeCall(t, v, VibeSendToolName, `{"session":"alpha","message":"continue the work"}`); res.IsError {
		t.Fatalf("send to a rehydrated worker = %q", res.Text)
	}
	if res := vibeCall(t, v, VibeWaitToolName, `{"sessions":["alpha"],"timeout":5}`); res.IsError || !strings.Contains(res.Text, "revived-done") {
		t.Fatalf("wait after resume = %q", res.Text)
	}
	if w := v.Workers()[0]; w.ID != "alpha" || w.Turn != 3 || w.Output != "revived-done" {
		t.Fatalf("worker after resume turn = %+v", w)
	}
	prompt := ""
	for _, m := range p.gotReqs[0].Messages {
		for _, b := range m.Content {
			if txt, ok := b.(ai.TextBlock); ok {
				prompt += txt.Text
			}
		}
	}
	if !strings.Contains(prompt, "done-output") || !strings.Contains(prompt, "continue the work") {
		t.Fatalf("the resumed turn must carry the prior result: %q", prompt)
	}

	// A session that ended with the mode off comes back off.
	if _, active := LoadVibe([]session.Entry{
		&session.CustomEntry{CustomType: VibeModeEntry, Data: map[string]any{"on": true}},
		&session.CustomEntry{CustomType: VibeModeEntry, Data: map[string]any{"on": false}},
	}); active {
		t.Fatal("the last mode flag must win")
	}
	// A descriptor upserted twice keeps the newest state.
	workers, _ = LoadVibe([]session.Entry{worker("alpha", "fast", VibeRunning), worker("alpha", "fast", VibeKilled)})
	if len(workers) != 1 || workers[0].Status != VibeKilled {
		t.Fatalf("upsert = %+v", workers)
	}
}

// A wait that times out is honest about it, and one with nothing to watch
// is not an error.
func TestVibeWaitTimeoutAndEmpty(t *testing.T) {
	blocker := &blockingTool{release: make(chan struct{})}
	defer blocker.unblock()
	p := &fakeProvider{calls: []fakeScript{
		{events: toolCallEvents("block", `{}`)},
		{events: yieldEvents(`{"result":"late"}`)},
	}}
	v := newTestVibeScope(t, p, &vibeLog{}, []tool.Tool{blocker})
	if err := v.Enter(); err != nil {
		t.Fatal(err)
	}
	if res := vibeCall(t, v, VibeWaitToolName, `{}`); !res.IsError || !strings.Contains(res.Text, "no workers") {
		t.Fatalf("wait with no workers = %q", res.Text)
	}
	if res := vibeCall(t, v, VibeSpawnToolName, `{"cli":"fast","prompt":"hold"}`); res.IsError {
		t.Fatalf("spawn = %q", res.Text)
	}
	start := time.Now()
	res := vibeCall(t, v, VibeWaitToolName, `{"timeout":0.2}`)
	if res.IsError || !strings.Contains(res.Text, "no worker settled") {
		t.Fatalf("timed-out wait = %q", res.Text)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("wait honored %v, want ~200ms", elapsed)
	}
	blocker.unblock()
	if res = vibeCall(t, v, VibeWaitToolName, `{"timeout":5}`); res.IsError || !strings.Contains(res.Text, "late") {
		t.Fatalf("wait after release = %q", res.Text)
	}
}
