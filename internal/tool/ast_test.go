package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// astFixture writes a small Go file and returns its directory.
func astFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := "package main\n\nfunc hello(name string) string { return \"hi \" + name }\n"
	if err := os.WriteFile(filepath.Join(dir, "s.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func requireASTGrep(t *testing.T) {
	t.Helper()
	if _, err := astGrepBinary(); err != nil {
		t.Skip("ast-grep not installed")
	}
}

func TestASTGrepFindsStructuralMatch(t *testing.T) {
	requireASTGrep(t)
	dir := astFixture(t)
	g := &ASTGrepTool{CWD: dir}
	args, _ := json.Marshal(map[string]any{
		"pattern": "func $NAME($$$ARGS) $RET { $$$BODY }",
		"path":    dir,
		"lang":    "go",
	})
	res, err := g.Execute(context.Background(), args)
	if err != nil || res.IsError {
		t.Fatalf("res = %+v err = %v", res, err)
	}
	if !strings.Contains(res.Text, "s.go:3") || !strings.Contains(res.Text, "func hello") {
		t.Fatalf("unexpected output: %q", res.Text)
	}
}

func TestASTGrepNoMatchIsNotAnError(t *testing.T) {
	requireASTGrep(t)
	dir := astFixture(t)
	g := &ASTGrepTool{CWD: dir}
	args, _ := json.Marshal(map[string]any{"pattern": "func neverMatchesAnything($X)", "path": dir, "lang": "go"})
	res, err := g.Execute(context.Background(), args)
	if err != nil || res.IsError {
		t.Fatalf("no-match must be an answer, not a failure: %+v err=%v", res, err)
	}
	if res.Text != "no matches" {
		t.Fatalf("text = %q", res.Text)
	}
}

func TestASTGrepRejectsEmptyPattern(t *testing.T) {
	res, _ := (&ASTGrepTool{CWD: t.TempDir()}).Execute(context.Background(), json.RawMessage(`{"pattern":"  "}`))
	if !res.IsError || !strings.Contains(res.Text, "pattern is required") {
		t.Fatalf("res = %+v", res)
	}
}

func TestASTEditStagesThenApplies(t *testing.T) {
	requireASTGrep(t)
	dir := astFixture(t)
	e := &ASTEditTool{CWD: dir}
	args, _ := json.Marshal(map[string]any{
		"pattern": "func hello($X string) string",
		"rewrite": "func greet($X string) string",
		"paths":   []string{dir},
		"lang":    "go",
	})

	// Staged: nothing on disk changes.
	res, err := e.Execute(context.Background(), args)
	if err != nil || res.IsError {
		t.Fatalf("stage: %+v err=%v", res, err)
	}
	if !strings.Contains(res.Text, "staged") || !strings.Contains(res.Text, "func greet") {
		t.Fatalf("stage output = %q", res.Text)
	}
	before := readFile(t, filepath.Join(dir, "s.go"))
	if !strings.Contains(before, "func hello") {
		t.Fatalf("staged run must not write: %q", before)
	}

	// Applied: the file changes.
	applyArgs, _ := json.Marshal(map[string]any{
		"pattern": "func hello($X string) string",
		"rewrite": "func greet($X string) string",
		"paths":   []string{dir},
		"lang":    "go",
		"apply":   true,
	})
	res, err = e.Execute(context.Background(), applyArgs)
	if err != nil || res.IsError {
		t.Fatalf("apply: %+v err=%v", res, err)
	}
	after := readFile(t, filepath.Join(dir, "s.go"))
	if !strings.Contains(after, "func greet") || strings.Contains(after, "func hello") {
		t.Fatalf("apply did not rewrite:\n%s", after)
	}
}

// TestASTEditApplyInvalidatesScanCache pins the cache contract for the
// out-of-band writer: an in-place AST rewrite must not leave a stale
// listing behind.
func TestASTEditApplyInvalidatesScanCache(t *testing.T) {
	requireASTGrep(t)
	dir := astFixture(t)
	glob := &GlobTool{CWD: dir}
	gargs, _ := json.Marshal(map[string]any{"pattern": "**/*.go", "path": dir})
	if res, err := glob.Execute(context.Background(), gargs); err != nil || res.IsError {
		t.Fatalf("glob: %+v err=%v", res, err)
	}
	e := &ASTEditTool{CWD: dir}
	applyArgs, _ := json.Marshal(map[string]any{
		"pattern": "func hello($X string) string",
		"rewrite": "func hi($X string) string",
		"paths":   []string{filepath.Join(dir, "s.go")},
		"lang":    "go",
		"apply":   true,
	})
	if res, err := e.Execute(context.Background(), applyArgs); err != nil || res.IsError {
		t.Fatalf("apply: %+v err=%v", res, err)
	}
	// A fresh file created after the rewrite is visible without TTL wait.
	if err := os.WriteFile(filepath.Join(dir, "added.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	SharedFSCache().Invalidate(filepath.Join(dir, "added.go"))
	res, err := glob.Execute(context.Background(), gargs)
	if err != nil || res.IsError {
		t.Fatalf("glob: %+v err=%v", res, err)
	}
	if !strings.Contains(res.Text, "added.go") {
		t.Fatalf("stale listing after AST apply: %q", res.Text)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
