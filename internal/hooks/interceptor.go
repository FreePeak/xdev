package hooks

import (
	"context"
	"encoding/json"
)

// Interceptor adapter: the Bus rides the same agent.Interceptor surface
// as extension managers, so the two compose through an interceptor chain.
// tool_call pre-hooks are fail-closed; tool_result hooks may rewrite the
// rendered text; Emit runs the event as a notification.

func (b *Bus) ToolCall(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	payload := map[string]any{"tool": name, "input": json.RawMessage(args)}
	res, err := b.Run(ctx, "tool_call", payload)
	if err != nil {
		return nil, err
	}
	if repl, ok := res["input"]; ok {
		out, err := json.Marshal(repl)
		if err != nil {
			return nil, err
		}
		return out, nil
	}
	return args, nil
}

func (b *Bus) ToolResult(ctx context.Context, name string, args, result json.RawMessage) json.RawMessage {
	res, err := b.Run(ctx, "tool_result", map[string]any{
		"tool": name, "args": json.RawMessage(args), "text": json.RawMessage(result),
	})
	if err != nil {
		return result // post-hooks never block
	}
	if text, ok := res["text"].(string); ok {
		return json.RawMessage(text)
	}
	return result
}

func (b *Bus) Emit(ctx context.Context, event string, payload any) {
	b.Notify(ctx, event, payload)
}
