package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/tool"
)

// The compaction record (omp rule 27) is filled on EVERY path that
// persists a compaction — the ladder, the handoff, the notes rollover and
// the CLI — so a reader sees the same facts whichever path wrote the line.

// TestTheHandoffPathFillsTheCompactionRecord pins the handoff path: the
// entry a handoff appends says what the context it left behind looked
// like.
func TestTheHandoffPathFillsTheCompactionRecord(t *testing.T) {
	store := seedHandoffStore(t)
	side := &docProvider{doc: "HANDOFF DOC"}
	ag := &Agent{
		Provider: side, Tools: tool.NewRegistry(), Model: "big", Store: store,
		Compaction: CompactionConfig{ContextWindow: 200_000, KeepRecentTokens: 10},
		Handoff:    HandoffSettings{},
	}
	if _, err := ag.HandoffDoc(context.Background(), "sys", ""); err != nil {
		t.Fatal(err)
	}

	entry := lastCompaction(t, store)
	if entry.TokensAfter <= 0 {
		t.Errorf("tokensAfter = %d, want the size of the surviving context", entry.TokensAfter)
	}
	if entry.ShortSummary == "" {
		t.Error("the handoff entry carries no short summary")
	}
	if entry.Method != MethodHandoff {
		t.Errorf("method = %q, want %q (the helper must not overwrite it)", entry.Method, MethodHandoff)
	}
}

// TestTheRolloverPathFillsTheCompactionRecord pins the notes rollover path:
// the boundary entry says what the context it left behind looked like.
func TestTheRolloverPathFillsTheCompactionRecord(t *testing.T) {
	store := notesOnDiskStore(t)
	const turns = 40
	for i := range turns {
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

	entry := lastCompaction(t, store)
	if entry.TokensAfter <= 0 {
		t.Errorf("tokensAfter = %d, want the size of the surviving context", entry.TokensAfter)
	}
	if entry.ShortSummary == "" {
		t.Error("the rollover boundary carries no short summary")
	}
}

// lastCompaction returns the compaction entry the store holds (the last
// one — the rollover appends its notebook revision after the boundary).
func lastCompaction(t *testing.T, store *session.Store) *session.CompactionEntry {
	t.Helper()
	var entry *session.CompactionEntry
	for _, e := range store.Entries() {
		if c, ok := e.(*session.CompactionEntry); ok {
			entry = c
		}
	}
	if entry == nil {
		t.Fatal("the store holds no compaction entry")
	}
	return entry
}

// TestAZeroContextWindowNeverTriggersACompaction pins the zero-window
// guard: threshold() is 0 without a window, so "used > 0" would fire on
// any history at all. omp never compacts without a window, so a session
// with no window must not compact on the token budget.
func TestAZeroContextWindowNeverTriggersACompaction(t *testing.T) {
	stubPressure(t, 0)
	history := []ai.Message{userText(strings.Repeat("x", 4000)), userText("keep"), userText("keep")}

	// A window of zero: the threshold is zero, so the unguarded check
	// ("used > 0") would fire.
	cfg := CompactionConfig{ContextWindow: 0, KeepRecentTokens: 5, ThresholdTokens: 1, Methods: []string{"soft"}}
	if got := cfg.threshold(); got != 0 {
		t.Fatalf("threshold = %d, want 0 (a zero window has no threshold)", got)
	}
	p := &fakeProvider{}
	a, s, _ := storeAgent(t, p, cfg)
	for _, m := range history {
		appendMsg(t, s, m)
	}
	if a.compactionDue(history) {
		t.Error("a zero context window must not trigger a threshold compaction")
	}
	_ = a.maybeCompact(context.Background(), history)
	if n := len(compactionEntries(s)); n != 0 {
		t.Fatalf("compaction entries = %d, want 0 (a zero window never compacts)", n)
	}

	// A negative window is the same case.
	cfgNeg := CompactionConfig{ContextWindow: -1, KeepRecentTokens: 5, ThresholdTokens: 1, Methods: []string{"soft"}}
	p2 := &fakeProvider{}
	a2, s2, _ := storeAgent(t, p2, cfgNeg)
	for _, m := range history {
		appendMsg(t, s2, m)
	}
	if a2.compactionDue(history) {
		t.Error("a negative context window must not trigger a threshold compaction")
	}
	_ = a2.maybeCompact(context.Background(), history)
	if n := len(compactionEntries(s2)); n != 0 {
		t.Fatalf("compaction entries = %d, want 0 (a negative window never compacts)", n)
	}
}

// TestThePersistedCompactionLineCarriesTheRecord pins the WIRE: the line
// that lands in the session file carries the surviving size and the
// one-line summary, whichever path wrote it.
func TestThePersistedCompactionLineCarriesTheRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	store := session.OpenMem("test", "t")
	if _, err := store.EnsureOnDisk(path, session.Options{}); err != nil {
		t.Fatal(err)
	}
	side := &docProvider{doc: "HANDOFF DOC"}
	ag := &Agent{
		Provider: side, Tools: tool.NewRegistry(), Model: "big", Store: store,
		Compaction: CompactionConfig{ContextWindow: 200_000, KeepRecentTokens: 10},
		Handoff:    HandoffSettings{},
	}
	for _, text := range []string{
		"first turn: " + strings.Repeat("alpha ", 40),
		"second turn: " + strings.Repeat("beta ", 40),
	} {
		if err := store.Append(&session.MessageEntry{Message: ai.Message{
			Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: text}},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ag.HandoffDoc(context.Background(), "sys", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	last := lines[len(lines)-1]
	var wire map[string]any
	if err := json.Unmarshal([]byte(last), &wire); err != nil {
		t.Fatalf("the appended line is not JSON: %v (%s)", err, last)
	}
	if _, ok := wire["tokensAfter"]; !ok {
		t.Errorf("the appended line carries no tokensAfter: %s", last)
	}
	short, ok := wire["shortSummary"].(string)
	if !ok || short == "" {
		t.Errorf("the appended line carries no shortSummary: %s", last)
	}
}
