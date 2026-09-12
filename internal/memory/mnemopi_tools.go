package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/FreePeak/xdev/internal/tool"
)

// The mnemopi tools (M12 #44): recall / retain / reflect / memory_edit.
// They are registered additively, only when the mnemopi backend is the
// configured one. Every method is off-safe: a nil or disabled backend
// answers with an explanatory error instead of panicking, so a partially
// wired registry can never take the session down.

// MnemopiRecallTool is the model-facing polyphonic recall.
type MnemopiRecallTool struct{ Mem *Mnemopi }

// MnemopiRetainTool records one memory explicitly.
type MnemopiRetainTool struct{ Mem *Mnemopi }

// MnemopiReflectTool runs the synthesis pass over the recent memories.
type MnemopiReflectTool struct{ Mem *Mnemopi }

// MemoryEditTool applies a bounded edit to a stored memory.
type MemoryEditTool struct{ Mem *Mnemopi }

// Tool names.
const (
	MnemopiRecallToolName  = "recall"
	MnemopiRetainToolName  = "retain"
	MnemopiReflectToolName = "reflect"
	MemoryEditToolName     = "memory_edit"
)

// mem returns the backend or nil when it is unusable.
func (t *MnemopiRecallTool) mem() *Mnemopi {
	if t == nil || t.Mem.Off() {
		return nil
	}
	return t.Mem
}

func (t *MnemopiRetainTool) mem() *Mnemopi {
	if t == nil || t.Mem.Off() {
		return nil
	}
	return t.Mem
}

func (t *MnemopiReflectTool) mem() *Mnemopi {
	if t == nil || t.Mem.Off() {
		return nil
	}
	return t.Mem
}

func (t *MemoryEditTool) mem() *Mnemopi {
	if t == nil || t.Mem.Off() {
		return nil
	}
	return t.Mem
}

// offErr is the shared "backend is not enabled" answer.
func offErr(name string) (tool.Result, error) {
	return tool.Result{Text: name + ": memory backend is not enabled (set memory: mnemopi in settings)", IsError: true}, nil
}

func (t *MnemopiRecallTool) Name() string { return MnemopiRecallToolName }

func (t *MnemopiRecallTool) Description() string {
	return "search long-term memory: fused keyword, tag, link-graph, recency and fact-type recall; use before re-deriving something the project may already remember"
}

func (t *MnemopiRecallTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "what to look for (words, tags, or a question)"},
    "limit": {"type": "integer", "description": "max memories to return (default from memory.mnemopi.recallLimit, hard max 32)"}
  },
  "required": ["query"]
}`)
}

func (t *MnemopiRecallTool) Execute(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	m := t.mem()
	if m == nil {
		return offErr(MnemopiRecallToolName)
	}
	var a struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return tool.Result{Text: "recall: malformed arguments: " + err.Error(), IsError: true}, nil
	}
	if strings.TrimSpace(a.Query) == "" {
		return tool.Result{Text: "recall: query is required", IsError: true}, nil
	}
	hits, err := m.Recall(ctx, a.Query, a.Limit)
	if err != nil {
		return tool.Result{Text: "recall: " + err.Error(), IsError: true}, nil
	}
	if len(hits) == 0 {
		return tool.Result{Text: "recall: no matching memories"}, nil
	}
	return tool.Result{
		Text:    renderHits(hits, m.injectionChars()),
		Details: map[string]any{"hits": len(hits)},
	}, nil
}

func (t *MnemopiRetainTool) Name() string { return MnemopiRetainToolName }

func (t *MnemopiRetainTool) Description() string {
	return "store durable memories (facts, lessons, decisions) with tags so later sessions recall them; batch related items in one call; use for project conventions, non-obvious fixes, and user preferences"
}

func (t *MnemopiRetainTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "items": {
      "type": "array",
      "description": "memories to retain (the reference backend's shape)",
      "items": {
        "type": "object",
        "properties": {
          "content": {"type": "string", "description": "information to remember"},
          "context": {"type": "string", "description": "source context"}
        },
        "required": ["content"]
      }
    },
    "text": {"type": "string", "description": "shorthand for a single memory (equivalent to one items entry)"},
    "kind": {"type": "string", "enum": ["fact", "lesson", "reflection"], "description": "default fact"},
    "tags": {"type": "array", "items": {"type": "string"}, "description": "up to 8 short tags used by tag recall and proactive linking"},
    "context": {"type": "string", "description": "optional: what prompted this memory (task, file, error)"}
  }
}`)
}

// mnemopiRetainItem is one memory in the retain batch.
type mnemopiRetainItem struct {
	Text   string
	Source string
}

