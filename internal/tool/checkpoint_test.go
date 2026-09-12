package tool

// Checkpoint/rewind tests (M13 #51). They run against a real on-disk session
// store because the contract is about what lands in the session file and what
// the rebuilt context shows — not about in-memory bookkeeping.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// checkpointFixture opens a real session store in a temp dir and returns it
// with both checkpoint tools bound to it.
func checkpointFixture(t *testing.T) (*session.Store, string, *CheckpointTool, *RewindTool) {
	t.Helper()
	dir := t.TempDir()
	store := session.OpenMem(dir, "checkpoint test")
	t.Cleanup(func() { _ = store.Close() })
	path := filepath.Join(dir, "s.jsonl")
	if _, err := store.EnsureOnDisk(path, session.Options{}); err != nil {
		t.Fatal(err)
	}
	return store, path, &CheckpointTool{Store: store}, &RewindTool{Store: store}
}

// say appends one user message and returns its entry id.
func say(t *testing.T, store *session.Store, text string) string {
	t.Helper()
	e := &session.MessageEntry{Message: ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: text}}}}
	if err := store.Append(e); err != nil {
		t.Fatal(err)
	}
	return e.Env.ID
}

// detailsMap asserts the result carries structured details as a map.
func detailsMap(t *testing.T, res Result) map[string]any {
	t.Helper()
	d, ok := res.Details.(map[string]any)
	if !ok {
		t.Fatalf("details = %T, want map[string]any", res.Details)
	}
	return d
}

