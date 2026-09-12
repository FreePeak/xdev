package tool

// Checkpoint/rewind (M13 #51). A checkpoint is a named bookmark entry on the
// session tree (session.CheckpointEntry); rewind re-points the leaf at the
// bookmarked entry with the store's existing branch machinery and appends a
// branch summary carrying the caller's report. Nothing is deleted: the
// abandoned exploration leaves the active path (so it is absent from the
// next provider call) but stays in the session file, and the branch marker
// makes the moved leaf replay on reopen.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// Bounds: every caller-supplied string is clipped before it becomes a
// session entry, and the list is capped newest-first.
const (
	checkpointNameMax = 200
	checkpointNoteMax = 1000
	rewindReportMax   = 2000
	checkpointListMax = 50
)

// checkpointStore is the session surface the checkpoint tools need.
// *session.Store implements it.
type checkpointStore interface {
	Append(session.Entry) error
	Entries() []session.Entry
	LeafID() string
	Branch(entryID string) error
}

// CheckpointTool records named checkpoints (op create|list). All state lives
// in the session entries, so the tool itself is stateless.
type CheckpointTool struct {
	// Store is the live session; nil answers "no session store".
	Store checkpointStore
}

// RewindTool branches the session back to a named checkpoint.
type RewindTool struct {
	Store checkpointStore
	// Running reports whether a turn is executing. Rewind is refused while
	// it returns true: the leaf would move under a live turn, whose
	// in-flight history is rebuilt from the store. Nil means no probe is
	// wired and rewind proceeds — see WireCheckpoint for the wiring point.
	Running func() bool
}

var (
	_ Tool = (*CheckpointTool)(nil)
	_ Tool = (*RewindTool)(nil)
)

// CheckpointToolName / RewindToolName are the model-facing tool names.
const (
	CheckpointToolName = "checkpoint"
	RewindToolName     = "rewind"
)

// Name implements Tool.
func (t *CheckpointTool) Name() string { return CheckpointToolName }

// Description implements Tool.
func (t *CheckpointTool) Description() string {
	return "Record a named checkpoint of the current session state (op create|list); rewind branches back to it."
}

// Parameters implements Tool.
func (t *CheckpointTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "op": {"type": "string", "enum": ["create", "list"], "description": "create (default) records a checkpoint; list shows recorded ones"},
    "name": {"type": "string", "description": "create: checkpoint name, the key rewind uses"},
    "note": {"type": "string", "description": "create: optional note kept with the checkpoint"}
  }
}`)
}

type checkpointArgs struct {
	Op   string `json:"op"`
	Name string `json:"name"`
	Note string `json:"note"`
}

// Execute implements Tool.
func (t *CheckpointTool) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{IsError: true, Text: "checkpoint: canceled: " + err.Error()}, nil
	}
	var a checkpointArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return Result{}, fmt.Errorf("checkpoint: invalid arguments: %w", err)
		}
	}
	switch op := strings.ToLower(strings.TrimSpace(a.Op)); op {
	case "", "create":
		return t.create(a), nil
	case "list":
		return t.list(), nil
	default:
		return Result{IsError: true, Text: fmt.Sprintf("checkpoint: unknown op %q (want create or list)", op)}, nil
	}
}

// create appends the bookmark entry for the current leaf and reports the
// checkpoint id it was assigned.
func (t *CheckpointTool) create(a checkpointArgs) Result {
	if t.Store == nil {
		return Result{IsError: true, Text: "checkpoint: no session store"}
	}
	name := boundText(a.Name, checkpointNameMax)
	if name == "" {
		return Result{IsError: true, Text: "checkpoint: name required — checkpoint {name, note} records one, op list shows them"}
	}
	leaf := t.Store.LeafID()
	if leaf == "" {
		return Result{IsError: true, Text: "checkpoint: session has no entries yet, so there is no entry to branch back to"}
	}
	e := &session.CheckpointEntry{Checkpoint: session.CheckpointPayload{
		Name:    name,
		EntryID: leaf,
		Note:    boundText(a.Note, checkpointNoteMax),
	}}
	if err := t.Store.Append(e); err != nil {
		return Result{IsError: true, Text: "checkpoint: " + err.Error()}
	}
	return Result{
		Text: fmt.Sprintf("Checkpoint %q recorded at entry %s (id %s).\nCall rewind {name: %q, report} to branch back here.",
			e.Checkpoint.Name, leaf, e.Env.ID, e.Checkpoint.Name),
		Details: map[string]any{
			"name":         e.Checkpoint.Name,
			"checkpointId": e.Env.ID,
			"entryId":      leaf,
			"created":      session.FormatStamp(e.Env.Timestamp),
		},
	}
}

// list renders the recorded checkpoints newest first, capped and bounded.
func (t *CheckpointTool) list() Result {
	if t.Store == nil {
		return Result{IsError: true, Text: "checkpoint: no session store"}
	}
	rows := checkpointRows(t.Store.Entries())
	if len(rows) == 0 {
		return Result{Text: "No checkpoints recorded. checkpoint {name, note} records one."}
	}
	total := len(rows)
	if len(rows) > checkpointListMax {
		rows = rows[:checkpointListMax]
	}
	var b strings.Builder
	if total > len(rows) {
		fmt.Fprintf(&b, "Newest %d of %d checkpoints:\n", len(rows), total)
	} else {
		fmt.Fprintf(&b, "%d checkpoint(s), newest first:\n", total)
	}
	details := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		created := session.FormatStamp(r.Created)
		fmt.Fprintf(&b, "%s  %s  id=%s entry=%s", created, r.Name, r.ID, r.EntryID)
		if r.Note != "" {
			b.WriteString("  note: " + r.Note)
		}
		b.WriteByte('\n')
		details = append(details, map[string]any{
			"name": r.Name, "id": r.ID, "entryId": r.EntryID, "created": created, "note": r.Note,
		})
	}
	return Result{Text: strings.TrimSuffix(b.String(), "\n"), Details: details}
}

// Name implements Tool.
func (t *RewindTool) Name() string { return RewindToolName }

// Description implements Tool.
func (t *RewindTool) Description() string {
	return "Branch the session back to a named checkpoint: later turns leave the active path and the report becomes its branch summary."
}

// Parameters implements Tool.
func (t *RewindTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "required": ["name", "report"],
  "properties": {
    "name": {"type": "string", "description": "checkpoint to rewind to (checkpoint op:list shows the names)"},
    "report": {"type": "string", "description": "concise summary of what was tried, learned, and must be kept — recorded as the branch summary the next turn reads"}
  }
}`)
}

