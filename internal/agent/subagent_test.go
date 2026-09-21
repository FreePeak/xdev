package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
)

// yieldEvents scripts one assistant turn that calls yield with args —
// including the terminal done event real providers always emit (without
// it the loop never materializes the tool call).
func yieldEvents(args string) []ai.Event {
	return []ai.Event{
		{Type: ai.EventStart, Provider: "fake", API: "fake-api", Model: "m"},
		{Type: ai.EventToolcallStart, ToolCallID: "y1", ToolName: "yield", StreamIndex: 0},
		{Type: ai.EventToolcallDelta, StreamIndex: 0, PartialJSON: args},
		{Type: ai.EventToolcallEnd, StreamIndex: 0, PartialJSON: args},
		ai.Donef(ai.StopReasonStop, nil, &ai.Message{
			Role: ai.RoleAssistant,
			Content: []ai.Block{
				ai.ToolCallBlock{ID: "y1", Name: "yield", Arguments: json.RawMessage(args), StreamIndex: 0},
			},
			StopReason: ai.StopReasonStop,
		}),
	}
}

func TestSubagentYieldHandoff(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: yieldEvents(`{"result":"all done","files":["/tmp/a.txt"]}`)},
	}}
	res, err := SpawnChild(context.Background(), SubagentSpec{
		Name: "worker", Prompt: "do it", Provider: p, Model: "m",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "yielded" || res.Text != "all done" {
		t.Fatalf("res = %+v", res)
	}
	if len(res.Files) != 1 || res.Files[0] != "/tmp/a.txt" {
		t.Fatalf("files = %v", res.Files)
	}
	if res.SessionID == "" {
		t.Fatal("child session id missing")
	}
}

// A child that finishes in prose is nudged to use the yield tool, and once
// the nudges run out the result says so: a bare "completed" used to be
// indistinguishable from a real structured handoff (parity finding T3 #6).
func TestSubagentCompletedWithoutYield(t *testing.T) {
	prose := func(text string) fakeScript {
		return fakeScript{events: []ai.Event{{Type: ai.EventStart}, textEvent(text), doneEvent(text)}}
	}
	p := &fakeProvider{calls: []fakeScript{
		prose("just an answer"),
		prose("nudge one"),
		prose("nudge two"),
	}}
	res, err := SpawnChild(context.Background(), SubagentSpec{
		Name: "chatty", Prompt: "answer", Provider: p,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "completed" {
		t.Fatalf("status = %q, want completed", res.Status)
	}
	if !strings.Contains(res.Note, "never called yield") {
		t.Fatalf("an unstructured handoff must carry the warning, got Note=%q", res.Note)
	}
	// The run plus one turn per nudge: the child really was asked.
	p.mu.Lock()
	n := len(p.gotReqs)
	p.mu.Unlock()
	if want := 1 + yieldNudges; n != want {
		t.Fatalf("provider turns = %d, want %d", n, want)
	}
}

// Yielding after a nudge clears the warning (the handoff is structured again).
func TestSubagentYieldsAfterNudge(t *testing.T) {
	prose := fakeScript{events: []ai.Event{{Type: ai.EventStart}, textEvent("prose"), doneEvent("prose")}}
	p := &fakeProvider{calls: []fakeScript{prose, {events: yieldEvents(`{"result":"structured"}`)}}}
	res, err := SpawnChild(context.Background(), SubagentSpec{
		Name: "late", Prompt: "answer", Provider: p,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "yielded" || res.Text != "structured" {
		t.Fatalf("res = %+v", res)
	}
	if strings.Contains(res.Note, "never called yield") {
		t.Fatalf("a recovered yield must not warn: %q", res.Note)
	}
}

func TestSubagentFailureSurfaces(t *testing.T) {
	// Auth is retried (bounded). Script enough 401s to drain the
	// escalation bound so the failure still surfaces to the parent.
	auth := &ai.HTTPError{API: "a", Status: 401, Body: "no key"}
	p := &fakeProvider{calls: []fakeScript{
		{err: auth}, {err: auth}, {err: auth},
	}}
	res, err := SpawnChild(context.Background(), SubagentSpec{
		Name: "doomed", Prompt: "x", Provider: p,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "failed" || !strings.Contains(res.Err, "401") {
		t.Fatalf("res = %+v", res)
	}
}

// objectSchema is the contract pinned in the schema tests.
var objectSchema = json.RawMessage(`{"type":"object","required":["summary"],"properties":{"summary":{"type":"string"}}}`)

func TestSubagentSchemaPermissive(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: yieldEvents(`{"result":{"note":"wrong shape"}}`)},
	}}
	res, err := SpawnChild(context.Background(), SubagentSpec{
		Name: "loose", Prompt: "x", Provider: p,
		Output: &SubagentOutput{Schema: objectSchema, Strict: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "yielded" || res.Note == "" {
		t.Fatalf("permissive must accept with a note: %+v", res)
	}
	if !strings.Contains(res.Note, `missing required property "summary"`) {
		t.Fatalf("note = %q", res.Note)
	}
	if len(p.gotReqs) != 1 {
		t.Fatalf("permissive must not re-run: %d calls", len(p.gotReqs))
	}
}

func TestSubagentSchemaStrictRepairs(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: yieldEvents(`{"result":{"note":"wrong shape"}}`)},
		{events: yieldEvents(`{"result":{"summary":"fixed it"},"files":["out.md"]}`)},
	}}
	res, err := SpawnChild(context.Background(), SubagentSpec{
		Name: "strict", Prompt: "x", Provider: p,
		Output: &SubagentOutput{Schema: objectSchema, Strict: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "yielded" {
		t.Fatalf("strict retry must recover: %+v", res)
	}
	if !strings.HasPrefix(res.Note, "repaired after:") {
		t.Fatalf("note = %q", res.Note)
	}
	var obj map[string]string
	if err := json.Unmarshal(res.Yield, &obj); err != nil || obj["summary"] != "fixed it" {
		t.Fatalf("yield = %s err=%v", res.Yield, err)
	}
	if len(res.Files) != 1 || res.Files[0] != "out.md" {
		t.Fatalf("files = %v", res.Files)
	}
	// The correction prompt reached the child exactly once.
	if len(p.gotReqs) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(p.gotReqs))
	}
	if !containsText(p.gotReqs[1].Messages, "does not match the required schema") {
		t.Fatalf("retry request lacks the correction prompt: %+v", p.gotReqs[1].Messages)
	}
}

func TestSubagentSchemaStrictStillBad(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: yieldEvents(`{"result":{"note":"bad"}}`)},
		{events: yieldEvents(`{"result":{"note":"still bad"}}`)},
	}}
	res, err := SpawnChild(context.Background(), SubagentSpec{
		Name: "stubborn", Prompt: "x", Provider: p,
		Output: &SubagentOutput{Schema: objectSchema, Strict: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "schema-mismatch" {
		t.Fatalf("res = %+v", res)
	}
}

func TestSubagentPersistsToDisk(t *testing.T) {
	dir := t.TempDir()
	p := &fakeProvider{calls: []fakeScript{
		{events: yieldEvents(`{"result":"ok","files":[]}`)},
	}}
	res, err := SpawnChild(context.Background(), SubagentSpec{
		Name: "persist", Prompt: "x", Provider: p, DataDir: dir, CWD: "/proj",
		ParentSessionID: "parent-123",
	})
	if err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "sessions", "*", "*"+res.SessionID+"*.jsonl"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("child session file: %v err=%v", matches, err)
	}
	// The parent link is on disk, and the child is marked non-resumable.
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"parentSession":"parent-123"`) {
		t.Fatalf("child header missing parentSession:\n%s", firstLines(raw, 2))
	}
	if !strings.Contains(string(raw), `"source":"subagent"`) {
		t.Fatalf("child title slot must stamp source=subagent:\n%s", firstLines(raw, 1))
	}
}

func firstLines(b []byte, n int) string {
	lines := strings.SplitN(string(b), "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

func containsText(msgs []ai.Message, sub string) bool {
	for _, m := range msgs {
		if strings.Contains(m.Text(), sub) {
			return true
		}
	}
	return false
}

// TestSubagentYieldOnlyIsolation pins the PRD contract (docs/PRD.md:314):
// a child transcript NEVER streams into parent context — only the yield
// payload is visible to the caller.
func TestSubagentYieldOnlyIsolation(t *testing.T) {
	// The child says a secret mid-run, then yields a different payload.
	secret := "INTERNAL-SECRET-TRAIL"
	p := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{
			{Type: ai.EventStart},
			{Type: ai.EventTextStart}, textEvent(secret),
			{Type: ai.EventTextDelta, Delta: " working..."},
			{Type: ai.EventToolcallStart, ToolCallID: "y1", ToolName: "yield", StreamIndex: 0},
			{Type: ai.EventToolcallDelta, StreamIndex: 0, PartialJSON: `{"result":"clean summary"}`},
			{Type: ai.EventToolcallEnd, StreamIndex: 0, PartialJSON: `{"result":"clean summary"}`},
			ai.Donef(ai.StopReasonStop, nil, &ai.Message{
				Role: ai.RoleAssistant,
				Content: []ai.Block{
					ai.TextBlock{Text: secret + " working..."},
					ai.ToolCallBlock{ID: "y1", Name: "yield", Arguments: json.RawMessage(`{"result":"clean summary"}`), StreamIndex: 0},
				},
				StopReason: ai.StopReasonStop,
			}),
		}},
	}}
	res, err := SpawnChild(context.Background(), SubagentSpec{Name: "isolated", Prompt: "x", Provider: p})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "yielded" || res.Text != "clean summary" {
		t.Fatalf("res = %+v", res)
	}
	// The SubagentResult is the entire parent-visible surface: the secret
	// must not appear anywhere in it.
	blob, _ := json.Marshal(res)
	if strings.Contains(string(blob), secret) {
		t.Fatalf("child transcript leaked into the parent-visible result: %s", blob)
	}
}

// The context-file hierarchy reached only the PARENT prompt: SpawnChild ran
// the child on whatever spec.System held, so a spawn carried the bare base
// prose and none of the standing conventions. A subagent is exactly the actor
// handed "just make this one-line fix", which is the case the conventions
// exist for — pin that the child request carries them.
func TestSubagentChildGetsContextFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	userDir := filepath.Join(home, ".xdev", "agent")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const rule = "the main checkout is READ-ONLY — report a PR URL when done"
	if err := os.WriteFile(filepath.Join(userDir, "AGENTS.md"), []byte(rule), 0o644); err != nil {
		t.Fatal(err)
	}
	proj := t.TempDir()
	if err := os.WriteFile(filepath.Join(proj, "AGENTS.md"), []byte("project rule here"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &fakeProvider{calls: []fakeScript{{events: yieldEvents(`{"result":"ok"}`)}}}
	spec := SubagentSpec{Name: "ctx", Prompt: "x", Provider: p, CWD: proj, System: "definition prose"}
	res, err := SpawnChild(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "yielded" {
		t.Fatalf("res = %+v", res)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.gotReqs) == 0 {
		t.Fatal("no request captured")
	}
	sys := p.gotReqs[0].System
	for _, want := range []string{rule, "project rule here", "definition prose"} {
		if !strings.Contains(sys, want) {
			t.Errorf("child system prompt missing %q:\n%s", want, sys)
		}
	}
	// The caller's spec is not mutated: the appended prompt is local to the
	// spawn, so a reused spec does not accumulate copies on each child.
	if spec.System != "definition prose" {
		t.Errorf("spec.System mutated: %q", spec.System)
	}
}
