// Context notes + new_context (M12 #45): the experimental notes-backed
// context window. A bounded notebook (16,384 bytes) is the durable working
// memory for a long session; a rollover drops the conversation's middle while
// keeping the notebook and the recent tail, so the model keeps its bearings
// without a recursive summary. Raw history stays recoverable through the
// history:// read seam.
//
// Storage reuses what the session model already has: each notebook revision is
// a CustomEntry (customType "experimental_context_notes", data {version,text})
// and a rollover is a CompactionEntry with a marker summary instead of a
// provider-generated one.
//
// The notebook reaches the model on demand (context_notes) and, across a
// rollover, through the boundary summary — the model never silently loses it.
package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/tool"
)

// Model-facing tool names.
const (
	ContextNotesToolName = "context_notes"
	NewContextToolName   = "new_context"
)

// NotesEntryType is the CustomEntry customType carrying one notebook revision
// (omp's experimental_context_notes entry).
const NotesEntryType = "experimental_context_notes"

// MaxNotesBytes caps the notebook: 16,384 UTF-8 bytes, omp parity. A larger
// replacement is byte-truncated on a rune boundary and marked.
const MaxNotesBytes = 16384

// notesTruncationMarker ends a truncated notebook. It counts against the cap,
// so the stored notebook never exceeds MaxNotesBytes.
const notesTruncationMarker = "\n…[context notes truncated: 16384-byte cap]"

// rolloverReason prefixes the boundary summary text: the in-band marker that
// tells a new_context boundary (no recursive summary) apart from a
// provider-generated compaction summary.
const rolloverReason = "[context rollover] new_context: no recursive summary was generated."

// Rollover tail bounds: the recent conversation retained across a rollover.
// Entry-count and byte caps, whichever binds first.
const (
	rolloverKeepEntries = 24
	rolloverKeepBytes   = 32 << 10
)

// Raw-history render bounds (the read tool paginates the returned text):
// historyRenderCap bounds one whole render, historyEntryCap one entry's body.
const (
	historyRenderCap = 1 << 20
	historyEntryCap  = 16 << 10
)

// rolloverPrerequisites is omp's notes-backed rollover set: without all four
// tools active the rollover would be a half-feature, so new_context refuses.
var rolloverPrerequisites = []string{ContextNotesToolName, NewContextToolName, "read", "grep"}

// NotesState is the session-scoped notebook plus the pending-rollover flag.
// Safe for concurrent use: the tools run on the tool pool while the loop reads
// the flag at step boundaries.
type NotesState struct {
	mu       sync.Mutex
	reg      *tool.Registry
	store    *session.Store
	rollover bool // a boundary committed, not yet rebuilt into live history
}

// NewNotesState returns an unbound state. reg is the live registry the
// prerequisite check reads; store may be nil until Bind (the store does not
// exist yet when the registry is built).
func NewNotesState(reg *tool.Registry, store *session.Store) *NotesState {
	return &NotesState{reg: reg, store: store}
}

// Bind attaches the session store and drops any state from the previous
// session. Called on session open and again on every session switch
// (/resume, /new, branch), where the notebook is a different one.
func (n *NotesState) Bind(store *session.Store) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.store = store
	n.rollover = false
}

// storeAt returns the bound store.
func (n *NotesState) storeAt() *session.Store {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.store
}

// MissingPrerequisites lists the rollover prerequisites that are not active in
// the registry. A nil registry reports every name (fail closed).
func (n *NotesState) MissingPrerequisites() []string {
	n.mu.Lock()
	reg := n.reg
	n.mu.Unlock()
	var missing []string
	for _, name := range rolloverPrerequisites {
		if reg == nil {
			missing = append(missing, name)
			continue
		}
		if _, ok := reg.Get(name); !ok {
			missing = append(missing, name)
		}
	}
	return missing
}

