// Package session implements xdev's on-disk session store (PRD §3.2):
// omp-compatible JSONL files (fixed-width title slot + session header +
// append-only entry tree), context reconstruction, session listing, and the
// sha256 blob store.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
)

// Envelope is the common header every entry carries on the wire:
// {"type":...,"id":"8-hex","parentId":"8-hex"|null,"timestamp":RFC3339 UTC}.
// Memory shape: ParentID "" is the root sentinel (marshals as null).
type Envelope struct {
	Type      string
	ID        string
	ParentID  string
	Timestamp time.Time
}

func parentPtr(p string) *string {
	if p == "" {
		return nil
	}
	return &p
}

func parentStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Entry is one append-only session entry. Concrete types are the pointer
// structs below; ParseEntry dispatches on the envelope's "type" field.
type Entry interface {
	Envelope() Envelope
}

// Entry type discriminators on the wire.
const (
	TypeModelChange   = "model_change"
	TypeMessage       = "message"
	TypeCompaction    = "compaction"
	TypeBranchSummary = "branch_summary"
	TypeResetBoundary = "reset_boundary"
	TypeCustom        = "custom"

	// TypeGoalUpdated is the session-scoped goal state snapshot (M11 #40),
	// appended by the goal tool on every goal state change. omp does not
	// model it; foreign readers keep the line as an opaque chain node.
	TypeGoalUpdated = "goal_updated"

	// TypeCheckpoint is a named session-tree bookmark (M13 #51): the entry
	// id rewind branches back to, the name it is found by, and a note.
	// omp does not model it; foreign readers keep the line as an opaque
	// chain node.
	TypeCheckpoint = "checkpoint"

	// TypeBranch is xdev's persisted branch marker (a CustomEntry whose
	// CustomType is "branch" with data {"to":entryID}); it is how Open
	// reconstructs the leaf pointer across restarts.
	TypeBranch = "branch"
)

// ErrUnknownEntryType marks a structurally valid entry whose "type" is not
// part of the model. ParseEntry returns (nil, ErrUnknownEntryType) for it;
// the store materializes the line as an UnknownEntry so the parent chain
// stays intact, and the line is never rewritten or removed from disk.
var ErrUnknownEntryType = errors.New("session: unknown entry type")

// MessageEntry carries one ai.Message (user/assistant/toolResult).
// Wire: {"type":"message",...,"message":{...}}.
type MessageEntry struct {
	Env     Envelope
	Message ai.Message
}

func (e *MessageEntry) Envelope() Envelope { return e.Env }

// ModelChangeEntry switches the active model.
// Wire: {"type":"model_change",...,"model":"provider/model","resolvedModelIsFallback":false}.
type ModelChangeEntry struct {
	Env                     Envelope
	Model                   string
	ResolvedModelIsFallback bool
}

func (e *ModelChangeEntry) Envelope() Envelope { return e.Env }

// CompactionEntry replaces pre-compaction history with a summary message.
// Wire: {"type":"compaction",...,"summary":{...},"firstKeptEntryId":...,"tokensBefore":N}.
type CompactionEntry struct {
	Env              Envelope
	Summary          ai.Message
	FirstKeptEntryID *string // nil → only the summary survives
	TokensBefore     int64
}

func (e *CompactionEntry) Envelope() Envelope { return e.Env }

// BranchSummaryEntry records context for an abandoned branch so the model
// knows what was tried. Wire: {"type":"branch_summary",...,"summary":{...}}.
type BranchSummaryEntry struct {
	Env     Envelope
	Summary ai.Message
}

func (e *BranchSummaryEntry) Envelope() Envelope { return e.Env }

// ResetBoundaryEntry marks a context cut: everything before it in the path
// is dropped. Payload-free. Wire: {"type":"reset_boundary",...}.
type ResetBoundaryEntry struct {
	Env Envelope
}

func (e *ResetBoundaryEntry) Envelope() Envelope { return e.Env }

// CustomEntry is an opaque extension record (tool_execution_start,
// session_exit, branch markers, ...). Wire:
// {"type":"custom",...,"customType":"...","data":{...}}.
type CustomEntry struct {
	Env        Envelope
	CustomType string
	Data       map[string]any
}

