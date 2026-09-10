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

func TestSubagentCompletedWithoutYield(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: []ai.Event{{Type: ai.EventStart}, textEvent("just an answer"), doneEvent("just an answer")}},
	}}
	res, err := SpawnChild(context.Background(), SubagentSpec{
		Name: "chatty", Prompt: "answer", Provider: p,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "completed" || res.Text != "just an answer" {
		t.Fatalf("res = %+v", res)
	}
}

func TestSubagentFailureSurfaces(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{err: &ai.HTTPError{API: "a", Status: 401, Body: "no key"}},
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
