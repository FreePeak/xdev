package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/session"
)

// sharpStub is the synthesis-seam double: it counts calls and replies with a
// canned decision object.
type sharpStub struct {
	mu    sync.Mutex
	calls int
	reply string
	err   error
}

func (m *sharpStub) complete(_ context.Context, _ string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.err != nil {
		return "", m.err
	}
	if m.reply == "" {
		return `{"scope":"style","bullet":"run gofmt before committing"}`, nil
	}
	return m.reply, nil
}

func (m *sharpStub) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func newTestSharp(t *testing.T, tune func(*SharpShooter)) *SharpShooter {
	t.Helper()
	s := &SharpShooter{Dir: filepath.Join(t.TempDir(), "memories"), Session: "sess-1"}
	if tune != nil {
		tune(s)
	}
	return s
}

func decisionFiles(t *testing.T, s *SharpShooter) []string {
	t.Helper()
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// TestFrictionThreshold: a statement restated (the 2nd occurrence) opens the
// gate; a single statement, unrelated statements, or one failed turn alone do
// not.
func TestFrictionThreshold(t *testing.T) {
	d := NewFrictionDetector(0)
	if d.Threshold != DefaultFrictionThreshold {
		t.Fatalf("default threshold = %d", d.Threshold)
	}
	if d.Observe(Turn{Text: "always run gofmt before committing"}) {
		t.Fatal("a single statement must not trigger")
	}
	if d.Score() != 0 {
		t.Fatalf("score after one statement = %d, want 0", d.Score())
	}
	if !d.Observe(Turn{Text: "  Always run gofmt before committing.  "}) {
		t.Fatal("the restated rule must trigger (case/space/punctuation folded)")
	}
	if got := d.Quotes(); len(got) != 1 {
		t.Fatalf("quotes = %v, want the one repeated statement", got)
	}

	// Unrelated turns never accumulate.
	other := NewFrictionDetector(0)
	for _, text := range []string{"add a flag", "run the tests", "why did that fail"} {
		if other.Observe(Turn{Text: text}) {
			t.Fatalf("%q must not trigger", text)
		}
	}

	// A lone instruction after a failure is one point: below the threshold.
	lone := NewFrictionDetector(0)
	if lone.Observe(Turn{Text: "no, use the stdlib instead", AfterFailure: true}) {
		t.Fatal("a single post-failure instruction must not trigger")
	}
	if lone.Score() != 1 {
		t.Fatalf("post-failure score = %d, want 1", lone.Score())
	}
	// Two failures, or a repeat plus a failure, do.
	two := NewFrictionDetector(0)
	two.Observe(Turn{Text: "try again", AfterFailure: true})
	if !two.Observe(Turn{Text: "try again", AfterFailure: true}) {
		t.Fatal("repeated post-failure instructions must trigger")
	}
}

// TestDecisionFileWriteShape pins the artifact: YAML frontmatter with scope,
// updated and source-session, then the consolidated bullet.
func TestDecisionFileWriteShape(t *testing.T) {
	stub := &sharpStub{}
	s := newTestSharp(t, func(s *SharpShooter) { s.Complete = stub.complete })
	rule := "always run gofmt before committing"
	if s.Observe(Turn{Text: rule}) {
		t.Fatal("one statement must not trigger a consolidation")
	}
	if !s.Observe(Turn{Text: rule}) {
		t.Fatal("the restated rule must trigger")
	}
	s.Wait()
	if stub.count() != 1 {
		t.Fatalf("consolidation calls = %d, want 1", stub.count())
	}

	raw := readFileOrEmpty(s.path("style"))
	if !strings.HasPrefix(raw, "---\nscope: style\n") || !strings.Contains(raw, "\nsource-session: sess-1\n") {
		t.Fatalf("frontmatter missing scope/source-session:\n%s", raw)
	}
	fields := map[string]string{}
	for _, line := range strings.Split(strings.TrimPrefix(raw, "---\n"), "\n") {
		if i := strings.Index(line, ": "); i > 0 {
			fields[line[:i]] = line[i+2:]
		}
		if strings.HasPrefix(line, "---") {
			break
		}
	}
	stamp, session := fields["updated"], fields["source-session"]
	if _, err := time.Parse(time.RFC3339, stamp); err != nil {
		t.Fatalf("updated %q is not RFC3339: %v", stamp, err)
	}
	if session != "sess-1" {
		t.Fatalf("source-session = %q", session)
	}
	if bullets := readBullets(raw); len(bullets) != 1 || bullets[0] != "run gofmt before committing" {
		t.Fatalf("bullets = %v", bullets)
	}

	// The injected guidance block is the decision bodies, frontmatter dropped.
	block := s.GuidanceBlock()
	if !strings.Contains(block, "# Memory Guidance") || !strings.Contains(block, "run gofmt before committing") {
		t.Fatalf("guidance block = %q", block)
	}
	if strings.Contains(block, "source-session") {
		t.Fatalf("frontmatter leaked into the injected block: %q", block)
	}
}

// TestDecisionFileCaps: over MaxBullets/MaxFileBytes the writer keeps the
// newest bullets, and always keeps at least the newest one.
func TestDecisionFileCaps(t *testing.T) {
	s := newTestSharp(t, func(s *SharpShooter) { s.MaxBullets = 3 })
	for i := 1; i <= 5; i++ {
		if err := s.SaveLesson(fmt.Sprintf("preference number %d", i), ""); err != nil {
			t.Fatal(err)
		}
	}
	bullets := readBullets(readFileOrEmpty(s.path("style")))
	if len(bullets) != 3 {
		t.Fatalf("bullets = %d, want the 3 newest: %v", len(bullets), bullets)
	}
	if bullets[len(bullets)-1] != "preference number 5" {
		t.Fatalf("newest bullet lost: %v", bullets)
	}
	if strings.Contains(strings.Join(bullets, "\n"), "preference number 1") {
		t.Fatalf("oldest bullet not dropped: %v", bullets)
	}

	// Byte cap: the file stays under it, newest first.
	bs := newTestSharp(t, func(s *SharpShooter) { s.MaxBullets = 50; s.MaxFileBytes = 200 })
	for i := 1; i <= 10; i++ {
		if err := bs.SaveLesson(strings.Repeat(fmt.Sprintf("rule %d ", i), 4), ""); err != nil {
			t.Fatal(err)
		}
	}
	raw := readFileOrEmpty(bs.path("style"))
	if len(raw) > 200 {
		t.Fatalf("decision file = %d bytes, want <= 200", len(raw))
	}
	if kept := readBullets(raw); len(kept) == 0 || !strings.Contains(kept[len(kept)-1], "rule 10") {
		t.Fatalf("newest bullet lost under the byte cap: %v", kept)
	}

	// A cap below the frontmatter still keeps the one bullet: a decision that
	// was made is never silently discarded.
	ts := newTestSharp(t, func(s *SharpShooter) { s.MaxFileBytes = 10 })
	if err := ts.SaveLesson("gofmt before commit", ""); err != nil {
		t.Fatal(err)
	}
	if kept := readBullets(readFileOrEmpty(ts.path("style"))); len(kept) != 1 {
		t.Fatalf("bullets = %v, want the single kept decision", kept)
	}
}

// TestObserveConsolidatesOncePerSession: the friction gate is session-scoped.
func TestObserveConsolidatesOncePerSession(t *testing.T) {
	stub := &sharpStub{}
	s := newTestSharp(t, func(s *SharpShooter) { s.Complete = stub.complete })
	rule := "never add a dependency when the stdlib does it"
	for i := 0; i < 6; i++ {
		s.Observe(Turn{Text: rule, AfterFailure: i%2 == 1})
	}
	s.Wait()
	if stub.count() != 1 {
		t.Fatalf("consolidation calls = %d, want exactly 1 per session", stub.count())
	}
	if bullets := readBullets(readFileOrEmpty(s.path("style"))); len(bullets) != 1 {
		t.Fatalf("bullets = %v, want one", bullets)
	}
}

// TestNoWritesWithoutFriction: a session that never restates itself and never
// fails writes nothing at all.
func TestNoWritesWithoutFriction(t *testing.T) {
	stub := &sharpStub{}
	s := newTestSharp(t, func(s *SharpShooter) { s.Complete = stub.complete })
	for _, text := range []string{"add the flag", "now run the tests", "look at loop.go"} {
		if s.Observe(Turn{Text: text}) {
			t.Fatalf("%q must not trigger", text)
		}
	}
	s.Wait()
	if got := decisionFiles(t, s); len(got) != 0 {
		t.Fatalf("files written without friction: %v", got)
	}
	if stub.count() != 0 {
		t.Fatalf("model called %d times without friction", stub.count())
	}
	if err := s.Consolidate(context.Background()); !errors.Is(err, ErrNoFriction) {
		t.Fatalf("consolidate without friction = %v, want ErrNoFriction", err)
	}
}

// TestSharpShooterDisabledIsInert: nil and dir-less backends cost nothing and
// write nothing.
func TestSharpShooterDisabledIsInert(t *testing.T) {
	var nilSS *SharpShooter
	empty := &SharpShooter{Complete: (&sharpStub{}).complete}
	for name, s := range map[string]*SharpShooter{"nil": nilSS, "no dir": empty} {
		if !s.Off() {
			t.Fatalf("%s: must report off", name)
		}
		if s.Observe(Turn{Text: "x"}) {
			t.Fatalf("%s: must never trigger", name)
		}
		if s.Summary() != "" || s.GuidanceBlock() != "" {
			t.Fatalf("%s: summary must be empty", name)
		}
		if _, err := s.Read(mem("root")); err == nil {
			t.Fatalf("%s: read must error", name)
		}
		if err := s.SaveLesson("x", ""); err == nil {
			t.Fatalf("%s: save must error", name)
		}
		if s.Stats() != "memory: off" {
			t.Fatalf("%s: stats = %q", name, s.Stats())
		}
		if err := s.Clear(); err == nil {
			t.Fatalf("%s: clear must error", name)
		}
		if err := s.Consolidate(context.Background()); err != nil {
			t.Fatalf("%s: consolidate must be a no-op: %v", name, err)
		}
		s.Wait()
	}
}

// TestSharpShooterStoreParity drives the backend through the same seam the
// prompt injection, the memory:// reader, the learn tool and /memory use.
func TestSharpShooterStoreParity(t *testing.T) {
	s := newTestSharp(t, nil)
	var store Store = s
	if store.Off() {
		t.Fatal("configured backend must be on")
	}
	if got, err := store.Read(mem("root")); err != nil || got != "(no decision files yet)" {
		t.Fatalf("empty read = %q, %v", got, err)
	}
	// The learn tool lands a lesson through SaveLesson, classified into the
	// architecture axis by its text.
	lesson := &LearnTool{Backend: store, SkillsDir: filepath.Join(t.TempDir(), "skills")}
	res, err := lesson.Execute(context.Background(), json.RawMessage(`{"memory":"the storage layer stays stdlib-only: no new dependency for the backend"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("learn failed: %s", res.Text)
	}
	raw, err := store.Read(mem("root/architecture.md"))
	if err != nil || !strings.Contains(raw, "no new dependency for the backend") {
		t.Fatalf("architecture.md = %q, %v", raw, err)
	}
	if !strings.Contains(raw, "scope: architecture") {
		t.Fatalf("wrong axis: %q", raw)
	}
	// memory://root is the injected text (frontmatter dropped), so it is not
	// byte-identical to the file.
	root, err := store.Read(mem("root"))
	if err != nil || !strings.Contains(root, "no new dependency for the backend") || strings.Contains(root, "scope:") {
		t.Fatalf("root = %q, %v", root, err)
	}
	if _, err := store.Read(mem("root/learned.md")); err == nil {
		t.Fatal("a path outside the bounded set must error, not invent a file")
	}
	if _, err := store.Read("nope"); err == nil {
		t.Fatal("a non-memory URL must error")
	}
	sum, files := store.Paths()
	if sum != s.Dir || !strings.Contains(files, "style.md") {
		t.Fatalf("paths = %q, %q", sum, files)
	}
	if got := store.Stats(); !strings.Contains(got, "sharpshooter") || !strings.Contains(got, "architecture.md: 1 bullets") {
		t.Fatalf("stats = %q", got)
	}
	if err := store.Clear(); err != nil {
		t.Fatal(err)
	}
	if got := decisionFiles(t, s); len(got) != 0 {
		t.Fatalf("clear left %v", got)
	}
	if store.Summary() != "" {
		t.Fatal("summary must be empty after clear")
	}
}

// TestSharpShooterFallbacks: without a model seam the friction quote is filed
// verbatim; a broken reply is reported and writes nothing.
func TestSharpShooterFallbacks(t *testing.T) {
	raw := newTestSharp(t, nil)
	rule := "keep the CLI flags boring"
	raw.Observe(Turn{Text: rule})
	raw.Observe(Turn{Text: rule})
	raw.Wait()
	if bullets := readBullets(readFileOrEmpty(raw.path("style"))); len(bullets) != 1 || bullets[0] != rule {
		t.Fatalf("verbatim fallback bullets = %v", bullets)
	}

	var got []error
	broken := newTestSharp(t, func(s *SharpShooter) {
		s.Complete = (&sharpStub{reply: "I think you should just be careful"}).complete
		s.OnError = func(err error) { got = append(got, err) }
	})
	broken.Observe(Turn{Text: rule})
	broken.Observe(Turn{Text: rule})
	broken.Wait()
	if len(got) == 0 || !strings.Contains(got[0].Error(), "no JSON object") {
		t.Fatalf("errors = %v", got)
	}
	if files := decisionFiles(t, broken); len(files) != 0 {
		t.Fatalf("a broken model reply wrote %v", files)
	}
}

// TestConsolidationUsesThePipelineLease: a second consolidator — another
// process, or a racing pass — drops the work instead of double-writing.
func TestConsolidationUsesThePipelineLease(t *testing.T) {
	stub := &sharpStub{}
	s := newTestSharp(t, func(s *SharpShooter) { s.Complete = stub.complete })
	rule := "table tests for anything with branches"
	s.Observe(Turn{Text: rule})
	s.Observe(Turn{Text: rule})
	s.Wait()

	release, _, err := acquireFileLease(filepath.Join(s.Dir, sharpLeaseFile), DefaultLeaseTTL)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Consolidate(context.Background()); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("consolidate under a held lease = %v, want ErrLeaseHeld", err)
	}
	release()
	if err := s.Consolidate(context.Background()); err != nil {
		t.Fatalf("consolidate after release: %v", err)
	}
	if bullets := readBullets(readFileOrEmpty(s.path("style"))); len(bullets) != 1 {
		t.Fatalf("bullets = %v, want the duplicate suppressed", bullets)
	}
}

// TestReplayCarriesFrictionAcrossProcesses: a session resumed in a new process
// keeps its restated-rule history, so the repeat is caught even though this
// process only ever saw one live turn.
func TestReplayCarriesFrictionAcrossProcesses(t *testing.T) {
	rule := "always run gofmt before committing"
	dataDir := t.TempDir()
	path := writeSession(t, dataDir, "/tmp/proj", "aaaaaaaa-0000-0000-0000-000000000009", false,
		rule, "unrelated one", "unrelated two")
	st, err := session.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	stub := &sharpStub{}
	s := newTestSharp(t, func(s *SharpShooter) { s.Complete = stub.complete })
	if err := s.Replay(st); err != nil {
		t.Fatal(err)
	}
	if stub.count() != 0 {
		t.Fatal("replay must not consolidate by itself")
	}
	// One live turn (folded case/punctuation included) is now the repeat.
	if !s.Observe(Turn{Text: "Always run gofmt before committing."}) {
		t.Fatal("the replayed rule should make this turn the repeat")
	}
	s.Wait()
	if stub.count() != 1 {
		t.Fatalf("consolidation calls = %d, want 1", stub.count())
	}
	if bullets := readBullets(readFileOrEmpty(s.path("style"))); len(bullets) != 1 || bullets[0] != "run gofmt before committing" {
		t.Fatalf("bullets = %v", bullets)
	}

	// The same single turn without the replayed history stays below the gate.
	fresh := newTestSharp(t, func(s *SharpShooter) { s.Complete = stub.complete })
	if fresh.Observe(Turn{Text: rule}) {
		t.Fatal("one live turn alone must not trigger")
	}

	// A transcript that already repeats itself opens the gate for the next
	// turn, whichever turn that is: the session has friction to file.
	rep := writeSession(t, dataDir, "/tmp/proj", "aaaaaaaa-0000-0000-0000-00000000000a", false, rule, rule)
	rst, err := session.Open(rep)
	if err != nil {
		t.Fatal(err)
	}
	defer rst.Close()
	open := newTestSharp(t, func(s *SharpShooter) { s.Complete = stub.complete })
	if err := open.Replay(rst); err != nil {
		t.Fatal(err)
	}
	if !open.Observe(Turn{Text: "carry on", AfterFailure: true}) {
		t.Fatal("friction already in the transcript must keep the gate open")
	}
	open.Wait()
	if open.Summary() == "" {
		t.Fatal("no decision was filed from the replayed friction")
	}
}

// TestFrictionRetentionIsBounded: user turns are arbitrary text, so the
// detector's per-session state has a ceiling (the process is under an RSS
// budget); repeats of already-tracked statements still register past it.
func TestFrictionRetentionIsBounded(t *testing.T) {
	d := NewFrictionDetector(0)
	rule := "always run gofmt before committing"
	d.Observe(Turn{Text: rule})
	for i := 0; i < maxFrictionKeys*4; i++ {
		d.Observe(Turn{Text: fmt.Sprintf("unrelated statement %d", i), AfterFailure: true})
	}
	if len(d.counts) > maxFrictionKeys {
		t.Fatalf("repeat keys = %d, want <= %d", len(d.counts), maxFrictionKeys)
	}
	if got := len(d.Quotes()); got > maxFrictionQuotes {
		t.Fatalf("quotes = %d, want <= %d", got, maxFrictionQuotes)
	}
	for _, q := range d.Quotes() {
		if len(q) > maxDecisionBulletChars+1 {
			t.Fatalf("retained quote of %d chars", len(q))
		}
	}
	// The statement from before the flood is still counted.
	if d.counts[normalizeTurn(rule)] != 1 {
		t.Fatal("a tracked statement was evicted")
	}
	if d.Score() == 0 {
		t.Fatal("friction from the flood was lost")
	}
}