type rewindArgs struct {
	Name   string `json:"name"`
	Report string `json:"report"`
}

// Execute implements Tool: refuse, resolve, branch, report.
func (t *RewindTool) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{IsError: true, Text: "rewind: canceled: " + err.Error()}, nil
	}
	var a rewindArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return Result{}, fmt.Errorf("rewind: invalid arguments: %w", err)
		}
	}
	if t.Store == nil {
		return Result{IsError: true, Text: "rewind: no session store"}, nil
	}
	// Safety: never move the leaf under a live turn.
	if t.Running != nil && t.Running() {
		return Result{IsError: true, Text: "rewind refused: a turn is running — the restored path would be rewritten under it. Rewind at a turn boundary."}, nil
	}
	name := boundText(a.Name, checkpointNameMax)
	report := boundText(a.Report, rewindReportMax)
	if name == "" {
		return Result{IsError: true, Text: "rewind: name required (checkpoint op:list shows the recorded names)"}, nil
	}
	if report == "" {
		return Result{IsError: true, Text: "rewind: report required — summarize what was tried so the kept path knows what was undone"}, nil
	}
	rows := checkpointRows(t.Store.Entries())
	var target *cpRow
	for i := range rows {
		if rows[i].Name == name {
			target = &rows[i] // newest match wins
			break
		}
	}
	if target == nil {
		return Result{IsError: true, Text: unknownCheckpointText(name, rows)}, nil
	}

	prevLeaf := t.Store.LeafID()
	if err := t.Store.Branch(target.EntryID); err != nil {
		return Result{IsError: true, Text: "rewind: " + err.Error()}, nil
	}
	// The report rides a branch summary so it converts to a user message on
	// the rebuilt path: the model reads what was undone, the abandoned turns
	// are gone from the next provider call, and the file keeps everything.
	summary := ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{
		Text: fmt.Sprintf("Rewind to checkpoint %q: %s", target.Name, report),
	}}}
	if err := t.Store.Append(&session.BranchSummaryEntry{Summary: summary}); err != nil {
		return Result{IsError: true, Text: "rewind: leaf moved to the checkpoint but the report entry failed: " + err.Error()}, nil
	}

	dropped := abandonedCount(t.Store.Entries(), prevLeaf, target.EntryID)
	unit := "entries"
	if dropped == 1 {
		unit = "entry"
	}
	return Result{
		Text: fmt.Sprintf("Rewound to checkpoint %q (entry %s). Report recorded; %d %s left the active path.",
			target.Name, target.EntryID, dropped, unit),
		Details: map[string]any{
			"name": target.Name, "entryId": target.EntryID, "report": report,
			"rewound": true, "entriesLeft": dropped,
		},
	}, nil
}

