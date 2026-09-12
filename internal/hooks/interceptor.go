package hooks

import (
	"context"
	"encoding/json"
	"strings"
)

// Interceptor adapter: the Bus rides the same agent.Interceptor surface
// as extension managers, so the two compose through an interceptor chain.
// tool_call pre-hooks are fail-closed; tool_result hooks may rewrite the
// rendered text; Emit runs the event as a notification.

func (b *Bus) ToolCall(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	// omp's tool_call payload names (toolName/input): a hook ported from the
	// baseline reads event.toolName. xdev's old `tool` key silently matched
	// nothing (parity finding T3 #14). `toolCallId` is the remaining gap —
	// it needs the agent.Interceptor signature widened to carry the call,
	// which no hook needs yet; recorded in docs/parity-delta.md.
	payload := map[string]any{"toolName": name, "input": json.RawMessage(args)}
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
	// omp's tool_result payload: toolName/input/content/isError. The result
	// the agent hands over is the rendered {text,isError} envelope, so
	// unpack it here rather than leaking that shape to hook authors.
	content, isError := unwrapResult(result)
	res, err := b.Run(ctx, "tool_result", map[string]any{
		"toolName": name, "input": json.RawMessage(args),
		"content": content, "isError": isError,
	})
	if err != nil {
		return result // post-hooks never block
	}
	// Mutations arrive as content (omp's key, a string or a block list) or
	// the legacy text; any of them replaces the rendered result.
	if patched, ok := mutationText(res, "content"); ok {
		return encodePatched(patched, res, result)
	}
	if text, ok := res["text"].(string); ok {
		return json.RawMessage(text)
	}
	return result
}

func (b *Bus) Emit(ctx context.Context, event string, payload any) {
	b.Notify(ctx, event, payload)
}

// unwrapResult splits the agent's rendered tool result ({"text":…,"isError":…})
// into the shape omp's tool_result hook payload uses. An undecodable payload
// is passed through as raw text so a custom renderer never loses content.
func unwrapResult(result json.RawMessage) (any, bool) {
	var p struct {
		Text    string `json:"text"`
		IsError bool   `json:"isError"`
	}
	if err := json.Unmarshal(result, &p); err != nil {
		return string(result), false
	}
	return []map[string]any{{"type": "text", "text": p.Text}}, p.IsError
}

// mutationText pulls replacement text out of a hook's answer under key k:
// either a plain string or omp's content block list.
func mutationText(res map[string]any, k string) (string, bool) {
	switch v := res[k].(type) {
	case string:
		return v, true
	case []any:
		var sb strings.Builder
		for _, b := range v {
			if m, ok := b.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					sb.WriteString(t)
				}
			}
		}
		return sb.String(), true
	}
	return "", false
}

// encodePatched re-renders the agent's result envelope with replaced text,
// keeping the isError flag unless the hook supplied one.
func encodePatched(text string, res map[string]any, original json.RawMessage) json.RawMessage {
	isErr := false
	if v, ok := res["isError"].(bool); ok {
		isErr = v
	} else {
		var p struct {
			IsError bool `json:"isError"`
		}
		_ = json.Unmarshal(original, &p)
		isErr = p.IsError
	}
	out, err := json.Marshal(map[string]any{"text": text, "isError": isErr})
	if err != nil {
		return original
	}
	return out
}
