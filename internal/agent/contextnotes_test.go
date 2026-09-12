// Tests for the M12 #45 notes-backed context window: the context_notes
// notebook (cap, round-trip, branch scoping, reset hiding), the new_context
// rollover (notebook + recent tail kept, middle dropped, no recursive summary),
// the history:// read seam, and the four-tool prerequisite gate.
package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/tool"
)

// notesOnDiskStore returns a memory-backed store already materialized on disk,
// so history:// reads the complete raw journal (post-windowing too).
func notesOnDiskStore(t *testing.T) *session.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notes.jsonl")
	store := session.OpenMem(t.TempDir(), "notes")
	if _, err := store.EnsureOnDisk(path, session.Options{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// turn appends one user/assistant exchange ("turn N") to the store.
func turn(t *testing.T, store *session.Store, n int) {
	t.Helper()
	if err := store.Append(&session.MessageEntry{Message: ai.Message{
		Role:    ai.RoleUser,
		Content: []ai.Block{ai.TextBlock{Text: "user turn " + strconv.Itoa(n)}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(&session.MessageEntry{Message: ai.Message{
		Role:       ai.RoleAssistant,
		Content:    []ai.Block{ai.TextBlock{Text: "assistant turn " + strconv.Itoa(n)}},
		StopReason: ai.StopReasonStop,
	}}); err != nil {
		t.Fatal(err)
	}
}

// notesPrereqRegistry registers the full rollover prerequisite set: read and
// grep stand in for the compaction/history surfaces. It returns the registry
// and its notes state (prerequisites satisfied by construction).
func notesPrereqRegistry(store *session.Store, cwd string) (*tool.Registry, *NotesState) {
	reg := tool.NewRegistry()
	ns := NewNotesState(reg, store)
	reg.Register(tool.NewReadTool())
	reg.Register(&tool.GrepTool{CWD: cwd})
	reg.Register(&NotesTool{Notes: ns})
	reg.Register(&NewContextTool{Notes: ns})
	return reg, ns
}

func TestContextNotesTruncatedAt16KiB(t *testing.T) {
	ns := NewNotesState(nil, notesOnDiskStore(t))
	big := strings.Repeat("a", 30000)
	stored, id, truncated, err := ns.Replace(big)
	if err != nil || !truncated {
		t.Fatalf("Replace: truncated=%v err=%v", truncated, err)
	}
	if id == "" {
		t.Fatal("stored revision must carry an entry id")
	}
	if len(stored) > MaxNotesBytes {
		t.Fatalf("stored = %d bytes, cap %d", len(stored), MaxNotesBytes)
	}
	if !strings.HasSuffix(stored, notesTruncationMarker) {
		t.Fatalf("truncation marker missing: %q", stored[len(stored)-60:])
	}
	if !utf8.ValidString(stored) {
		t.Fatal("stored notebook must stay valid UTF-8")
	}
	// The marker counts against the cap: the readable prefix is exactly
	// cap-minus-marker, and everything after that is the marker.
	if len(stored) != MaxNotesBytes || strings.TrimSuffix(stored, notesTruncationMarker) != strings.Repeat("a", MaxNotesBytes-len(notesTruncationMarker)) {
		t.Fatalf("stored = %d bytes, want the cap minus marker carried forward", len(stored))
	}
	// Multibyte input cuts on a rune boundary, not mid-character.
	stored2, _, truncated2, err := ns.Replace(strings.Repeat("é", 10000)) // 2 bytes each
	if err != nil || !truncated2 {
		t.Fatalf("Replace(multibyte): truncated=%v err=%v", truncated2, err)
	}
	if len(stored2) > MaxNotesBytes || !utf8.ValidString(stored2) {
		t.Fatalf("multibyte truncation: %d bytes, valid=%v", len(stored2), utf8.ValidString(stored2))
	}
}

func TestContextNotesReplaceViewRoundTrip(t *testing.T) {
	ns := NewNotesState(nil, notesOnDiskStore(t))
	if _, _, ok := ns.View(); ok {
		t.Fatal("fresh session must have no notes")
	}
	stored, id, truncated, err := ns.Replace("goal: land #45\n- notebook survives rollover")
	if err != nil || truncated {
		t.Fatalf("Replace: truncated=%v err=%v", truncated, err)
	}
	text, entryID, ok := ns.View()
	if !ok || text != stored || entryID != id {
		t.Fatalf("View = (%q, %q, %v), want stored revision %q", text, entryID, ok, id)
	}
	nt := &NotesTool{Notes: ns}
	res, err := nt.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil || res.IsError || res.Text != stored {
		t.Fatalf("read op: res=%q err=%v", res.Text, err)
	}
	// A replacement supersedes the revision.
	res2, err := nt.Execute(context.Background(), json.RawMessage(`{"text":"v2"}`))
	if err != nil || res2.IsError {
		t.Fatalf("replace op: res=%+v err=%v", res2, err)
	}
	if text2, _, _ := ns.View(); text2 != "v2" {
		t.Fatalf("View after replace = %q", text2)
	}
	// Clearing appends an empty revision; the read op then reports absence.
	res3, err := nt.Execute(context.Background(), json.RawMessage(`{"text":""}`))
	if err != nil || res3.IsError {
		t.Fatalf("clear op: res=%+v err=%v", res3, err)
	}
	res4, err := nt.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil || res4.IsError {
		t.Fatalf("read after clear: err=%v", err)
	}
	if res4.Text != "No context notes are stored for this session branch." {
		t.Fatalf("read after clear = %q", res4.Text)
	}
}

func TestContextNotesBranchScoped(t *testing.T) {
	store := notesOnDiskStore(t)
	ns := NewNotesState(nil, store)

	if err := store.Append(&session.MessageEntry{Message: ai.Message{
		Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "one"}},
	}}); err != nil {
		t.Fatal(err)
	}
	branchPoint := store.LeafID()
	if err := store.Append(&session.MessageEntry{Message: ai.Message{
		Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "two"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ns.Replace("A"); err != nil {
		t.Fatal(err)
	}
	branchARev := store.LeafID()

	// Switch to a branch created before the "A" revision: no notebook there.
	if err := store.Branch(branchPoint); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := ns.View(); ok {
		t.Fatal("notebook revision from another branch must not be visible")
	}
	if _, _, _, err := ns.Replace("B"); err != nil {
		t.Fatal(err)
	}
	if text, _, _ := ns.View(); text != "B" {
		t.Fatalf("View on branch B = %q", text)
	}
	// Switch back: the branch's own notebook returns.
	if err := store.Branch(branchARev); err != nil {
		t.Fatal(err)
	}
	if text, _, _ := ns.View(); text != "A" {
		t.Fatalf("View back on branch A = %q", text)
	}
}

func TestContextNotesHiddenByResetBoundary(t *testing.T) {
	store := notesOnDiskStore(t)
	ns := NewNotesState(nil, store)
	if _, _, _, err := ns.Replace("pre-reset"); err != nil {
		t.Fatal(err)
	}
	if err := store.ResetLeaf(); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := ns.View(); ok {
		t.Fatal("a context reset must hide earlier notebook revisions")
	}
}

func TestNewContextRolloverKeepsNotebookAndTail(t *testing.T) {
	store := notesOnDiskStore(t)
	const turns = 40
	for i := 0; i < turns; i++ {
		if i == turns/2 {
			if err := store.Append(&session.MessageEntry{Message: ai.Message{
				Role:    ai.RoleUser,
				Content: []ai.Block{ai.TextBlock{Text: "MIDDLE-MARKER user turn " + strconv.Itoa(i)}},
			}}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		turn(t, store, i)
	}
	reg, ns := notesPrereqRegistry(store, store.CWD())
	if _, _, _, err := ns.Replace("ship the notes-backed rollover"); err != nil {
		t.Fatal(err)
	}

	nc, _ := reg.Get(NewContextToolName)
	res, err := nc.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil || res.IsError {
		t.Fatalf("new_context: res=%+v err=%v", res, err)
	}
	if res.Text != "New context window requested." {
		t.Fatalf("new_context text = %q", res.Text)
	}
	if committed, _ := res.Details.(map[string]any)["committed"].(bool); !committed {
		t.Fatalf("details = %+v, want committed=true", res.Details)
	}

	// The boundary is a compaction entry with the rollover marker (no
	// recursive summary), carrying the notebook.
	var boundary *session.CompactionEntry
	for _, e := range store.Entries() {
		if c, ok := e.(*session.CompactionEntry); ok {
			boundary = c
		}
	}
	if boundary == nil {
		t.Fatal("rollover must append a compaction boundary entry")
	}
	if !IsRolloverBoundary(boundary) {
		t.Fatalf("boundary summary = %q, want the new_context marker", boundary.Summary.Text())
	}
	if boundary.Summary.Text() != rolloverSummary("ship the notes-backed rollover") {
		t.Fatalf("boundary summary = %q, want the notebook carried", boundary.Summary.Text())
	}

	// The rebuilt context keeps the notebook and the recent tail, drops the
	// middle.
	built, err := session.BuildContext(store.Entries(), store.LeafID(), session.SystemPrompt{})
	if err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	for _, m := range built.Messages {
		got.WriteString(m.Text())
		got.WriteString("\n")
	}
	joined := got.String()
	if !strings.Contains(joined, "ship the notes-backed rollover") {
		t.Fatal("rebuilt context must keep the notebook")
	}
	if !strings.Contains(joined, "assistant turn 39") {
		t.Fatal("rebuilt context must keep the recent tail")
	}
	if strings.Contains(joined, "MIDDLE-MARKER") {
		t.Fatal("rebuilt context must drop the middle")
	}
	if len(built.Messages) >= turns {
		t.Fatalf("rebuilt context has %d messages, want the bounded tail", len(built.Messages))
	}

	// The notebook revision survives the boundary: reads still project it.
	if text, _, _ := ns.View(); text != "ship the notes-backed rollover" {
		t.Fatalf("View after rollover = %q", text)
	}

	// The loop seam: a pending rollover replaces stale live history with the
	// rebuilt context, and the flag is consumed.
	stale := make([]ai.Message, 0, turns)
	for i := 0; i < turns; i++ {
		stale = append(stale, ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "stale turn " + strconv.Itoa(i)}}})
	}
	rebuilt := ns.RebuildIfPending(store, stale)
	if len(rebuilt) >= len(stale) {
		t.Fatalf("RebuildIfPending returned %d messages, want the rebuilt tail", len(rebuilt))
	}
	for _, m := range rebuilt {
		if strings.Contains(m.Text(), "stale turn") {
			t.Fatal("RebuildIfPending must replace stale history with store content")
		}
	}
	if len(rebuilt) == 0 || !strings.Contains(rebuilt[0].Text(), "ship the notes-backed rollover") {
		t.Fatal("rebuilt history must start with the boundary summary carrying the notebook")
	}
	if again := ns.RebuildIfPending(store, stale); len(again) != len(stale) {
		t.Fatal("a consumed rollover must not rebuild again")
	}
}

func TestNewContextNothingToDrop(t *testing.T) {
	store := notesOnDiskStore(t)
	turn(t, store, 0)
	reg, ns := notesPrereqRegistry(store, store.CWD())
	before := len(store.Entries())
	nc, _ := reg.Get(NewContextToolName)
	res, err := nc.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil || res.IsError {
		t.Fatalf("new_context: res=%+v err=%v", res, err)
	}
	if committed, _ := res.Details.(map[string]any)["committed"].(bool); committed {
		t.Fatalf("details = %+v, want committed=false on a tiny history", res.Details)
	}
	if len(store.Entries()) != before {
		t.Fatal("nothing to drop: no boundary entry may be appended")
	}
	// The pending flag must not be set when nothing was committed.
	keep := []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "keep"}}}}
	if rebuilt := ns.RebuildIfPending(store, keep); len(rebuilt) != 1 || rebuilt[0].Text() != "keep" {
		t.Fatal("no committed rollover: history must pass through unchanged")
	}
}

func TestNewContextPrerequisiteError(t *testing.T) {
	store := notesOnDiskStore(t)
	turn(t, store, 0)
	turn(t, store, 1)
	before := len(store.Entries())

	// grep is missing: the rollover refuses and appends nothing.
	reg := tool.NewRegistry()
	reg.Register(tool.NewReadTool())
	ns := NewNotesState(reg, store)
	reg.Register(&NotesTool{Notes: ns})
	reg.Register(&NewContextTool{Notes: ns})
	nc, _ := reg.Get(NewContextToolName)
	res, err := nc.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("res = %+v, want the prerequisite error", res)
	}
	for _, want := range []string{"grep", "context_notes", "new_context", "read"} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("error %q must name %q", res.Text, want)
		}
	}
	if len(store.Entries()) != before {
		t.Fatal("refused rollover must not append a boundary")
	}
	// A nil registry fails closed: every prerequisite reported missing.
	if got := NewNotesState(nil, nil).MissingPrerequisites(); len(got) != 4 {
		t.Fatalf("MissingPrerequisites on nil registry = %v", got)
	}
}

