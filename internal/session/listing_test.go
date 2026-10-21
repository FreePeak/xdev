package session

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
)

func TestBlobPutGetRoundTrip(t *testing.T) {
	bs := NewBlobStore(t.TempDir())
	data := []byte("some image payload \x00\x01\x02")
	ref, err := bs.Put(data)
	if err != nil {
		t.Fatal(err)
	}
	want := "blob:sha256:"
	if !strings.HasPrefix(ref, want) {
		t.Fatalf("ref = %q, want prefix %q", ref, want)
	}
	sum := sha256.Sum256(data)
	if ref != want+hex.EncodeToString(sum[:]) {
		t.Fatalf("ref = %q, want content-addressed %q", ref, want+hex.EncodeToString(sum[:]))
	}
	got, err := bs.Get(ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatal("round-trip mismatch")
	}
	if !bs.Exists(ref) {
		t.Fatal("Exists = false after Put")
	}
	if bs.Exists(want + strings.Repeat("0", 64)) {
		t.Fatal("Exists true for missing blob")
	}
}

func TestBlobPutIdempotent(t *testing.T) {
	bs := NewBlobStore(t.TempDir())
	ref1, err := bs.Put([]byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	ref2, err := bs.Put([]byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	if ref1 != ref2 {
		t.Fatalf("refs differ: %q vs %q", ref1, ref2)
	}
	entries, _ := os.ReadDir(bs.Root())
	if len(entries) != 1 {
		t.Fatalf("blob dir has %d files, want 1", len(entries))
	}
}

func TestBlobRefValidation(t *testing.T) {
	bs := NewBlobStore(t.TempDir())
	if _, err := bs.Get("not-a-ref"); err == nil {
		t.Fatal("garbage ref accepted")
	}
	if _, err := bs.Get("blob:sha256:zz"); err == nil {
		t.Fatal("short ref accepted")
	}
	if _, err := bs.Get("blob:sha256:" + strings.Repeat("g", 64)); err == nil {
		t.Fatal("non-hex digest accepted")
	}
	// Traversal attempt must be rejected by hex validation.
	if _, err := bs.Get("blob:sha256:" + strings.Repeat("..", 32)); err == nil {
		t.Fatal("traversal ref accepted")
	}
	if bs.Exists("blob:sha256:zz") {
		t.Fatal("Exists true for invalid ref")
	}
}

func TestBlobTempRenameNoLeftovers(t *testing.T) {
	root := t.TempDir()
	bs := &BlobStore{root: root}
	if _, err := bs.Put([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".blob-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

// --- listing ---

func writeSession(t *testing.T, dir, bucket, name, title, cwd string) string {
	t.Helper()
	d := filepath.Join(dir, "sessions", bucket)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	slot := MarshalTitleSlot(title, TitleSourceAuto, slotTS)
	hdr := MarshalHeader(SessionHeader{Version: 3, ID: "id-" + name, Timestamp: ts0, CWD: cwd, Title: title, TitleSource: TitleSourceAuto})
	body := MarshalMust(t, userMsg("11111111", "", "turn"))
	p := filepath.Join(d, name)
	if err := os.WriteFile(p, concat(slot, hdr, append(body, '\n')), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestListFindsBothSessionsSorted(t *testing.T) {
	dir := t.TempDir()
	pOld := writeSession(t, dir, "-tmp-proj", "2026-01-01T00-00-00.000Z_a.jsonl", "older one", "/tmp/proj")
	pNew := writeSession(t, dir, "-tmp-proj", "2026-06-01T00-00-00.000Z_b.jsonl", "newer two", "/tmp/proj")
	// Force distinct mtimes (tempdir writes may share a timestamp).
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(pNew, future, future); err != nil {
		t.Fatal(err)
	}

	metas, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 2 {
		t.Fatalf("listed %d, want 2", len(metas))
	}
	if metas[0].Title != "newer two" || metas[1].Title != "older one" {
		t.Fatalf("sort by ModTime desc broken: %+v", metas)
	}
	if metas[0].Path != pNew || metas[1].Path != pOld {
		t.Fatal("paths mixed up")
	}
	for _, m := range metas {
		if m.ID == "" || m.CWD != "/tmp/proj" || m.SizeBytes == 0 || m.ModTime.IsZero() {
			t.Fatalf("incomplete meta: %+v", m)
		}
		if m.Timestamp.IsZero() {
			t.Fatal("header timestamp lost")
		}
	}
}

func TestListIgnoresNonSessionFiles(t *testing.T) {
	dir := t.TempDir()
	bucket := filepath.Join(dir, "sessions", "-x")
	if err := os.MkdirAll(bucket, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bucket, "notes.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bucket, "junk.jsonl"), []byte("not a session\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSession(t, dir, "-x", "ok.jsonl", "good", "/x")
	metas, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 || metas[0].Title != "good" {
		t.Fatalf("metas = %+v", metas)
	}
}

func TestListMissingDir(t *testing.T) {
	metas, err := List(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("missing dataDir must be empty, not error: %v", err)
	}
	if len(metas) != 0 {
		t.Fatalf("metas = %d", len(metas))
	}
}

func TestEncodeCWDBucket(t *testing.T) {
	cases := map[string]string{
		"/Users/x/proj":      "-Users-x-proj",
		"/tmp/scratch":       "-tmp-scratch",
		"/":                  "-",
		"/Users/x/proj/sub/": "-Users-x-proj-sub", // trailing slash cleaned
	}
	for in, want := range cases {
		if got := EncodeCWDBucket(in); got != want {
			t.Errorf("EncodeCWDBucket(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSessionFilePathShape(t *testing.T) {
	now := time.Date(2026, 9, 7, 3, 20, 45, 0, time.UTC)
	p := SessionFilePath("/data", "/Users/x/proj", now, "abcd1234")
	want := "/data/sessions/-Users-x-proj/2026-09-07T03-20-45.000Z_abcd1234.jsonl"
	if p != want {
		t.Fatalf("path = %q, want %q", p, want)
	}
}

func TestListReadsOnlyPrefix(t *testing.T) {
	// A session with a huge body must still list fast from the 4 KiB prefix;
	// verify by checking a file whose tail contains data we do NOT parse.
	dir := t.TempDir()
	p := writeSession(t, dir, "-x", "big.jsonl", "big title", "/x")
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(strings.Repeat(`{"type":"message","id":"99999999","parentId":"11111111","timestamp":"2026-09-07T03:20:45.220Z","message":{"role":"user","content":[{"type":"text","text":"padding padding padding"}]}}`+"\n", 2000))
	f.Close()
	metas, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 || metas[0].Title != "big title" || metas[0].ID != "id-big.jsonl" {
		t.Fatalf("prefix parse failed: %+v", metas)
	}
}

func TestStatCacheInvalidation(t *testing.T) {
	dir := t.TempDir()
	cache := newStatCache()
	p := writeSession(t, dir, "-x", "s.jsonl", "before", "/x")

	m1, err := listWithCache(dir, cache)
	if err != nil || len(m1) != 1 || m1[0].Title != "before" {
		t.Fatalf("first list: %+v %v", m1, err)
	}
	// Rewrite the title; stat changes → cache must invalidate.
	os.WriteFile(p, concat(MarshalTitleSlot("after", TitleSourceManual, slotTS),
		MarshalHeader(SessionHeader{Version: 3, ID: "id-s", Timestamp: ts0, CWD: "/x", Title: "after", TitleSource: TitleSourceManual})), 0o644)
	future := time.Now().Add(2 * time.Hour)
	os.Chtimes(p, future, future)

	m2, err := listWithCache(dir, cache)
	if err != nil || len(m2) != 1 || m2[0].Title != "after" {
		t.Fatalf("cached list returned stale title: %+v %v", m2, err)
	}
}

func TestScanPathsParallelBounded(t *testing.T) {
	dir := t.TempDir()
	for i := range 30 {
		writeSession(t, dir, "-x", time.Now().Add(time.Duration(i)*time.Second).Format("2006-01-02T15-04-05.000Z")+"_"+string(rune('a'+i%26))+string(rune('a'+i/26))+".jsonl", "t", "/x")
	}
	cache := newStatCache()
	var paths []string
	bucket := filepath.Join(dir, "sessions", "-x")
	entries, _ := os.ReadDir(bucket)
	for _, e := range entries {
		paths = append(paths, filepath.Join(bucket, e.Name()))
	}
	metas := scanPaths(paths, cache)
	if len(metas) != 30 {
		t.Fatalf("scanned %d, want 30", len(metas))
	}
}

// TestListRecordsParentSession: children (subagent/fork headers carrying
// parentSession) are visible to callers so resume paths can skip them.
func TestListRecordsParentSession(t *testing.T) {
	dir := t.TempDir()
	bucket := filepath.Join(dir, "sessions", EncodeCWDBucket("/proj"))
	if err := os.MkdirAll(bucket, 0o755); err != nil {
		t.Fatal(err)
	}
	root := OpenMem("/proj", "user session")
	rootPath, err := root.EnsureOnDisk(filepath.Join(bucket, "root.jsonl"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	child := OpenMem("/proj", "subagent: worker")
	if _, err := child.EnsureOnDisk(filepath.Join(bucket, "child.jsonl"),
		Options{ParentSession: root.ID()}); err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}

	metas, err := listWithCache(dir, newStatCache())
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]SessionMeta{}
	for _, m := range metas {
		byPath[m.Path] = m
	}
	if got := byPath[rootPath].ParentSession; got != "" {
		t.Fatalf("root parentSession = %q, want empty", got)
	}
	var childMeta SessionMeta
	for _, m := range metas {
		if strings.Contains(m.Path, "child.jsonl") {
			childMeta = m
		}
	}
	if childMeta.ID != child.ID() {
		t.Fatalf("child meta not found: %+v", metas)
	}
	if childMeta.ParentSession != root.ID() {
		t.Fatalf("child parentSession = %q, want %q", childMeta.ParentSession, root.ID())
	}
}

// TestListDistinguishesForkFromSubagent pins the resume-path contract:
// forks carry parentSession but stay "auto" (user sessions ARE resumable);
// subagent children stamp titleSource "subagent" and must be skipped.
func TestListDistinguishesForkFromSubagent(t *testing.T) {
	dir := t.TempDir()
	bucket := filepath.Join(dir, "sessions", EncodeCWDBucket("/proj"))
	if err := os.MkdirAll(bucket, 0o755); err != nil {
		t.Fatal(err)
	}
	mk := func(name string, cfg func(*Store)) string {
		st := OpenMem("/proj", name)
		cfg(st)
		path, err := st.EnsureOnDisk(filepath.Join(bucket, name+".jsonl"), Options{ParentSession: "parent-id"})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		return path
	}
	forkPath := mk("fork", func(st *Store) {})
	childPath := mk("child", func(st *Store) { st.SetTitleSourceSubagent() })

	metas, err := listWithCache(dir, newStatCache())
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]SessionMeta{}
	for _, m := range metas {
		byPath[m.Path] = m
	}
	if got := byPath[forkPath].TitleSource; got != TitleSourceAuto {
		t.Fatalf("fork titleSource = %q, want auto (forks are resumable)", got)
	}
	if got := byPath[forkPath].ParentSession; got != "parent-id" {
		t.Fatalf("fork parentSession = %q", got)
	}
	if got := byPath[childPath].TitleSource; got != TitleSourceSubagent {
		t.Fatalf("child titleSource = %q, want subagent (must be skipped by resume)", got)
	}
}

// --- title / preview fallbacks (omp rules 35, 45) ---

// writePreviewSession lays down a session file with the given title slot and
// body lines, and returns its path.
func writePreviewSession(t *testing.T, dir, name, title string, body ...string) string {
	t.Helper()
	bucket := filepath.Join(dir, "sessions", "-x")
	if err := os.MkdirAll(bucket, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.Write(MarshalTitleSlot(title, TitleSourceAuto, slotTS))
	b.Write(MarshalHeader(SessionHeader{Version: 3, ID: "id-" + name, Timestamp: ts0, CWD: "/x", Title: title, TitleSource: TitleSourceAuto}))
	for _, l := range body {
		b.WriteString(l)
		b.WriteString("\n")
	}
	p := filepath.Join(bucket, name)
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestSanitizeTitleFoldsToOneLine: a title is model- and user-written text,
// so it is folded to one printable line before anything paints it.
func TestSanitizeTitleFoldsToOneLine(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"fix the parser", "fix the parser"},
		{"first line\nsecond line", "first line"},
		{"first line\r\nsecond line", "first line"},
		{"with\x00control\x07chars", "withcontrolchars"},
		{"   padded   ", "padded"},
		{"\n\n", ""},
		{"", ""},
		// A legit unicode title is NOT mangled: the sanitiser drops control
		// characters, not letters. CJK, an emoji and an accented word all
		// survive whole.
		{"修复解析器", "修复解析器"},
		{"ship it 🚀", "ship it 🚀"},
		{"café au lait", "café au lait"},
		{"emoji with a control char 🚀\x00", "emoji with a control char 🚀"},
	} {
		if got := SanitizeTitle(tc.in); got != tc.want {
			t.Errorf("SanitizeTitle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestListPreviewFallsBackToTheFirstMessage: a session with no title still
// has to say what it was about — the first prompt is the next best name.
func TestListPreviewFallsBackToTheFirstMessage(t *testing.T) {
	dir := t.TempDir()
	line := string(MarshalMust(t, userMsg("11111111", "", "why does the parser drop the last block?")))
	writePreviewSession(t, dir, "untitled.jsonl", "", line)

	metas, err := listWithCache(dir, newStatCache())
	if err != nil || len(metas) != 1 {
		t.Fatalf("list: %+v %v", metas, err)
	}
	if metas[0].Title != "" {
		t.Fatalf("title = %q, want empty", metas[0].Title)
	}
	want := "why does the parser drop the last block?"
	if metas[0].Preview != want {
		t.Fatalf("preview = %q, want %q", metas[0].Preview, want)
	}
}

// TestListPreviewFallsBackToUntitled: no title and no prompt at all still
// leaves the row something to say — the session's own creation time, in the
// LOCAL zone (the same zone the row's mtime column is printed in).
func TestListPreviewFallsBackToUntitled(t *testing.T) {
	// A fixed local zone, so the assertion pins the RENDERED clock and the
	// timezone it is rendered in, not the machine's: a UTC rendering would
	// print a different hour and fail here.
	prev := time.Local
	time.Local = time.FixedZone("test", 2*60*60)
	t.Cleanup(func() { time.Local = prev })

	dir := t.TempDir()
	writePreviewSession(t, dir, "bare.jsonl", "")

	metas, err := listWithCache(dir, newStatCache())
	if err != nil || len(metas) != 1 {
		t.Fatalf("list: %+v %v", metas, err)
	}
	// ts0 is the fixture's header timestamp: 03:20:45 UTC, which is 05:20
	// in the fixed +02:00 zone.
	want := "Untitled · " + ts0.In(time.Local).Format("15:04")
	if ts0.In(time.Local).Format("15:04") == ts0.UTC().Format("15:04") {
		t.Fatalf("the fixture cannot tell UTC from local: %s", ts0)
	}
	if metas[0].Preview != want {
		t.Fatalf("preview = %q, want %q (the local clock, not UTC)", metas[0].Preview, want)
	}
}

// TestListPreviewUsesTheCompactionSummary: a session that was compacted
// knows what it was about — the compaction record's one-line summary is the
// preview when there is no title.
func TestListPreviewUsesTheCompactionSummary(t *testing.T) {
	dir := t.TempDir()
	compaction := string(MarshalMust(t, &CompactionEntry{
		Env:          Envelope{Type: TypeCompaction, ID: "c1", ParentID: "", Timestamp: ts0},
		Summary:      ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "the whole plan"}}},
		ShortSummary: "the parser rewrite plan",
	}))
	writePreviewSession(t, dir, "compacted.jsonl", "", compaction)

	metas, err := listWithCache(dir, newStatCache())
	if err != nil || len(metas) != 1 {
		t.Fatalf("list: %+v %v", metas, err)
	}
	if metas[0].Preview != "the parser rewrite plan" {
		t.Fatalf("preview = %q, want the compaction summary", metas[0].Preview)
	}
}

// TestListPreviewPrefersTheTitle: the title still wins — the fallbacks are
// for a session that has none.
func TestListPreviewPrefersTheTitle(t *testing.T) {
	dir := t.TempDir()
	line := string(MarshalMust(t, userMsg("11111111", "", "the prompt")))
	writePreviewSession(t, dir, "titled.jsonl", "fix the parser bug", line)

	metas, err := listWithCache(dir, newStatCache())
	if err != nil || len(metas) != 1 {
		t.Fatalf("list: %+v %v", metas, err)
	}
	if metas[0].Preview != "fix the parser bug" {
		t.Fatalf("preview = %q, want the title", metas[0].Preview)
	}
}
