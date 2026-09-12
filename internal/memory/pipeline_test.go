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

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// stubModel is the injected model seam: it records prompts and answers with a
// canned reply, so both phases are exercised without a provider.
type stubModel struct {
	mu      sync.Mutex
	prompts []string

	// entered is closed when the first call arrives; release blocks that
	// first call until the test closes it (concurrency tests).
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newStubModel() *stubModel { return &stubModel{} }

func (s *stubModel) complete(_ context.Context, prompt string) (string, error) {
	s.mu.Lock()
	s.prompts = append(s.prompts, prompt)
	s.mu.Unlock()
	if s.entered != nil {
		s.once.Do(func() { close(s.entered) })
		<-s.release
	}
	if strings.Contains(prompt, "New findings:") {
		return `{"summary":"# Memory\n\n- consolidated fact","lessons":["lesson from consolidation"]}`, nil
	}
	return `{"notes":["note one"],"lessons":["lesson one"]}`, nil
}

func (s *stubModel) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.prompts...)
}

// writeSession materializes a real session file (the pipeline reads what the
// CLI writes) holding the given user turns, each followed by an assistant
// reply so the transcript looks like a real exchange.
func writeSession(t *testing.T, dataDir, cwd, id string, subagent bool, turns ...string) string {
	t.Helper()
	path := session.SessionFilePath(dataDir, cwd, time.Now(), id)
	st := session.OpenMem(cwd, "fixture")
	if subagent {
		st.SetTitleSourceSubagent()
	}
	if _, err := st.EnsureOnDisk(path, session.Options{}); err != nil {
		t.Fatalf("materialize session: %v", err)
	}
	for i, turn := range turns {
		appendMsg(t, st, ai.RoleUser, turn)
		appendMsg(t, st, ai.RoleAssistant, fmt.Sprintf("ack %d", i))
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close session: %v", err)
	}
	return path
}

func appendMsg(t *testing.T, st *session.Store, role ai.Role, text string) {
	t.Helper()
	err := st.Append(&session.MessageEntry{Message: ai.Message{
		Role:    role,
		Content: []ai.Block{ai.TextBlock{Text: text}},
	}})
	if err != nil {
		t.Fatalf("append %s: %v", role, err)
	}
}

func newTestPipeline(t *testing.T, model func(context.Context, string) (string, error), tune func(*Pipeline)) (*Pipeline, *Backend, string) {
	t.Helper()
	dataDir := t.TempDir()
	b := &Backend{Dir: filepath.Join(dataDir, "memory")}
	p := &Pipeline{Backend: b, DataDir: dataDir, Complete: model}
	if tune != nil {
		tune(p)
	}
	return p, b, dataDir
}

// TestPipelineTwoPhasesWriteThroughCaps is the end-to-end pass: phase 1 over
// the changed session, phase 2 into MEMORY.md + learned.md, both stubs
// driving the model seam.
func TestPipelineTwoPhasesWriteThroughCaps(t *testing.T) {
	stub := newStubModel()
	p, b, dataDir := newTestPipeline(t, stub.complete, nil)
	writeSession(t, dataDir, "/tmp/proj", "aaaaaaaa-0000-0000-0000-000000000001", false,
		"always run gofmt before committing")
	writeSession(t, dataDir, "/tmp/proj", "bbbbbbbb-0000-0000-0000-000000000002", true,
		"subagent turn that must never be extracted")

	res, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Extracted != 1 || res.Candidates != 2 || res.Lessons != 1 || !res.Consolidated {
		t.Fatalf("result = %+v, want one session, 2 candidates, 1 lesson, consolidated", res)
	}
	raw, _ := b.Read(mem("root/MEMORY.md"))
	if !strings.Contains(raw, "consolidated fact") {
		t.Fatalf("MEMORY.md = %q", raw)
	}
	raw, _ = b.Read(mem("root/learned.md"))
	if !strings.Contains(raw, "lesson from consolidation") {
		t.Fatalf("learned.md = %q", raw)
	}
	prompts := stub.calls()
	if len(prompts) != 2 {
		t.Fatalf("model calls = %d, want 1 extraction + 1 consolidation: %q", len(prompts), prompts)
	}
	if !strings.Contains(prompts[0], "always run gofmt") {
		t.Fatalf("extraction prompt lacks the transcript: %q", prompts[0])
	}
	if strings.Contains(strings.Join(prompts, "\n"), "subagent turn") {
		t.Fatal("a subagent session reached the model")
	}
	if !strings.Contains(prompts[1], "note one") || !strings.Contains(prompts[1], "New findings:") {
		t.Fatalf("consolidation prompt lacks the candidates: %q", prompts[1])
	}
	// The injected block is the pipeline's output path.
	if block := b.GuidanceBlock(); !strings.Contains(block, "consolidated fact") {
		t.Fatalf("guidance block = %q", block)
	}
}