// View projects the latest notebook revision on the active branch chain.
// ok=false means no revision is visible (never written, or hidden behind a
// reset boundary).
func (n *NotesState) View() (text, entryID string, ok bool) {
	store := n.storeAt()
	if store == nil {
		return "", "", false
	}
	for _, e := range notesChain(store) {
		c, isCustom := e.(*session.CustomEntry)
		if !isCustom || c.CustomType != NotesEntryType {
			continue
		}
		return notesText(c), e.Envelope().ID, true
	}
	return "", "", false
}

// Replace overwrites the notebook with text (empty clears it) and appends the
// new revision. The returned text is what was stored, which is text
// byte-truncated to MaxNotesBytes when truncated is true.
func (n *NotesState) Replace(text string) (stored, entryID string, truncated bool, err error) {
	stored, truncated = truncateNotes(text)
	store := n.storeAt()
	if store == nil {
		return "", "", false, fmt.Errorf("context_notes: no session to store notes in")
	}
	e := &session.CustomEntry{
		CustomType: NotesEntryType,
		Data:       map[string]any{"version": 1, "text": stored},
	}
	if err := store.Append(e); err != nil {
		return "", "", false, fmt.Errorf("context_notes: persist notebook: %w", err)
	}
	return stored, e.Env.ID, truncated, nil
}

// CommitRollover appends the notes-backed boundary: a compaction entry with a
// marker summary instead of a recursive summary, anchored so the recent tail
// survives, followed by a fresh notebook revision (windowing drops every entry
// before the boundary, so the notebook must be re-established on the new
// segment). committed=false means there was nothing to drop — no entry was
// appended and no rollover is pending.
func (n *NotesState) CommitRollover() (committed bool, err error) {
	store := n.storeAt()
	if store == nil {
		return false, fmt.Errorf("new_context: no session to roll over")
	}
	res, err := session.BuildContext(store.Entries(), store.LeafID(), session.SystemPrompt{})
	if err != nil {
		return false, fmt.Errorf("new_context: build context: %w", err)
	}
	cut := rolloverCut(res)
	if cut < 0 {
		return false, nil
	}
	notebook, _, _ := n.View()
	anchor := res.EntryIDs[cut]
	entry := &session.CompactionEntry{
		Summary: ai.Message{
			Role:       ai.RoleAssistant,
			Content:    []ai.Block{ai.TextBlock{Text: rolloverSummary(notebook)}},
			StopReason: ai.StopReasonStop,
		},
		FirstKeptEntryID: &anchor,
		TokensBefore:     contextTokens(res.Messages),
	}
	if err := store.Append(entry); err != nil {
		return false, fmt.Errorf("new_context: persist boundary: %w", err)
	}
	if notebook != "" {
		if err := store.Append(&session.CustomEntry{
			CustomType: NotesEntryType,
			Data:       map[string]any{"version": 1, "text": notebook},
		}); err != nil {
			logx.Errorf("new_context: persist notebook revision: %v", err)
		}
	}
	n.mu.Lock()
	n.rollover = true
	n.mu.Unlock()
	return true, nil
}

// RebuildIfPending returns history rebuilt from store when a rollover is
// pending, and history unchanged otherwise (the flag is consumed either way).
// A nil store falls back to the bound one.
func (n *NotesState) RebuildIfPending(store *session.Store, history []ai.Message) []ai.Message {
	n.mu.Lock()
	pending := n.rollover
	n.rollover = false
	bound := n.store
	n.mu.Unlock()
	if !pending {
		return history
	}
	if store == nil {
		store = bound
	}
	if store == nil {
		return history
	}
	res, err := session.BuildContext(store.Entries(), store.LeafID(), session.SystemPrompt{})
	if err != nil {
		logx.Errorf("new_context: rebuild after rollover: %v", err)
		return history
	}
	return res.Messages
}

