package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newMnemopi builds an ensured backend in a temp dir, scoped to a real
// project directory (projectName walks up looking for .git, so the directory
// must exist for the bank name to be the basename).
func newMnemopi(t *testing.T, project string, opts ...func(*Mnemopi)) *Mnemopi {
	t.Helper()
	dir := t.TempDir()
	cwd := filepath.Join(dir, project)
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", cwd, err)
	}
	m := &Mnemopi{Dir: filepath.Join(dir, "memory"), CWD: cwd, Scope: ScopeProject}
	for _, o := range opts {
		o(m)
	}
	if err := m.Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func mustRetain(t *testing.T, m *Mnemopi, f Fact) RetainResult {
	t.Helper()
	res, err := m.Retain(context.Background(), f)
	if err != nil {
		t.Fatalf("Retain(%q): %v", f.Text, err)
	}
	return res
}

func recallTexts(t *testing.T, m *Mnemopi, query string, limit int) []string {
	t.Helper()
	hits, err := m.Recall(context.Background(), query, limit)
	if err != nil {
		t.Fatalf("Recall(%q): %v", query, err)
	}
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Fact.Text)
	}
	return out
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// --- schema + bank scoping ---

func TestMnemopiSchemaAndBankScoping(t *testing.T) {
	m := newMnemopi(t, "proj-a")
	// Ensure is idempotent (reopening an existing store must not fail).
	if err := m.Ensure(); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+m.dbPath())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	for _, table := range []string{"banks", "facts", "fact_tags", "edges", "queue", "facts_fts"} {
		var name string
		if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE name = ?`, table).Scan(&name); err != nil {
			t.Errorf("schema: table %s missing: %v", table, err)
		}
	}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("user_version = %d, want %d", version, schemaVersion)
	}

	// Project scope: a fact retained under proj-a is invisible to proj-b.
	mustRetain(t, m, Fact{Text: "proj-a fact: the deploy script lives in scripts/ci.sh"})
	other := &Mnemopi{Dir: m.Dir, CWD: filepath.Join(t.TempDir(), "proj-b"), Scope: ScopeProject}
	if err := other.Ensure(); err != nil {
		t.Fatalf("Ensure proj-b: %v", err)
	}
	t.Cleanup(func() { _ = other.Close() })
	if got := recallTexts(t, other, "deploy script", 0); len(got) != 0 {
		t.Errorf("proj-b recalled proj-a's memory: %v", got)
	}
	if got := recallTexts(t, m, "deploy script", 0); len(got) != 1 {
		t.Errorf("proj-a recall = %v, want its own fact", got)
	}

	// The global bank is readable from every project bank.
	global := &Mnemopi{Dir: m.Dir, CWD: filepath.Join(t.TempDir(), "proj-c"), Scope: ScopeGlobal}
	if err := global.Ensure(); err != nil {
		t.Fatalf("Ensure global: %v", err)
	}
	t.Cleanup(func() { _ = global.Close() })
	mustRetain(t, global, Fact{Text: "global fact: never commit the local onegw key"})
	if got := recallTexts(t, m, "onegw key", 0); !contains(got, "global fact: never commit the local onegw key") {
		t.Errorf("project bank did not read the global bank: %v", got)
	}

	// project-tagged: the tag is part of the bank identity.
	tagged := func(tag string) *Mnemopi {
		return &Mnemopi{Dir: m.Dir, CWD: m.CWD, Scope: ScopeProjectTagged, Tag: tag}
	}
	red := tagged("red")
	if err := red.Ensure(); err != nil {
		t.Fatalf("Ensure tagged: %v", err)
	}
	t.Cleanup(func() { _ = red.Close() })
	mustRetain(t, red, Fact{Text: "tagged red fact: the red cluster runs on port 8081"})
	const redFact = "tagged red fact: the red cluster runs on port 8081"
	blue := tagged("blue")
	if got := recallTexts(t, blue, "red cluster", 0); contains(got, redFact) {
		t.Errorf("blue tag bank recalled the red tag bank: %v", got)
	}
	if got := recallTexts(t, red, "red cluster", 0); !contains(got, redFact) {
		t.Errorf("red tag bank recall = %v, want its own fact", got)
	}
}

// --- retain / recall round trip ---

func TestMnemopiRetainRecallRoundTrip(t *testing.T) {
	m := newMnemopi(t, "proj")
	ctx := context.Background()
	res := mustRetain(t, m, Fact{
		Text:   "the gateway caps requests at 4 concurrent streams",
		Kind:   KindLesson,
		Tags:   []string{"Gateway", "limits", "gateway"},
		Source: "unit test",
	})
	if res.ID == 0 || res.Deduped {
		t.Fatalf("first retain = %+v, want a fresh id", res)
	}
	if res.Linked != 0 {
		t.Errorf("first retain linked %d facts, want 0 (nothing to link to)", res.Linked)
	}

	hits, err := m.Recall(ctx, "concurrent streams", 0)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("recall returned %d hits, want 1", len(hits))
	}
	got := hits[0].Fact
	if got.ID != res.ID || got.Kind != KindLesson || !strings.Contains(got.Text, "4 concurrent streams") {
		t.Errorf("recalled fact = %+v", got)
	}
	if strings.Join(got.Tags, ",") != "gateway,limits" {
		t.Errorf("tags = %v, want normalized+deduped [gateway limits]", got.Tags)
	}
	if !contains(hits[0].Modes, "keyword") {
		t.Errorf("modes = %v, want a keyword hit", hits[0].Modes)
	}
	if got.Bank != "project:proj" {
		t.Errorf("bank = %q, want project:proj", got.Bank)
	}

	// Retaining the same text again refreshes the same memory.
	again := mustRetain(t, m, Fact{Text: "the gateway caps requests at 4 concurrent streams", Tags: []string{"retry"}})
	if !again.Deduped || again.ID != res.ID {
		t.Errorf("duplicate retain = %+v, want dedupe onto #%d", again, res.ID)
	}
	if facts := listFactIDs(t, m); len(facts) != 1 {
		t.Errorf("store holds %d facts, want 1 after a duplicate retain", len(facts))
	}
	f, err := m.FactByID(ctx, res.ID)
	if err != nil {
		t.Fatalf("FactByID: %v", err)
	}
	if len(f.Tags) != 3 {
		t.Errorf("tags after merge = %v, want the union (gateway, limits, retry)", f.Tags)
	}
}

func listFactIDs(t *testing.T, m *Mnemopi) []int64 {
	t.Helper()
	db, err := m.handle()
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	ids, err := scanIDs(db.Query(`SELECT id FROM facts`))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	return ids
}

// --- the RRF merge ---

func TestFuseReciprocalRankFusion(t *testing.T) {
	// id 3 is found by one mode only; id 2 is agreed on by two.
	lists := []ranked{
		{mode: "keyword", ids: []int64{1, 2}},
		{mode: "tag", ids: []int64{2, 4}},
		{mode: "graph", ids: []int64{3}},
	}
	score, modes := fuse(lists)
	if _, ok := score[3]; !ok {
		t.Fatalf("a hit only one mode found was dropped: %v", score)
	}
	if strings.Join(modes[3], "+") != "graph" {
		t.Errorf("modes[3] = %v, want [graph]", modes[3])
	}
	if strings.Join(modes[2], "+") != "keyword+tag" {
		t.Errorf("modes[2] = %v, want [keyword tag]", modes[2])
	}
	// A rank-2 keyword hit and a rank-1 graph hit score the same; agreeing
	// modes must beat both.
	if score[2] <= score[1] || score[2] <= score[3] {
		t.Errorf("score[2]=%v must beat score[1]=%v and score[3]=%v", score[2], score[1], score[3])
	}
	if score[1] != score[3] {
		t.Errorf("equal-rank single-mode hits scored differently: %v vs %v", score[1], score[3])
	}
}

// TestMnemopiRecallModeAttribution pins the polyphonic property on a real
// store: a fact reachable only through the link graph still surfaces, and the
// hit names the mode that found it.
func TestMnemopiRecallModeAttribution(t *testing.T) {
	m := newMnemopi(t, "proj")
	ctx := context.Background()
	// The seed carries the query token; its linked neighbour does not.
	seed := mustRetain(t, m, Fact{Text: "the gateway pool keeps 4 streams warm", Tags: []string{"pool"}})
	neighbour := mustRetain(t, m, Fact{Text: "the warm spare box is idle by policy", Tags: []string{"pool"}})
	if neighbour.Linked == 0 {
		t.Fatal("retaining a fact sharing a tag did not link it (no proactive edge)")
	}
	tagged := mustRetain(t, m, Fact{Text: "the rollout checklist lives in docs/rollout.md", Tags: []string{"release"}})

	hitFor := func(id int64, hits []Hit) (Hit, bool) {
		for _, h := range hits {
			if h.Fact.ID == id {
				return h, true
			}
		}
		return Hit{}, false
	}

	hits, err := m.Recall(ctx, "gateway", 0)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	seedHit, ok := hitFor(seed.ID, hits)
	if !ok || !contains(seedHit.Modes, "keyword") {
		t.Errorf("seed hit = %+v, want a keyword mode", seedHit)
	}
	nb, ok := hitFor(neighbour.ID, hits)
	if !ok {
		t.Fatalf("the graph-linked neighbour did not surface: %v", hits)
	}
	if !contains(nb.Modes, "graph") {
		t.Errorf("neighbour modes = %v, want the link graph among them", nb.Modes)
	}
	if seedHit.Score <= 0 || nb.Score <= 0 {
		t.Errorf("scores must be positive: seed=%v neighbour=%v", seedHit.Score, nb.Score)
	}

	// A tag query reaches the tagged fact without its text matching.
	tagHits, err := m.Recall(ctx, "release", 0)
	if err != nil {
		t.Fatalf("Recall(tag): %v", err)
	}
	th, ok := hitFor(tagged.ID, tagHits)
	if !ok {
		t.Fatalf("tag-only hit missing: %v", tagHits)
	}
	if !contains(th.Modes, "tag") {
		t.Errorf("tagged modes = %v, want a tag hit", th.Modes)
	}

	// An intent word ranks lessons through the kind mode.
	lesson := mustRetain(t, m, Fact{Text: "never run the migration twice", Kind: KindLesson})
	kindHits, err := m.Recall(ctx, "lessons about migrations", 0)
	if err != nil {
		t.Fatalf("Recall(kind): %v", err)
	}
	lh, ok := hitFor(lesson.ID, kindHits)
	if !ok || !contains(lh.Modes, "kind") {
		t.Errorf("lesson hit = %+v (%v), want a kind-mode hit", lh, kindHits)
	}
}

// --- bounds ---

func TestMnemopiRecallLimitAndInjectionCap(t *testing.T) {
	m := newMnemopi(t, "proj", func(m *Mnemopi) {
		m.RecallLimit = 3
		m.InjectionTokenLimit = 40 // ~160 chars of injection budget
	})
	for i := 0; i < 12; i++ {
		mustRetain(t, m, Fact{Text: strings.Repeat("quantum ", 4) + "memory number " + strconv.Itoa(i), Tags: []string{"bulk"}})
	}
	hits, err := m.Recall(context.Background(), "quantum", 0)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(hits) != 3 {
		t.Errorf("recall returned %d hits, want the configured recallLimit 3", len(hits))
	}
	if hits, err = m.Recall(context.Background(), "quantum", 99); err != nil {
		t.Fatalf("Recall(99): %v", err)
	}
	if len(hits) > MaxRecallLimit {
		t.Errorf("recall returned %d hits, want the hard cap %d", len(hits), MaxRecallLimit)
	}

	text, err := m.RecallText(context.Background(), "quantum")
	if err != nil {
		t.Fatalf("RecallText: %v", err)
	}
	budget := m.InjectionTokenLimit * charsPerToken
	if len(text) > budget+120 {
		t.Errorf("injected block is %d chars, want it inside the %d-char budget (+marker)", len(text), budget)
	}
	if !strings.Contains(text, "not shown") {
		t.Errorf("a clipped injection must say so:\n%s", text)
	}
}

func TestMnemopiMemoryEditBounds(t *testing.T) {
	m := newMnemopi(t, "proj")
	ctx := context.Background()
	res := mustRetain(t, m, Fact{Text: "port 8080", Tags: []string{"a"}})

	if _, err := m.MemoryEdit(ctx, FactEdit{ID: res.ID + 999, Text: ptr("x")}); err == nil {
		t.Error("editing an unknown id must fail")
	}
	if _, err := m.MemoryEdit(ctx, FactEdit{ID: res.ID}); err == nil {
		t.Error("an edit with no change must fail")
	}
	if _, err := m.MemoryEdit(ctx, FactEdit{ID: res.ID, Kind: ptr("nonsense")}); err == nil {
		t.Error("an unknown kind must fail")
	}
	if _, err := m.MemoryEdit(ctx, FactEdit{ID: res.ID, Text: ptr("   ")}); err == nil {
		t.Error("an empty replacement must fail (delete is explicit)")
	}

	long := strings.Repeat("y", maxFactChars*2)
	edited, err := m.MemoryEdit(ctx, FactEdit{ID: res.ID, Text: &long})
	if err != nil {
		t.Fatalf("MemoryEdit(long): %v", err)
	}
	if len(edited.Text) > maxFactChars {
		t.Errorf("stored text is %d chars, want it bounded at %d", len(edited.Text), maxFactChars)
	}
	if !strings.Contains(edited.Text, "truncated") {
		t.Errorf("clipped text must carry the marker: %q", edited.Text[len(edited.Text)-32:])
	}

	many := []string{"t1", "t2", "t3", "t4", "t5", "t6", "t7", "t8", "t9", "t10"}
	edited, err = m.MemoryEdit(ctx, FactEdit{ID: res.ID, Tags: &many})
	if err != nil {
		t.Fatalf("MemoryEdit(tags): %v", err)
	}
	if len(edited.Tags) != maxTagsPerFact {
		t.Errorf("tags = %v, want them bounded at %d", edited.Tags, maxTagsPerFact)
	}

	// A fact in another project's bank is not editable from here.
	other := &Mnemopi{Dir: m.Dir, CWD: filepath.Join(t.TempDir(), "proj-other"), Scope: ScopeProject}
	if err := other.Ensure(); err != nil {
		t.Fatalf("Ensure other: %v", err)
	}
	t.Cleanup(func() { _ = other.Close() })
	foreign := mustRetain(t, other, Fact{Text: "another project's secret"})
	if _, err := m.MemoryEdit(ctx, FactEdit{ID: foreign.ID, Delete: true}); err == nil {
		t.Fatal("one project must not edit (or delete) another project's memory")
	}

	if _, err := m.MemoryEdit(ctx, FactEdit{ID: res.ID, Delete: true}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := m.FactByID(ctx, res.ID); err == nil {
		t.Error("a deleted fact must be gone")
	}
}

func ptr[T any](v T) *T { return &v }

// --- queue / sync / reflect ---

func TestMnemopiQueueSyncAndReflect(t *testing.T) {
	var prompts []string
	m := newMnemopi(t, "proj", func(m *Mnemopi) {
		m.Complete = func(_ context.Context, prompt string) (string, error) {
			prompts = append(prompts, prompt)
			return "consolidated: the project is ready for release", nil
		}
	})
	ctx := context.Background()
	if _, err := m.Enqueue("queued fact one: builds run with CGO_ENABLED=0", "test"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := m.Enqueue("queued fact two: the release tag is v0.3", "test"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := m.EnqueueSync(); err != nil {
		t.Fatalf("EnqueueSync: %v", err)
	}
	if stats := m.QueueStats(); !strings.Contains(stats, "3 pending") {
		t.Errorf("queue stats = %q, want 3 pending", stats)
	}

	res, err := m.Sync(ctx, 5*time.Second)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.Applied != 2 || res.Remaining != 0 {
		t.Errorf("Sync = %+v, want 2 applied and an empty queue", res)
	}
	if !res.Consolidated || res.Reflections != 1 {
		t.Errorf("Sync = %+v, want one reflection", res)
	}
	if len(prompts) != 1 || !strings.Contains(prompts[0], "queued fact one") {
		t.Errorf("the reflection prompt must carry the retained facts, got %q", prompts)
	}
	if got := recallTexts(t, m, "ready for release", 0); !contains(got, "consolidated: the project is ready for release") {
		t.Errorf("the reflection is not recallable: %v", got)
	}

	// The budget is a real bound: an exhausted budget leaves items queued.
	if _, err := m.Enqueue("still queued", "test"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	res, err = m.Sync(ctx, time.Nanosecond)
	if err != nil {
		t.Fatalf("Sync(ns): %v", err)
	}
	if res.Applied != 0 || res.Remaining != 1 {
		t.Errorf("Sync with an exhausted budget = %+v, want the item left queued", res)
	}

	// Turn accounting: the trigger fires exactly every N turns, and a
	// negative setting disables it outright.
	m.RetainEveryNTurns = -1
	if due, err := m.NoteTurn(); err != nil || due {
		t.Errorf("a negative retainEveryNTurns must disable the trigger (due=%v err=%v)", due, err)
	}
	m.RetainEveryNTurns = 3
	for turn, wantDue := range []bool{false, false, true, false, false, true} {
		due, err := m.NoteTurn()
		if err != nil {
			t.Fatalf("NoteTurn: %v", err)
		}
		if due != wantDue {
			t.Errorf("turn %d: due = %v, want %v", turn+1, due, wantDue)
		}
	}
	if _, err := m.Enqueue("drain me", "test"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Two retains are queued here: the one the exhausted-budget sync left
	// behind, plus the one just added.
	if drained := m.Drain(ctx); drained.Applied != 2 || drained.Remaining != 0 {
		t.Errorf("Drain = %+v, want both queued retains applied", drained)
	}
}

func TestMnemopiReflectWithoutSeam(t *testing.T) {
	m := newMnemopi(t, "proj", func(m *Mnemopi) { m.LLMMode = "none" })
	ctx := context.Background()
	mustRetain(t, m, Fact{Text: "a fact to reflect on"})
	if _, err := m.Reflect(ctx, ""); err == nil {
		t.Fatal("reflect without a synthesis seam must fail loudly, not silently")
	}
	if _, err := m.Enqueue("queued while synthesis is off", "test"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	res, err := m.Sync(ctx, 5*time.Second)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.Applied != 1 || res.Consolidated {
		t.Errorf("Sync = %+v, want the retain applied and no consolidation", res)
	}
	if stats := m.QueueStats(); !strings.Contains(stats, "synthesis: off") {
		t.Errorf("queue stats must name the missing synthesis seam: %q", stats)
	}
}

// --- parity through the memory:// seam ---

// TestMemoryStoreSeamParity drives every Store method over both local
// backends: the memory:// URLs, the injection block and the lifecycle verbs
// must mean the same thing whichever backend is configured.
func TestMemoryStoreSeamParity(t *testing.T) {
	dir := t.TempDir()
	local := &Backend{Dir: filepath.Join(dir, "markdown")}
	if err := local.Ensure(); err != nil {
		t.Fatalf("local Ensure: %v", err)
	}
	mnemopi := newMnemopi(t, "proj")

	for _, tc := range []struct {
		name  string
		store PipelineStore
	}{
		{"local", local},
		{"mnemopi", mnemopi},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.store
			if s.Off() {
				t.Fatal("a configured backend must not report off")
			}
			if err := s.SaveLesson("the parity lesson: both backends answer the same seam", "parity test"); err != nil {
				t.Fatalf("SaveLesson: %v", err)
			}
			if err := s.WriteSummary("parity summary text"); err != nil {
				t.Fatalf("WriteSummary: %v", err)
			}
			root, err := s.Read("memory://root")
			if err != nil {
				t.Fatalf("Read(root): %v", err)
			}
			for _, want := range []string{"parity summary text", "parity lesson"} {
				if !strings.Contains(root, want) {
					t.Errorf("memory://root = %q, want it to contain %q", root, want)
				}
			}
			block := s.GuidanceBlock()
			if !strings.HasPrefix(block, "# Memory Guidance") || !strings.Contains(block, "parity summary text") {
				t.Errorf("GuidanceBlock = %q", block)
			}
			learned, err := s.Read("memory://root/learned.md")
			if err != nil {
				t.Fatalf("Read(learned): %v", err)
			}
			if !strings.Contains(learned, "parity lesson") {
				t.Errorf("memory://root/learned.md = %q", learned)
			}
			if _, err := s.Read("memory://root/nope"); err == nil {
				t.Error("an unknown memory:// path must fail")
			}
			if summary, lessons := s.Paths(); summary == "" || lessons == "" {
				t.Errorf("Paths = (%q, %q), want both locations named", summary, lessons)
			}
			if stats := s.Stats(); stats == "" {
				t.Error("Stats must report something")
			}
			if err := s.Clear(); err != nil {
				t.Fatalf("Clear: %v", err)
			}
			after, err := s.Read("memory://root")
			if err != nil {
				t.Fatalf("Read after Clear: %v", err)
			}
			if after != "(no memories stored yet)" {
				t.Errorf("after Clear: %q, want the empty-store answer", after)
			}
		})
	}
}

// TestMnemopiReadFullRowAndPreviewClip pins the reference seam: recall
// previews are clipped (the id is always in the line), and the full row is
// readable through memory://<id> before an edit.
func TestMnemopiReadFullRowAndPreviewClip(t *testing.T) {
	m := newMnemopi(t, "proj")
	ctx := context.Background()
	long := strings.Repeat("detail ", 120) + "END OF MEMORY"
	res := mustRetain(t, m, Fact{Text: long, Tags: []string{"longform"}, Source: "clip test"})

	hits, err := m.Recall(ctx, "detail", 0)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	text, err := m.RecallText(ctx, "detail")
	if err != nil {
		t.Fatalf("RecallText: %v", err)
	}
	if !strings.Contains(text, fmt.Sprintf("(id: %d)", res.ID)) {
		t.Errorf("the preview must carry the memory id for memory_edit: %q", text)
	}
	if strings.Contains(text, "END OF MEMORY") {
		t.Errorf("the preview must be clipped at %d chars: %q", previewChars, text)
	}
	if len(hits) == 0 || !strings.Contains(hits[0].Fact.Text, "END OF MEMORY") {
		t.Error("the stored fact itself must stay untruncated (only the preview clips)")
	}

	full, err := m.Read(fmt.Sprintf("memory://%d", res.ID))
	if err != nil {
		t.Fatalf("Read(memory://%d): %v", res.ID, err)
	}
	for _, want := range []string{"kind: fact", "bank: project:proj", "tags: longform", "source: clip test", "END OF MEMORY"} {
		if !strings.Contains(full, want) {
			t.Errorf("memory://%d is missing %q:\n%s", res.ID, want, full)
		}
	}
	if _, err := m.Read("memory://999999"); err == nil {
		t.Error("reading an unknown memory id must fail")
	}
}

// TestMnemopiToolsSchemaParity exercises the tool layer over the store: the
// reference input shapes (items batch, _op, query) must work, and the
// unsupported ones must say so instead of silently doing something else.
func TestMnemopiToolsSchemaParity(t *testing.T) {
	m := newMnemopi(t, "proj", func(m *Mnemopi) {
		m.Complete = func(context.Context, string) (string, error) { return "synthesised overview", nil }
	})
	ctx := context.Background()
	retain := &MnemopiRetainTool{Mem: m}
	res, err := retain.Execute(ctx, json.RawMessage(`{"items":[{"content":"first remembered fact about queues","context":"call 1"},{"content":"second remembered fact about leases"}],"tags":["ops"]}`))
	if err != nil || res.IsError {
		t.Fatalf("batch retain = %+v (%v)", res, err)
	}
	if !strings.Contains(res.Text, "retained 2 memories") {
		t.Errorf("batch retain receipt = %q, want a count", res.Text)
	}
	if _, err := retain.Execute(ctx, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("empty retain: %v", err)
	}
	if r, _ := retain.Execute(ctx, json.RawMessage(`{}`)); !r.IsError {
		t.Error("a retain with neither items nor text must fail")
	}

	recall := &MnemopiRecallTool{Mem: m}
	r, err := recall.Execute(ctx, json.RawMessage(`{"query":"leases"}`))
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if !strings.Contains(r.Text, "second remembered fact about leases") || !strings.Contains(r.Text, "(id: ") {
		t.Errorf("recall output = %q, want the memory with its id", r.Text)
	}

	// reflect accepts the reference's query argument (and the topic alias).
	reflect := &MnemopiReflectTool{Mem: m}
	r, err = reflect.Execute(ctx, json.RawMessage(`{"query":"what do we know about leases?"}`))
	if err != nil || r.IsError {
		t.Fatalf("reflect = %+v (%v)", r, err)
	}
	if !strings.Contains(r.Text, "synthesised overview") {
		t.Errorf("reflect output = %q, want the synthesis", r.Text)
	}
	if r, _ = reflect.Execute(ctx, json.RawMessage(`{"topic":"leases"}`)); r.IsError {
		t.Errorf("the topic alias must keep working: %+v", r)
	}

	// memory_edit: the reference ops map onto update/forget; invalidate has
	// no counterpart and must be refused with a reason.
	edit := &MemoryEditTool{Mem: m}
	ids := res.Details.(map[string]any)["ids"].([]int64)
	forget := fmt.Sprintf(`{"_op":"forget","id":%d}`, ids[0])
	if r, err = edit.Execute(ctx, json.RawMessage(forget)); err != nil || r.IsError {
		t.Fatalf("forget = %+v (%v)", r, err)
	}
	if _, err := m.FactByID(ctx, ids[0]); err == nil {
		t.Error("_op forget must delete the memory")
	}
	if r, _ = edit.Execute(ctx, json.RawMessage(`{"_op":"invalidate","id":1}`)); !r.IsError || !strings.Contains(r.Text, "invalidate") {
		t.Errorf("invalidate must be refused explicitly: %+v", r)
	}
	if r, _ = edit.Execute(ctx, json.RawMessage(`{"_op":"nonsense","id":1}`)); !r.IsError {
		t.Error("an unknown _op must fail")
	}
	update := fmt.Sprintf(`{"_op":"update","id":%d,"text":"rewritten second fact"}`, ids[1])
	if r, err = edit.Execute(ctx, json.RawMessage(update)); err != nil || r.IsError {
		t.Fatalf("update = %+v (%v)", r, err)
	}
	if f, err := m.FactByID(ctx, ids[1]); err != nil || f.Text != "rewritten second fact" {
		t.Errorf("updated fact = %+v (%v)", f, err)
	}
}

// TestMnemopiStoreNilSafety: an unwired backend must degrade, never panic.
func TestMnemopiStoreNilSafety(t *testing.T) {
	var nilStore *Mnemopi
	if !nilStore.Off() {
		t.Error("a nil backend must report off")
	}
	if err := nilStore.Ensure(); err != nil {
		t.Errorf("nil Ensure = %v, want nil", err)
	}
	if got := nilStore.Summary(); got != "" {
		t.Errorf("nil Summary = %q, want empty", got)
	}
	if _, err := nilStore.Retain(context.Background(), Fact{Text: "x"}); err == nil {
		t.Error("nil Retain must report the backend is off")
	}
	if _, err := nilStore.Recall(context.Background(), "x", 0); err == nil {
		t.Error("nil Recall must report the backend is off")
	}
	if got := nilStore.Stats(); got != "memory: off" {
		t.Errorf("nil Stats = %q, want memory: off", got)
	}
}
