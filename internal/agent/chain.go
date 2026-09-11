package agent

import (
	"context"
	"encoding/json"
)

// InterceptorChain routes one interception point through several
// interceptors (hooks bus, extension manager) in order: the first denial
// on ToolCall short-circuits; ToolResult patches apply in sequence.
// An empty chain is a nil Interceptor for all purposes (nil-safe methods).
type InterceptorChain []Interceptor

func (c InterceptorChain) ToolCall(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	for _, i := range c {
		revised, err := i.ToolCall(ctx, name, args)
		if err != nil {
			return nil, err
		}
		args = revised
	}
	return args, nil
}

func (c InterceptorChain) ToolResult(ctx context.Context, name string, args, result json.RawMessage) json.RawMessage {
	for _, i := range c {
		if patched := i.ToolResult(ctx, name, args, result); len(patched) > 0 {
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