func TestCheckpointRewindRoundTrip(t *testing.T) {
	store, path, cp, rw := checkpointFixture(t)
	say(t, store, "hello")
	root := say(t, store, "the state worth keeping")

	res := runTool(t, cp, map[string]any{"name": "keep", "note": "before exploring"})
	if res.IsError {
		t.Fatalf("create failed: %s", res.Text)
	}
	cpID, _ := detailsMap(t, res)["checkpointId"].(string)
	if cpID == "" {
		t.Fatalf("no checkpoint id returned: %s", res.Text)
	}
	cpEntry, ok := store.Entry(cpID).(*session.CheckpointEntry)
	if !ok {
		t.Fatalf("checkpoint id %q is not a checkpoint entry", cpID)
	}
	if cpEntry.Checkpoint.Name != "keep" || cpEntry.Checkpoint.EntryID != root || cpEntry.Checkpoint.Note != "before exploring" {
		t.Fatalf("checkpoint payload = %+v, want {keep %s before exploring}", cpEntry.Checkpoint, root)
	}
	if got := store.LeafID(); got != cpID {
		t.Fatalf("creating a checkpoint moved the leaf to %s, want the checkpoint entry %s", got, cpID)
	}

	// The exploration the rewind is about to abandon.
	say(t, store, "exploration one")
	deadEnd := say(t, store, "exploration two")
	before := len(store.Entries())

	res = runTool(t, rw, map[string]any{"name": "keep", "report": "one and two were wrong; the root state stands"})
	if res.IsError {
		t.Fatalf("rewind failed: %s", res.Text)
	}

	// The leaf is now the report entry, parented at the checkpoint target:
	// the next provider call rebuilds from that point, not from the dead end.
	leafID := store.LeafID()
	report, ok := store.Entry(leafID).(*session.BranchSummaryEntry)
	if !ok {
		t.Fatalf("leaf = %T, want *session.BranchSummaryEntry", store.Entry(leafID))
	}
	if env := report.Envelope(); env.ParentID != root {
		t.Fatalf("report parent = %s, want the checkpoint entry %s", env.ParentID, root)
	}
	if txt := report.Summary.Text(); !strings.Contains(txt, `Rewind to checkpoint "keep"`) || !strings.Contains(txt, "one and two were wrong") {
		t.Fatalf("report text = %q", txt)
	}

	// The branch marker rode the store's existing machinery.
	var marker *session.CustomEntry
	for _, e := range store.Entries() {
		if c, ok := e.(*session.CustomEntry); ok && c.CustomType == session.TypeBranch {
			marker = c
		}
	}
	if marker == nil || marker.Data["to"] != root {
		t.Fatalf("branch marker = %+v, want to=%s", marker, root)
	}

	// Nothing is deleted: the two abandoned messages are still in the file,
	// alongside the marker and the report.
	after := store.Entries()
	if len(after) != before+2 {
		t.Fatalf("entries = %d, want %d (branch marker + report)", len(after), before+2)
	}
	onDisk := map[string]bool{}
	for _, e := range after {
		onDisk[e.Envelope().ID] = true
	}
	for _, id := range []string{root, deadEnd, cpID} {
		if !onDisk[id] {
			t.Fatalf("entry %s was dropped from the session", id)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "before exploring") {
		t.Fatal("checkpoint entry never reached the session file")
	}

	// Rebuilt context: the report is visible, the abandoned turns are not.
	res2, err := session.BuildContext(after, leafID, session.SystemPrompt{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Messages) == 0 {
		t.Fatal("rebuilt context is empty")
	}
	last := res2.Messages[len(res2.Messages)-1]
	if !strings.Contains(last.Text(), "Rewind to checkpoint") {
		t.Fatalf("last rebuilt message = %q, want the rewind report", last.Text())
	}
	for _, m := range res2.Messages {
		if strings.Contains(m.Text(), "exploration one") {
			t.Fatalf("abandoned turn still in the rebuilt context: %q", m.Text())
		}
	}
	if d := detailsMap(t, res); d["entriesLeft"] != 3 {
		t.Fatalf("details entriesLeft = %v, want 3 (two messages + the rewind's own entry)", d["entriesLeft"])
	}
}

// TestRewindReplaysOnReopen: the moved leaf survives a process restart, so a
// resumed session continues on the kept path instead of the dead end.
func TestRewindReplaysOnReopen(t *testing.T) {
	store, path, cp, rw := checkpointFixture(t)
	say(t, store, "start")
	say(t, store, "keep me")
	runTool(t, cp, map[string]any{"name": "keep"})
	say(t, store, "abandon me")
	runTool(t, rw, map[string]any{"name": "keep", "report": "abandoned"})
	wantLeaf := store.LeafID()
	if wantLeaf == "" {
		t.Fatal("no leaf after rewind")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := session.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if got := reopened.LeafID(); got != wantLeaf {
		t.Fatalf("reopened leaf = %s, want %s", got, wantLeaf)
	}
	ctxRes, err := session.BuildContext(reopened.Entries(), reopened.LeafID(), session.SystemPrompt{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ctxRes.Messages {
		if strings.Contains(m.Text(), "abandon me") {
			t.Fatalf("abandoned message survived the resume: %q", m.Text())
		}
	}
	// The checkpoint is still reachable through the reopened store.
	list := &CheckpointTool{Store: reopened}
	res := runTool(t, list, map[string]any{"op": "list"})
	if res.IsError || !strings.Contains(res.Text, "keep") {
		t.Fatalf("list after reopen = %q (err=%v)", res.Text, res.IsError)
	}
}

func TestCheckpointListShape(t *testing.T) {
	store, _, cp, _ := checkpointFixture(t)

	res := runTool(t, cp, map[string]any{"op": "list"})
	if res.IsError {
		t.Fatalf("empty list errored: %s", res.Text)
	}
	if !strings.Contains(res.Text, "No checkpoints") {
		t.Fatalf("empty list = %q", res.Text)
	}
	say(t, store, "one")
	firstLeaf := say(t, store, "two")
	runTool(t, cp, map[string]any{"name": "alpha", "note": "n1"})
	say(t, store, "three")
	runTool(t, cp, map[string]any{"name": "omega"})
	res = runTool(t, cp, map[string]any{"op": "list"})
	if res.IsError {
		t.Fatalf("list errored: %s", res.Text)
	}
	iAlpha, iOmega := strings.Index(res.Text, "alpha"), strings.Index(res.Text, "omega")
	if iAlpha < 0 || iOmega < 0 || iOmega > iAlpha {
		t.Fatalf("list not newest first:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "note: n1") {
		t.Fatalf("note missing from list:\n%s", res.Text)
	}
	rows, ok := res.Details.([]map[string]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("rows = %#v, want 2", res.Details)
	}
	if rows[0]["name"] != "omega" {
		t.Fatalf("first row = %#v, want the newest checkpoint", rows[0])
	}
	for _, r := range rows {
		for _, k := range []string{"name", "id", "entryId", "created"} {
			if s, _ := r[k].(string); s == "" {
				t.Fatalf("row %#v is missing %q", r, k)
			}
		}
	}
	if rows[1]["entryId"] != firstLeaf {
		t.Fatalf("alpha.entryId = %v, want the leaf at checkpoint time %s", rows[1]["entryId"], firstLeaf)
	}
}

// TestCheckpointListIsBounded: the list caps at checkpointListMax newest rows
// and says so instead of silently truncating.
func TestCheckpointListIsBounded(t *testing.T) {
	store, _, cp, _ := checkpointFixture(t)
	say(t, store, "seed")
	total := checkpointListMax + 5
	for i := range total {
		runTool(t, cp, map[string]any{"name": fmt.Sprintf("cp-%03d", i)})
	}
	res := runTool(t, cp, map[string]any{"op": "list"})
	rows, ok := res.Details.([]map[string]any)
	if !ok || len(rows) != checkpointListMax {
		t.Fatalf("rows = %d, want the %d cap", len(rows), checkpointListMax)
	}
	if rows[0]["name"] != fmt.Sprintf("cp-%03d", total-1) {
		t.Fatalf("newest row = %v, want cp-%03d", rows[0]["name"], total-1)
	}
	if !strings.Contains(res.Text, fmt.Sprintf("Newest %d of %d checkpoints", checkpointListMax, total)) {
		t.Fatalf("list header = %q", res.Text)
	}
}

func TestRewindUnknownCheckpointListsNames(t *testing.T) {
	store, _, cp, rw := checkpointFixture(t)
	res := runTool(t, rw, map[string]any{"name": "nope", "report": "r"})
	if !res.IsError || !strings.Contains(res.Text, "no checkpoints yet") {
		t.Fatalf("rewind on an empty session = %q (err=%v)", res.Text, res.IsError)
	}

	say(t, store, "state")
	runTool(t, cp, map[string]any{"name": "alpha"})
	runTool(t, cp, map[string]any{"name": "beta"})
	res = runTool(t, rw, map[string]any{"name": "gamma", "report": "r"})
	if !res.IsError {
		t.Fatalf("unknown checkpoint accepted: %s", res.Text)
	}
	if !strings.Contains(res.Text, `"gamma"`) || !strings.Contains(res.Text, "alpha") || !strings.Contains(res.Text, "beta") {
		t.Fatalf("unknown-name error does not name the available checkpoints: %q", res.Text)
	}
}

// TestRewindRefusedWhileTurnRunning: the running probe is the safety seam for
// a harness that drives rewind itself; a refusal must leave the session
// untouched.
func TestRewindRefusedWhileTurnRunning(t *testing.T) {
	store, _, cp, rw := checkpointFixture(t)
	say(t, store, "state worth keeping")
	runTool(t, cp, map[string]any{"name": "keep"})
	say(t, store, "exploration")
	leafBefore, entriesBefore := store.LeafID(), len(store.Entries())

	rw.Running = func() bool { return true }
	res := runTool(t, rw, map[string]any{"name": "keep", "report": "r"})
	if !res.IsError || !strings.Contains(res.Text, "a turn is running") {
		t.Fatalf("rewind while a turn runs = %q (err=%v)", res.Text, res.IsError)
	}
	if store.LeafID() != leafBefore || len(store.Entries()) != entriesBefore {
		t.Fatalf("refused rewind mutated the session (leaf %s→%s, %d→%d entries)",
			leafBefore, store.LeafID(), entriesBefore, len(store.Entries()))
	}

	// The same call succeeds once the turn is over.
	rw.Running = func() bool { return false }
	res = runTool(t, rw, map[string]any{"name": "keep", "report": "done"})
	if res.IsError {
		t.Fatalf("rewind after the turn = %s", res.Text)
	}
	if store.LeafID() == leafBefore {
		t.Fatal("rewind did not move the leaf")
	}
}

func TestCheckpointArgsValidation(t *testing.T) {
	store, _, cp, rw := checkpointFixture(t)

	if res := runTool(t, cp, map[string]any{"name": "x"}); !res.IsError || !strings.Contains(res.Text, "no entries yet") {
		t.Fatalf("checkpoint on an empty session = %q (err=%v)", res.Text, res.IsError)
	}
	say(t, store, "state")
	if res := runTool(t, cp, map[string]any{"name": "  "}); !res.IsError || !strings.Contains(res.Text, "name required") {
		t.Fatalf("blank name = %q (err=%v)", res.Text, res.IsError)
	}
	if res := runTool(t, cp, map[string]any{"op": "drop", "name": "x"}); !res.IsError || !strings.Contains(res.Text, `unknown op "drop"`) {
		t.Fatalf("bad op = %q (err=%v)", res.Text, res.IsError)
	}
	runTool(t, cp, map[string]any{"name": "keep"})

	if res := runTool(t, rw, map[string]any{"name": "keep"}); !res.IsError || !strings.Contains(res.Text, "report required") {
		t.Fatalf("missing report = %q (err=%v)", res.Text, res.IsError)
	}
	if res := runTool(t, rw, map[string]any{"report": "r"}); !res.IsError || !strings.Contains(res.Text, "name required") {
		t.Fatalf("missing name = %q (err=%v)", res.Text, res.IsError)
	}

	// Unbound tools (no session yet) must answer instead of panicking.
	for _, tl := range []Tool{&CheckpointTool{}, &RewindTool{}} {
		if res := runTool(t, tl, map[string]any{"name": "keep", "report": "r"}); !res.IsError || !strings.Contains(res.Text, "no session store") {
			t.Fatalf("%s unbound = %q (err=%v)", tl.Name(), res.Text, res.IsError)
		}
	}

	// A bound registry answers through WireCheckpoint, the production path.
	reg := NewRegistry()
	reg.Register(&CheckpointTool{})
	reg.Register(&RewindTool{})
	if n := WireCheckpoint(reg, store, nil); n != 2 {
		t.Fatalf("WireCheckpoint bound %d tools, want 2", n)
	}
	res := runTool(t, toolFrom(t, reg, "checkpoint"), map[string]any{"op": "list"})
	if res.IsError || !strings.Contains(res.Text, "keep") {
		t.Fatalf("wired list = %q (err=%v)", res.Text, res.IsError)
	}
}

// TestBoundTextClips: caller-supplied text is bounded before it becomes a
// session entry.
func TestBoundTextClips(t *testing.T) {
	if got := boundText("  hi  ", 10); got != "hi" {
		t.Fatalf("trim = %q", got)
	}
	if got := boundText("abcdef", 3); got != "abc…" {
		t.Fatalf("clip = %q", got)
	}
	// Multi-byte runes are clipped on a rune boundary, never mid-rune.
	if got := boundText("héllo wörld", 5); got != "héllo…" {
		t.Fatalf("rune clip = %q", got)
	}
}

// TestCheckpointWireArgsAreJSON guards the tool contract the model sees: both
// schemas parse and declare the fields the implementations read.
func TestCheckpointWireArgsAreJSON(t *testing.T) {
	for _, tl := range []Tool{&CheckpointTool{}, &RewindTool{}} {
		var schema struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		if err := json.Unmarshal(tl.Parameters(), &schema); err != nil {
			t.Fatalf("%s parameters: %v", tl.Name(), err)
		}
		if schema.Type != "object" || len(schema.Properties) == 0 {
			t.Fatalf("%s parameters = %s", tl.Name(), tl.Parameters())
		}
		if tl.Description() == "" || strings.Contains(tl.Description(), "\n") {
			t.Fatalf("%s description must be a non-empty one-liner", tl.Name())
		}
	}
}
