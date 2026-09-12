// Package share implements the session export and E2E-encrypted share
// operations (M14 #61, research §4): one self-contained HTML document per
// session — inline CSS, no external assets, no JS needed to read it — and a
// sealed snapshot served over loopback whose AES-256-GCM key rides in the
// link fragment, so the server never sees the plaintext.
package share

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/session"
)

// Options tunes how a store becomes a Document.
type Options struct {
	// SystemPrompt is the resolved system prompt of the live session. The
	// store never persists it, so only a caller running the session can
	// supply it; "" omits the section.
	SystemPrompt string
	// Model overrides the model named in the header. "" falls back to the
	// last model_change entry on the path.
	Model string
}

// Document is one session transcript, ready to render.
type Document struct {
	SessionID    string
	Title        string
	Model        string
	CWD          string
	Created      string
	Exported     string
	LeafID       string
	Source       string
	SystemPrompt string
	Entries      []Entry
}

// Entry is one session entry as the export shows it. Kind is the stable
// discriminator (also the CSS class and the data-kind attribute).
type Entry struct {
	ID        string
	Kind      string
	Title     string
	Timestamp string
	Note      string
	Parts     []Part
}

// Part is one rendered piece of an entry body.
type Part struct {
	Kind    string // text | thinking | tool_call | tool_result | data | image
	Text    string
	Name    string
	CallID  string
	IsError bool
}

// Entry kinds.
const (
	KindUser          = "user"
	KindAssistant     = "assistant"
	KindToolResult    = "tool_result"
	KindModelChange   = "model_change"
	KindCompaction    = "compaction"
	KindBranchSummary = "branch_summary"
	KindResetBoundary = "reset_boundary"
	KindCustom        = "custom"
	KindGoal          = "goal"
	KindCheckpoint    = "checkpoint"
	KindUnknown       = "unknown"
)

// FromStore walks every entry in file order. An export is an archive, so it
// keeps the entries the model never sees again — compaction summaries,
// abandoned-branch summaries, custom/hook records, foreign entry types —
// which is exactly what session.BuildContext (the /dump path) drops.
func FromStore(store *session.Store, opts Options) Document {
	doc := Document{
		SessionID:    store.ID(),
		Title:        store.Title(),
		CWD:          store.CWD(),
		LeafID:       store.LeafID(),
		Source:       store.Path(),
		Exported:     stamp(time.Now()),
		Model:        opts.Model,
		SystemPrompt: opts.SystemPrompt,
	}
	derivedModel := ""
	for _, e := range store.Entries() {
		doc.Entries = append(doc.Entries, entryOf(e))
		env := e.Envelope()
		if doc.Created == "" && !env.Timestamp.IsZero() {
			doc.Created = stamp(env.Timestamp)
		}
		// The latest model on record wins: a model_change when the user
		// switched, else the model that produced the last assistant turn.
		switch t := e.(type) {
		case *session.ModelChangeEntry:
			if t.Model != "" {
				derivedModel = t.Model
			}
		case *session.MessageEntry:
			if t.Message.Model != "" {
				derivedModel = t.Message.Model
			}
		}
	}
	if doc.Model == "" {
		doc.Model = derivedModel
	}
	return doc
}

// stamp renders one timestamp for display (UTC, second precision).
func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}