func (e *CustomEntry) Envelope() Envelope { return e.Env }

// GoalPayload is the goal snapshot carried by GoalUpdatedEntry (agent.Goal's
// persisted shape; the session package must not import agent, so the fields
// are mirrored here).
type GoalPayload struct {
	Objective   string    `json:"objective"`
	Status      string    `json:"status"`
	TokenBudget int64     `json:"tokenBudget,omitempty"`
	Spent       int64     `json:"spent,omitempty"`
	Evidence    []string  `json:"evidence,omitempty"`
	Created     time.Time `json:"created"`
}

// GoalUpdatedEntry records one goal state change (M11 #40). Every transition
// appends a full snapshot, so the last entry replays the current goal on
// reopen. Wire: {"type":"goal_updated",...,"goal":{...}}.
type GoalUpdatedEntry struct {
	Env  Envelope
	Goal GoalPayload
}

func (e *GoalUpdatedEntry) Envelope() Envelope { return e.Env }

// CheckpointPayload is the bookmark carried by CheckpointEntry: Name is how
// a later rewind finds it, EntryID is the leaf the session branched from,
// and Note is free-form context for the human/model reading the list.
type CheckpointPayload struct {
	Name    string `json:"name"`
	EntryID string `json:"entryId"`
	Note    string `json:"note,omitempty"`
}

// CheckpointEntry records one named checkpoint (M13 #51). The entry is
// metadata: context conversion ignores it, and the leaf pointer is never
// moved by creating one — rewind moves the leaf back to EntryID. EntryID
// may name a compaction-dropped (windowed) entry, in which case rewind
// fails loudly rather than silently branching from the wrong place.
// Wire: {"type":"checkpoint",...,"checkpoint":{"name":...,"entryId":...}}.
type CheckpointEntry struct {
	Env        Envelope
	Checkpoint CheckpointPayload
}

func (e *CheckpointEntry) Envelope() Envelope { return e.Env }

// UnknownEntry preserves an entry type outside xdev's parsed set while
// keeping the parent chain intact — real omp files carry title_change /
// thinking_level_change / service_tier_change entries mid-chain, and
// dropping them from the tree would sever the leaf→root walk. The raw line
// is kept verbatim so a full rewrite round-trips byte-exactly; context
// conversion ignores these nodes.
type UnknownEntry struct {
	Env       Envelope
	EntryType string
	Raw       json.RawMessage
}

func (e *UnknownEntry) Envelope() Envelope { return e.Env }

// wireTime renders timestamps exactly like omp (JS Date.toISOString):
// RFC3339 UTC with always-3-digit milliseconds ("2026-09-07T03:20:45.220Z").
// Go's default time.Time marshal trims trailing zeros (".22Z"), which parses
// fine but diverges from omp byte-for-byte.
type wireTime time.Time

const ompStampFormat = "2006-01-02T15:04:05.000Z07:00"

func (t wireTime) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Time(t).UTC().Format(ompStampFormat))
}

func (t *wireTime) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return err
	}
	*t = wireTime(parsed)
	return nil
}

func shrinkOneRune(s string) string {
	// Cut one rune from the end (keeping the string valid UTF-8).
	r := []rune(s)
	return string(r[:len(r)-1])
}

// padFor reports how many pad spaces fit given the no-pad serialization of
// the slot and the target content width. The appended tail is
// `,"pad":"` + pad spaces + `"}` (10 bytes + pad), and it replaces the
// base line's closing brace (−1): total = len(line) + 9 + pad.
func padFor(base titleSlotWire, contentWidth int) int {
	line, err := json.Marshal(base)
	if err != nil {
		return 0
	}
	return contentWidth - len(line) - 9
}

// FormatStamp renders t in omp's on-disk timestamp format.
func FormatStamp(t time.Time) string { return t.UTC().Format(ompStampFormat) }

