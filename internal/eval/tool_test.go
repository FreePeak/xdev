package eval

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func newTestTool(t *testing.T) *Tool {
	t.Helper()
	if _, err := ResolvePython(); err != nil {
		t.Skipf("python3 unavailable: %v", err)
	}
	tt := NewTool(t.TempDir())
	t.Cleanup(func() { _ = tt.Close() })
	return tt
}

func call(t *testing.T, tt *Tool, args string) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := tt.Execute(ctx, json.RawMessage(args))
	if err != nil {
		t.Fatalf("Execute(%s): %v", args, err)
	}
	return res.Text, res.IsError
}

func callErr(t *testing.T, tt *Tool, args string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := tt.Execute(ctx, json.RawMessage(args))
	if err != nil {
		t.Fatalf("Execute(%s): %v", args, err)
	}
	if !res.IsError {
		t.Fatalf("Execute(%s): expected error result, got %q", args, res.Text)
	}
	return errors.New(res.Text)
}

func TestToolNameAndSchema(t *testing.T) {
	tt := newTestTool(t)
	if tt.Name() != "eval" {
		t.Fatalf("Name() = %q", tt.Name())
	}
	var schema map[string]any
	if err := json.Unmarshal(tt.Parameters(), &schema); err != nil {
		t.Fatalf("Parameters() is not valid JSON: %v", err)
	}
	if _, ok := schema["properties"]; !ok {
		t.Fatalf("schema has no properties: %s", tt.Parameters())
	}
	if !strings.Contains(tt.Description(), "Persistent Python kernel") {
		t.Fatalf("description does not describe the tool: %q", tt.Description())
	}
}

func TestToolRejectsJS(t *testing.T) {
	tt := newTestTool(t)
	for _, lang := range []string{"js", "javascript", "node"} {
		text, isErr := call(t, tt, `{"code":"1+1","language":"`+lang+`"}`)
		if !isErr || !strings.Contains(text, "not supported") {
			t.Fatalf("language %q: got isErr=%v text=%q, want a 'not supported' error", lang, isErr, text)
		}
	}
	// py (and unset) still work.
	if text, isErr := call(t, tt, `{"code":"6*7","language":"py"}`); isErr || !strings.Contains(text, "42") {
		t.Fatalf("language py rejected: isErr=%v text=%q", isErr, text)
	}
}

func TestToolStatePersists(t *testing.T) {
	tt := newTestTool(t)
	if text, isErr := call(t, tt, `{"code":"a = 20"}`); isErr {
		t.Fatalf("first cell failed: %q", text)
	}
	text, isErr := call(t, tt, `{"code":"a + 22"}`)
	if isErr || !strings.Contains(text, "42") {
		t.Fatalf("state lost: isErr=%v text=%q", isErr, text)
	}
}

func TestToolErrorAndStderr(t *testing.T) {
	tt := newTestTool(t)
	text, isErr := call(t, tt, `{"code":"import sys; sys.stderr.write(\"to stderr\"); raise RuntimeError(\"nope\")"}`)
	if !isErr {
		t.Fatalf("error cell did not set IsError: %q", text)
	}
	if !strings.Contains(text, "to stderr") || !strings.Contains(text, "RuntimeError") {
		t.Fatalf("stderr/error not surfaced: %q", text)
	}
}

func TestToolReset(t *testing.T) {
	tt := newTestTool(t)
	call(t, tt, `{"code":"keep = 1"}`)
	text, isErr := call(t, tt, `{"code":"keep","reset":true}`)
	if !isErr || !strings.Contains(text, "NameError") {
		t.Fatalf("reset=true did not wipe before the cell: isErr=%v text=%q", isErr, text)
	}
	// Bare reset with no code.
	if text, isErr := call(t, tt, `{"code":"","reset":true}`); isErr || !strings.Contains(text, "reset") {
		t.Fatalf("bare reset failed: isErr=%v text=%q", isErr, text)
	}
}

func TestToolTimeoutReportsInline(t *testing.T) {
	tt := newTestTool(t)
	// timeout below the background threshold → the cell is waited on and the
	// timeout reported inline.
	start := time.Now()
	text, isErr := call(t, tt, `{"code":"import time; time.sleep(20)","timeout":1}`)
	if !isErr || !strings.Contains(text, "timed out") {
		t.Fatalf("timeout not reported inline: isErr=%v text=%q", isErr, text)
	}
	if got := time.Since(start); got > 10*time.Second {
		t.Fatalf("timeout took too long: %v", got)
	}
	// The kernel survives the interrupt.
	if text, isErr := call(t, tt, `{"code":"1+1"}`); isErr || !strings.Contains(text, "2") {
		t.Fatalf("kernel did not survive the timeout: isErr=%v text=%q", isErr, text)
	}
}

func TestToolTimeoutClampedAndValidated(t *testing.T) {
	tt := newTestTool(t)
	if err := callErr(t, tt, `{"code":"1","timeout":-1}`); !strings.Contains(err.Error(), ">= 0") {
		t.Fatalf("negative timeout not rejected: %v", err)
	}
	// 0 disables the limit — a fast cell still completes under it.
	if text, isErr := call(t, tt, `{"code":"7*6","timeout":0}`); isErr || !strings.Contains(text, "42") {
		t.Fatalf("timeout=0 rejected a cell: isErr=%v text=%q", isErr, text)
	}
}