// cpRow is one checkpoint projected for listing and lookup.
type cpRow struct {
	Name    string
	ID      string
	EntryID string
	Created time.Time
	Note    string
}

// checkpointRows projects every checkpoint entry in file order, reversed to
// newest first. It scans ALL loaded entries, not just the active path, so a
// checkpoint stays listable after another rewind moved the leaf past it. A
// store windowed past a compaction boundary (load drops pre-boundary
// entries) no longer holds those checkpoints: they are simply absent from
// the list, and rewind reports the name as unknown rather than branching
// from the wrong place.
func checkpointRows(entries []session.Entry) []cpRow {
	var rows []cpRow
	for _, e := range entries {
		cp, ok := e.(*session.CheckpointEntry)
		if !ok {
			continue
		}
		env := cp.Envelope()
		rows = append(rows, cpRow{
			Name: cp.Checkpoint.Name, ID: env.ID, EntryID: cp.Checkpoint.EntryID,
			Created: env.Timestamp, Note: cp.Checkpoint.Note,
		})
	}
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows
}

// unknownCheckpointText names the checkpoints a rewind could have meant.
func unknownCheckpointText(name string, rows []cpRow) string {
	if len(rows) == 0 {
		return fmt.Sprintf("rewind: unknown checkpoint %q — this session has no checkpoints yet (record one with the checkpoint tool)", name)
	}
	names := make([]string, 0, len(rows))
	seen := map[string]bool{}
	for _, r := range rows {
		if !seen[r.Name] {
			seen[r.Name] = true
			names = append(names, r.Name)
		}
	}
	return fmt.Sprintf("rewind: unknown checkpoint %q (available: %s)", name, strings.Join(names, ", "))
}

// abandonedCount counts the entries on the pre-rewind leaf's path that the
// branch abandoned (everything strictly after the checkpoint entry).
// ponytail: O(path) rescan per rewind, and a store windowed past a
// compaction boundary may have dropped ancestors, making the count a lower
// bound. Upgrade path: record the path depth on the entry at load time.
func abandonedCount(entries []session.Entry, fromLeaf, toEntry string) int {
	byID := make(map[string]session.Entry, len(entries))
	for _, e := range entries {
		byID[e.Envelope().ID] = e
	}
	n := 0
	for cur, hops := byID[fromLeaf], 0; cur != nil && hops <= len(entries); hops++ {
		env := cur.Envelope()
		if env.ID == toEntry {
			break
		}
		n++
		cur = byID[env.ParentID]
	}
	return n
}

// boundText trims s and clips it to at most max runes (UTF-8 safe), marking
// a clip so a truncated note or report is never mistaken for the whole text.
func boundText(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= max {
		return string(r)
	}
	return string(r[:max]) + "…"
}

// WireCheckpoint binds the session store to the registry's checkpoint tools
// (and the optional running-turn probe to rewind). Called next to
// WireTodoSink on session open, /resume, and session switches so a rewind
// always targets the live session. Returns how many tools were bound (0 when
// the registry holds neither).
//
// The running probe is the safety seam for a harness that drives rewind
// itself — the TUI rewind selector or a slash command — and must not move
// the leaf under a streaming turn. A model-issued rewind runs INSIDE its own
// turn, so production wiring leaves running nil there (a probe that is true
// for the whole run would refuse every model rewind); internal/agent/loop.go
// Run or a tui.App running accessor is the place to source it once the
// selector lands.
func WireCheckpoint(reg *Registry, store *session.Store, running func() bool) int {
	if reg == nil || store == nil {
		return 0
	}
	bound := 0
	if t, ok := reg.Get(CheckpointToolName); ok {
		if ct, ok := t.(*CheckpointTool); ok {
			ct.Store = store
			bound++
		}
	}
	if t, ok := reg.Get(RewindToolName); ok {
		if rt, ok := t.(*RewindTool); ok {
			rt.Store = store
			rt.Running = running
			bound++
		}
	}
	return bound
}