// IsRolloverBoundary reports whether a compaction entry is a new_context
// boundary rather than a provider-generated summary.
func IsRolloverBoundary(e *session.CompactionEntry) bool {
	return e != nil && strings.HasPrefix(e.Summary.Text(), rolloverReason)
}

// NotesStateOf returns the notes state wired into a registry (nil when the
// registry has no context_notes tool) — how the loop and cmd reach the same
// state.
func NotesStateOf(reg *tool.Registry) *NotesState {
	if reg == nil {
		return nil
	}
	t, ok := reg.Get(ContextNotesToolName)
	if !ok {
		return nil
	}
	nt, ok := t.(*NotesTool)
	if !ok {
		return nil
	}
	return nt.Notes
}

// notesRollover is the agent loop's step-boundary seam (M12 #45): after a
// new_context call committed a boundary, the live history must be rebuilt from
// the store so the dropped middle leaves the provider request. Call it next to
// maybeCompact (loop.go).
func (a *Agent) notesRollover(history []ai.Message) []ai.Message {
	ns := NotesStateOf(a.Tools)
	if ns == nil || a.Store == nil {
		return history
	}
	return ns.RebuildIfPending(a.Store, history)
}

// notesChain walks the active branch leaf→root, newest entry first, stopping
// at a reset boundary: a reset hides earlier notebook revisions.
func notesChain(store *session.Store) []session.Entry {
	limit := len(store.Entries()) + 1 // cycle bound
	out := make([]session.Entry, 0, 8)
	for id := store.LeafID(); id != "" && len(out) < limit; {
		e := store.Entry(id)
		if e == nil {
			break
		}
		if _, isReset := e.(*session.ResetBoundaryEntry); isReset {
			break
		}
		out = append(out, e)
		id = e.Envelope().ParentID
	}
	return out
}

// notesText reads the notebook payload out of a revision entry.
func notesText(c *session.CustomEntry) string {
	if c == nil || c.Data == nil {
		return ""
	}
	text, _ := c.Data["text"].(string)
	return text
}

// truncateNotes enforces MaxNotesBytes, cutting on a rune boundary and ending
// with the marker (which counts against the cap).
func truncateNotes(text string) (string, bool) {
	if len(text) <= MaxNotesBytes {
		return text, false
	}
	keep := MaxNotesBytes - len(notesTruncationMarker)
	for keep > 0 && !utf8.RuneStart(text[keep]) {
		keep--
	}
	return text[:keep] + notesTruncationMarker, true
}

// rolloverCut picks where the retained tail starts in res: the last
// rolloverKeepEntries messages, capped at rolloverKeepBytes of text, advanced
// off tool results (their call would be dropped with the middle) and onto a
// message with a real entry anchor. -1 when nothing can be dropped (fewer than
// one exchange leaves the window, or the tail has no anchor).
func rolloverCut(res *session.ContextResult) int {
	msgs := res.Messages
	cut, size := len(msgs), 0
	for cut > 1 && len(msgs)-cut < rolloverKeepEntries && size < rolloverKeepBytes {
		cut--
		size += len(msgs[cut].Text()) + 64
	}
	for cut < len(msgs)-1 && msgs[cut].Role == ai.RoleToolResult {
		cut++
	}
	for cut < len(msgs)-1 && (cut >= len(res.EntryIDs) || res.EntryIDs[cut] == "") {
		cut++
	}
	if cut < 2 || cut >= len(msgs) || cut >= len(res.EntryIDs) || res.EntryIDs[cut] == "" {
		return -1
	}
	return cut
}

// rolloverSummary renders the boundary message: the reason, where raw history
// lives, and the notebook (the retained working memory).
func rolloverSummary(notebook string) string {
	var b strings.Builder
	b.WriteString(rolloverReason)
	b.WriteString(" History before this point stays on disk: read history://current for the raw transcript.")
	if notebook != "" {
		b.WriteString("\n\n## Context notebook\n")
		b.WriteString(notebook)
	}
	return b.String()
}

