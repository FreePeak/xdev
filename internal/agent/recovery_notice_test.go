package agent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/tool"
)

// A recovery round that nobody can read is not a recovery round. Every
// announcement used to be an AddSystemBlock — display-only — so a turn that
// spent 53 minutes inside one of the bounded loops left a session file whose
// last record was a toolResult from before the loop started. On the
// reporting machine 0 of 132 session files held a persisted retry notice,
// which is why a7e17741 (2026-09-28) took an afternoon to diagnose from the
// file alone: there was nothing in it to read.
//
// The three notice* funcs are the screen voice and must keep firing every
// round. The three persist* calls are the file voice: one CustomEntry per
// kind, throttled to the first round and then every maxNoticeStride-th.

// recovery_notice_every_round persists at the first round and then on the
// stride. A loop bounded at 12 rounds writes 4 records, not 12, and not 1
// either: a single record cannot say whether the loop was still climbing.
func TestRecoveryNoticeIsPersistedThrottled(t *testing.T) {
	down := fakeScript{err: errors.New("dial tcp 127.0.0.1:8080: connect: connection refused")}
	calls := make([]fakeScript, maxSilentRecoveryRounds*(oneShotRetry().MaxRetries+1)+4)
	for i := range calls {
		calls[i] = down
	}
	p := &fakeProvider{calls: calls}
	a, s, _ := storeAgent(t, p, CompactionConfig{})
	a.Retry = RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Infinite: true}
	if _, err := a.Run(context.Background(), "sys", submitHistory(t, s, "hi")); err == nil {
		t.Fatal("a host that never answers must end the turn with an error")
	}
	notices := customNotices(t, s, "recovery_all_targets_down")
	if len(notices) == 0 {
		t.Fatal("an unbounded wait must be written to the session file, not only the screen")
	}
	var got []int
	for _, n := range notices {
		round, ok := n["round"].(int)
		if !ok {
			t.Fatalf("notice round is %T, want int (the in-memory store keeps Go values): %v", n["round"], n)
		}
		got = append(got, round)
	}
	// Every stride from 1, and no more: the loop reached 12, so the
	// throttled notices are 1, 3, 6, 9, 12 — not 12 records, not one.
	wantRounds := []int{1, 3, 6, 9, 12}
	if len(got) != len(wantRounds) {
		t.Fatalf("persisted notice rounds = %v, want %v: the notice is throttled, not dropped", got, wantRounds)
	}
	for i, r := range got {
		if r != wantRounds[i] {
			t.Fatalf("notice %d round = %d, want %d", i, r, wantRounds[i])
		}
		if _, ok := notices[i]["error"]; !ok {
			t.Fatalf("notice %d has no error: %v", i, notices[i])
		}
		if _, ok := notices[i]["delaySeconds"]; !ok {
			t.Fatalf("notice %d has no delay: %v", i, notices[i])
		}
	}
}

// The record must say how much budget is left, so a reader of the file can
// tell a three-round loop from a twelve-round one without knowing the
// constants.
func TestRecoveryNoticeCarriesTheRemainingBudget(t *testing.T) {
	down := fakeScript{err: errors.New("connection refused")}
	calls := make([]fakeScript, 8)
	for i := range calls {
		calls[i] = down
	}
	p := &fakeProvider{calls: calls}
	a, s, _ := storeAgent(t, p, CompactionConfig{})
	a.Retry = RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Infinite: true}
	if _, err := a.Run(context.Background(), "sys", submitHistory(t, s, "hi")); err == nil {
		t.Fatal("a host that never answers must end the turn with an error")
	}
	notices := customNotices(t, s, "recovery_all_targets_down")
	if len(notices) == 0 {
		t.Fatal("no notice persisted")
	}
	first := notices[0]
	left, ok := first["remainingRounds"]
	if !ok {
		t.Fatalf("first notice has no remainingRounds: %v", first)
	}
	if want := maxSilentRecoveryRounds - 1; left != want {
		t.Fatalf("first notice remainingRounds = %v (%T), want %d", left, left, want)
	}
}

