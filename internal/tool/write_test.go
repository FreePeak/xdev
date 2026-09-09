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