// NotesTool is the context_notes tool: read or replace the branch-scoped
// notebook. Ops are view (no text) and replace (text, empty clears).
type NotesTool struct{ Notes *NotesState }

// Name implements tool.Tool.
func (t *NotesTool) Name() string { return ContextNotesToolName }

// Description implements tool.Tool.
func (t *NotesTool) Description() string {
	return "Read or replace this branch's context notebook (16 KiB cap); omit text to read, empty string clears. It survives new_context rollovers."
}

// Parameters implements tool.Tool.
func (t *NotesTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "text": {"type": "string", "description": "entire replacement notebook; omit to read, empty string to clear"}
  }
}`)
}

// Execute implements tool.Tool.
func (t *NotesTool) Execute(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	if err := ctx.Err(); err != nil {
		return tool.Result{IsError: true, Text: "context_notes: canceled"}, nil
	}
	var a struct {
		Text *string `json:"text"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return tool.Result{}, fmt.Errorf("context_notes: invalid arguments: %w", err)
	}
	if t.Notes == nil {
		return tool.Result{IsError: true, Text: "context_notes: notes state unavailable"}, nil
	}
	if a.Text == nil {
		text, id, ok := t.Notes.View()
		if !ok || text == "" {
			return tool.Result{
				Text:    "No context notes are stored for this session branch.",
				Details: map[string]any{"entryId": id},
			}, nil
		}
		return tool.Result{
			Text:    text,
			Details: map[string]any{"text": text, "entryId": id},
		}, nil
	}
	stored, id, truncated, err := t.Notes.Replace(*a.Text)
	if err != nil {
		return tool.Result{IsError: true, Text: err.Error()}, nil
	}
	text := "Context notes saved."
	if truncated {
		text = fmt.Sprintf("Context notes saved (%d-byte cap: truncated).", MaxNotesBytes)
	}
	return tool.Result{
		Text: text,
		Details: map[string]any{
			"entryId":   id,
			"text":      stored,
			"bytes":     len(stored),
			"truncated": truncated,
		},
	}, nil
}

// NewContextTool is the new_context tool: it requests a fresh experimental
// context window. The rollover commits synchronously (so it survives process
// exit) and the agent loop rebuilds its live history from the store at the next
// step boundary.
type NewContextTool struct{ Notes *NotesState }

// Name implements tool.Tool.
func (t *NewContextTool) Name() string { return NewContextToolName }

// Description implements tool.Tool.
func (t *NewContextTool) Description() string {
	return "Request a fresh context window: drop the middle of the conversation, keeping the notebook and the recent tail; raw history stays readable via read history://current."
}

// Parameters implements tool.Tool.
func (t *NewContextTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type": "object", "properties": {}}`)
}

// Execute implements tool.Tool.
func (t *NewContextTool) Execute(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	if err := ctx.Err(); err != nil {
		return tool.Result{IsError: true, Text: "new_context: canceled"}, nil
	}
	if t.Notes == nil {
		return tool.Result{IsError: true, Text: "new_context: notes state unavailable"}, nil
	}
	if missing := t.Notes.MissingPrerequisites(); len(missing) > 0 {
		return tool.Result{IsError: true, Text: fmt.Sprintf(
			"new_context: notes-backed rollover requires all four tools active (context_notes, new_context, read, grep); missing: %s. Tool configurations without them keep the existing compaction behavior.",
			strings.Join(missing, ", "))}, nil
	}
	committed, err := t.Notes.CommitRollover()
	if err != nil {
		return tool.Result{IsError: true, Text: err.Error()}, nil
	}
	return tool.Result{
		Text:    "New context window requested.",
		Details: map[string]any{"requested": true, "committed": committed},
	}, nil
}