// TestPipelineWatermarkSkipsUnchangedAndTurnlessSessions pins the two skip
// rules: unchanged since the last run, and changed without new user turns.
func TestPipelineWatermarkSkipsUnchangedAndTurnlessSessions(t *testing.T) {
	stub := newStubModel()
	p, _, dataDir := newTestPipeline(t, stub.complete, nil)
	path := writeSession(t, dataDir, "/tmp/proj", "aaaaaaaa-0000-0000-0000-000000000003", false, "first turn")

	if _, err := p.Run(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if got := len(stub.calls()); got != 2 {
		t.Fatalf("first run model calls = %d, want 2", got)
	}
	if _, err := os.Stat(p.watermarkPath()); err != nil {
		t.Fatalf("watermark not written: %v", err)
	}

	// Unchanged: a second run costs no model call at all.
	res, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.Extracted != 0 || res.Candidates != 0 || res.Consolidated {
		t.Fatalf("second run = %+v, want a no-op", res)
	}
	if got := len(stub.calls()); got != 2 {
		t.Fatalf("unchanged session was re-extracted: %d model calls", got)
	}

	// Changed but gained no user turn: still skipped (tool churn only).
	st, err := session.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	appendMsg(t, st, ai.RoleAssistant, "a later assistant message with no new user turn")
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	res, err = p.Run(context.Background())
	if err != nil {
		t.Fatalf("third run: %v", err)
	}
	if res.Extracted != 0 {
		t.Fatalf("assistant-only change extracted: %+v", res)
	}

	// A new user turn re-opens the session.
	st, err = session.Open(path)
	if err != nil {
		t.Fatalf("reopen 2: %v", err)
	}
	appendMsg(t, st, ai.RoleUser, "second turn")
	if err := st.Close(); err != nil {
		t.Fatalf("close 2: %v", err)
	}
	res, err = p.Run(context.Background())
	if err != nil {
		t.Fatalf("fourth run: %v", err)
	}
	if res.Extracted != 1 {
		t.Fatalf("new user turn not extracted: %+v", res)
	}
	if last := stub.calls(); !strings.Contains(last[2], "second turn") {
		t.Fatalf("re-extraction prompt lacks the new turn: %q", last[2])
	}
}

// TestPipelineLeaseBlocksConcurrentRun: a run in flight (here one blocked in
// phase 1) rejects a second Run with ErrLeaseHeld.
func TestPipelineLeaseBlocksConcurrentRun(t *testing.T) {
	stub := newStubModel()
	stub.entered, stub.release = make(chan struct{}), make(chan struct{})
	p, _, dataDir := newTestPipeline(t, stub.complete, nil)
	writeSession(t, dataDir, "/tmp/proj", "aaaaaaaa-0000-0000-0000-000000000004", false, "a turn")

	done := make(chan Result, 1)
	go func() {
		res, err := p.Run(context.Background())
		if err != nil {
			t.Errorf("in-flight run: %v", err)
		}
		done <- res
	}()
	<-stub.entered

	if _, err := p.Run(context.Background()); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("concurrent Run error = %v, want ErrLeaseHeld", err)
	}
	close(stub.release)
	select {
	case res := <-done:
		if res.Extracted != 1 {
			t.Fatalf("in-flight run = %+v, want the extraction", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight run never finished")
	}
	if _, err := os.Stat(p.leasePath()); !os.IsNotExist(err) {
		t.Fatal("lease not released after the run")
	}
}

// TestPipelineLeaseStaleReclaimAndOwnership covers the crash paths.
func TestPipelineLeaseStaleReclaimAndOwnership(t *testing.T) {
	stub := newStubModel()
	p, _, dataDir := newTestPipeline(t, stub.complete, nil)
	writeSession(t, dataDir, "/tmp/proj", "aaaaaaaa-0000-0000-0000-000000000005", false, "a turn")
	if err := os.MkdirAll(p.Backend.Dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// A fresh foreign lease: refused, and left untouched.
	writeLeaseAt(t, p, "other-run", time.Now())
	if _, err := p.Run(context.Background()); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("fresh foreign lease error = %v, want ErrLeaseHeld", err)
	}
	if l, ok := readLease(p.leasePath()); !ok || l.Run != "other-run" {
		t.Fatalf("foreign lease was disturbed: %+v ok=%v", l, ok)
	}

	// A stale lease (the crashed-run case) is reclaimed and the run happens.
	writeLeaseAt(t, p, "dead-run", time.Now().Add(-2*time.Hour))
	res, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("stale lease run: %v", err)
	}
	if res.Extracted != 1 {
		t.Fatalf("stale lease not reclaimed: %+v", res)
	}
	if _, err := os.Stat(p.leasePath()); !os.IsNotExist(err) {
		t.Fatal("lease not released after the reclaiming run")
	}

	// Heartbeat: it re-stamps our lease, refuses a lease we lost, and a lost
	// lease is not deleted by our release.
	release, runID, err := p.acquireLease()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	before, _ := readLease(p.leasePath())
	time.Sleep(2 * time.Millisecond)
	if err := p.heartbeat(runID); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	after, _ := readLease(p.leasePath())
	if !stampAfter(t, after.At, before.At) {
		t.Fatalf("heartbeat did not refresh the stamp: %s -> %s", before.At, after.At)
	}
	writeLeaseAt(t, p, "thief", time.Now())
	if err := p.heartbeat(runID); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("heartbeat on a stolen lease = %v, want ErrLeaseHeld", err)
	}
	release()
	if l, ok := readLease(p.leasePath()); !ok || l.Run != "thief" {
		t.Fatal("release deleted a lease it no longer owned")
	}
}

