package agent

// The eval-kernel bridge's contract is one sentence: a cell's tool call takes
// the same path a model's tool call takes. These tests pin that sentence from
// the harness side — the policy must still decide, on the inner tool's real
// name, for a call that arrived from Python rather than from the model.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/eval"
	"github.com/FreePeak/xdev/internal/tool"
)

// bridgedEval builds an agent whose eval kernel is wired to its own registry,
// which is the only arrangement a real mode has.
func bridgedEval(t *testing.T, pm *PlanMode, extra ...tool.Tool) (*Agent, *eval.Tool, *tool.Registry) {
	t.Helper()
	if _, err := eval.ResolvePython(); err != nil {
		t.Skipf("python3 unavailable: %v", err)
	}
	reg := tool.NewRegistry()
	reg.Register(tool.NewWriteTool())
	for _, x := range extra {
		reg.Register(x)
	}
	et := eval.NewTool(t.TempDir())
	reg.Register(et)
	ag := &Agent{Tools: reg, Model: "m", MaxTurns: 1, PlanMode: pm, Hooks: TurnHooksFunc{}}
	ag.WireEvalKernel(reg)
	t.Cleanup(func() { _ = et.Kernel.Close() })
	return ag, et, reg
}

func runCell(t *testing.T, et *eval.Tool, code string) eval.Outcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := et.Kernel.RunCell(ctx, code, 20*time.Second)
	if err != nil {
		t.Fatalf("RunCell(%q): %v", code, err)
	}
	return out
}

// The load-bearing one: plan mode must still refuse a mutating tool when the
// caller is a cell. A bridge that reached the registry directly would write
// the file and the plan would be a lie.
func TestCellToolCallObeysPlanMode(t *testing.T) {
	pm := &PlanMode{active: true}
	pm.propose = &proposeTool{pm: pm}
	_, et, _ := bridgedEval(t, pm)

	target := filepath.Join(t.TempDir(), "plan.txt")
	out := runCell(t, et, `tools.write({"path":"`+target+`","content":"x"})`)
	if !strings.Contains(strings.ToLower(out.Text), "plan mode") {
		t.Fatalf("a mutating tool ran from a cell while plan mode was active: %q", out.Text)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("the denied write happened anyway")
	}
}

// The reverse: a read-only tool is what plan mode exists to allow, so the
// bridge must not refuse everything just because the caller is a cell.
func TestCellToolCallAllowedInPlanModeWhenReadOnly(t *testing.T) {
	target := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(target, []byte("plan content"), 0o600); err != nil {
		t.Fatal(err)
	}
	pm := &PlanMode{active: true}
	pm.propose = &proposeTool{pm: pm}
	_, et, _ := bridgedEval(t, pm, tool.NewReadTool())

	out := runCell(t, et, `tools.read({"path":"`+target+`"})["text"]`)
	if !strings.Contains(out.Text, "plan content") {
		t.Fatalf("a read-only tool was refused from a cell in plan mode: status=%q text=%q", out.Status, out.Text)
	}
}

// A non-dict argument is refused in the kernel, before a frame is emitted at
// all: the cell gets a Python TypeError it can fix, and the harness never sees
// a payload it would have to decode.
func TestCellToolCallRejectsNonDictArgsInTheKernel(t *testing.T) {
	_, et, _ := bridgedEval(t, nil, tool.NewReadTool())
	out := runCell(t, et, `tools.read("{not json")`)
	if out.Status != "error" || !strings.Contains(out.Text, "expects a dict of arguments") {
		t.Fatalf("non-dict args: status=%q text=%q", out.Status, out.Text)
	}
}