// The three recovery kinds each name their own ceiling, so a reader can tell
// an empty-turn loop from a host outage. The kind is the record's own
// customType, which is what a jq one-liner reads.
func TestRecoveryNoticeKindsCarryTheirOwnCeiling(t *testing.T) {
	for _, tc := range []struct {
		kind  string
		bound int
	}{
		{"recovery_all_targets_down", maxSilentRecoveryRounds},
		{"recovery_empty_turn", maxEmptyTurnRecoveries},
		{"recovery_continuation", maxPostContentContinuations},
	} {
		if got := boundRounds(tc.kind); got != tc.bound {
			t.Fatalf("boundRounds(%q) = %d, want %d", tc.kind, got, tc.bound)
		}
	}
}

// The empty-turn recovery is the loop a7e17741 was almost certainly in, and
// it is the one whose announcement the file most needs: a model answering
// nothing writes no other record at all.
func TestEmptyTurnRecoveryIsPersisted(t *testing.T) {
	blank := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.ThinkingBlock{Thinking: "(context elided)"}}}
	blankScript := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, blank)}}
	calls := make([]fakeScript, maxEmptyTurnNudges+maxEmptyTurnRecoveries+4)
	for i := range calls {
		calls[i] = blankScript
	}
	p := &fakeProvider{calls: calls}
	a, s, _ := storeAgent(t, p, CompactionConfig{})
	a.Retry = RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, RetryAllErrors: true}
	if _, err := a.Run(context.Background(), "sys", submitHistory(t, s, "hi")); !errors.Is(err, ErrEmptyTurn) {
		t.Fatalf("err = %v, want ErrEmptyTurn", err)
	}
	notices := customNotices(t, s, "recovery_empty_turn")
	if len(notices) == 0 {
		t.Fatal("an empty-turn recovery loop must say so in the session file")
	}
	if got := notices[0]["remainingRounds"]; got != maxEmptyTurnRecoveries-1 {
		t.Fatalf("remainingRounds = %v (%T), want %d", got, got, maxEmptyTurnRecoveries-1)
	}
}

// The notice must not become a turn. session.buildContext switches only on
// MessageEntry, CompactionEntry and BranchSummaryEntry, so a CustomEntry is
// invisible to the model — and that is load-bearing, not incidental: a
// persisted retry line that reached the provider would be a user message
// the human never typed.
func TestRecoveryNoticeNeverReachesTheModel(t *testing.T) {
	blank := &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
		Content: []ai.Block{ai.ThinkingBlock{Thinking: "(context elided)"}}}
	blankScript := fakeScript{events: []ai.Event{ai.Donef(ai.StopReasonStop, nil, blank)}}
	answered := fakeScript{events: []ai.Event{doneEvent("done")}}
	calls := make([]fakeScript, 0, maxEmptyTurnNudges+maxEmptyTurnRecoveries+4)
	for range maxEmptyTurnNudges + 2 {
		calls = append(calls, blankScript)
	}
	calls = append(calls, answered)
	p := &fakeProvider{calls: calls}
	a, s, _ := storeAgent(t, p, CompactionConfig{})
	a.Retry = RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, RetryAllErrors: true}
	if _, err := a.Run(context.Background(), "sys", submitHistory(t, s, "hi")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(customNotices(t, s, "recovery_empty_turn")) == 0 {
		t.Fatal("the notice was not written at all — the previous test's fixture did not reach it")
	}
	res, err := session.BuildContext(s.Entries(), s.LeafID(), session.SystemPrompt{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range res.Messages {
		if strings.Contains(m.Text(), "recovery_") || strings.Contains(m.Text(), "retrying in") {
			t.Fatalf("the persisted notice reached the model as a turn: %+v", m)
		}
	}
}

// customNotices returns the Data maps of every CustomEntry of one kind, in
// file order. The values come back as float64 because they round-trip
// through JSON.
func customNotices(t *testing.T, s *session.Store, kind string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, e := range s.Entries() {
		c, ok := e.(*session.CustomEntry)
		if !ok || c.CustomType != kind {
			continue
		}
		out = append(out, c.Data)
	}
	return out
}

// The record has to survive a reopen. Every assertion above reads the
// in-memory store, which cannot catch a wire-format miss — and the whole
// point of writing the notice is that a human (or a later session) can read
// the FILE, long after the process is gone.
func TestRecoveryNoticeSurvivesReopen(t *testing.T) {
	down := fakeScript{err: errors.New("dial tcp 127.0.0.1:8080: connect: connection refused")}
	calls := make([]fakeScript, 8)
	for i := range calls {
		calls[i] = down
	}
	st := session.OpenMem("/proj", "notice")
	path, err := st.EnsureOnDisk(filepath.Join(t.TempDir(), "s.jsonl"), session.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Append(&session.MessageEntry{Env: session.Envelope{ID: "u1"}, Message: ai.Message{
		Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}},
	}}); err != nil {
		t.Fatal(err)
	}
	p := &fakeProvider{calls: calls}
	hooks := TurnHooksFunc{
		OnMessageEndF:    func(m *ai.Message) { _ = st.Append(&session.MessageEntry{Message: *m}) },
		OnToolResultMsgF: func(m *ai.Message) { _ = st.Append(&session.MessageEntry{Message: *m}) },
	}
	a := &Agent{Provider: p, Tools: tool.NewRegistry(), Hooks: hooks, Store: st}
	a.Retry = RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Infinite: true}
	if _, err := a.Run(context.Background(), "sys", []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}},
	}); err == nil {
		t.Fatal("a host that never answers must end the turn with an error")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := session.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	found := 0
	for _, e := range reopened.Entries() {
		c, ok := e.(*session.CustomEntry)
		if !ok || c.CustomType != "recovery_all_targets_down" {
			continue
		}
		found++
		// The values come back through JSON, so the on-disk type is float64 —
		// a notice whose numbers do not round-trip would read as null to jq.
		if _, ok := c.Data["round"].(float64); !ok {
			t.Fatalf("on-disk round is %T, want float64 from the JSON round-trip: %v", c.Data["round"], c.Data)
		}
	}
	if found == 0 {
		t.Fatal("the recovery notice did not survive a reopen — the record is unreadable from the file")
	}
}

