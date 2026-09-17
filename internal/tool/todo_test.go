package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/session"
)

// todoArgsRaw marshals a todo call body.
func todoArgsRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	return args(t, v)
}

// todoStatuses flattens the tool's state into content→status for assertions.
func todoStatuses(t *testing.T, tt *TodoTool) map[string]TodoStatus {
	t.Helper()
	tt.mu.Lock()
	defer tt.mu.Unlock()
	out := map[string]TodoStatus{}
	for i := range tt.phases {
		for j := range tt.phases[i].Tasks {
			out[tt.phases[i].Tasks[j].Content] = tt.phases[i].Tasks[j].Status
		}
	}
	return out
}

func todoExec(t *testing.T, tt *TodoTool, v any) Result {
	t.Helper()
	res, err := tt.Execute(context.Background(), todoArgsRaw(t, v))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// recordingSink captures every persisted snapshot.
type recordingSink struct{ calls [][]TodoPhase }

func (r *recordingSink) AppendTodo(phases []TodoPhase) error {
	r.calls = append(r.calls, clonePhases(phases))
	return nil
}

func testTodoTool() *TodoTool {
	return &TodoTool{}
}

func TestTodoInitPhasedPromotesFirst(t *testing.T) {
	tt := testTodoTool()
	res := todoExec(t, tt, map[string]any{
		"op": "init",
		"list": []map[string]any{
			{"phase": "Foundation", "items": []string{"read code", "write plan"}},
			{"phase": "Build", "items": []string{"implement tool"}},
		},
	})
	if res.IsError {
		t.Fatalf("init failed: %s", res.Text)
	}
	got := todoStatuses(t, tt)
	want := map[string]TodoStatus{
		"read code":      TodoInProgress,
		"write plan":     TodoPending,
		"implement tool": TodoPending,
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("status[%q] = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
	if !strings.Contains(res.Text, "I. Foundation") || !strings.Contains(res.Text, "II. Build") {
		t.Fatalf("render must list phases: %q", res.Text)
	}
	if !strings.Contains(res.Text, "[/] read code") || !strings.Contains(res.Text, "[ ] write plan") {
		t.Fatalf("render must show status markers: %q", res.Text)
	}
}

func TestTodoInitFlatSynthesizesPhaseAndReplaces(t *testing.T) {
	tt := testTodoTool()
	todoExec(t, tt, map[string]any{"op": "init", "list": []map[string]any{
		{"phase": "Old", "items": []string{"stale task"}},
	}})
	res := todoExec(t, tt, map[string]any{"op": "init", "items": []string{"fresh task"}})
	if res.IsError {
		t.Fatalf("flat init failed: %s", res.Text)
	}
	tt.mu.Lock()
	defer tt.mu.Unlock()
	if len(tt.phases) != 1 || tt.phases[0].Name != defaultInitPhase {
		t.Fatalf("flat init must synthesize %q, got %+v", defaultInitPhase, tt.phases)
	}
	if len(tt.phases[0].Tasks) != 1 || tt.phases[0].Tasks[0].Content != "fresh task" {
		t.Fatalf("init must replace the old list: %+v", tt.phases)
	}
	if tt.phases[0].Tasks[0].Status != TodoInProgress {
		t.Fatalf("first pending must auto-promote after init: %+v", tt.phases[0].Tasks)
	}
}

func TestTodoInitRejectsDuplicatesAndEmpty(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"duplicate phase": {"op": "init", "list": []map[string]any{
			{"phase": "A", "items": []string{"x"}},
			{"phase": "A", "items": []string{"y"}},
		}},
		"duplicate task": {"op": "init", "list": []map[string]any{
			{"phase": "A", "items": []string{"x"}},
			{"phase": "B", "items": []string{"x"}},
		}},
		"missing list": {"op": "init"},
		"empty phase":  {"op": "init", "list": []map[string]any{{"phase": "A", "items": []string{}}}},
	} {
		tt := testTodoTool()
		if res := todoExec(t, tt, body); !res.IsError {
			t.Fatalf("%s: expected error, got %q", name, res.Text)
		}
		if got := todoStatuses(t, tt); len(got) != 0 {
			t.Fatalf("%s: failed init must not change state: %v", name, got)
		}
	}
}

func TestTodoStartDemotesOthersAndPointerMovesForward(t *testing.T) {
	tt := testTodoTool()
	todoExec(t, tt, map[string]any{"op": "init", "items": []string{"a", "b", "c"}})

	todoExec(t, tt, map[string]any{"op": "start", "task": "c"})
	got := todoStatuses(t, tt)
	if got["a"] != TodoPending || got["b"] != TodoPending || got["c"] != TodoInProgress {
		t.Fatalf("start must demote the previous in-progress task: %v", got)
	}

	// Completing the active task moves the pointer back to the earliest
	// pending task, not to the next one in line order of completion.
	todoExec(t, tt, map[string]any{"op": "done", "task": "c"})
	got = todoStatuses(t, tt)
	if got["c"] != TodoCompleted || got["a"] != TodoInProgress {
		t.Fatalf("done must promote the earliest pending task: %v", got)
	}
	if got["b"] != TodoPending {
		t.Fatalf("a later pending task must not jump the queue: %v", got)
	}
}

func TestTodoDonePhaseAndWholeList(t *testing.T) {
	tt := testTodoTool()
	todoExec(t, tt, map[string]any{"op": "init", "list": []map[string]any{
		{"phase": "One", "items": []string{"a", "b"}},
		{"phase": "Two", "items": []string{"c"}},
	}})

	todoExec(t, tt, map[string]any{"op": "done", "phase": "One"})
	got := todoStatuses(t, tt)
	if got["a"] != TodoCompleted || got["b"] != TodoCompleted {
		t.Fatalf("phase-wide done must complete the phase: %v", got)
	}
	if got["c"] != TodoInProgress {
		t.Fatalf("promotion must pick up the next phase: %v", got)
	}

	todoExec(t, tt, map[string]any{"op": "done"})
	got = todoStatuses(t, tt)
	for k, v := range got {
		if v != TodoCompleted {
			t.Fatalf("bare done must complete everything; %q = %q", k, v)
		}
	}
}

func TestTodoDropMarksDropped(t *testing.T) {
	tt := testTodoTool()
	todoExec(t, tt, map[string]any{"op": "init", "items": []string{"a", "b"}})
	todoExec(t, tt, map[string]any{"op": "drop", "task": "a"})
	got := todoStatuses(t, tt)
	if got["a"] != TodoDropped {
		t.Fatalf("drop must mark dropped: %v", got)
	}
	if got["b"] != TodoInProgress {
		t.Fatalf("dropping the active task must promote the next pending: %v", got)
	}
}

func TestTodoBlockExcludesFromPromotionAndUnblockRestores(t *testing.T) {
	tt := testTodoTool()
	todoExec(t, tt, map[string]any{"op": "init", "items": []string{"a"}})

	res := todoExec(t, tt, map[string]any{"op": "block", "task": "a", "reason": "waiting on\n  the  API"})
	got := todoStatuses(t, tt)
	if got["a"] != TodoBlocked {
		t.Fatalf("block must mark blocked: %v", got)
	}
	if !strings.Contains(res.Text, "(blocked: waiting on the API)") {
		t.Fatalf("blocker note must collapse whitespace into the render: %q", res.Text)
	}

	// Blocked tasks never auto-promote: with only a blocked task left,
	// there is no active task at all.
	tt.mu.Lock()
	active := 0
	for i := range tt.phases {
		for j := range tt.phases[i].Tasks {
			if tt.phases[i].Tasks[j].Status == TodoInProgress {
				active++
			}
		}
	}
	tt.mu.Unlock()
	if active != 0 {
		t.Fatalf("blocked task auto-promoted: %d active", active)
	}

	todoExec(t, tt, map[string]any{"op": "unblock", "task": "a"})
	got = todoStatuses(t, tt)
	if got["a"] != TodoInProgress {
		t.Fatalf("unblock must return the task to pending and let it promote: %v", got)
	}

	// Unblocking a live task is a silent no-op.
	if res := todoExec(t, tt, map[string]any{"op": "unblock", "task": "a"}); res.IsError {
		t.Fatalf("unblock of a non-blocked task must not error: %s", res.Text)
	}
	if res := todoExec(t, tt, map[string]any{"op": "block"}); !res.IsError {
		t.Fatal("block without a target must error")
	}
}

func TestTodoAppendLazyPhaseAndRemove(t *testing.T) {
	tt := testTodoTool()
	todoExec(t, tt, map[string]any{"op": "init", "items": []string{"a"}})

	res := todoExec(t, tt, map[string]any{"op": "append", "phase": "Later", "items": []string{"b", "c"}})
	if res.IsError {
		t.Fatalf("append failed: %s", res.Text)
	}
	got := todoStatuses(t, tt)
	if got["b"] != TodoPending || got["c"] != TodoPending {
		t.Fatalf("appended tasks must start pending: %v", got)
	}
	if got["a"] != TodoInProgress {
		t.Fatalf("append must not steal the active pointer from an earlier pending task: %v", got)
	}
	if res := todoExec(t, tt, map[string]any{"op": "append", "phase": "Later", "items": []string{"b"}}); !res.IsError {
		t.Fatal("append of a duplicate task must error")
	}

	// rm of the in-progress task promotes the earliest remaining pending.
	todoExec(t, tt, map[string]any{"op": "rm", "task": "a"})
	got = todoStatuses(t, tt)
	if _, exists := got["a"]; exists {
		t.Fatalf("rm must remove the task: %v", got)
	}
	if got["b"] != TodoInProgress {
		t.Fatalf("rm of the active task must promote: %v", got)
	}

	// rm phase clears the tasks but keeps the phase; bare rm clears all.
	todoExec(t, tt, map[string]any{"op": "rm", "phase": "Later"})
	tt.mu.Lock()
	var names []string
	for i := range tt.phases {
		names = append(names, tt.phases[i].Name)
	}
	tt.mu.Unlock()
	if len(names) != 2 || names[1] != "Later" {
		t.Fatalf("rm phase must keep the phase: %v", names)
	}
	todoExec(t, tt, map[string]any{"op": "rm"})
	if got := todoStatuses(t, tt); len(got) != 0 {
		t.Fatalf("bare rm must clear every task: %v", got)
	}
}

func TestTodoTargetErrors(t *testing.T) {
	tt := testTodoTool()
	todoExec(t, tt, map[string]any{"op": "init", "items": []string{"real task"}})

	res := todoExec(t, tt, map[string]any{"op": "start", "task": "task-2"})
	if !res.IsError || !strings.Contains(res.Text, "by content, not by IDs") {
		t.Fatalf("invented ID must get the content hint: %q", res.Text)
	}
	res = todoExec(t, tt, map[string]any{"op": "done", "task": "nope"})
	if !res.IsError || !strings.Contains(res.Text, `Task "nope" not found`) {
		t.Fatalf("unknown task error: %q", res.Text)
	}
	res = todoExec(t, tt, map[string]any{"op": "done", "phase": "Missing"})
	if !res.IsError || !strings.Contains(res.Text, `Phase "Missing" not found`) {
		t.Fatalf("unknown phase error: %q", res.Text)
	}
	res = todoExec(t, tt, map[string]any{"op": "frobnicate"})
	if !res.IsError || !strings.Contains(res.Text, "unknown op") {
		t.Fatalf("unknown op error: %q", res.Text)
	}
	// Failed targeting ops leave the list untouched.
	if got := todoStatuses(t, tt); got["real task"] != TodoInProgress {
		t.Fatalf("failed op changed state: %v", got)
	}
}

func TestTodoViewIsReadOnlyAndUnpersisted(t *testing.T) {
	tt := testTodoTool()
	sink := &recordingSink{}
	tt.Sink = sink
	todoExec(t, tt, map[string]any{"op": "init", "items": []string{"a", "b"}})
	if len(sink.calls) != 1 {
		t.Fatalf("init must persist once, got %d", len(sink.calls))
	}
	res := todoExec(t, tt, map[string]any{"op": "view"})
	if res.IsError {
		t.Fatalf("view failed: %s", res.Text)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("view must not persist, got %d calls", len(sink.calls))
	}
	if got := todoStatuses(t, tt); got["a"] != TodoInProgress {
		t.Fatalf("view must not mutate: %v", got)
	}
	if !strings.Contains(res.Text, "[ ] b") {
		t.Fatalf("view must echo the list: %q", res.Text)
	}
}

func TestTodoPersistsEveryStateChange(t *testing.T) {
	tt := testTodoTool()
	sink := &recordingSink{}
	tt.Sink = sink
	todoExec(t, tt, map[string]any{"op": "init", "items": []string{"a", "b"}})
	todoExec(t, tt, map[string]any{"op": "done", "task": "a"})
	if len(sink.calls) != 2 {
		t.Fatalf("persist calls = %d, want 2", len(sink.calls))
	}
	last := sink.calls[1]
	if len(last) != 1 || last[0].Tasks[0].Status != TodoCompleted {
		t.Fatalf("persisted snapshot is stale: %+v", last)
	}
	// The sink must see a clone: mutating the tool afterwards must not
	// rewrite history in the recorded snapshot.
	todoExec(t, tt, map[string]any{"op": "drop", "task": "b"})
	if sink.calls[1][0].Tasks[0].Status != TodoCompleted {
		t.Fatal("recorded snapshot was mutated after the fact")
	}
}

func TestTodoInferMissingOp(t *testing.T) {
	tt := testTodoTool()
	if res := todoExec(t, tt, map[string]any{"list": []map[string]any{{"phase": "P", "items": []string{"x"}}}}); res.IsError {
		t.Fatalf("list payload must infer init: %s", res.Text)
	}
	if res := todoExec(t, tt, map[string]any{"items": []string{"y"}, "phase": "P"}); res.IsError {
		t.Fatalf("items+phase must infer append: %s", res.Text)
	}
	fresh := testTodoTool()
	if res := todoExec(t, fresh, map[string]any{"items": []string{"z"}}); res.IsError {
		t.Fatalf("items on an empty list must infer init: %s", res.Text)
	}
	if res := todoExec(t, tt, map[string]any{"task": "x"}); !res.IsError {
		t.Fatal("targeting args alone must stay an error")
	}
}

func TestTodoPersistsAsUserTodoEditEntry(t *testing.T) {
	tt := testTodoTool()
	dir := t.TempDir()
	store := session.OpenMem("/proj", "todo test")
	if _, err := store.EnsureOnDisk(filepath.Join(dir, "s.jsonl"), session.Options{}); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	reg.Register(tt)
	if !WireTodoSink(reg, store) {
		t.Fatal("WireTodoSink must find the todo tool")
	}
	todoExec(t, tt, map[string]any{"op": "init", "items": []string{"ship it"}})

	entries := store.Entries()
	var found *session.CustomEntry
	for _, e := range entries {
		if ce, ok := e.(*session.CustomEntry); ok && ce.CustomType == "user_todo_edit" {
			found = ce
		}
	}
	if found == nil {
		t.Fatalf("no user_todo_edit entry in %+v", entries)
	}
	if phases, ok := found.Data["phases"].([]TodoPhase); !ok || len(phases) != 1 {
		t.Fatalf("phases payload = %#v", found.Data["phases"])
	}
	raw, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"customType":"user_todo_edit"`) || !strings.Contains(string(raw), "ship it") {
		t.Fatalf("JSONL line missing the todo record: %s", raw)
	}
	// Round-trip: the payload decodes back into phases.
	var decoded struct {
		Phases []TodoPhase `json:"phases"`
	}
	b, err := json.Marshal(found.Data)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Phases) != 1 || decoded.Phases[0].Tasks[0].Content != "ship it" {
		t.Fatalf("round-trip mismatch: %+v", decoded)
	}
}

func TestTodoRegistryRegistration(t *testing.T) {
	reg := NewRegistry()
	reg.Register(NewTodoTool())
	if _, ok := reg.Get("todo"); !ok {
		t.Fatal("todo tool must register under its name")
	}
	defs := reg.Defs()
	var params string
	for _, d := range defs {
		if d.Name == "todo" {
			params = string(d.Parameters)
		}
	}
	if !strings.Contains(params, `"init"`) || !strings.Contains(params, `"unblock"`) {
		t.Fatalf("todo schema incomplete: %s", params)
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(params), &schema); err != nil {
		t.Fatalf("todo schema is not valid JSON: %v", err)
	}
	if _, ok := schema["properties"].(map[string]any)["run_in_background"]; ok {
		t.Fatal("todo schema must not carry bash params")
	}
	// The bash tool's schema is what carries run_in_background.
	bash := NewBashTool("")
	if !strings.Contains(string(bash.Parameters()), "run_in_background") {
		t.Fatalf("bash schema must advertise run_in_background: %s", bash.Parameters())
	}
}

func TestTodoOpenForReminderExcludesBlockedAndFinished(t *testing.T) {
	tt := testTodoTool()
	todoExec(t, tt, map[string]any{"op": "init", "list": []map[string]any{
		{"phase": "Work", "items": []string{"active", "waiting", "finished"}},
	}})
	todoExec(t, tt, map[string]any{"op": "block", "task": "waiting", "reason": "user decision"})
	todoExec(t, tt, map[string]any{"op": "done", "task": "finished"})

	open := OpenForReminder(tt.Snapshot())
	if len(open) != 1 || open[0].Name != "Work" {
		t.Fatalf("reminder phases = %+v", open)
	}
	if len(open[0].Tasks) != 1 || open[0].Tasks[0].Content != "active" {
		t.Fatalf("reminder tasks = %+v (blocked and completed must be excluded)", open[0].Tasks)
	}

	// Snapshot is a copy: mutating it must not touch live state.
	snap := tt.Snapshot()
	snap[0].Tasks[0].Status = TodoDropped
	if got := todoStatuses(t, tt); got["active"] != TodoInProgress {
		t.Fatalf("Snapshot leaked live state: %v", got)
	}
}

// View is the dock's task source (#291 §1): the same states the model sees, with
// the counts in the heading so the section reads in one line. "" when there is
// nothing to show — an empty section is a lie by omission, an absent one is not.
func TestTodoViewHeadingsAndEmptiness(t *testing.T) {
	tt := testTodoTool()
	if got := tt.View(); got != "" {
		t.Fatalf("empty list must render nothing, got %q", got)
	}
	if res := todoExec(t, tt, map[string]any{"op": "init", "items": []string{"wire the dock", "write tests"}}); res.IsError {
		t.Fatal(res.Text)
	}
	head, rest, found := strings.Cut(tt.View(), "\n")
	if !found {
		t.Fatalf("view must have a heading and rows:\n%s", tt.View())
	}
	if head != "TASKS · 0/2 done" {
		t.Fatalf("heading = %q", head)
	}
	// init auto-promotes the earliest pending task, and the view carries the
	// model's own markers so the two renderings cannot disagree about a state.
	if !strings.Contains(rest, "[/] wire the dock") || !strings.Contains(rest, "[ ] write tests") {
		t.Fatalf("rows missing:\n%s", rest)
	}
	// A phase name only appears when there is more than one to tell apart.
	if strings.Contains(rest, "Tasks") {
		t.Fatalf("a flat list must not name its one phase:\n%s", rest)
	}
	if res := todoExec(t, tt, map[string]any{"op": "done", "task": "wire the dock"}); res.IsError {
		t.Fatal(res.Text)
	}
	if head, _, _ := strings.Cut(tt.View(), "\n"); head != "TASKS · 1/2 done" {
		t.Fatalf("heading after a done: %q", head)
	}
	if res := todoExec(t, tt, map[string]any{"op": "block", "task": "write tests", "reason": "waiting on review"}); res.IsError {
		t.Fatal(res.Text)
	}
	out := tt.View()
	if !strings.Contains(out, "1 blocked") || !strings.Contains(out, "(blocked: waiting on review)") {
		t.Fatalf("a blocker must be visible:\n%s", out)
	}
	if res := todoExec(t, tt, map[string]any{"op": "init", "list": []map[string]any{
		{"phase": "Foundation", "items": []string{"a"}},
		{"phase": "Polish", "items": []string{"b"}},
	}}); res.IsError {
		t.Fatal(res.Text)
	}
	out = tt.View()
	if !strings.Contains(out, "Foundation") || !strings.Contains(out, "Polish") {
		t.Fatalf("a phased list must name its phases:\n%s", out)
	}
}
