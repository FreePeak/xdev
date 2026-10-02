package agent

// Eval-kernel tool bridge (M13 #268): the eval cell calls the harness's tools.
//
// The catalog bridge (WireCatalog) and this one are the same contract. A cell
// that could execute a tool itself would skip the plan-mode gate, the approval
// policy and the hook chain, so the kernel holds no execution path: it frames
// the call and hands it back here, where it becomes an ordinary runOneTool.

import (
	"context"
	"encoding/json"

	"github.com/FreePeak/xdev/internal/eval"
	"github.com/FreePeak/xdev/internal/tool"
)

// WireEvalKernel points the eval kernel's `tools` object at this agent's call
// path, for every eval tool in reg. A cell then calls any registered tool the
// same way the model does, minus the round trip: the answer comes back into the
// cell instead of into a new assistant message.
//
// The kernel refuses `eval` itself and the catalog bridge tools by name, so a
// cell can neither spawn a cell nor reach the registry twice over. No kernel,
// or a registry with no eval tool, is a no-op.
func (a *Agent) WireEvalKernel(reg *tool.Registry) {
	if reg == nil {
		return
	}
	if t, ok := reg.Get("eval"); ok {
		if et, ok := t.(*eval.Tool); ok && et.Kernel != nil {
			et.Kernel.SetRunner(a.runEvalTool)
		}
	}
}

// runEvalTool executes one tool a cell asked for. It is runCatalogCall with
// one difference that matters: the args come from a Python dict rather than
// from a model, so malformed JSON is refused here instead of reaching
// runOneTool, which would report a decode error the cell cannot act on.
func (a *Agent) runEvalTool(ctx context.Context, name string, args json.RawMessage) (tool.Result, error) {
	if len(args) > 0 && !json.Valid(args) {
		return tool.Result{Text: "eval: tools." + name + ": arguments are not valid JSON", IsError: true}, nil
	}
	return a.runCatalogCall(ctx, name, args)
}
