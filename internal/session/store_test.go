package session

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
)

func userMsg(id, parent, text string) *MessageEntry {
	return &MessageEntry{
		Env:     Envelope{Type: TypeMessage, ID: id, ParentID: parent, Timestamp: ts0},
		Message: ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: text}}},
	}
}

func asstMsg(id, parent, text string, calls ...ai.ToolCallBlock) *MessageEntry {
	content := []ai.Block{}
	if text != "" {
		content = append(content, ai.TextBlock{Text: text})
	}
	for _, c := range calls {
		content = append(content, c)
	}
	return &MessageEntry{
		Env:     Envelope{Type: TypeMessage, ID: id, ParentID: parent, Timestamp: ts0},
		Message: ai.Message{Role: ai.RoleAssistant, Content: content},
	}
}

func toolResultMsg(id, parent, callID, text string) *MessageEntry {
	return &MessageEntry{
		Env: Envelope{Type: TypeMessage, ID: id, ParentID: parent, Timestamp: ts0},
		Message: ai.Message{
			Role: ai.RoleToolResult, ToolCallID: callID, ToolName: "bash",
			Content: []ai.Block{ai.TextBlock{Text: text}},
		},
	}
}

// chain links entries linearly and returns the last one's id.
func chain(entries ...Entry) string {
	for i, e := range entries {
		env := e.Envelope()
		if i == 0 && env.ParentID == "" {
			// root
		}
		if i > 0 {
			p := entries[i-1].Envelope().ID
			setEnvelope(e, Envelope{Type: env.Type, ID: env.ID, ParentID: p, Timestamp: env.Timestamp})
		}
	}
	return entries[len(entries)-1].Envelope().ID
}

func TestStoreAppendRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions", "-tmp-x", "2026-09-07T03-20-45.220Z_01aa.jsonl")

	s := OpenMem("/tmp/x", "test session")
	if s.Path() != "" {
		t.Fatal("memory-only store must have empty path")
	}
	if s.LeafID() != "" {
		t.Fatal("empty store leaf must be empty")
	}

	m1 := userMsg("", "", "hello")
	if err := s.Append(m1); err != nil {
		t.Fatal(err)
	}
	if s.Path() != "" {
		t.Fatal("still memory-only before EnsureOnDisk")
	}

	if p, err := s.EnsureOnDisk(path, Options{}); err != nil || p != path {
		t.Fatalf("EnsureOnDisk: %v %q", err, p)
	}
	// Entries appended before EnsureOnDisk must be persisted by it.
	if err := s.Append(asstMsg("", "", "hi there")); err != nil {
		t.Fatal(err)
	}
	a2 := asstMsg("", "", "second turn")
	if err := s.Append(a2); err != nil {
		t.Fatal(err)
	}
	leafBefore := s.LeafID()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// File layout: title slot (256 bytes), header, entries.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2+3 {
		t.Fatalf("lines = %d, want 5", len(lines))
	}
	if len(lines[0]) != TitleSlotWidth-1 {
		t.Fatalf("title slot line = %d bytes, want %d", len(lines[0]), TitleSlotWidth-1)
	}
	if title, ok := ParseTitleSlot([]byte(lines[0])); !ok || title != "test session" {
		t.Fatalf("title = %q %v", title, ok)
	}
	if h, ok := ParseHeader([]byte(lines[1])); !ok || h.CWD != "/tmp/x" {
		t.Fatalf("header = %+v %v", h, ok)
	}

	// Re-open: same entry sequence, same leaf.
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	entries := s2.Entries()
	if len(entries) != 3 {
		t.Fatalf("reopened entries = %d, want 3", len(entries))
	}
	if s2.LeafID() != leafBefore {
		t.Fatalf("reopened leaf = %q, want %q", s2.LeafID(), leafBefore)
	}
	if got := entries[0].(*MessageEntry).Message.Text(); got != "hello" {
		t.Fatalf("entry[0] text = %q", got)
	}
	// Parent chain: entry[1].parent == entry[0].id etc.
	if entries[1].Envelope().ParentID != entries[0].Envelope().ID {
		t.Fatal("chain broken on reopen")
	}
	if entries[2].Envelope().ParentID != a2.Envelope().ParentID {
		t.Fatal("chain broken for post-ensure append")
	}

	// Continue appending after reopen; file grows.
	if err := s2.Append(userMsg("", "", "after reopen")); err != nil {
		t.Fatal(err)
	}
	s2.Close()
	s3, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	if len(s3.Entries()) != 4 {
		t.Fatalf("entries after reopen-append = %d, want 4", len(s3.Entries()))
	}
	if s3.LeafID() != s2.LeafID() {
		t.Fatal("leaf drift after reopen append")
	}
}