// entryOf converts one store entry into its export shape.
func entryOf(e session.Entry) Entry {
	env := e.Envelope()
	out := Entry{ID: env.ID, Timestamp: stamp(env.Timestamp)}
	switch t := e.(type) {
	case *session.MessageEntry:
		out.Kind, out.Title = messageKind(t.Message.Role)
		out.Parts = messageParts(t.Message)
		if t.Message.IsError {
			out.Note = "error"
		}
	case *session.ModelChangeEntry:
		out.Kind, out.Title, out.Note = KindModelChange, "Model", t.Model
		if t.ResolvedModelIsFallback {
			out.Note += " (fallback)"
		}
	case *session.CompactionEntry:
		out.Kind, out.Title = KindCompaction, "Compaction"
		out.Note = fmt.Sprintf("summary replaces history; ~%d tokens before", t.TokensBefore)
		out.Parts = messageParts(t.Summary)
	case *session.BranchSummaryEntry:
		out.Kind, out.Title = KindBranchSummary, "Branch summary"
		out.Parts = messageParts(t.Summary)
	case *session.ResetBoundaryEntry:
		out.Kind, out.Title = KindResetBoundary, "Reset boundary"
		out.Note = "context cut: entries before this one left the model context"
	case *session.CustomEntry:
		out.Kind, out.Title = KindCustom, "Custom — "+t.CustomType
		out.Note = t.CustomType
		if data, err := json.MarshalIndent(t.Data, "", "  "); err == nil && len(t.Data) > 0 {
			out.Parts = []Part{{Kind: "data", Text: string(data)}}
		}
	case *session.GoalUpdatedEntry:
		out.Kind, out.Title, out.Note = KindGoal, "Goal", t.Goal.Status
		if t.Goal.TokenBudget > 0 {
			out.Note = fmt.Sprintf("%s · %d/%d tokens", t.Goal.Status, t.Goal.Spent, t.Goal.TokenBudget)
		}
		out.Parts = []Part{{Kind: "text", Text: t.Goal.Objective}}
		if len(t.Goal.Evidence) > 0 {
			out.Parts = append(out.Parts, Part{Kind: "data", Text: "evidence:\n- " + strings.Join(t.Goal.Evidence, "\n- ")})
		}
	case *session.CheckpointEntry:
		out.Kind, out.Title, out.Note = KindCheckpoint, "Checkpoint", t.Checkpoint.Name
		out.Parts = []Part{{Kind: "data", Text: "branches from " + t.Checkpoint.EntryID + noteSuffix(t.Checkpoint.Note)}}
	case *session.UnknownEntry:
		out.Kind, out.Title = KindUnknown, "Foreign entry — "+t.EntryType
		out.Note = t.EntryType
		if len(t.Raw) > 0 {
			out.Parts = []Part{{Kind: "data", Text: string(t.Raw)}}
		}
	default:
		out.Kind, out.Title = KindUnknown, "Entry"
	}
	return out
}

func noteSuffix(note string) string {
	if note == "" {
		return ""
	}
	return "\n" + note
}

// messageKind maps a message role to the export kind + heading.
func messageKind(role ai.Role) (kind, title string) {
	switch role {
	case ai.RoleUser:
		return KindUser, "User"
	case ai.RoleAssistant:
		return KindAssistant, "Assistant"
	case ai.RoleToolResult:
		return KindToolResult, "Tool result"
	default:
		return KindUnknown, "Message — " + string(role)
	}
}

// messageParts renders one message's blocks. Every block type the model
// model knows is carried through: text, thinking, tool calls (name +
// arguments), images.
func messageParts(m ai.Message) []Part {
	var parts []Part
	for _, b := range m.Content {
		switch t := b.(type) {
		case ai.TextBlock:
			parts = append(parts, Part{Kind: "text", Text: t.Text})
		case ai.ThinkingBlock:
			parts = append(parts, Part{Kind: "thinking", Text: t.Thinking})
		case ai.ToolCallBlock:
			parts = append(parts, Part{
				Kind:   "tool_call",
				Name:   t.Name,
				CallID: t.ID,
				Text:   indentJSON(t.Arguments),
			})
		case ai.ImageBlock:
			label := t.Source.MediaType
			if label == "" {
				label = t.Source.Type
			}
			parts = append(parts, Part{Kind: "image", Text: "[image: " + label + "]"})
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return parts
}

// indentJSON pretty-prints tool arguments; anything that is not JSON is
// passed through verbatim (a partial stream can leave a truncated object).
func indentJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

// Render writes the self-contained HTML export. html/template escapes every
// interpolated value, so markup inside a message renders as literal text.
func Render(doc Document) (string, error) {
	var b bytes.Buffer
	if err := exportTmpl.Execute(&b, doc); err != nil {
		return "", fmt.Errorf("share: render: %w", err)
	}
	return b.String(), nil
}

// Export renders the session and writes it to path. An empty path uses the
// default ~/.xdev/agent/exports/<shortid>.html (mirroring /dump's dumps
// dir); a missing parent directory is created. It returns the written path.
func Export(store *session.Store, opts Options, path string) (string, error) {
	html, err := Render(FromStore(store, opts))
	if err != nil {
		return "", err
	}
	if path == "" {
		dir := filepath.Join(config.DataDir(), "exports")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		id := store.ID()
		if len(id) > 8 {
			id = id[:8]
		}
		path = filepath.Join(dir, id+".html")
	} else if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(path, []byte(html), 0o644); err != nil {
		return "", err
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs, nil
	}
	return path, nil
}
