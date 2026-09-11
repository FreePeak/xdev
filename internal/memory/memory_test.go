package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mem builds a memory:// URL at runtime: the literal is rewritten by the
// harness's internal-URI expansion, so tests must not embed it.
func mem(path string) string { return "memory:" + "//" + path }

func newTestBackend(t *testing.T) *Backend {
	t.Helper()
	return &Backend{Dir: filepath.Join(t.TempDir(), "memory")}
}

func TestOffBackendIsInert(t *testing.T) {
	var b *Backend
	if !b.Off() {
		t.Fatal("nil backend must report off")
	}
	if got := b.Summary(); got != "" {
		t.Fatalf("nil summary = %q", got)
	}
	if got := b.GuidanceBlock(); got != "" {
		t.Fatalf("nil guidance = %q", got)
	}
	if _, err := b.Read(mem("root")); err == nil {
		t.Fatal("nil backend read must error")
	}
	if err := b.SaveLesson("x", ""); err == nil {
		t.Fatal("nil backend save must error")
	}
}

func TestSaveLessonAndSummaryInjection(t *testing.T) {
	b := newTestBackend(t)
	if err := b.SaveLesson("run gofmt before committing", "commit hygiene"); err != nil {
		t.Fatal(err)
	}
	if err := b.SaveLesson("prefer table tests", ""); err != nil {
		t.Fatal(err)
	}
	sum := b.Summary()
	if !strings.Contains(sum, "gofmt") || !strings.Contains(sum, "context") {
		t.Fatalf("summary = %q", sum)
	}
	block := b.GuidanceBlock()
	if !strings.Contains(block, "# Memory Guidance") || !strings.Contains(block, "not authoritative") {
		t.Fatalf("guidance block = %q", block)
	}
	// The raw lesson file is the source the prompt block is derived from.
	if raw, _ := b.Read(mem("root/learned.md")); !strings.Contains(raw, "prefer table tests") {
		t.Fatalf("learned.md = %q", raw)
	}
}

func TestSummaryCapMarksTruncation(t *testing.T) {
	b := newTestBackend(t)
	if err := b.Ensure(); err != nil {
		t.Fatal(err)
	}
	var long strings.Builder
	for i := 0; i < 500; i++ {
		long.WriteString("line of memory text here\n")
	}
	if err := b.WriteSummary(long.String()); err != nil {
		t.Fatal(err)
	}
	b.SummaryCapChars = 200
	got := b.Summary()
	if len(got) > 400 || !strings.Contains(got, "capped") {
		t.Fatalf("cap not applied: %q", got)
	}
}

func TestReadUnknownPathErrors(t *testing.T) {
	b := newTestBackend(t)
	for _, uri := range []string{mem("root/nope.md"), mem("other"), "http://x"} {
		if _, err := b.Read(uri); err == nil {
			t.Fatalf("%q must error", uri)
		}
	}
}

func TestClearRemovesBothFiles(t *testing.T) {
	b := newTestBackend(t)
	if err := b.SaveLesson("a", ""); err != nil {
		t.Fatal(err)
	}
	if err := b.WriteSummary("summary"); err != nil {
		t.Fatal(err)
	}
	if err := b.Clear(); err != nil {
		t.Fatal(err)
	}
	if s := b.Summary(); s != "" {
		t.Fatalf("summary after clear = %q", s)
	}
	if _, err := os.Stat(filepath.Join(b.Dir, "learned.md")); !os.IsNotExist(err) {
		t.Fatal("learned.md must be gone")
	}
}

func TestReadMissingReturnsPlaceholder(t *testing.T) {
	b := newTestBackend(t)
	got, err := b.Read(mem("root"))
	if err != nil || !strings.Contains(got, "no memories") {
		t.Fatalf("empty read = %q, %v", got, err)
	}
}

func TestLessonCapKeepsNewest(t *testing.T) {
	b := newTestBackend(t)
	for _, l := range []string{"old lesson", "middle lesson", "new lesson"} {
		if err := b.SaveLesson(l, ""); err != nil {
			t.Fatal(err)
		}
	}
	b.LessonCap = 2
	sum := b.Summary()
	if strings.Contains(sum, "old lesson") {
		t.Fatalf("oldest lesson must be dropped: %q", sum)
	}
	if !strings.Contains(sum, "new lesson") {
		t.Fatalf("newest lesson must survive: %q", sum)
	}
}

func TestLearnToolStoresLessonThenSkill(t *testing.T) {
	b := newTestBackend(t)
	skillsDir := filepath.Join(t.TempDir(), "managed-skills")
	lt := &LearnTool{Backend: b, SkillsDir: skillsDir}
	res, err := lt.Execute(context.Background(), json.RawMessage(`{
		"memory": "always run gofmt",
		"context": "CI failed on formatting",
		"skill": {"action":"create","name":"gofmt-first","description":"format Go before committing","body":"Run gofmt -w ."}
	}`))
	if err != nil || res.IsError {
		t.Fatalf("learn = %q err=%v", res.Text, err)
	}
	if got := b.Summary(); !strings.Contains(got, "always run gofmt") {
		t.Fatalf("lesson not stored: %q", got)
	}
	skillPath := filepath.Join(skillsDir, "gofmt-first", "SKILL.md")
	raw, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("skill not written: %v", err)
	}
	if !strings.Contains(string(raw), "description: format Go before committing") {
		t.Fatalf("skill frontmatter = %q", raw)
	}
}

func TestLearnToolSkillFailureKeepsLesson(t *testing.T) {
	b := newTestBackend(t)
	lt := &LearnTool{Backend: b, SkillsDir: filepath.Join(t.TempDir(), "sk")}
	// create over an existing skill fails; the lesson must still land.
	args := `{"memory":"lesson one","skill":{"action":"create","name":"dup","description":"d","body":"b"}}`
	if res, _ := lt.Execute(context.Background(), json.RawMessage(args)); res.IsError {
		t.Fatalf("first create failed: %q", res.Text)
	}
	res, _ := lt.Execute(context.Background(), json.RawMessage(args))
	if res.IsError {
		t.Fatalf("skill conflict must be a partial success, not an error: %q", res.Text)
	}
	if !strings.Contains(res.Text, "not written") {
		t.Fatalf("partial success must say so: %q", res.Text)
	}
	if got := b.Summary(); strings.Count(got, "lesson one") != 2 {
		t.Fatalf("both lessons must be stored: %q", got)
	}
}

func TestLearnToolRejectsBadSkillName(t *testing.T) {
	b := newTestBackend(t)
	lt := &LearnTool{Backend: b, SkillsDir: t.TempDir()}
	res, _ := lt.Execute(context.Background(), json.RawMessage(`{
		"memory":"x","skill":{"action":"create","name":"Bad Name","description":"d","body":"b"}}`))
	if !res.IsError && !strings.Contains(res.Text, "not written") {
		t.Fatalf("bad name must be reported: %q", res.Text)
	}
}

func TestLearnToolRequiresMemory(t *testing.T) {
	lt := &LearnTool{Backend: newTestBackend(t)}
	res, _ := lt.Execute(context.Background(), json.RawMessage(`{"memory":""}`))
	if !res.IsError {
		t.Fatalf("empty lesson must error: %q", res.Text)
	}
}