// stampAfter reports whether RFC3339Nano stamp b is later than a.
func stampAfter(t *testing.T, b, a string) bool {
	t.Helper()
	bt, err := time.Parse(time.RFC3339Nano, b)
	if err != nil {
		t.Fatalf("parse %q: %v", b, err)
	}
	at, err := time.Parse(time.RFC3339Nano, a)
	if err != nil {
		t.Fatalf("parse %q: %v", a, err)
	}
	return bt.After(at)
}

func writeLeaseAt(t *testing.T, p *Pipeline, runID string, at time.Time) {
	t.Helper()
	raw, err := json.Marshal(lease{PID: 4242, Run: runID, At: at.UTC().Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.leasePath(), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestPipelineCapsBoundTheRun: MaxStarts/ScanLimit bound how many sessions
// one run touches, InputCap the transcript, MaxLessons + the summary cap the
// phase-2 writes.
func TestPipelineCapsBoundTheRun(t *testing.T) {
	stub := newStubModel()
	p, _, dataDir := newTestPipeline(t, stub.complete, func(p *Pipeline) {
		p.MaxStarts = 2
	})
	for i := range 5 {
		writeSession(t, dataDir, "/tmp/proj", fmt.Sprintf("aaaaaaaa-0000-0000-0000-00000000000%d", i+6),
			false, "turn for session")
	}
	res, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Scanned != 2 || res.Extracted != 2 {
		t.Fatalf("MaxStarts not honored: %+v", res)
	}
	if got := len(stub.calls()); got != 3 { // 2 extractions + 1 consolidation
		t.Fatalf("model calls = %d, want 3", got)
	}

	// ScanLimit alone bounds the listing pass.
	stub2 := newStubModel()
	p2, _, dataDir2 := newTestPipeline(t, stub2.complete, func(p *Pipeline) {
		p.ScanLimit = 3
	})
	for i := range 5 {
		writeSession(t, dataDir2, "/tmp/proj", fmt.Sprintf("cccccccc-0000-0000-0000-00000000000%d", i+1),
			false, "turn for session")
	}
	res2, err := p2.Run(context.Background())
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if res2.Scanned != 3 || res2.Extracted != 3 {
		t.Fatalf("ScanLimit not honored: %+v", res2)
	}

	// InputCap keeps the tail of a long transcript and marks the cut.
	stub3 := newStubModel()
	p3, _, dataDir3 := newTestPipeline(t, stub3.complete, func(p *Pipeline) {
		p.InputCap = 120
	})
	writeSession(t, dataDir3, "/tmp/proj", "dddddddd-0000-0000-0000-000000000009", false,
		strings.Repeat("x", 400)+" the newest tail", strings.Repeat("y", 400)+" last words here")
	if _, err := p3.Run(context.Background()); err != nil {
		t.Fatalf("run 3: %v", err)
	}
	prompt := stub3.calls()[0]
	if !strings.Contains(prompt, "earlier turns omitted") || !strings.Contains(prompt, "last words here") {
		t.Fatalf("InputCap tail not applied: %q", prompt)
	}

	// Phase 2: the lesson count is capped and the summary goes through
	// capText's marker.
	many := make([]string, 0, 30)
	for i := range 30 {
		many = append(many, fmt.Sprintf("lesson %d", i))
	}
	reply, err := json.Marshal(map[string]any{
		"summary": strings.Repeat("long line of summary\n", 200),
		"lessons": many,
	})
	if err != nil {
		t.Fatal(err)
	}
	p4, b4, dataDir4 := newTestPipeline(t, func(_ context.Context, prompt string) (string, error) {
		if strings.Contains(prompt, "New findings:") {
			return string(reply), nil
		}
		return `{"notes":["n"],"lessons":["l"]}`, nil
	}, func(p *Pipeline) {
		p.MaxLessons = 3
	})
	b4.SummaryCapChars = 200
	b4.LessonCap = 2
	writeSession(t, dataDir4, "/tmp/proj", "eeeeeeee-0000-0000-0000-00000000000a", false, "a turn")
	res4, err := p4.Run(context.Background())
	if err != nil {
		t.Fatalf("run 4: %v", err)
	}
	if res4.Lessons != 3 {
		t.Fatalf("MaxLessons not honored: %+v", res4)
	}
	summary, _ := b4.Read(mem("root/MEMORY.md"))
	if !strings.Contains(summary, "summary capped") {
		t.Fatalf("summary cap marker missing: %q", summary)
	}
	if cap := 200 + len("\n… (summary capped; read memory://root/MEMORY.md for the rest)"); len(summary) > cap {
		t.Fatalf("MEMORY.md %d chars over cap %d", len(summary), cap)
	}
	// The injection path keeps its own lesson cap.
	injected := b4.Summary()
	i := strings.Index(injected, "Lessons:")
	if i < 0 {
		t.Fatalf("lessons missing from the injected block: %q", injected)
	}
	// newline-led "- " rows after the Lessons: header (the header itself is
	// not one).
	if n := strings.Count(injected[i:], "\n- "); n != 2 {
		t.Fatalf("injected lessons = %d, want the LessonCap of 2", n)
	}
}

// TestPipelineSkipsSessionsWithoutUserTurns: a session that only ever holds
// assistant output (an aborted or fully synthetic child) is never extracted.
func TestPipelineSkipsSessionsWithoutUserTurns(t *testing.T) {
	stub := newStubModel()
	p, _, dataDir := newTestPipeline(t, stub.complete, nil)
	path := session.SessionFilePath(dataDir, "/tmp/proj", time.Now(), "ffffffff-0000-0000-0000-00000000000b")
	st := session.OpenMem("/tmp/proj", "fixture")
	if _, err := st.EnsureOnDisk(path, session.Options{}); err != nil {
		t.Fatal(err)
	}
	appendMsg(t, st, ai.RoleAssistant, "no user turn here")
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	res, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Extracted != 0 || res.Candidates != 0 {
		t.Fatalf("turnless session extracted: %+v", res)
	}
	if got := len(stub.calls()); got != 0 {
		t.Fatalf("model called %d times for a turnless session", got)
	}
}

// TestPipelineOffIsInert: no seam, no data dir, or an off backend means the
// pipeline does nothing at all (and the session-end hook can call it blindly).
func TestPipelineOffIsInert(t *testing.T) {
	dataDir := t.TempDir()
	cases := []struct {
		name string
		p    *Pipeline
	}{
		{"nil", nil},
		{"no complete", &Pipeline{Backend: &Backend{Dir: filepath.Join(dataDir, "memory")}, DataDir: dataDir}},
		{"no data dir", &Pipeline{Backend: &Backend{Dir: filepath.Join(dataDir, "memory")}, Complete: newStubModel().complete}},
		{"backend off", &Pipeline{DataDir: dataDir, Complete: newStubModel().complete}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := tc.p.Run(context.Background())
			if err != nil || res != (Result{}) {
				t.Fatalf("off pipeline = %+v, %v", res, err)
			}
			tc.p.StartBackground(context.Background()) // must not panic or block
		})
	}
}

// TestPipelineMalformedRepliesKeepTheWatermark: a garbled model reply is
// reported, skipped, and retried next run instead of poisoning the store.
func TestPipelineMalformedRepliesKeepTheWatermark(t *testing.T) {
	var errs []error
	model := func(_ context.Context, prompt string) (string, error) {
		if strings.Contains(prompt, "New findings:") {
			return `not json at all`, nil
		}
		return `also not json`, nil
	}
	p, b, dataDir := newTestPipeline(t, model, func(p *Pipeline) {
		p.OnError = func(err error) { errs = append(errs, err) }
	})
	writeSession(t, dataDir, "/tmp/proj", "aaaaaaaa-0000-0000-0000-00000000000c", false, "a turn")

	res, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Extracted != 0 || len(errs) != 1 {
		t.Fatalf("malformed extraction = %+v, errs=%v", res, errs)
	}
	if raw, _ := b.Read(mem("root/MEMORY.md")); raw != "" {
		t.Fatalf("garbage reached MEMORY.md: %q", raw)
	}
	wm := p.loadWatermark()
	if len(wm.Sessions) != 0 {
		t.Fatalf("watermark advanced past a failed session: %+v", wm.Sessions)
	}

	// A good reply on the retry lands.
	stub := newStubModel()
	p.Complete = stub.complete
	res, err = p.Run(context.Background())
	if err != nil {
		t.Fatalf("retry run: %v", err)
	}
	if res.Extracted != 1 {
		t.Fatalf("retry did not extract: %+v", res)
	}
}

// TestPipelineStubDrivesPhaseTwoPrompt: the consolidation prompt carries the
// current MEMORY.md and the recent lessons, so the model merges rather than
// overwrites from nothing.
func TestPipelineStubDrivesPhaseTwoPrompt(t *testing.T) {
	stub := newStubModel()
	p, b, dataDir := newTestPipeline(t, stub.complete, nil)
	if err := b.SaveLesson("existing lesson", "seed"); err != nil {
		t.Fatal(err)
	}
	writeSession(t, dataDir, "/tmp/proj", "aaaaaaaa-0000-0000-0000-00000000000d", false, "a turn")
	if _, err := p.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	prompts := stub.calls()
	if len(prompts) != 2 {
		t.Fatalf("model calls = %d, want 2", len(prompts))
	}
	if !strings.Contains(prompts[1], "existing lesson") {
		t.Fatalf("consolidation prompt lacks the recent lessons: %q", prompts[1])
	}
	if !strings.Contains(prompts[1], "Current MEMORY.md") {
		t.Fatalf("consolidation prompt lacks the summary slot: %q", prompts[1])
	}
}
