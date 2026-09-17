package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// blockInterceptor refuses one tool name at the extension seam.
type blockInterceptor struct{ name string }

func (b blockInterceptor) ToolCall(_ context.Context, call ai.ToolCallBlock) (json.RawMessage, error) {
	name := call.Name
	if name == b.name {
		return nil, errors.New("blocked by test interceptor")
	}
	return nil, nil
}

func (b blockInterceptor) ToolResult(_ context.Context, _ ai.ToolCallBlock, _ json.RawMessage, _ bool) json.RawMessage {
	return nil
}

func (b blockInterceptor) Emit(context.Context, string, any) {}

// catalogAgent builds an agent whose registry defers the "spy" tool and
// exposes the three bridge tools, wired the way the CLI wires them.
func catalogAgent(t *testing.T, pol tool.ApprovalPolicy, approve ApprovalFunc) (*Agent, *[]string) {
	t.Helper()
	var ran []string
	reg := tool.NewRegistry()
	reg.Register(spyTool{ran: &ran})
	reg.Defer("spy", "records that it ran", "test")
	cat := reg.Catalog()
	reg.Register(tool.NewToolSearchTool(cat))
	reg.Register(tool.NewToolDescribeTool(cat))
	reg.Register(tool.NewToolCallTool(cat))
	a := &Agent{Tools: reg, Hooks: &hookLog{}, Model: "m", Policy: pol, Approve: approve}
	a.WireCatalog(cat)
	return a, &ran
}

// bridgeCall is the model's tool_call payload for the deferred spy tool.
func bridgeCall(args string) ai.ToolCallBlock {
	return ai.ToolCallBlock{
		Name:      tool.ToolCallName,
		Arguments: json.RawMessage(`{"name":"spy","args":` + args + `}`),
	}
}

func TestCatalogBridgeRunsTheDeferredTool(t *testing.T) {
	a, ran := catalogAgent(t, tool.ApprovalPolicy{}, nil)
	msg := a.runOneTool(context.Background(), bridgeCall(`{"command":"ls"}`))
	if msg.IsError {
		t.Fatalf("bridged call failed: %s", msg.Text())
	}
	if !strings.Contains(msg.Text(), "spy ran") {
		t.Fatalf("tool result not relayed: %s", msg.Text())
	}
	if len(*ran) != 1 || !strings.Contains((*ran)[0], "ls") {
		t.Fatalf("deferred tool did not run with its args: %v", *ran)
	}
}

// The approval policy must decide on the REAL tool name: a bridge that
// presented "tool_call" as the subject would let every deny rule be
// sidestepped by moving the call behind it.
func TestCatalogBridgeHonorsTheApprovalPolicy(t *testing.T) {
	pol := tool.ApprovalPolicy{PerTool: map[string]tool.Action{"spy": tool.ActionDeny}}
	a, ran := catalogAgent(t, pol, nil)
	msg := a.runOneTool(context.Background(), bridgeCall(`{}`))
	if !msg.IsError || !strings.Contains(msg.Text(), "denied") {
		t.Fatalf("deny verdict not reported: %s", msg.Text())
	}
	if len(*ran) != 0 {
		t.Fatalf("a denied deferred tool ran: %v", *ran)
	}
	// A prompt verdict nobody can answer (print mode has no user) refuses
	// for the same reason.
	a2, ran2 := catalogAgent(t, tool.ApprovalPolicy{PerTool: map[string]tool.Action{"spy": tool.ActionPrompt}}, nil)
	if msg := a2.runOneTool(context.Background(), bridgeCall(`{}`)); !msg.IsError || len(*ran2) != 0 {
		t.Fatalf("unanswered prompt ran the tool: %s (%v)", msg.Text(), *ran2)
	}
}

func TestCatalogBridgeHonorsTheInterceptorChain(t *testing.T) {
	a, ran := catalogAgent(t, tool.ApprovalPolicy{}, nil)
	a.Intercept = blockInterceptor{name: "spy"}
	msg := a.runOneTool(context.Background(), bridgeCall(`{}`))
	if !msg.IsError || !strings.Contains(msg.Text(), "blocked") {
		t.Fatalf("blocked call not reported: %s", msg.Text())
	}
	if len(*ran) != 0 {
		t.Fatalf("interceptor-blocked deferred tool ran: %v", *ran)
	}
}

// A deferred tool is out of the provider tool schema but named in the prompt
// index — the split that keeps the eager list small without hiding the tool.
func TestDeferredToolIsIndexedNotSchemaed(t *testing.T) {
	a, _ := catalogAgent(t, tool.ApprovalPolicy{}, nil)
	for _, d := range a.toolDefs() {
		if d.Name == "spy" {
			t.Fatal("deferred tool leaked into the provider tool schema")
		}
	}
	idx := BuildDeferredIndex(a.Tools.Deferred())
	if !strings.Contains(idx, "spy: records that it ran") || !strings.Contains(idx, tool.ToolSearchName) {
		t.Fatalf("prompt index does not name the deferred tool:\n%s", idx)
	}
	if BuildDeferredIndex(nil) != "" {
		t.Fatal("a catalog-free prompt must not gain an index section")
	}
	if _, ok := a.Tools.Get(tool.ToolSearchName); !ok {
		t.Fatal("bridge tools missing from the registry")
	}
}
