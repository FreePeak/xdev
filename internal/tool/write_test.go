package tool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteCreatesNestedDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deep", "nested", "new.txt")
	res, err := NewWriteTool().Execute(t.Context(), fsToolArgs(t, map[string]any{"path": path, "content": "one\ntwo\n"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Text)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "one\ntwo\n" {
		t.Fatalf("file = %q", data)
	}
	if want := "Wrote " + path + " (8 bytes, 2 lines)"; res.Text != want {
		t.Fatalf("text = %q, want %q", res.Text, want)
	}
	d := res.Details.(map[string]any)
	if d["created"] != true || d["bytesWritten"] != 8 {
		t.Fatalf("details = %v", d)
	}
	if res.Details.(map[string]any)["priorBytes"] != nil {
		t.Fatal("created file must not report priorBytes")
	}
}

func TestWriteOverwriteExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := NewWriteTool().Execute(t.Context(), fsToolArgs(t, map[string]any{"path": path, "content": "new content"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Text)
	}
	d := res.Details.(map[string]any)
	if d["created"] != false || d["priorBytes"] != int64(10) {
		t.Fatalf("details = %v, want created=false priorBytes=10", d)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new content" {
		t.Fatalf("file = %q", data)
	}
}

func TestWriteNoTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	res, err := NewWriteTool().Execute(t.Context(), fsToolArgs(t, map[string]any{"path": path, "content": "a\nb"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Text)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "a\nb" {
		t.Fatalf("content must be written verbatim, got %q", data)
	}
	if want := "Wrote " + path + " (3 bytes, 2 lines)"; res.Text != want {
		t.Fatalf("text = %q, want %q", res.Text, want)
	}
}

func TestWriteRefusesDirectoryTarget(t *testing.T) {
	dir := t.TempDir()
	res, err := NewWriteTool().Execute(t.Context(), fsToolArgs(t, map[string]any{"path": dir, "content": "x"}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "is a directory") {
		t.Fatalf("text = %q, isError = %v", res.Text, res.IsError)
	}
}

// A write's Details carry the change: every line added for a new file, the
// real before/after for an overwrite. Text keeps the one-line summary the
// model and the session log read.
func TestWriteAttachesUnifiedDiff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")

	res, err := NewWriteTool().Execute(t.Context(), fsToolArgs(t, map[string]any{"path": path, "content": "one\ntwo\n"}))
	if err != nil || res.IsError {
		t.Fatalf("create failed: %v %v", err, res)
	}
	d := res.Details.(map[string]any)
	diff, _ := d["unifiedDiff"].(string)
	if !strings.Contains(diff, "--- /dev/null") || !strings.Contains(diff, "+one") || !strings.Contains(diff, "+two") {
		t.Errorf("created file diff wrong:\n%q", diff)
	}
	if minus, _ := tallyRows(diff); minus != 0 {
		t.Errorf("created file diff shows %d removals:\n%q", minus, diff)
	}
	if res.Text != "Wrote "+path+" (8 bytes, 2 lines)" {
		t.Errorf("Text = %q", res.Text)
	}

	res, err = NewWriteTool().Execute(t.Context(), fsToolArgs(t, map[string]any{"path": path, "content": "one\nTWO\n"}))
	if err != nil || res.IsError {
		t.Fatalf("overwrite failed: %v %v", err, res)
	}
	diff = res.Details.(map[string]any)["unifiedDiff"].(string)
	if !strings.Contains(diff, "-two") || !strings.Contains(diff, "+TWO") {
		t.Errorf("overwrite diff wrong:\n%q", diff)
	}
}

// Rewriting identical content is not a change: no diff, so no painted one.
func TestWriteIdenticalContentHasNoDiff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("same\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := NewWriteTool().Execute(t.Context(), fsToolArgs(t, map[string]any{"path": path, "content": "same\n"}))
	if err != nil || res.IsError {
		t.Fatalf("write failed: %v %v", err, res)
	}
	if d := res.Details.(map[string]any); d["unifiedDiff"] != nil {
		t.Errorf("unchanged write produced a diff: %v", d["unifiedDiff"])
	}
}