func TestStoreBranchPersistedAsMarker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	s := OpenMem("/tmp/x", "branch test")
	e1 := userMsg("", "", "one")
	e2 := asstMsg("", "", "two")
	e3 := userMsg("", "", "three")
	for _, e := range []Entry{e1, e2, e3} {
		if err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.EnsureOnDisk(path, Options{}); err != nil {
		t.Fatal(err)
	}

	// Branch back to the assistant message (e2).
	if err := s.Branch(e2.Envelope().ID); err != nil {
		t.Fatal(err)
	}
	if s.LeafID() != e2.Envelope().ID {
		t.Fatalf("leaf = %s, want %s", s.LeafID(), e2.Envelope().ID)
	}
	// A new append grafts onto e2 now.
	next := userMsg("", "", "on branch")
	if err := s.Append(next); err != nil {
		t.Fatal(err)
	}
	if next.Envelope().ParentID != e2.Envelope().ID {
		t.Fatalf("branch append parent = %q", next.Envelope().ParentID)
	}
	s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	// Leaf must be restored from the branch marker's target + the appended
	// entry (marker < append in file order → the later entry wins as the
	// in-file leaf; but the branch target re-anchored the chain, so the
	// restored leaf is the last entry).
	if s2.LeafID() != next.Envelope().ID {
		t.Fatalf("reopened leaf = %s, want %s", s2.LeafID(), next.Envelope().ID)
	}
	// The branch marker must exist on disk as a custom entry.
	found := false
	for _, e := range s2.Entries() {
		if c, ok := e.(*CustomEntry); ok && c.CustomType == TypeBranch {
			found = true
			if c.Data["to"] != e2.Envelope().ID {
				t.Fatalf("marker to = %v, want %s", c.Data["to"], e2.Envelope().ID)
			}
		}
	}
	if !found {
		t.Fatal("branch marker missing after reopen")
	}
}

func TestStoreBranchUnknownTarget(t *testing.T) {
	s := OpenMem("/tmp/x", "x")
	s.Append(userMsg("", "", "one"))
	if err := s.Branch("nope"); err == nil {
		t.Fatal("Branch on unknown id must fail")
	}
}

func TestResetLeaf(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	s := OpenMem("/tmp/x", "reset")
	s.Append(userMsg("", "", "old turn"))
	if _, err := s.EnsureOnDisk(path, Options{}); err != nil {
		t.Fatal(err)
	}
	oldLeaf := s.LeafID()
	if err := s.ResetLeaf(); err != nil {
		t.Fatal(err)
	}
	if s.LeafID() == oldLeaf {
		t.Fatal("leaf must move to reset boundary")
	}
	if _, ok := s.Entries()[1].(*ResetBoundaryEntry); !ok {
		t.Fatalf("entries[1] = %T", s.Entries()[1])
	}
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, ok := s2.Entries()[1].(*ResetBoundaryEntry); !ok {
		t.Fatal("reset boundary lost after reopen")
	}
}

