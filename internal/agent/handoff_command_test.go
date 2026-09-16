package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/tool"
)

// docProvider is the handoff side-request partner: it records every request
// and answers with doc (after fail leading transient failures).
type docProvider struct {
	doc     string
	fail    int
	gotReqs []ai.StreamRequest
}

func (d *docProvider) Name() string { return "doc-host" }
func (d *docProvider) API() string  { return "openai-responses" }

func (d *docProvider) Stream(_ context.Context, req ai.StreamRequest) (<-chan ai.Event, error) {
	d.gotReqs = append(d.gotReqs, req)
	if d.fail > 0 {
		d.fail--
		return nil, &ai.HTTPError{API: d.API(), Status: 503, Body: "boom"}
	}
	ch := make(chan ai.Event, 4)
	go func() {
		defer close(ch)
		ch <- ai.Event{Type: ai.EventStart, Provider: d.Name(), API: d.API(), Model: req.Model}
		ch <- ai.Event{Type: ai.EventTextDelta, Delta: d.doc}
		ch <- ai.Donef(ai.StopReasonStop, nil, &ai.Message{
			Role:       ai.RoleAssistant,
			Content:    []ai.Block{ai.TextBlock{Text: d.doc}},
			StopReason: ai.StopReasonStop,
		})
	}()
	return ch, nil
}

// redacting rewrites every "SECRET" to a placeholder, like M13 #55's
// configured redactor, so the alignment test can prove the side request runs
// through the same redaction the live turn does.
type redacting struct{}

func (redacting) Apply(s string) string {
	return strings.ReplaceAll(s, "SECRET", "$$1$$")
}

func (redacting) Expand(s string) string { return s }

// seedHandoffStore builds a session with two user turns (a long first one so
// there is a droppable prefix).
func seedHandoffStore(t *testing.T) *session.Store {
	t.Helper()
	store := session.OpenMem(t.TempDir(), "handoff")
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
	return store
}