func TestHistoryURICurrentAndFull(t *testing.T) {
	store := notesOnDiskStore(t)
	ns := NewNotesState(nil, store)

	for i := 0; i < 3; i++ {
		turn(t, store, i)
	}
	branchPoint := store.LeafID()
	// One more turn on the active branch, then the leaf moves back and the
	// other branch writes its own turn.
	turn(t, store, 3)
	if err := store.Branch(branchPoint); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(&session.MessageEntry{Message: ai.Message{
		Role:    ai.RoleUser,
		Content: []ai.Block{ai.TextBlock{Text: "BRANCH-B-ONLY"}},
	}}); err != nil {
		t.Fatal(err)
	}

	// current: the active branch only, raw (never windowed).
	current, err := ns.ResolveHistory("history://current")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(current, "BRANCH-B-ONLY") {
		t.Fatal("history://current must contain the active branch's messages")
	}
	if strings.Contains(current, "assistant turn 3") {
		t.Fatal("history://current must not contain the sibling branch")
	}
	// full: every branch, with the jump marked.
	full, err := ns.ResolveHistory("history://full")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"BRANCH-B-ONLY", "assistant turn 3"} {
		if !strings.Contains(full, want) {
			t.Fatalf("history://full must contain %q", want)
		}
	}
	if !strings.Contains(full, "parent=") {
		t.Fatal("history://full must mark branch points")
	}
	if same, err := ns.ResolveHistory("history://current/full"); err != nil || same != full {
		t.Fatalf("history://current/full must resolve to the full view (err=%v)", err)
	}
	if _, err := ns.ResolveHistory("history://nope"); err == nil {
		t.Fatal("unknown target must error")
	}

	// The read tool seam: read history://current resolves end-to-end.
	tool.RegisterURIScheme("history", ns.ResolveHistory)
	rt := tool.NewReadTool()
	res, err := rt.Execute(context.Background(), json.RawMessage(`{"path":"history://current"}`))
	if err != nil || res.IsError {
		t.Fatalf("read history://current: res=%+v err=%v", res.Text, err)
	}
	if !strings.Contains(res.Text, "BRANCH-B-ONLY") {
		t.Fatalf("read history://current = %q", res.Text)
	}
}

