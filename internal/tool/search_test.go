package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteInvalidatesSharedScanCache pins the producer wiring: a
// write→immediate-glob in the same process must see the new file, well
// inside the 1s TTL. Without Invalidate on the write path the listing
// would be stale.
func TestWriteInvalidatesSharedScanCache(t *testing.T) {
	dir := t.TempDir()
	// Seed the cache through the glob tool.
	glob := &GlobTool{CWD: dir}
	args, _ := json.Marshal(map[string]any{"pattern": "**/*.go", "path": dir})
	if _, err := glob.Execute(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	// Write a file, then glob again immediately.
	w := &WriteTool{}
	wargs, _ := json.Marshal(map[string]any{"path": filepath.Join(dir, "fresh.go"), "content": "package fresh\n"})
	res, err := w.Execute(context.Background(), wargs)
	if err != nil || res.IsError {
		t.Fatalf("write: %+v err=%v", res, err)
	}
	res, err = glob.Execute(context.Background(), args)
	if err != nil || res.IsError {
		t.Fatalf("glob: %+v err=%v", res, err)
	}
	if !strings.Contains(res.Text, "fresh.go") {
		t.Fatalf("glob served a stale listing after write: %q", res.Text)
	}
}

// TestBashInvalidatesSharedScanCache is the same contract via bash.
func TestBashInvalidatesSharedScanCache(t *testing.T) {
	dir := t.TempDir()
	glob := &GlobTool{CWD: dir}
	args, _ := json.Marshal(map[string]any{"pattern": "**/*.txt", "path": dir})
	if _, err := glob.Execute(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	b := NewBashTool(dir)
	bargs, _ := json.Marshal(map[string]any{"command": "touch made-by-bash.txt"})
	if res, err := b.Execute(context.Background(), bargs); err != nil || res.IsError {
		t.Fatalf("bash: %+v err=%v", res, err)
	}
	res, err := glob.Execute(context.Background(), args)
	if err != nil || res.IsError {
		t.Fatalf("glob: %+v err=%v", res, err)
	}
	if !strings.Contains(res.Text, "made-by-bash.txt") {
		t.Fatalf("glob served a stale listing after bash: %q", res.Text)
	}
}

// TestGlobMatchesRecursivePattern covers the ** expansion the stdlib
// cannot express.
func TestGlobMatchesRecursivePattern(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"top.go", "a/b/inner.go", "a/other.txt"} {
		full := filepath.Join(dir, p)
		if err := mkdirFor(full); err != nil {
			t.Fatal(err)
		}
		if err := writeFileFor(full, "x"); err != nil {
			t.Fatal(err)
		}
	}
	glob := &GlobTool{CWD: dir}
	args, _ := json.Marshal(map[string]any{"pattern": "**/*.go", "path": dir})
	res, err := glob.Execute(context.Background(), args)
	if err != nil || res.IsError {
		t.Fatalf("glob: %+v err=%v", res, err)
	}
	for _, want := range []string{"top.go", "inner.go"} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("missing %q in %q", want, res.Text)
		}
	}
	if strings.Contains(res.Text, "other.txt") {
		t.Fatalf("non-matching file returned: %q", res.Text)
	}
}

// TestGrepFindsMatchesAndCaps covers the pure-Go fallback's answer shape.
func TestGrepFindsMatchesAndCaps(t *testing.T) {
	dir := t.TempDir()
	if err := writeFileFor(filepath.Join(dir, "x.go"), "alpha\nbeta\nalpha again\n"); err != nil {
		t.Fatal(err)
	}
	g := &GrepTool{CWD: dir, MaxMatches: 1}
	args, _ := json.Marshal(map[string]any{"pattern": "alpha", "path": dir})
	res, err := g.Execute(context.Background(), args)
	if err != nil || res.IsError {
		t.Fatalf("grep: %+v err=%v", res, err)
	}
	lines := strings.Split(strings.TrimSpace(res.Text), "\n")
	if len(lines) > 2 { // 1 match + the cap note
		t.Fatalf("cap not honored: %q", res.Text)
	}
	if !strings.Contains(res.Text, "x.go:1") {
		t.Fatalf("missing file:line: %q", res.Text)
	}
}

func mkdirFor(path string) error { return os.MkdirAll(filepath.Dir(path), 0o755) }

func writeFileFor(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

// TestGrepAgreesWithAndWithoutRipgrep pins the two paths' semantics: the
// answer must not depend on whether rg happens to be installed. Both must
// report every matching line (rg's old --max-count 1 capped each file at
// one match, so the same tool gave different answers on different boxes).
func TestGrepAgreesWithAndWithoutRipgrep(t *testing.T) {
	// Both path forms: rg echoes the search root back, so a relative
	// argument exercises a different code shape than an absolute one.
	for _, mode := range []string{"abs", "rel"} {
		t.Run(mode, func(t *testing.T) { testGrepPathAgreement(t, mode) })
	}
}

func testGrepPathAgreement(t *testing.T, pathMode string) {
	dir := t.TempDir()
	content := "alpha one\nbeta\nalpha two\nalpha three\n"
	if err := os.WriteFile(filepath.Join(dir, "f.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	searchPath := dir
	if pathMode == "rel" {
		searchPath = "." // relative to the tool's cwd
	}
	args, _ := json.Marshal(map[string]any{"pattern": "alpha", "path": searchPath})

	orig := lookPath
	defer func() { lookPath = orig }()

	// Go fallback.
	lookPath = func(string) (string, error) { return "", os.ErrNotExist }
	g := &GrepTool{CWD: dir}
	fallback, err := g.Execute(context.Background(), args)
	if err != nil || fallback.IsError {
		t.Fatalf("fallback: %+v err=%v", fallback, err)
	}
	// rg fast path (skipped on machines without ripgrep).
	lookPath = orig
	if _, err := lookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	withRG, err := g.Execute(context.Background(), args)
	if err != nil || withRG.IsError {
		t.Fatalf("rg: %+v err=%v", withRG, err)
	}

	// Compare the WHOLE rendered strings the model reads: comparing only
	// path+line prefixes is what let a separator difference slip past.
	if fallback.Text != withRG.Text {
		t.Fatalf("paths disagree\n--- go ---\n%s\n--- rg ---\n%s", fallback.Text, withRG.Text)
	}
	if n := len(strings.Split(strings.TrimSpace(fallback.Text), "\n")); n != 3 {
		t.Fatalf("expected 3 matches, got %d:\n%s", n, fallback.Text)
	}
}