// entryWire is the single-union shape: the envelope flattened with every
// payload variant inline; exactly one payload is populated per entry type.
type entryWire struct {
	Type      string   `json:"type"`
	ID        string   `json:"id"`
	ParentID  *string  `json:"parentId"`
	Timestamp wireTime `json:"timestamp"`

	Message          json.RawMessage    `json:"message,omitempty"`
	Model            *string            `json:"model,omitempty"`
	ResolvedFallback *bool              `json:"resolvedModelIsFallback,omitempty"`
	Summary          json.RawMessage    `json:"summary,omitempty"`
	FirstKeptEntryID *string            `json:"firstKeptEntryId,omitempty"`
	TokensBefore     *int64             `json:"tokensBefore,omitempty"`
	CustomType       string             `json:"customType,omitempty"`
	Data             map[string]any     `json:"data,omitempty"`
	Goal             *GoalPayload       `json:"goal,omitempty"`
	Checkpoint       *CheckpointPayload `json:"checkpoint,omitempty"`
}

func (w entryWire) envelope() Envelope {
	return Envelope{Type: w.Type, ID: w.ID, ParentID: parentStr(w.ParentID), Timestamp: time.Time(w.Timestamp)}
}

// MarshalEntry serializes e to one JSONL line (no trailing newline).
func MarshalEntry(e Entry) ([]byte, error) {
	if u, ok := e.(*UnknownEntry); ok {
		return append(json.RawMessage(nil), u.Raw...), nil // verbatim
	}
	var w entryWire
	switch t := e.(type) {
	case *MessageEntry:
		w.Type = TypeMessage
		w.Message = mustMarshal(t.Message)
	case *ModelChangeEntry:
		w.Type = TypeModelChange
		w.Model = &t.Model
		fallback := t.ResolvedModelIsFallback
		w.ResolvedFallback = &fallback
	case *CompactionEntry:
		w.Type = TypeCompaction
		w.Summary = mustMarshal(t.Summary)
		w.FirstKeptEntryID = t.FirstKeptEntryID
		tokens := t.TokensBefore
		w.TokensBefore = &tokens
	case *BranchSummaryEntry:
		w.Type = TypeBranchSummary
		w.Summary = mustMarshal(t.Summary)
	case *ResetBoundaryEntry:
		w.Type = TypeResetBoundary
	case *CustomEntry:
		w.Type = TypeCustom
		w.CustomType = t.CustomType
		w.Data = t.Data
	case *GoalUpdatedEntry:
		w.Type = TypeGoalUpdated
		w.Goal = &t.Goal
	case *CheckpointEntry:
		w.Type = TypeCheckpoint
		w.Checkpoint = &t.Checkpoint
	default:
		return nil, fmt.Errorf("%w: cannot marshal %T", ErrUnknownEntryType, e)
	}
	env := e.Envelope()
	w.ID = env.ID
	w.ParentID = parentPtr(env.ParentID)
	w.Timestamp = wireTime(env.Timestamp)
	return json.Marshal(w)
}

// ParseEntry decodes one JSONL line into an Entry, dispatching on "type".
// Structurally valid but unknown types return (nil, ErrUnknownEntryType).
func ParseEntry(line []byte) (Entry, error) {
	var w entryWire
	if err := json.Unmarshal(line, &w); err != nil {
		return nil, fmt.Errorf("session: parse entry: %w", err)
	}
	env := w.envelope()
	switch w.Type {
	case TypeMessage:
		m, err := decodeMessage(w.Message, env.ID)
		if err != nil {
			return nil, err
		}
		return &MessageEntry{Env: env, Message: m}, nil
	case TypeModelChange:
		e := &ModelChangeEntry{Env: env}
		if w.Model != nil {
			e.Model = *w.Model
		}
		if w.ResolvedFallback != nil {
			e.ResolvedModelIsFallback = *w.ResolvedFallback
		}
		return e, nil
	case TypeCompaction:
		s, err := decodeMessage(w.Summary, env.ID)
		if err != nil {
			return nil, err
		}
		e := &CompactionEntry{Env: env, Summary: s, FirstKeptEntryID: w.FirstKeptEntryID}
		if w.TokensBefore != nil {
			e.TokensBefore = *w.TokensBefore
		}
		return e, nil
	case TypeBranchSummary:
		s, err := decodeMessage(w.Summary, env.ID)
		if err != nil {
			return nil, err
		}
		return &BranchSummaryEntry{Env: env, Summary: s}, nil
	case TypeResetBoundary:
		return &ResetBoundaryEntry{Env: env}, nil
	case TypeCustom:
		return &CustomEntry{Env: env, CustomType: w.CustomType, Data: w.Data}, nil
	case TypeGoalUpdated:
		e := &GoalUpdatedEntry{Env: env}
		if w.Goal != nil {
			e.Goal = *w.Goal
		}
		return e, nil
	case TypeCheckpoint:
		e := &CheckpointEntry{Env: env}
		if w.Checkpoint != nil {
			e.Checkpoint = *w.Checkpoint
		}
		return e, nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownEntryType, w.Type)
	}
}