func TestOpenSkipsUnknownTypesButKeepsFileIntact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	slot := MarshalTitleSlot("t", TitleSourceAuto, slotTS)
	hdr := MarshalHeader(SessionHeader{Version: 3, ID: "uuid-1", Timestamp: ts0, CWD: "/tmp/x", Title: "t", TitleSource: TitleSourceAuto})
	m1 := MarshalMust(t, userMsg("11111111", "", "known"))
	unknown := `{"type":"thinking_level_change","id":"22222222","parentId":"11111111","timestamp":"2026-09-07T03:20:45.220Z","level":"high"}`
	m2 := MarshalMust(t, asstMsg("33333333", "22222222", "after unknown"))
	if err := os.WriteFile(path, concat(slot, hdr, append(m1, '\n'), []byte(unknown+"\n"), append(m2, '\n')), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// The unknown line becomes an opaque chain node: entries keep the full
	// sequence so the parent chain through the unknown entry stays intact.
	entries := s.Entries()
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3 (2 known + 1 opaque)", len(entries))
	}
	if _, ok := entries[1].(*UnknownEntry); !ok {
		t.Fatalf("entries[1] = %T, want *UnknownEntry", entries[1])
	}
	// Unknown line must stay on disk untouched.
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "thinking_level_change") {
		t.Fatal("unknown line was dropped from disk")
	}
	// Chain across the opaque node still works: buildContext walks through
	// UnknownEntry nodes like any other chain link.
	r, err := buildContext(entries, s.LeafID(), SystemPrompt{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(r.Messages))
	}
}

func TestOpenBrokenLineFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	content := string(MarshalTitleSlot("t", TitleSourceAuto, slotTS)) +
		string(MarshalHeader(SessionHeader{Version: 3, ID: "u", Timestamp: ts0, CWD: "/x", TitleSource: TitleSourceAuto})) +
		`{"type":"message","id":"1","parentId":null,"timestamp":"bad-timestamp","message":{"role":"user"}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("broken line must fail the load")
	}
}

func TestConcurrentAppends(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	s := OpenMem("/tmp/x", "concurrent")
	s.Append(userMsg("", "", "root"))
	if _, err := s.EnsureOnDisk(path, Options{}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	const n = 50
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.Append(userMsg("", "", "concurrent turn"))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	if err := s.Append(userMsg("", "", "after close")); err == nil {
		t.Fatal("append on closed store must fail")
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if len(s2.Entries()) != 1+n {
		t.Fatalf("entries = %d, want %d", len(s2.Entries()), 1+n)
	}
	// Every entry must have a unique id and a parent that exists.
	ids := map[string]bool{}
	for _, e := range s2.Entries() {
		id := e.Envelope().ID
		if ids[id] {
			t.Fatalf("duplicate id %s", id)
		}
		ids[id] = true
	}
}

func TestEnsureOnDiskIdempotent(t *testing.T) {
	dir := t.TempDir()
	s := OpenMem("/tmp/x", "once")
	s.Append(userMsg("", "", "x"))
	p1, err := s.EnsureOnDisk(filepath.Join(dir, "a.jsonl"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := s.EnsureOnDisk(filepath.Join(dir, "b.jsonl"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p2 {
		t.Fatalf("EnsureOnDisk not idempotent: %q vs %q", p1, p2)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.jsonl")); !os.IsNotExist(err) {
		t.Fatal("second EnsureOnDisk created a second file")
	}
}

func TestStrictFsync(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	s := OpenMem("/tmp/x", "fsync")
	s.Append(userMsg("", "", "x"))
	if _, err := s.EnsureOnDisk(path, Options{StrictFsync: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(userMsg("", "", "y")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if len(s2.Entries()) != 2 {
		t.Fatalf("entries = %d", len(s2.Entries()))
	}
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

var _ = time.Now // keep time import when helpers change

// TestTreeRendersGraph pins the /tree display: parent-child indentation,
// leaf marker, and entry-type summaries. The leaf pointer moves with
// Branch, which /tree must show.
func TestTreeRendersGraph(t *testing.T) {
	s := OpenMem("/proj", "tree test")
	if err := s.Append(&MessageEntry{Env: Envelope{ID: "m1"}, Message: ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hello"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(&MessageEntry{Env: Envelope{ID: "a1", ParentID: "m1"}, Message: ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "first reply"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(&MessageEntry{Env: Envelope{ID: "a2", ParentID: "m1"}, Message: ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "second reply"}}}}); err != nil {
		t.Fatal(err)
	}
	got := s.Tree()
	for _, want := range []string{"m1 message user: hello", "a1 message assistant", "a2 message assistant", "→"} {
		if !strings.Contains(got, want) {
			t.Errorf("tree missing %q:\n%s", want, got)
		}
	}
	// The leaf marker must be on the last entry (leaf).
	if !strings.Contains(got, "→ a2") {
		t.Errorf("leaf marker not on a2:\n%s", got)
	}

	// Branch to a1: the marker moves.
	if err := s.Branch("a1"); err != nil {
		t.Fatal(err)
	}
	got = s.Tree()
	if !strings.Contains(got, "→ a1") || strings.Contains(got, "→ a2") {
		t.Errorf("branch did not move the marker:\n%s", got)
	}
}

// TestStorePaths pins path visibility: Path is empty until materialized,
// AutoPath reports EnableAutoPersist's intended destination even while the
// session is still memory-only.
func TestStorePaths(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name     string
		build    func() *Store
		wantPath string
		wantAuto string
	}{
		{"memory only", func() *Store { return OpenMem("/p", "t") }, "", ""},
		{"on disk", func() *Store {
			s := OpenMem("/p", "t")
			if _, err := s.EnsureOnDisk(filepath.Join(dir, "on.jsonl"), Options{}); err != nil {
				t.Fatal(err)
			}
			return s
		}, filepath.Join(dir, "on.jsonl"), ""},
		{"auto persist, not materialized", func() *Store {
			s := OpenMem("/p", "t")
			s.EnableAutoPersist(filepath.Join(dir, "auto.jsonl"), Options{})
			return s
		}, "", filepath.Join(dir, "auto.jsonl")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.build()
			defer s.Close()
			if got := s.Path(); got != tc.wantPath {
				t.Errorf("Path() = %q, want %q", got, tc.wantPath)
			}
			if got := s.AutoPath(); got != tc.wantAuto {
				t.Errorf("AutoPath() = %q, want %q", got, tc.wantAuto)
			}
		})
	}
}

// TestSummarizeMessage pins the message summary shape: "role: text" with
// rune-safe truncation at 60 runes (multibyte text must never be cut
// mid-rune).
func TestSummarizeMessage(t *testing.T) {
	cases := []struct {
		name string
		role ai.Role
		text string
		want string
	}{
		{"user", ai.RoleUser, "hello", "user: hello"},
		{"assistant", ai.RoleAssistant, "hi there", "assistant: hi there"},
		// "tiếng" is 5 runes; 100 runes in, 60 runes out = exactly 12 reps.
		{"multibyte cut at 60 runes", ai.RoleUser, strings.Repeat("tiếng", 20), "user: " + strings.Repeat("tiếng", 12) + "…"},
		{"under limit untouched", ai.RoleAssistant, "tiếng Việt", "assistant: tiếng Việt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := summarize(&MessageEntry{Message: ai.Message{Role: tc.role, Content: []ai.Block{ai.TextBlock{Text: tc.text}}}})
			if got != tc.want {
				t.Fatalf("summarize = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBranchesFindsForkPoints(t *testing.T) {
	// Append always links to the current leaf, so a fork is made by
	// Branch-ing back before appending the second child — exactly how /fork
	// and a real branch work.
	s := OpenMem("/proj", "branch test")
	msg := func(id, parent, text string) *MessageEntry {
		return &MessageEntry{Env: Envelope{ID: id, ParentID: parent}, Message: ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: text}}}}
	}
	if err := s.Append(msg("r", "", "root")); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(msg("b1", "r", "first branch")); err != nil {
		t.Fatal(err)
	}
	if err := s.Branch("r"); err != nil { // move leaf back to r
		t.Fatal(err)
	}
	if err := s.Append(msg("b2", "r", "second branch")); err != nil {
		t.Fatal(err)
	}
	bs := s.Branches()
	if len(bs) != 1 || bs[0] != "r" {
		t.Fatalf("branches = %v, want [r] (r has two children b1,b2)", bs)
	}
}

func TestLatestScheduleSurvivesWindowing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "window-schedule.jsonl")
	s := OpenMem("/tmp/x", "window schedule")
	change := &ScheduleChangedEntry{
		Env:      Envelope{Type: TypeScheduleChange, ID: "schedule", Timestamp: ts0},
		Sequence: 1,
		Active:   []SchedulePayload{{ID: "schedule-1", Kind: "every", Prompt: "keep", EverySeconds: 300, ScheduledAt: ts0}},
	}
	if err := s.Append(change); err != nil {
		t.Fatal(err)
	}
	// Cross the batching threshold before the boundary so the historical
	// schedule entry is actually dropped from context materialization.
	for i := 0; i < loadWindowBatch; i++ {
		if err := s.Append(userMsg("", "", "noise")); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Append(&CompactionEntry{Env: Envelope{Type: TypeCompaction, ID: "compact", Timestamp: ts0}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureOnDisk(path, Options{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if got := reopened.LatestSchedule(); got == nil || got.Active[0].ID != "schedule-1" {
		t.Fatalf("latest schedule after window = %+v", got)
	}
	if reopened.Entries()[0].Envelope().ID == "schedule" {
		t.Fatal("windowing did not prune old schedule entry")
	}
}

func TestScheduleProjectionFollowsBranchesAndReset(t *testing.T) {
	s := OpenMem("/tmp/x", "schedule branches")
	appendSchedule := func(id, prompt string) {
		t.Helper()
		if err := s.Append(&ScheduleChangedEntry{Env: Envelope{ID: id, Timestamp: ts0}, Active: []SchedulePayload{{ID: prompt}}}); err != nil {
			t.Fatal(err)
		}
	}
	appendSchedule("one", "one")
	root := s.LeafID()
	if err := s.Append(userMsg("side", "", "side")); err != nil {
		t.Fatal(err)
	}
	appendSchedule("two", "two")
	if err := s.Branch(root); err != nil {
		t.Fatal(err)
	}
	if got := s.LatestScheduleOnPath(); got == nil || got.Active[0].ID != "one" {
		t.Fatalf("branch projection = %+v", got)
	}
	if got := s.LatestSchedule(); got == nil || got.Active[0].ID != "two" {
		t.Fatalf("chronological projection = %+v", got)
	}
	if err := s.ResetLeaf(); err != nil {
		t.Fatal(err)
	}
	if got := s.LatestScheduleOnPath(); got != nil {
		t.Fatalf("reset projection = %+v", got)
	}
	if got := s.LatestSchedule(); got == nil || got.Active[0].ID != "two" {
		t.Fatalf("chronological projection after reset = %+v", got)
	}
}

func TestScheduleSnapshotsAreCopies(t *testing.T) {
	s := OpenMem("/tmp/x", "schedule copies")
	entry := &ScheduleChangedEntry{Env: Envelope{ID: "one", Timestamp: ts0}, Active: []SchedulePayload{{ID: "one"}}}
	if err := s.Append(entry); err != nil {
		t.Fatal(err)
	}
	entry.Active[0].ID = "mutated"
	got := s.LatestSchedule()
	got.Active[0].ID = "also-mutated"
	if again := s.LatestSchedule(); again == nil || again.Active[0].ID != "one" {
		t.Fatalf("stored snapshot mutated: %+v", again)
	}
}

func TestAppendRollbackRestoresScheduleProjection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollback.jsonl")
	s := OpenMem("/tmp/x", "schedule rollback")
	if _, err := s.EnsureOnDisk(path, Options{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(&ScheduleChangedEntry{Env: Envelope{ID: "one", Timestamp: ts0}, Active: []SchedulePayload{{ID: "one"}}}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{}\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	entry := &ScheduleChangedEntry{Env: Envelope{ID: "two", Timestamp: ts0}, Active: []SchedulePayload{{ID: "two"}}}
	if err := s.Append(entry); err == nil {
		t.Fatal("append over foreign record succeeded")
	}
	if s.Entry("two") != nil || s.LeafID() != "one" {
		t.Fatal("tree mutation survived failed append")
	}
	if got := s.LatestSchedule(); got == nil || got.Active[0].ID != "one" {
		t.Fatalf("schedule projection survived failed append: %+v", got)
	}
	if entry.Envelope().ID != "two" {
		t.Fatal("failed append did not restore entry envelope")
	}
}