func (t *MnemopiRetainTool) Execute(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	m := t.mem()
	if m == nil {
		return offErr(MnemopiRetainToolName)
	}
	var a struct {
		Items []struct {
			Content string `json:"content"`
			Context string `json:"context"`
		} `json:"items"`
		Text    string   `json:"text"`
		Kind    string   `json:"kind"`
		Tags    []string `json:"tags"`
		Context string   `json:"context"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return tool.Result{Text: "retain: malformed arguments: " + err.Error(), IsError: true}, nil
	}
	items := make([]mnemopiRetainItem, 0, len(a.Items)+1)
	for _, it := range a.Items {
		items = append(items, mnemopiRetainItem{Text: it.Content, Source: it.Context})
	}
	if strings.TrimSpace(a.Text) != "" {
		items = append(items, mnemopiRetainItem{Text: a.Text, Source: a.Context})
	}
	if len(items) == 0 {
		return tool.Result{Text: "retain: items or text is required", IsError: true}, nil
	}
	ids := make([]int64, 0, len(items))
	deduped, linked := 0, 0
	for _, it := range items {
		res, err := m.Retain(ctx, Fact{Kind: a.Kind, Text: it.Text, Tags: a.Tags, Source: it.Source})
		if err != nil {
			// Partial success is reported honestly: what landed stays.
			return tool.Result{
				Text:    fmt.Sprintf("retain: %d of %d stored, then %v", len(ids), len(items), err),
				Details: map[string]any{"ids": ids},
				IsError: len(ids) == 0,
			}, nil
		}
		ids = append(ids, res.ID)
		if res.Deduped {
			deduped++
		}
		linked += res.Linked
	}
	fact, err := m.FactByID(ctx, ids[len(ids)-1])
	bank := ""
	if err == nil {
		bank = fact.Bank
	}
	out := fmt.Sprintf("retained %d memories in bank %s (ids: %s)", len(ids), bank, joinIDs(ids))
	if deduped > 0 {
		out += fmt.Sprintf("; %d already held that text (refreshed)", deduped)
	}
	if linked > 0 {
		out += fmt.Sprintf("; %d proactive links", linked)
	}
	return tool.Result{Text: out, Details: map[string]any{"ids": ids, "bank": bank, "linked": linked}}, nil
}

// joinIDs renders ids as "12, 13" for the tool receipt.
func joinIDs(ids []int64) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return strings.Join(parts, ", ")
}

func (t *MnemopiReflectTool) Name() string { return MnemopiReflectToolName }

func (t *MnemopiReflectTool) Description() string {
	return "consolidate recent memories into one durable synthesis (reviewer-model pass) and store it; use when memories accumulated or contradict each other"
}

func (t *MnemopiReflectTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "optional focus for the synthesis (defaults to the recent memories as a whole)"},
    "topic": {"type": "string", "description": "alias of query"}
  }
}`)
}

func (t *MnemopiReflectTool) Execute(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	m := t.mem()
	if m == nil {
		return offErr(MnemopiReflectToolName)
	}
	var a struct {
		// Query is the reference backend's argument name; topic is kept as
		// an alias so a model that reached for either still works.
		Query string `json:"query"`
		Topic string `json:"topic"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return tool.Result{Text: "reflect: malformed arguments: " + err.Error(), IsError: true}, nil
	}
	topic := a.Query
	if strings.TrimSpace(topic) == "" {
		topic = a.Topic
	}
	fact, err := m.Reflect(ctx, topic)
	if err != nil {
		return tool.Result{Text: "reflect: " + err.Error(), IsError: true}, nil
	}
	return tool.Result{
		Text:    fmt.Sprintf("reflection #%d stored in bank %s:\n%s", fact.ID, fact.Bank, fact.Text),
		Details: map[string]any{"id": fact.ID, "bank": fact.Bank},
	}, nil
}

func (t *MemoryEditTool) Name() string { return MemoryEditToolName }

func (t *MemoryEditTool) Description() string {
	return "correct or delete one stored memory by id (bounded: text <= 4000 chars, <= 8 tags); use to fix a wrong or stale memory instead of storing a contradicting duplicate"
}

func (t *MemoryEditTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "_op": {"type": "string", "enum": ["update", "forget"], "description": "operation; update is the default, forget deletes"},
    "id": {"type": "integer", "description": "memory id as returned by retain/reflect/recall or read via memory://<id>"},
    "text": {"type": "string", "description": "replacement text (<= 4000 chars)"},
    "tags": {"type": "array", "items": {"type": "string"}, "description": "replacement tag list (<= 8 tags, <= 32 chars each)"},
    "kind": {"type": "string", "enum": ["fact", "lesson", "reflection", "summary"]},
    "delete": {"type": "boolean", "description": "delete this memory instead of editing it"}
  },
  "required": ["id"]
}`)
}

func (t *MemoryEditTool) Execute(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	m := t.mem()
	if m == nil {
		return offErr(MemoryEditToolName)
	}
	var a struct {
		// Op mirrors the reference backend's _op (update|forget);
		// invalidate has no counterpart here (this store keeps no
		// supersession history) and is refused explicitly.
		Op     string    `json:"_op"`
		ID     int64     `json:"id"`
		Text   *string   `json:"text"`
		Tags   *[]string `json:"tags"`
		Kind   *string   `json:"kind"`
		Delete bool      `json:"delete"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return tool.Result{Text: "memory_edit: malformed arguments: " + err.Error(), IsError: true}, nil
	}
	switch a.Op {
	case "", "update":
	case "forget":
		a.Delete = true
	case "invalidate":
		return tool.Result{Text: "memory_edit: _op invalidate is not supported (this store keeps no supersession history); use update or forget", IsError: true}, nil
	default:
		return tool.Result{Text: "memory_edit: unknown _op " + a.Op + " (update|forget)", IsError: true}, nil
	}
	if a.Text == nil && a.Tags == nil && a.Kind == nil && !a.Delete {
		return tool.Result{Text: "memory_edit: nothing to change (give text, tags, kind, or delete: true)", IsError: true}, nil
	}
	fact, err := m.MemoryEdit(ctx, FactEdit{ID: a.ID, Text: a.Text, Tags: a.Tags, Kind: a.Kind, Delete: a.Delete})
	if err != nil {
		return tool.Result{Text: "memory_edit: " + err.Error(), IsError: true}, nil
	}
	if a.Delete {
		return tool.Result{Text: fmt.Sprintf("memory #%d deleted", a.ID), Details: map[string]any{"id": a.ID, "deleted": true}}, nil
	}
	out := fmt.Sprintf("memory #%d updated", fact.ID)
	if len(fact.Tags) > 0 {
		out += " (tags: " + strings.Join(fact.Tags, ", ") + ")"
	}
	return tool.Result{Text: out + ":\n" + fact.Text, Details: map[string]any{"id": fact.ID, "text": fact.Text}}, nil
}