// The live reproduction on 2026-09-29 caught the case every other test
// missed: a run whose only writes ARE recovery notices produced NO session
// file at all. internal/session's store materialises on disk at the first
// ASSISTANT message (appendLocked), and a run that never gets one — which is
// exactly the run this notice describes — appended into memory and lost
// everything. So the notice has to materialise the session itself, the same
// move schedule.go:317 makes before its first durable fact.
//
// This test uses OpenMem + EnableAutoPersist, which is what print mode does
// (cmd/xdev/print.go), so it reproduces the live shape rather than a
// pre-materialised store.
func TestRecoveryNoticeMaterialisesAnUnstartedSession(t *testing.T) {
	down := fakeScript{err: errors.New("connection refused")}
	calls := make([]fakeScript, 8)
	for i := range calls {
		calls[i] = down
	}
	st := session.OpenMem("/proj", "unstarted")
	st.EnableAutoPersist(filepath.Join(t.TempDir(), "auto.jsonl"), session.Options{})
	if st.Path() != "" {
		t.Fatal("precondition: the store must not be on disk yet")
	}
	p := &fakeProvider{calls: calls}
	hooks := TurnHooksFunc{
		OnMessageEndF:    func(m *ai.Message) { _ = st.Append(&session.MessageEntry{Message: *m}) },
		OnToolResultMsgF: func(m *ai.Message) { _ = st.Append(&session.MessageEntry{Message: *m}) },
	}
	a := &Agent{Provider: p, Tools: tool.NewRegistry(), Hooks: hooks, Store: st}
	a.Retry = RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Infinite: true}
	if _, err := a.Run(context.Background(), "sys", []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}},
	}); err == nil {
		t.Fatal("a host that never answers must end the turn with an error")
	}
	if st.Path() == "" {
		t.Fatal("a run that only wrote recovery notices left NO session file: the record the whole feature exists for was in memory only")
	}
	if n := len(customNotices(t, st, "recovery_all_targets_down")); n == 0 {
		t.Fatal("the notice is not readable from the materialised store")
	}
}
