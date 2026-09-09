package tool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// editFile writes a fresh 5-line fixture and returns its path.
func editFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEditPutReplace(t *testing.T) {
	path := editFile(t, t.TempDir(), "f.txt", "a\nb\nc\nd\ne\n")
	res, err := NewEditTool().Execute(t.Context(), fsToolArgs(t, map[string]any{
		"path": path,
		"ops": []map[string]any{{
			"op":    "PUT",
			"range": map[string]any{"start": 2, "end": 3},
			"lines": []string{"+x1", "+x2", "+x3"},
		}},
	}))
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
	if string(data) != "a\nx1\nx2\nx3\nd\ne\n" {
		t.Fatalf("file = %q", data)
	}
	d := res.Details.(map[string]any)
	if d["linesBefore"] != 5 || d["linesAfter"] != 6 || d["opsApplied"] != 1 {
		t.Fatalf("details = %v", d)
	}
	summary := d["diffSummary"].([]string)
	if len(summary) != 1 || summary[0] != "2-3: +3 lines -2 lines" {
		t.Fatalf("diffSummary = %#v", summary)
	}
}

func TestEditMultiOpRenumbering(t *testing.T) {
	path := editFile(t, t.TempDir(), "f.txt", "a\nb\nc\nd\ne\n")
	res, err := NewEditTool().Execute(t.Context(), fsToolArgs(t, map[string]any{
		"path": path,
		"ops": []map[string]any{
			// Op 1: replace b (line 2) with two lines -> a,x,y,c,d,e.
			{"op": "PUT", "range": map[string]any{"line": 2}, "lines": []string{"+x", "+y"}},
			// Op 2: numbers refer to post-op-1 state; line 4 is now c.
			{"op": "CUT", "range": map[string]any{"line": 4}},
		},
	}))
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
	if string(data) != "a\nx\ny\nd\ne\n" {
		t.Fatalf("file = %q, want renumbered result", data)
	}
	if res.Details.(map[string]any)["opsApplied"] != 2 {
		t.Fatalf("details = %v", res.Details)
	}
}

func TestEditOutOfRangeLeavesFileUntouched(t *testing.T) {
	dir := t.TempDir()
	path := editFile(t, dir, "f.txt", "a\nb\nc\n")
	res, err := NewEditTool().Execute(t.Context(), fsToolArgs(t, map[string]any{
		"path": path,
		"ops": []map[string]any{
			{"op": "PUT", "range": map[string]any{"line": 1}, "lines": []string{"+z"}},
			{"op": "CUT", "range": map[string]any{"line": 5}},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected IsError for out-of-range op")
	}
	if !strings.Contains(res.Text, "op 2") {
		t.Fatalf("error must name the failing op: %s", res.Text)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "a\nb\nc\n" {
		t.Fatalf("file changed despite failed edit: %q", data)
	}
}

func TestEditEmptyBodyDeletes(t *testing.T) {
	path := editFile(t, t.TempDir(), "f.txt", "a\nb\nc\nd\ne\n")
	res, err := NewEditTool().Execute(t.Context(), fsToolArgs(t, map[string]any{
		"path": path,
		"ops": []map[string]any{{
			"op":    "PUT",
			"range": map[string]any{"start": 2, "end": 3},
			"lines": []string{},
		}},
	}))
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
	if string(data) != "a\nd\ne\n" {
		t.Fatalf("file = %q, want b/c deleted", data)
	}
}

func TestEditPlusEscape(t *testing.T) {
	path := editFile(t, t.TempDir(), "f.txt", "one\n")
	res, err := NewEditTool().Execute(t.Context(), fsToolArgs(t, map[string]any{
		"path": path,
		"ops": []map[string]any{{
			"op":    "PUT",
			"range": map[string]any{"line": 1},
			"lines": []string{"++literal-plus", "+plain"},
		}},
	}))
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
	if string(data) != "+literal-plus\nplain\n" {
		t.Fatalf("file = %q", data)
	}
}

func TestEditSnapshotConfirmation(t *testing.T) {
	path := editFile(t, t.TempDir(), "f.txt", "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n")
	res, err := NewEditTool().Execute(t.Context(), fsToolArgs(t, map[string]any{
		"path": path,
		"ops": []map[string]any{{
			"op":    "PUT",
			"range": map[string]any{"line": 3},
			"lines": []string{"+THREE"},
		}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Text)
	}
	// Header is "[path#hash]" and the window centers on the first edit
	// region with 1-based numbering.
	if !strings.HasPrefix(res.Text, "["+path+"#") {
		t.Fatalf("missing snapshot header: %q", res.Text[:min(40, len(res.Text))])
	}
	for _, want := range []string{"\n1:1", "\n3:THREE", "\n6:6", "\n…"} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("snapshot missing %q:\n%s", want, res.Text)
		}
	}
	if strings.Contains(res.Text, "\n7:7") {
		t.Fatalf("window exceeds radius:\n%s", res.Text)
	}
}

func TestEditMVFile(t *testing.T) {
	dir := t.TempDir()
	path := editFile(t, dir, "old.txt", "content\n")
	dest := filepath.Join(dir, "moved", "new.txt")
	res, err := NewEditTool().Execute(t.Context(), fsToolArgs(t, map[string]any{
		"path": path,
		"ops":  []map[string]any{{"op": "MV", "dest": dest}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Text)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("source still exists (err=%v)", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "content\n" {
		t.Fatalf("moved file = %q", data)
	}
	if res.Details.(map[string]any)["resolvedPath"] != dest {
		t.Fatalf("details = %v", res.Details)
	}
	if !strings.HasPrefix(res.Text, "Moved "+path+" to "+dest+"\n["+dest+"#") {
		t.Fatalf("text = %q", res.Text)
	}
}

func TestEditMVAfterLineOps(t *testing.T) {
	dir := t.TempDir()
	path := editFile(t, dir, "old.txt", "a\nb\nc\n")
	dest := filepath.Join(dir, "new.txt")
	res, err := NewEditTool().Execute(t.Context(), fsToolArgs(t, map[string]any{
		"path": path,
		"ops": []map[string]any{
			{"op": "PUT", "range": map[string]any{"line": 2}, "lines": []string{"+B2"}},
			{"op": "MV", "dest": dest},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Text)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "a\nB2\nc\n" {
		t.Fatalf("moved file = %q", data)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("source still exists")
	}
}

func TestEditMVBadDestLeavesFileUntouched(t *testing.T) {
	dir := t.TempDir()
	existingDir := filepath.Join(dir, "subdir")
	if err := os.Mkdir(existingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := editFile(t, dir, "f.txt", "a\n")
	res, err := NewEditTool().Execute(t.Context(), fsToolArgs(t, map[string]any{
		"path": path,
		"ops":  []map[string]any{{"op": "MV", "dest": existingDir}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected IsError for directory destination")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "a\n" {
		t.Fatalf("file changed despite failed MV: %q", data)
	}
}

func TestEditUnknownOp(t *testing.T) {
	path := editFile(t, t.TempDir(), "f.txt", "a\n")
	res, err := NewEditTool().Execute(t.Context(), fsToolArgs(t, map[string]any{
		"path": path,
		"ops":  []map[string]any{{"op": "REM"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "unknown op") {
		t.Fatalf("text = %q, isError = %v", res.Text, res.IsError)
	}
}

func TestEditNoOps(t *testing.T) {
	path := editFile(t, t.TempDir(), "f.txt", "a\n")
	res, err := NewEditTool().Execute(t.Context(), fsToolArgs(t, map[string]any{"path": path, "ops": []map[string]any{}}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected IsError for empty ops")
	}
}
