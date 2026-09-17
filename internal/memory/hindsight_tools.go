package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/FreePeak/xdev/internal/tool"
)

// The Hindsight backend exposes the model-facing trio omp documents:
// recall, retain, reflect. memory_edit is deliberately absent — upstream
// Hindsight memories are not edited through this backend.

// RecallTool is the model-facing `recall`.
type RecallTool struct{ Backend *Hindsight }

const (
	RecallToolName  = "recall"
	RetainToolName  = "retain"
	ReflectToolName = "reflect"
)

func (t *RecallTool) Name() string { return RecallToolName }

func (t *RecallTool) Description() string {
	return "search long-term memory (Hindsight) for relevant facts, decisions, and prior work; use before assuming history is unknown"
}

func (t *RecallTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "what to recall; natural language"}
  },
  "required": ["query"]
}`)
}

func (t *RecallTool) Execute(_ context.Context, args json.RawMessage) (tool.Result, error) {
	var a struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return tool.Result{Text: "recall: malformed arguments: " + err.Error(), IsError: true}, nil
	}
	if strings.TrimSpace(a.Query) == "" {
		return tool.Result{Text: "recall: query is required", IsError: true}, nil
	}
	if t.Backend == nil || t.Backend.Off() {
		return tool.Result{Text: "recall: hindsight backend is not configured", IsError: true}, nil
	}
	notes, err := t.Backend.Recall(a.Query)
	if err != nil {
		return tool.Result{Text: "recall: " + err.Error(), IsError: true}, nil
	}
	if len(notes) == 0 {
		return tool.Result{Text: "recall: nothing found for " + oneLine(a.Query)}, nil
	}
	var b strings.Builder
	for _, n := range notes {
		txt := collapseWS(n.Text)
		if txt == "" {
			continue
		}
		b.WriteString("- " + txt + "\n")
	}
	bank, tag := t.Backend.Scope()
	head := fmt.Sprintf("recall: %d memory(ies) from bank %s", len(notes), bank)
	if tag != "" {
		head += " (" + tag + ")"
	}
	return tool.Result{Text: head + "\n" + strings.TrimSpace(b.String())}, nil
}

// RetainTool is the model-facing `retain`.
type RetainTool struct{ Backend *Hindsight }

func (t *RetainTool) Name() string { return RetainToolName }

func (t *RetainTool) Description() string {
	return "store a durable fact, decision, or preference in long-term memory (Hindsight); use for anything a future session should know"
}

func (t *RetainTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "text": {"type": "string", "description": "what to remember: the fact, when it applies, and why"}
  },
  "required": ["text"]
}`)
}

func (t *RetainTool) Execute(_ context.Context, args json.RawMessage) (tool.Result, error) {
	var a struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return tool.Result{Text: "retain: malformed arguments: " + err.Error(), IsError: true}, nil
	}
	if strings.TrimSpace(a.Text) == "" {
		return tool.Result{Text: "retain: text is required", IsError: true}, nil
	}
	if t.Backend == nil || t.Backend.Off() {
		return tool.Result{Text: "retain: hindsight backend is not configured", IsError: true}, nil
	}
	if err := t.Backend.Retain(a.Text); err != nil {
		return tool.Result{Text: "retain: " + err.Error(), IsError: true}, nil
	}
	bank, tag := t.Backend.Scope()
	out := "retain: stored in bank " + bank
	if tag != "" {
		out += " (" + tag + ")"
	}
	return tool.Result{Text: out}, nil
}

// ReflectTool is the model-facing `reflect`.
type ReflectTool struct{ Backend *Hindsight }

func (t *ReflectTool) Name() string { return ReflectToolName }

func (t *ReflectTool) Description() string {
	return "ask long-term memory (Hindsight) to answer a question from what it has stored; slower than recall, use when recall results need synthesis"
}

func (t *ReflectTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "the question to answer from memory"}
  },
  "required": ["query"]
}`)
}

func (t *ReflectTool) Execute(_ context.Context, args json.RawMessage) (tool.Result, error) {
	var a struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return tool.Result{Text: "reflect: malformed arguments: " + err.Error(), IsError: true}, nil
	}
	if strings.TrimSpace(a.Query) == "" {
		return tool.Result{Text: "reflect: query is required", IsError: true}, nil
	}
	if t.Backend == nil || t.Backend.Off() {
		return tool.Result{Text: "reflect: hindsight backend is not configured", IsError: true}, nil
	}
	text, err := t.Backend.Reflect(a.Query)
	if err != nil {
		return tool.Result{Text: "reflect: " + err.Error(), IsError: true}, nil
	}
	if text == "" {
		text = "(the server returned no answer)"
	}
	return tool.Result{Text: text}, nil
}