// TestHandoffSideRequestMirrorsLiveTurn is the cache-alignment contract: the
// handoff side request must re-send exactly what a live turn sends (same
// system prompt, same history prefix, same redaction) plus one trailing
// instruction, and no tool surface at all — a divergence here silently costs
// every handoff its warm prefix.
func TestHandoffSideRequestMirrorsLiveTurn(t *testing.T) {
	store := seedHandoffStore(t)
	live := &docProvider{doc: "live answer"}
	side := &docProvider{doc: "## Goal\nship it"}

	pm := &PlanMode{active: true, note: "plan note"}
	ag := &Agent{
		Provider: live, Tools: tool.NewRegistry(), Model: "big-model", Store: store,
		Compaction: CompactionConfig{ContextWindow: 200_000},
		PlanMode:   pm,
		Redactor:   redacting{},
		Handoff: HandoffSettings{
			Target: FailoverTarget{Provider: side, Model: "smol-model"},
		},
	}
	// A live turn on the same history, with a system prompt carrying a
	// secret so redaction is observable.
	history, err := sessionMessages(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ag.Run(context.Background(), "sys SECRET", history); err != nil {
		t.Fatal(err)
	}
	if len(live.gotReqs) != 1 {
		t.Fatalf("live turn made %d requests, want 1", len(live.gotReqs))
	}
	liveReq := live.gotReqs[0]

	doc, err := ag.HandoffDoc(context.Background(), "sys SECRET", "focus on the parser")
	if err != nil {
		t.Fatal(err)
	}
	if doc != "## Goal\nship it" {
		t.Fatalf("document = %q", doc)
	}
	if len(side.gotReqs) != 1 {
		t.Fatalf("handoff made %d requests, want 1", len(side.gotReqs))
	}
	req := side.gotReqs[0]

	// 1. The system prompt is byte-identical to the live turn's.
	if req.System != liveReq.System {
		t.Fatalf("handoff system = %q, live system = %q", req.System, liveReq.System)
	}
	if strings.Contains(req.System, "SECRET") {
		t.Fatal("handoff system reached the provider unredacted")
	}
	// 2. The history arrives as the same transformed prefix.
	if len(req.Messages) != len(liveReq.Messages)+1 {
		t.Fatalf("handoff carried %d messages, live turn %d (want +1 trailing)",
			len(req.Messages), len(liveReq.Messages))
	}
	for i, m := range liveReq.Messages {
		if req.Messages[i].Text() != m.Text() || req.Messages[i].Role != m.Role {
			t.Fatalf("message %d differs from the live turn: %q vs %q", i, req.Messages[i].Text(), m.Text())
		}
	}
	// 3. The last message is the trailing document instruction.
	last := req.Messages[len(req.Messages)-1]
	if last.Role != ai.RoleUser || !strings.Contains(last.Text(), "handoff document") {
		t.Fatalf("trailing message = %v %q", last.Role, last.Text())
	}
	if !strings.Contains(last.Text(), "focus on the parser") {
		t.Fatalf("trailing message dropped the /handoff instruction: %q", last.Text())
	}
	// 4. toolChoice none: no tool surface reaches the provider.
	if len(req.Tools) != 0 {
		t.Fatalf("handoff sent %d tool definitions, want none", len(req.Tools))
	}
	// 5. It runs on the handoff target (the @smol role), not the live model.
	if req.Model != "smol-model" {
		t.Fatalf("handoff model = %q, want smol-model", req.Model)
	}
}

// TestHandoffCommitsCompactionEntryAtCut covers the entry contract: a normal
// CompactionEntry on the current session, cut at the keepRecent boundary,
// tagged as a handoff and wrapped so a reader can tell it from a summary.
func TestHandoffCommitsCompactionEntryAtCut(t *testing.T) {
	store := seedHandoffStore(t)
	side := &docProvider{doc: "HANDOFF DOC"}
	var compactions []int64
	ag := &Agent{
		Provider: side, Tools: tool.NewRegistry(), Model: "big", Store: store,
		Compaction: CompactionConfig{ContextWindow: 200_000, KeepRecentTokens: 10},
		Handoff:    HandoffSettings{},
		Hooks:      TurnHooksFunc{OnCompactionF: func(before int64) { compactions = append(compactions, before) }},
	}
	entries := store.Entries()
	secondID := entries[1].Envelope().ID

	if _, err := ag.HandoffDoc(context.Background(), "sys", ""); err != nil {
		t.Fatal(err)
	}

	last := store.Entries()[len(store.Entries())-1]
	ce, ok := last.(*session.CompactionEntry)
	if !ok {
		t.Fatalf("last entry is %T, want a CompactionEntry", last)
	}
	if ce.FirstKeptEntryID == nil || *ce.FirstKeptEntryID != secondID {
		t.Fatalf("firstKeptEntryId = %v, want %q (the keepRecent cut)", ce.FirstKeptEntryID, secondID)
	}
	if ce.Summary.Attribution != HandoffAttribution {
		t.Fatalf("handoff tag = %q, want %q", ce.Summary.Attribution, HandoffAttribution)
	}
	if ce.Summary.Role != ai.RoleUser {
		// A briefing, not an assistant utterance: a leading assistant turn
		// without a reasoning echo is rejected by thinking-mode upstreams.
		t.Fatalf("summary role = %v, want the user briefing", ce.Summary.Role)
	}
	if !strings.Contains(ce.Summary.Text(), handoffOpenTag) || !strings.Contains(ce.Summary.Text(), handoffCloseTag) {
		t.Fatalf("document not wrapped: %q", ce.Summary.Text())
	}
	if ce.TokensBefore <= 0 {
		t.Fatalf("tokensBefore = %d, want the pre-handoff context size", ce.TokensBefore)
	}
	// The compaction is announced on the hook bus exactly like a summary.
	if len(compactions) != 1 || compactions[0] != ce.TokensBefore {
		t.Fatalf("OnCompaction calls = %v, want one with %d", compactions, ce.TokensBefore)
	}

	// The next turn continues from the document + kept window, not the raw
	// history.
	res, err := session.BuildContext(store.Entries(), store.LeafID(), session.SystemPrompt{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 2 {
		t.Fatalf("rebuilt context has %d messages, want the document + kept tail", len(res.Messages))
	}
	if !strings.Contains(res.Messages[0].Text(), "HANDOFF DOC") {
		t.Fatalf("first rebuilt message = %q, want the handoff document", res.Messages[0].Text())
	}
	if !strings.Contains(res.Messages[1].Text(), "second turn") {
		t.Fatalf("kept message = %q, want the second turn", res.Messages[1].Text())
	}
}

// TestHandoffShortSessionHandsOffEverything: an explicit /handoff must never
// fail for being short — with nothing droppable the whole history goes to
// the document (nil firstKeptEntryId).
func TestHandoffShortSessionHandsOffEverything(t *testing.T) {
	store := session.OpenMem(t.TempDir(), "short")
	if err := store.Append(&session.MessageEntry{Message: ai.Message{
		Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "one turn only"}},
	}}); err != nil {
		t.Fatal(err)
	}
	side := &docProvider{doc: "DOC"}
	ag := &Agent{
		Provider: side, Tools: tool.NewRegistry(), Model: "big", Store: store,
		Compaction: CompactionConfig{ContextWindow: 200_000},
	}
	if _, err := ag.HandoffDoc(context.Background(), "sys", ""); err != nil {
		t.Fatal(err)
	}
	ce, ok := store.Entries()[len(store.Entries())-1].(*session.CompactionEntry)
	if !ok {
		t.Fatalf("last entry is %T, want a CompactionEntry", store.Entries()[len(store.Entries())-1])
	}
	if ce.FirstKeptEntryID != nil {
		t.Fatalf("firstKeptEntryId = %q, want nil (nothing was droppable)", *ce.FirstKeptEntryID)
	}
	res, err := session.BuildContext(store.Entries(), store.LeafID(), session.SystemPrompt{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 1 || !strings.Contains(res.Messages[0].Text(), "DOC") {
		t.Fatalf("rebuilt context = %+v, want the document alone", res.Messages)
	}
}

// TestHandoffFailureKeepsHistory: a failed side request must leave the
// session untouched — committing an empty document would erase the context.
func TestHandoffFailureKeepsHistory(t *testing.T) {
	store := seedHandoffStore(t)
	before := len(store.Entries())
	side := &docProvider{doc: "never delivered", fail: HandoffMaxAttempts + 2}
	ag := &Agent{
		Provider: side, Tools: tool.NewRegistry(), Model: "big", Store: store,
		Compaction: CompactionConfig{ContextWindow: 200_000},
		Retry:      RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond},
	}
	if _, err := ag.HandoffDoc(context.Background(), "sys", ""); err == nil {
		t.Fatal("handoff succeeded with a dead provider")
	}
	if got := len(store.Entries()); got != before {
		t.Fatalf("store grew to %d entries after a failed handoff, want %d", got, before)
	}
	// Transient failures are retried (oneshot retry), auth is not.
	if len(side.gotReqs) != HandoffMaxAttempts {
		t.Fatalf("side requests = %d, want %d attempts", len(side.gotReqs), HandoffMaxAttempts)
	}
	side.fail = 1
	side.gotReqs = nil
	if _, err := ag.HandoffDoc(context.Background(), "sys", ""); err != nil {
		t.Fatalf("one transient failure must be retried and succeed: %v", err)
	}
	if len(side.gotReqs) != 2 {
		t.Fatalf("side requests = %d, want the retry (2)", len(side.gotReqs))
	}
}

// todoSinkRec records the snapshots a todo reset persists.
type todoSinkRec struct{ snapshots [][]tool.TodoPhase }

func (r *todoSinkRec) AppendTodo(phases []tool.TodoPhase) error {
	r.snapshots = append(r.snapshots, phases)
	return nil
}

// TestHandoffResetsBranchState: the handed-off context must not carry
// per-branch state the dropped history produced — plan mode, the todo list,
// and the advisor feed cursor all rewind.
func TestHandoffResetsBranchState(t *testing.T) {
	store := seedHandoffStore(t)
	side := &docProvider{doc: "DOC"}
	reg := tool.NewRegistry()
	todoTool := tool.NewTodoTool()
	sink := &todoSinkRec{}
	todoTool.Sink = sink
	reg.Register(todoTool)
	if _, err := todoTool.Execute(context.Background(), []byte(`{"op":"init","list":[{"phase":"P","items":["a","b"]}]}`)); err != nil {
		t.Fatal(err)
	}
	pm := &PlanMode{active: true, pending: "the old plan"}
	var resetKept []ai.Message
	ag := &Agent{
		Provider: side, Tools: reg, Model: "big", Store: store,
		Compaction: CompactionConfig{ContextWindow: 200_000, KeepRecentTokens: 10},
		PlanMode:   pm,
		Handoff: HandoffSettings{
			Reset: func(kept []ai.Message) { resetKept = kept },
		},
	}
	if _, err := ag.HandoffDoc(context.Background(), "sys", ""); err != nil {
		t.Fatal(err)
	}
	if pm.active || pm.pending != "" {
		t.Fatalf("plan mode survived the handoff: active=%v pending=%q", pm.active, pm.pending)
	}
	for _, p := range todoTool.Snapshot() {
		if len(p.Tasks) != 0 {
			t.Fatalf("todo list survived the handoff: %+v", p.Tasks)
		}
	}
	if len(sink.snapshots) == 0 || len(sink.snapshots[len(sink.snapshots)-1][0].Tasks) != 0 {
		t.Fatalf("todo reset was not persisted: %+v", sink.snapshots)
	}
	// The advisor cursor rewinds to the kept window, not the dropped one.
	if len(resetKept) != 1 || !strings.Contains(resetKept[0].Text(), "second turn") {
		t.Fatalf("reset got %d messages, want the kept tail", len(resetKept))
	}
}

// TestHandoffMethodOrderSelectsRung: `handoff` is a compaction method —
// when the order names it and the token budget is due, the boundary commits
// a handoff document instead of a summary.
func TestHandoffMethodOrderSelectsRung(t *testing.T) {
	// The handoff method resolves through HandoffOrder; a typo in the rest
	// still falls back to the shipped default ladder.
	order := HandoffOrder("threshold,handoff")
	if len(order) != 2 || order[0] != "threshold" || order[1] != MethodHandoff {
		t.Fatalf("HandoffOrder(threshold,handoff) = %v", order)
	}
	if order := HandoffOrder(""); len(order) != 3 || slices.Contains(order, MethodHandoff) {
		t.Fatalf("HandoffOrder(\"\") = %v, want the default ladder without handoff", order)
	}
	if order := HandoffOrder("handoff"); len(order) != 1 || order[0] != MethodHandoff {
		t.Fatalf("HandoffOrder(handoff) = %v, want the method alone", order)
	}

	store := seedHandoffStore(t)
	side := &docProvider{doc: "RUNG DOC"}
	pm := &PlanMode{active: true}
	ag := &Agent{
		Provider: side, Tools: tool.NewRegistry(), Model: "big", Store: store,
		Compaction: CompactionConfig{ContextWindow: 100, ReserveTokens: 1, KeepRecentTokens: 10, Methods: []string{MethodHandoff}},
		PlanMode:   pm,
	}
	history, err := sessionMessages(store)
	if err != nil {
		t.Fatal(err)
	}
	if !ag.HandoffDue(history) {
		t.Fatal("handoff rung not due above the threshold")
	}
	rebuilt, ok := ag.HandoffRung(context.Background(), "sys", history)
	if !ok {
		t.Fatal("handoff rung did not fire")
	}
	if len(rebuilt) == 0 || !strings.Contains(rebuilt[0].Text(), "RUNG DOC") {
		t.Fatalf("rebuilt history = %+v, want the document first", rebuilt)
	}
	last := store.Entries()[len(store.Entries())-1]
	ce, isCompaction := last.(*session.CompactionEntry)
	if !isCompaction || ce.Summary.Attribution != HandoffAttribution {
		t.Fatalf("boundary entry = %T, want a tagged CompactionEntry", last)
	}
	// A run in flight keeps its plan-mode sub-state: lifting the read-only
	// policy mid-run would contradict the reminder the run still carries.
	if !pm.active {
		t.Fatal("the automatic rung reset plan mode mid-run")
	}
	// The rebuilt context is under the threshold, so the rung is one-shot.
	if ag.HandoffDue(rebuilt) {
		t.Fatal("handoff rung still due after the handoff")
	}

	// Not selected → the summary path keeps the boundary.
	summaryOnly := &Agent{
		Provider: side, Tools: tool.NewRegistry(), Model: "big", Store: seedHandoffStore(t),
		Compaction: CompactionConfig{ContextWindow: 100, ReserveTokens: 1, KeepRecentTokens: 10, Methods: []string{methodThreshold}},
	}
	sh, err := sessionMessages(summaryOnly.Store)
	if err != nil {
		t.Fatal(err)
	}
	if summaryOnly.HandoffDue(sh) {
		t.Fatal("handoff rung fired without being selected in methodOrder")
	}
	if _, fired := summaryOnly.HandoffRung(context.Background(), "sys", sh); fired {
		t.Fatal("HandoffRung committed a document for an unselected method")
	}
}

// TestHandoffSavesArtifact: settings handoff.saveToDisk mirrors the document
// to <dataDir>/handoffs/<shortid>.md.
func TestHandoffSavesArtifact(t *testing.T) {
	store := seedHandoffStore(t)
	side := &docProvider{doc: "# Handoff\n\n## Goal\nship"}
	dir := filepath.Join(t.TempDir(), "handoffs")
	ag := &Agent{
		Provider: side, Tools: tool.NewRegistry(), Model: "big", Store: store,
		Compaction: CompactionConfig{ContextWindow: 200_000},
		Handoff:    HandoffSettings{SaveDir: dir},
	}
	doc, err := ag.HandoffDoc(context.Background(), "sys", "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, store.ID()[:8]+".md")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("artifact not written: %v", err)
	}
	if string(b) != doc+"\n" {
		t.Fatalf("artifact = %q, want the document", b)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("artifact mode = %v, want 0600", fi.Mode().Perm())
	}
	// No SaveDir → nothing on disk beyond the session.
	plain := &Agent{
		Provider: side, Tools: tool.NewRegistry(), Model: "big", Store: seedHandoffStore(t),
		Compaction: CompactionConfig{ContextWindow: 200_000},
	}
	if _, err := plain.HandoffDoc(context.Background(), "sys", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, plain.Store.ID()[:8]+".md")); !os.IsNotExist(err) {
		t.Fatalf("artifact written without SaveDir: %v", err)
	}
}

// sessionMessages rebuilds the store's live context (the same call the
// handoff and Run make).
func sessionMessages(store *session.Store) ([]ai.Message, error) {
	res, err := session.BuildContext(store.Entries(), store.LeafID(), session.SystemPrompt{})
	if err != nil {
		return nil, err
	}
	return res.Messages, nil
}