// ParseEnvelope decodes just the envelope of an entry line. Used by the
// store to keep chain connectivity for unknown entry types.
func ParseEnvelope(line []byte) (Envelope, error) {
	var w entryWire
	if err := json.Unmarshal(line, &w); err != nil {
		return Envelope{}, fmt.Errorf("session: parse envelope: %w", err)
	}
	return w.envelope(), nil
}

// decodeMessage unmarshals an embedded ai.Message, normalizing omp's float
// duration/ttft fields to whole milliseconds first.
func decodeMessage(raw json.RawMessage, id string) (ai.Message, error) {
	var m ai.Message
	if len(raw) == 0 || string(raw) == "null" {
		return m, nil
	}
	if err := json.Unmarshal(fixupOMPMessage(raw), &m); err != nil {
		return m, fmt.Errorf("session: entry %s: %w", id, err)
	}
	return m, nil
}

// fixupOMPMessage adapts real omp message payloads to the frozen ai.Message
// contract (read-only, session-local):
//   - float "duration"/"ttft" (e.g. 6332.941457999999) → int64 ms
//   - numeric "completedAt" ms-epoch (e.g. 1788751260558) → RFC3339 string
func fixupOMPMessage(raw json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw
	}
	changed := false
	for _, key := range []string{"duration", "ttft"} {
		val, ok := fields[key]
		if !ok || len(val) == 0 || val[0] == '"' || !jsonNumberHasFraction(val) {
			continue
		}
		var f float64
		if err := json.Unmarshal(val, &f); err != nil {
			continue
		}
		fields[key], _ = json.Marshal(int64(f))
		changed = true
	}
	if val, ok := fields["completedAt"]; ok && len(val) > 0 && val[0] != '"' {
		var ms float64
		if err := json.Unmarshal(val, &ms); err == nil {
			t := time.UnixMilli(int64(ms)).UTC()
			fields["completedAt"], _ = json.Marshal(FormatStamp(t))
			changed = true
		}
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return raw
	}
	return out
}

func jsonNumberHasFraction(b []byte) bool {
	for _, c := range b {
		if c == '.' || c == 'e' || c == 'E' {
			return true
		}
	}
	return false
}

// NewID returns a fresh 8-hex-char entry id (crypto/rand).
func NewID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is unrecoverable in practice; degrade to a
		// time-derived id rather than returning a colliding empty string.
		return fmt.Sprintf("%08x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// mustMarshal infallibly serializes v; ai.Message only errors on block types
// ParseBlock already rejects.
func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{"role":"user","content":[]}`)
	}
	return b
}

// --- file-level records (lines 1 and 2) ---

// TitleSlotWidth is the fixed byte width of line 1 of every session file,
// newline included (verified against real omp files: 255 content bytes +
// "\n"). The "pad" member absorbs the slack so rename/list never rewrite the
// body.
const TitleSlotWidth = 256

// Title-source values for the fixed-width title slot. Auto = generated,
// manual = user rename, subagent = child session owned by another agent
// (resume paths skip these: a child is never a user continuation; forks
// stay "auto" and carry parentSession instead).
const (
	TitleSourceAuto     = "auto"
	TitleSourceManual   = "manual"
	TitleSourceSubagent = "subagent"
)

// titleSlotWire is the line-1 shape WITHOUT the pad field; MarshalTitleSlot
// appends `,"pad":"<spaces>"` before the closing brace to hit the fixed
// width exactly.
type titleSlotWire struct {
	Type      string `json:"type"`
	V         int    `json:"v"`
	Title     string `json:"title"`
	Source    string `json:"source"`
	UpdatedAt string `json:"updatedAt"`
}

// MarshalTitleSlot renders the fixed-width title line including its newline:
// {"type":"title","v":1,"title":...,"source":"auto"|"manual","updatedAt":...,
// "pad":"<spaces>"} — exactly TitleSlotWidth bytes on disk. Over-long titles
// are truncated on a UTF-8 boundary so the slot width invariant holds.
func MarshalTitleSlot(title, source string, updatedAt time.Time) []byte {
	base := titleSlotWire{Type: "title", V: 1, Title: title, Source: source, UpdatedAt: FormatStamp(updatedAt)}
	pad := padFor(base, TitleSlotWidth-1) // -1 for the newline
	for pad < 0 && len(base.Title) > 0 {
		base.Title = shrinkOneRune(base.Title)
		pad = padFor(base, TitleSlotWidth-1)
	}
	if pad < 0 {
		pad = 0 // pathological: field overhead alone exceeds the slot
	}
	line, err := json.Marshal(base)
	if err != nil { // unreachable: plain strings/int
		return []byte(strings.Repeat(" ", TitleSlotWidth-1) + "\n")
	}
	out := make([]byte, 0, TitleSlotWidth)
	out = append(out, line[:len(line)-1]...) // drop closing brace
	out = append(out, `,"pad":"`...)
	out = append(out, strings.Repeat(" ", pad)...)
	out = append(out, `"}`...)
	out = append(out, '\n')
	return out
}