// ResolveHistory is the history:// read-seam resolver (M12 #45):
// history://current is the active branch's raw transcript — every entry,
// including the history an earlier rollover dropped from the model context —
// and history://full is the whole journal across every branch
// (history://current/full is accepted as the same full view).
func (n *NotesState) ResolveHistory(uri string) (string, error) {
	target := strings.Trim(strings.TrimPrefix(uri[strings.Index(uri, "://")+3:], "/"), "/")
	switch target {
	case "current":
		return n.renderHistory(false)
	case "full", "current/full":
		return n.renderHistory(true)
	default:
		return "", fmt.Errorf("history: unknown target %q (use history://current or history://full)", target)
	}
}

// renderHistory renders the journal: the active branch chain (all=false) or
// every entry in file order, branches marked (all=true).
func (n *NotesState) renderHistory(all bool) (string, error) {
	store := n.storeAt()
	if store == nil {
		return "", fmt.Errorf("history: no session")
	}
	recs, err := journalRecords(store)
	if err != nil {
		return "", err
	}
	byID := make(map[string]session.Entry, len(recs))
	for _, r := range recs {
		if r.entry != nil {
			byID[r.entry.Envelope().ID] = r.entry
		}
	}
	leaf := store.LeafID()
	if byID[leaf] == nil && len(recs) > 0 {
		leaf = lastEntryID(recs)
	}
	onChain := map[string]bool{}
	if !all {
		onChain = chainIDs(byID, leaf)
	}

	var b strings.Builder
	where := store.Path()
	if where == "" {
		where = "(memory-only session)"
	}
	target := "current"
	if all {
		target = "full"
	}
	fmt.Fprintf(&b, "# history://%s — %s, leaf=%s\n", target, where, dash(leaf))
	prevID, dropped := "", 0
	for _, r := range recs {
		if !all && (r.entry == nil || !onChain[r.entry.Envelope().ID]) {
			continue
		}
		if b.Len() > historyRenderCap {
			dropped++
			continue
		}
		renderRecord(&b, r, prevID)
		if r.entry != nil {
			prevID = r.entry.Envelope().ID
		}
	}
	if dropped > 0 {
		fmt.Fprintf(&b, "\n…[history truncated at %d KiB: %d more entries — read %s directly]\n", historyRenderCap>>10, dropped, where)
	}
	return b.String(), nil
}

// lastEntryID returns the newest parsed entry id in file order.
func lastEntryID(recs []journalRecord) string {
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].entry != nil {
			return recs[i].entry.Envelope().ID
		}
	}
	return ""
}

// chainIDs collects the ids on the active branch path (leaf→root, cycle
// bounded).
func chainIDs(byID map[string]session.Entry, leaf string) map[string]bool {
	on := map[string]bool{}
	for id := leaf; id != "" && !on[id]; {
		e := byID[id]
		if e == nil {
			break
		}
		on[id] = true
		id = e.Envelope().ParentID
	}
	return on
}

// renderRecord renders one journal line: the envelope, then whatever payload
// the entry carries. An unparsed line is shown verbatim — raw history is the
// recovery path, so nothing is silently dropped.
func renderRecord(b *strings.Builder, r journalRecord, prevID string) {
	if r.entry == nil {
		fmt.Fprintf(b, "[raw] %s\n", clipText(strings.TrimSpace(r.raw), historyEntryCap))
		return
	}
	env := r.entry.Envelope()
	fmt.Fprintf(b, "[%s %s %s]", env.ID, env.Timestamp.UTC().Format(time.RFC3339), env.Type)
	if env.ParentID != prevID {
		fmt.Fprintf(b, " parent=%s", dash(env.ParentID))
	}
	b.WriteString(" ")
	switch t := r.entry.(type) {
	case *session.MessageEntry:
		fmt.Fprintf(b, "%s: %s", t.Message.Role, clipText(messageTranscript(t.Message), historyEntryCap))
	case *session.CompactionEntry:
		fmt.Fprintf(b, "firstKept=%s tokensBefore=%d summary: %s",
			dash(deref(t.FirstKeptEntryID)), t.TokensBefore, clipText(t.Summary.Text(), historyEntryCap))
	case *session.BranchSummaryEntry:
		b.WriteString("summary: " + clipText(t.Summary.Text(), historyEntryCap))
	case *session.ModelChangeEntry:
		b.WriteString("model=" + t.Model)
	case *session.CustomEntry:
		fmt.Fprintf(b, "%s %s", t.CustomType, clipText(mustJSON(t.Data), historyEntryCap))
	case *session.GoalUpdatedEntry:
		fmt.Fprintf(b, "%s %s", t.Goal.Status, clipText(t.Goal.Objective, historyEntryCap))
	case *session.UnknownEntry:
		fmt.Fprintf(b, "%s %s", t.EntryType, clipText(string(t.Raw), historyEntryCap))
	default:
		b.WriteString(env.Type)
	}
	b.WriteString("\n")
}

