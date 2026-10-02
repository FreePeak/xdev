package eval

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/tool"
)

// stubTool answers a fixed string so the bridge is tested without the agent
// loop; the policy path itself is covered by internal/agent's bridge tests.
type stubTool struct{ text string }

func (stubTool) Name() string        { return "stub" }
func (stubTool) Description() string { return "stub" }
func (stubTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)
}
func (s stubTool) Execute(_ context.Context, args json.RawMessage) (tool.Result, error) {
	var a struct {
		Q string `json:"q"`
	}
	_ = json.Unmarshal(args, &a)
	return tool.Result{Text: "stub:" + a.Q}, nil
}

// TestCellCallsHostTool is the whole feature in one cell: `tools.x({...})`
// reaches the harness runner and the answer comes back into the cell.
func TestCellCallsHostTool(t *testing.T) {
	k := newTestKernel(t)
	k.SetRunner(func(_ context.Context, name string, args json.RawMessage) (tool.Result, error) {
		return stubTool{}.Execute(context.Background(), args)
	})
	out := runCell(t, k, `r = tools.stub({"q": "hi"}); r["text"]`, DefaultCellTimeout)
	if out.Status != "ok" {
		t.Fatalf("cell status=%q text=%q", out.Status, out.Text)
	}
	if !strings.Contains(out.Text, "stub:hi") {
		t.Fatalf("host tool result did not reach the cell: %q", out.Text)
	}
}

// A second call in the same kernel proves the answer path stays open — the
// first answer must not consume the reader.
func TestCellCallsHostToolTwice(t *testing.T) {
	k := newTestKernel(t)
	k.SetRunner(func(_ context.Context, name string, args json.RawMessage) (tool.Result, error) {
		return stubTool{}.Execute(context.Background(), args)
	})
	out := runCell(t, k, "sum(len(tools.stub({'q': str(i)})['text']) for i in range(3))", DefaultCellTimeout)
	if out.Status != "ok" || !strings.Contains(out.Text, "18") {
		t.Fatalf("three calls: status=%q text=%q", out.Status, out.Text)
	}
}

// No runner means refusal, not a direct call: a bridge that executed tools
// itself would be a policy bypass.
func TestCellWithoutRunnerRefuses(t *testing.T) {
	k := newTestKernel(t)
	out := runCell(t, k, `tools.stub({"q": "x"})`, DefaultCellTimeout)
	if out.Status != "error" || !strings.Contains(out.Text, "no runner installed") {
		t.Fatalf("unwired kernel must refuse: status=%q text=%q", out.Status, out.Text)
	}
}

// A cell must not be able to reach the registry twice over, nor spawn a cell.
func TestCellCannotCallBridgeOrEval(t *testing.T) {
	k := newTestKernel(t)
	k.SetRunner(func(_ context.Context, name string, args json.RawMessage) (tool.Result, error) {
		return stubTool{}.Execute(context.Background(), args)
	})
	for _, name := range []string{"eval", "tool_call"} {
		out := runCell(t, k, `tools.`+name+`({})`, DefaultCellTimeout)
		if out.Status != "error" || !strings.Contains(out.Text, "cannot be called from a cell") {
			t.Fatalf("%s: status=%q text=%q", name, out.Status, out.Text)
		}
	}
}

// The hard one: a cell blocked on a tool that never answers must raise when the
// cell is interrupted, not hang. This is the cancellable slot the design doc
// calls out — a cell waiting on a host that will not reply is a wedged session.
func TestInterruptedCellRaisesWhileWaitingOnTool(t *testing.T) {
	k := newTestKernel(t)
	release := make(chan struct{})
	k.SetRunner(func(ctx context.Context, _ string, _ json.RawMessage) (tool.Result, error) {
		<-release
		return tool.Result{Text: "late"}, nil
	})
	defer close(release)

	done := make(chan Outcome, 1)
	go func() {
		out, err := k.RunCell(context.Background(), `tools.stub({"q": "slow"})`, 0)
		if err != nil {
			t.Errorf("RunCell: %v", err)
		}
		done <- out
	}()
	// Let the cell emit its frame and block on the answer.
	time.Sleep(500 * time.Millisecond)
	o, _ := k.Interrupt()
	if o.ID == 0 {
		t.Fatal("interrupt found no running cell")
	}
	select {
	case got := <-done:
		if strings.Contains(got.Text, "late") {
			t.Fatalf("a cancelled call must not deliver its late answer: %q", got.Text)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the cell hung after the interrupt")
	}
}

// A runner that outlives the cell must see its context cancelled, so a wedged
// tool cannot keep running behind a cancelled cell.
func TestCellCancelAbortsRunningTool(t *testing.T) {
	k := newTestKernel(t)
	aborted := make(chan struct{}, 1)
	k.SetRunner(func(ctx context.Context, _ string, _ json.RawMessage) (tool.Result, error) {
		<-ctx.Done()
		aborted <- struct{}{}
		return tool.Result{}, ctx.Err()
	})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = k.RunCell(ctx, `tools.stub({"q": "x"})`, 0)
	}()
	time.Sleep(500 * time.Millisecond)
	k.Interrupt()
	select {
	case <-aborted:
	case <-time.After(10 * time.Second):
		t.Fatal("the tool context was not cancelled with the cell")
	}
}