// ParseTitleSlot decodes line 1 (without newline). Returns ("", false) when
// the line is not a title slot.
func ParseTitleSlot(line []byte) (string, bool) {
	title, _, ok := ParseTitleSlotSource(line)
	return title, ok
}

// ParseTitleSlotSource decodes line 1 including its source field ("auto" /
// "manual" / "subagent") — resume paths use it to skip child sessions.
func ParseTitleSlotSource(line []byte) (title, source string, ok bool) {
	var aux struct {
		Type   string `json:"type"`
		Title  string `json:"title"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal(line, &aux); err != nil || aux.Type != "title" {
		return "", "", false
	}
	return aux.Title, aux.Source, true
}

// SessionHeader is line 2 of a session file.
// Wire: {"type":"session","version":3,"id":<uuid>,"parentSession":<uuid>,
// "timestamp":...,"cwd":...,"title":...,"titleSource":"auto"}.
// ParentSession is set on forks/branches ("" for root sessions; omitted
// on the wire).
type SessionHeader struct {
	Version       int
	ID            string
	ParentSession string
	Timestamp     time.Time
	CWD           string
	Title         string
	TitleSource   string
}

type sessionHeaderWire struct {
	Type          string   `json:"type"`
	Version       int      `json:"version"`
	ID            string   `json:"id"`
	ParentSession string   `json:"parentSession,omitempty"`
	Timestamp     wireTime `json:"timestamp"`
	CWD           string   `json:"cwd"`
	Title         string   `json:"title"`
	TitleSource   string   `json:"titleSource"`
}

// MarshalHeader renders the session header line (trailing newline included).
func MarshalHeader(h SessionHeader) []byte {
	w := sessionHeaderWire{
		Type: "session", Version: 3,
		ID: h.ID, Timestamp: wireTime(h.Timestamp), CWD: h.CWD,
		Title: h.Title, TitleSource: h.TitleSource, ParentSession: h.ParentSession,
	}
	if w.Version == 0 {
		w.Version = 3
	}
	if w.TitleSource == "" {
		w.TitleSource = TitleSourceAuto
	}
	line, err := json.Marshal(w)
	if err != nil { // unreachable: strings + int + wireTime
		line = []byte(`{"type":"session","version":3}`)
	}
	return append(line, '\n')
}

// ParseHeader decodes line 2 (without newline). Returns (zero, false) when
// the line is not a session header.
func ParseHeader(line []byte) (SessionHeader, bool) {
	var w sessionHeaderWire
	if err := json.Unmarshal(line, &w); err != nil || w.Type != "session" {
		return SessionHeader{}, false
	}
	return SessionHeader{
		Version: w.Version, ID: w.ID, Timestamp: time.Time(w.Timestamp),
		CWD: w.CWD, Title: w.Title, TitleSource: w.TitleSource,
		ParentSession: w.ParentSession,
	}, true
}