// messageTranscript renders one message for the raw transcript.
func messageTranscript(m ai.Message) string {
	var b strings.Builder
	if m.ToolCallID != "" {
		fmt.Fprintf(&b, "(result for %s) ", m.ToolCallID)
	}
	for _, blk := range m.Content {
		switch x := blk.(type) {
		case ai.TextBlock:
			b.WriteString(x.Text)
		case ai.ThinkingBlock:
			b.WriteString("[thinking] " + x.Thinking)
		case ai.ToolCallBlock:
			fmt.Fprintf(&b, "[tool %s %s]", x.Name, string(x.Arguments))
		case ai.ImageBlock:
			b.WriteString("[image]")
		}
		b.WriteString(" ")
	}
	if s := strings.TrimSpace(b.String()); s != "" {
		return s
	}
	return "(no content)"
}

// journalRecords reads the session journal: from disk when the session is
// persisted (the in-memory store drops pre-boundary entries through windowing,
// so the file is the only complete copy of raw history), from memory otherwise.
func journalRecords(store *session.Store) ([]journalRecord, error) {
	path := store.Path()
	if path == "" {
		entries := store.Entries()
		out := make([]journalRecord, 0, len(entries))
		for _, e := range entries {
			out = append(out, journalRecord{entry: e})
		}
		return out, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("history: open %s: %w", path, err)
	}
	defer f.Close()

	var out []journalRecord
	sc := bufio.NewScanner(f)
	// One entry line can carry a large payload (images are externalized, tool
	// output is not), so allow well past bufio's 64 KiB default.
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if _, ok := session.ParseTitleSlot(line); ok {
			continue // fixed-width title slot: not history
		}
		if _, ok := session.ParseHeader(line); ok {
			continue
		}
		e, err := session.ParseEntry(line)
		if err == nil {
			out = append(out, journalRecord{entry: e})
			continue
		}
		// Unmodelled or malformed: keep the line, keep the chain node.
		if env, envErr := session.ParseEnvelope(line); envErr == nil && env.ID != "" {
			out = append(out, journalRecord{entry: &session.UnknownEntry{
				Env:       env,
				EntryType: env.Type,
				Raw:       bytes.Clone(line),
			}})
			continue
		}
		out = append(out, journalRecord{raw: string(line)})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("history: read %s: %w", path, err)
	}
	return out, nil
}

// journalRecord is one raw journal line: a parsed entry, or a line neither the
// entry parser nor the envelope parser models (kept verbatim).
type journalRecord struct {
	entry session.Entry
	raw   string
}

// clipText bounds one rendered body, cutting on a rune boundary and naming how
// much was elided.
func clipText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf("%s…[%d bytes elided]", s[:cut], len(s)-cut)
}

// deref renders an optional entry id.
func deref(p *string) string {
	if p == nil {
		return "-"
	}
	return *p
}

// dash renders an absent chain field.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// mustJSON renders a custom payload compactly ("" on the impossible error).
func mustJSON(v any) string {
	if v == nil {
		return "{}"
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(raw)
}
