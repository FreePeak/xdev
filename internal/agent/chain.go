package agent

import (
	"context"
	"encoding/json"
	"github.com/FreePeak/xdev/internal/ai"
)

// InterceptorChain routes one interception point through several
// interceptors (hooks bus, extension manager) in order: the first denial
// on ToolCall short-circuits; ToolResult patches apply in sequence.
// An empty chain is a nil Interceptor for all purposes (nil-safe methods).
type InterceptorChain []Interceptor

func (c InterceptorChain) ToolCall(ctx context.Context, call ai.ToolCallBlock) (json.RawMessage, error) {
	args := call.Arguments
	for _, i := range c {
		revised, err := i.ToolCall(ctx, call)
		if err != nil {
			return nil, err
		}
		args = revised
	}
	return args, nil
}

func (c InterceptorChain) ToolResult(ctx context.Context, call ai.ToolCallBlock, result json.RawMessage, isError bool) json.RawMessage {
	for _, i := range c {
		if patched := i.ToolResult(ctx, call, result, isError); len(patched) > 0 {
			result = patched
		}
	}
	return result
}

func (c InterceptorChain) Emit(ctx context.Context, event string, payload any) {
	for _, i := range c {
		i.Emit(ctx, event, payload)
	}
}

// NewChain drops nil interceptors and returns nil for an empty chain so
// the agent's nil-check on Intercept keeps working.
func NewChain(parts ...Interceptor) Interceptor {
	var c InterceptorChain
	for _, p := range parts {
		if p != nil {
			c = append(c, p)
		}
	}
	if len(c) == 0 {
		return nil
	}
	return c
}