func TestToolBackgroundsLongCell(t *testing.T) {
	tt := newTestTool(t)
	tt.BackgroundAfter = 300 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	res, err := tt.Execute(ctx, json.RawMessage(`{"code":"import time; time.sleep(3); print(\"late out\")"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("backgrounded cell reported error: %q", res.Text)
	}
	if !strings.Contains(res.Text, "job #1") || !strings.Contains(res.Text, "backgrounded") {
		t.Fatalf("cell not backgrounded: %q", res.Text)
	}
	if got := time.Since(start); got >= 2*time.Second {
		t.Fatalf("Execute blocked for the whole cell: %v", got)
	}
	var details struct {
		JobID int64  `json:"job_id"`
		State string `json:"status"`
	}
	if err := json.Unmarshal([]byte(mapJSON(t, res.Details)), &details); err != nil {
		t.Fatalf("Details is not JSON: %v", err)
	}
	if details.JobID != 1 || details.State != "running" {
		t.Fatalf("details = %+v", details)
	}

	// A second call while the cell runs reports ErrBusy...
	busy, isErr := call(t, tt, `{"code":"1"}`)
	if !isErr || !strings.Contains(busy, "cell is still running") {
		t.Fatalf("busy not reported: isErr=%v text=%q", isErr, busy)
	}

	// ...and the finished job is reported on the next call.
	deadline := time.Now().Add(10 * time.Second)
	var final string
	for time.Now().Before(deadline) {
		deadlineCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
		res, err := tt.Execute(deadlineCtx, json.RawMessage(`{"code":"1+1"}`))
		done()
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		final = res.Text
		if !strings.Contains(final, "job #1 finished") {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		break
	}
	if !strings.Contains(final, "job #1 finished") {
		t.Fatalf("finished job not reported: %q", final)
	}
	if !strings.Contains(final, "late out") {
		t.Fatalf("backgrounded cell output lost: %q", final)
	}
}

func TestToolInterruptedContext(t *testing.T) {
	tt := newTestTool(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	res, err := tt.Execute(ctx, json.RawMessage(`{"code":"import time; time.sleep(10)"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Text, "interrupted") {
		t.Fatalf("cancelled context not reported: isErr=%v text=%q", res.IsError, res.Text)
	}
	if tt.Jobs.Running() != nil && len(tt.Jobs.Running()) != 0 {
		t.Fatalf("cancelled cell left a running job: %v", tt.Jobs.Running())
	}
	// The kernel is still usable.
	if text, isErr := call(t, tt, `{"code":"3+3"}`); isErr || !strings.Contains(text, "6") {
		t.Fatalf("kernel unusable after interrupt: isErr=%v text=%q", isErr, text)
	}
}

func TestJobsTable(t *testing.T) {
	js := NewJobs()
	if js.Len() != 0 || js.Render() != "no eval jobs\n" {
		t.Fatalf("empty table: len=%d render=%q", js.Len(), js.Render())
	}
	job := js.Add("code one")
	if job.ID != 1 || js.Len() != 1 {
		t.Fatalf("Add: id=%d len=%d", job.ID, js.Len())
	}
	job2 := js.Add("code two")
	if job2.ID != 2 {
		t.Fatalf("second id = %d", job2.ID)
	}
	if got := len(js.Running()); got != 2 {
		t.Fatalf("running = %d", got)
	}
	if !strings.Contains(js.Render(), "running") {
		t.Fatalf("render missing running job: %q", js.Render())
	}
	job.Finish(Outcome{Status: "ok", Text: "done"})
	if job.State() != "ok" || !job.Done() {
		t.Fatalf("State after finish = %q", job.State())
	}
	finished := js.DrainFinished()
	if len(finished) != 1 || finished[0].ID != 1 {
		t.Fatalf("drained = %+v", finished)
	}
	if again := js.DrainFinished(); len(again) != 0 {
		t.Fatalf("drained twice: %+v", again)
	}
	// The bounded table forgets old finished jobs.
	for i := 0; i < jobHistory+5; i++ {
		j := js.Add("burst")
		j.Finish(Outcome{Status: "ok"})
	}
	if js.Len() > jobHistory {
		t.Fatalf("table grew past the bound: %d", js.Len())
	}
	// Unfinished jobs are never evicted.
	last := js.Add("still running")
	if _, ok := js.Get(last.ID); !ok {
		t.Fatalf("running job evicted")
	}
	if _, ok := js.Get(job.ID); ok {
		t.Fatalf("old finished job not evicted")
	}
}

func TestToolCloseIsIdempotent(t *testing.T) {
	tt := newTestTool(t)
	if err := tt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := tt.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if text, isErr := call(t, tt, `{"code":"1"}`); !isErr || !strings.Contains(text, "closed") {
		t.Fatalf("use after close: isErr=%v text=%q", isErr, text)
	}
}

func mapJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal details: %v", err)
	}
	return b
}
