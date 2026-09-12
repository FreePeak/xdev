package agent

// Deferred-tool bridge wiring (M13 #54). The catalog in internal/tool knows
// which tools are deferred and how to find them; it deliberately holds no
// execution path, so that a bridged call cannot slip past the harness gates.
// This file supplies that path.

import (
	"context"
	"encoding/json"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// WireCatalog points a deferred-tool catalog at this agent's call path. Every
// tool_call the bridge runs then goes through runOneTool exactly as a direct
// call does — plan-mode gate, approval policy, interceptor chain, hooks, TTSR
// reminder — so the policy decides on the deferred tool's own name rather than
// on "tool_call". A nil catalog is a no-op.
func (a *Agent) WireCatalog(c *tool.Catalog) {
	if c == nil {
		return
	}
	c.SetRunner(a.runCatalogCall)
}

// runCatalogCall executes one deferred tool for the bridge: the synthetic call
// carries the INNER name (the catalog only ever bridges deferred tools, and
// never a bridge tool itself), and runOneTool turns it into a tool result
// message, which is unpacked back into a Result for the outer tool_call.
func (a *Agent) runCatalogCall(ctx context.Context, name string, args json.RawMessage) (tool.Result, error) {
	msg := a.runOneTool(ctx, ai.ToolCallBlock{Name: name, Arguments: args})
	res := tool.Result{IsError: msg.IsError, Details: msg.Details}
	for _, b := range msg.Content {
		if tb, ok := b.(ai.TextBlock); ok {
			res.Text += tb.Text
		}
	}
	return res, nil
}