// TestHistorySurvivesRolloverWindow is the recovery guarantee: after a
// rollover, the dropped middle is still reachable through history://current.
func TestHistorySurvivesRolloverWindow(t *testing.T) {
	store := notesOnDiskStore(t)
	for i := 0; i < 30; i++ {
		if i == 1 {
			if err := store.Append(&session.MessageEntry{Message: ai.Message{
				Role:    ai.RoleUser,
				Content: []ai.Block{ai.TextBlock{Text: "EARLY-MARKER"}},
			}}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		turn(t, store, i)
	}
	reg, ns := notesPrereqRegistry(store, store.CWD())
	if _, _, _, err := ns.Replace("notebook"); err != nil {
		t.Fatal(err)
	}
	nc, _ := reg.Get(NewContextToolName)
	if res, err := nc.Execute(context.Background(), json.RawMessage(`{}`)); err != nil || res.IsError {
		t.Fatalf("new_context: res=%+v err=%v", res, err)
	}
	raw, err := ns.ResolveHistory("history://current")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, "EARLY-MARKER") {
		t.Fatal("history://current must still expose the rolled-over middle")
	}
	// ...while the model context no longer carries it.
	built, err := session.BuildContext(store.Entries(), store.LeafID(), session.SystemPrompt{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range built.Messages {
		if strings.Contains(m.Text(), "EARLY-MARKER") {
			t.Fatal("the rolled-over middle must leave the model context")
		}
	}
}

func TestNotesStateBindSwitchesSession(t *testing.T) {
	a, b := notesOnDiskStore(t), notesOnDiskStore(t)
	ns := NewNotesState(nil, a)
	if _, _, _, err := ns.Replace("session A notes"); err != nil {
		t.Fatal(err)
	}
	ns.Bind(b)
	if _, _, ok := ns.View(); ok {
		t.Fatal("a session switch must not leak the previous notebook")
	}
	ns.Bind(a)
	if text, _, _ := ns.View(); text != "session A notes" {
		t.Fatalf("View after rebind = %q", text)
	}
}

func TestNotesStateOfRegistry(t *testing.T) {
	reg, ns := notesPrereqRegistry(notesOnDiskStore(t), "/tmp")
	if got := NotesStateOf(reg); got != ns {
		t.Fatal("NotesStateOf must return the wired state")
	}
	if got := NotesStateOf(tool.NewRegistry()); got != nil {
		t.Fatal("a registry without the notes tool must return nil")
	}
}