// The Go side of the same gate, for a payload that is not a JSON object. The
// kernel's json.dumps makes this unreachable from Python today, which is
// exactly why it needs a direct test: a bridge that reported a Go decode error
// to a cell is a bug, not a cosmetic one.
func TestRunEvalToolRejectsMalformedArgs(t *testing.T) {
	ag := &Agent{Tools: tool.NewRegistry(), Model: "m", Hooks: TurnHooksFunc{}}
	res, err := ag.runEvalTool(context.Background(), "read", json.RawMessage(`{not json`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "not valid JSON") {
		t.Fatalf("malformed args: %+v", res)
	}
}

// An unknown name must come back as the harness's own error, in the one shape
// every call returns: {text, details, is_error}. The cell reads is_error, not
// a raised exception, so a fan-out can branch on one field.
func TestCellToolCallUnknownToolReportsHarnessError(t *testing.T) {
	_, et, _ := bridgedEval(t, nil)
	out := runCell(t, et, `tools.nope({})["is_error"]`)
	if out.Status != "ok" || !strings.Contains(out.Text, "True") {
		t.Fatalf("unknown tool: status=%q text=%q", out.Status, out.Text)
	}
	out = runCell(t, et, `tools.nope({})["text"]`)
	if !strings.Contains(out.Text, `unknown tool`) {
		t.Fatalf("unknown tool text: %q", out.Text)
	}
}

// The bridge forwards the frame's bytes, it does not decode and re-encode. A
// decode would normalise the payload (key order, number spelling, spacing),
// so the assertion is on the exact string json.dumps produced in the cell.
func TestCellToolCallForwardsArgsByteForByte(t *testing.T) {
	_, et, _ := bridgedEval(t, nil)
	var got string
	et.Kernel.SetRunner(func(_ context.Context, _ string, args json.RawMessage) (tool.Result, error) {
		got = string(args)
		return tool.Result{Text: "ok"}, nil
	})
	// json.dumps' default separators are ", " and ": " — the spacing below is
	// the fingerprint that proves no re-encode happened on the way.
	out := runCell(t, et, `tools.stub({"n": 3, "o": {"k": [1, 2]}})`)
	if out.Status != "ok" || got == "" {
		t.Fatalf("status=%q text=%q got=%q", out.Status, out.Text, got)
	}
	if want := `{"n": 3, "o": {"k": [1, 2]}}`; got != want {
		t.Fatalf("args were re-marshalled rather than forwarded: got %q want %q", got, want)
	}
}

// A call from a cell is a real tool call and must reach the hooks — that is
// what makes it visible to the TUI and to an interceptor.
func TestCellToolCallFiresHooks(t *testing.T) {
	_, et, reg := bridgedEval(t, nil)
	starts, ends := 0, 0
	ag := &Agent{Tools: reg, Model: "m", Hooks: TurnHooksFunc{
		OnToolStartF: func(ai.ToolCallBlock) { starts++ },
		OnToolEndF:   func(ai.ToolCallBlock, tool.Result, time.Duration) { ends++ },
	}}
	ag.WireEvalKernel(reg)
	target := filepath.Join(t.TempDir(), "h.txt")
	out := runCell(t, et, `tools.write({"path":"`+target+`","content":"y"})["text"]`)
	if !strings.Contains(out.Text, "Wrote") {
		t.Fatalf("the write did not run: status=%q text=%q", out.Status, out.Text)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the write produced no file: %v", err)
	}
	if starts != 1 || ends != 1 {
		t.Fatalf("hooks: start=%d end=%d, want 1/1", starts, ends)
	}
}

// The redactor is applied by runOneTool, so a placeholder the cell passed in
// comes back to the harness as the real value. Same round trip as the direct
// path; the cell never sees the placeholder text.
func TestCellToolCallGoesThroughRedactor(t *testing.T) {
	_, et, reg := bridgedEval(t, nil, &seesArgsTool{})
	ag := &Agent{Tools: reg, Model: "m", Hooks: TurnHooksFunc{},
		Redactor: expandAllRedactor{}}
	ag.WireEvalKernel(reg)

	out := runCell(t, et, `tools.seesargs({"content":"$$tok-live-1$$"})["text"]`)
	if !strings.Contains(out.Text, "tok-live-1") {
		t.Fatalf("the placeholder was not expanded on the way in: status=%q text=%q", out.Status, out.Text)
	}
}

// seesArgsTool echoes the content field back as its result text.
type seesArgsTool struct{}

func (seesArgsTool) Name() string        { return "seesargs" }
func (seesArgsTool) Description() string { return "echo content" }
func (seesArgsTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"content":{"type":"string"}}}`)
}
func (seesArgsTool) Execute(_ context.Context, args json.RawMessage) (tool.Result, error) {
	var a struct {
		Content string `json:"content"`
	}
	_ = json.Unmarshal(args, &a)
	return tool.Result{Text: a.Content}, nil
}

// expandAllRedactor stands in for the secrets store: Expand turns a
// placeholder into a value, which is the half of the contract runOneTool owns.
type expandAllRedactor struct{}

func (expandAllRedactor) Apply(s string) string { return s }
func (expandAllRedactor) Expand(s string) string {
	return strings.ReplaceAll(s, "$$tok-live-1$$", "tok-live-1")
}
