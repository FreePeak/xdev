package tool

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadWholeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := NewReadTool().Execute(t.Context(), fsToolArgs(t, map[string]any{"path": path}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Text)
	}
	if want := "1:alpha\n2:beta\n3:gamma"; res.Text != want {
		t.Fatalf("text = %q, want %q", res.Text, want)
	}
	d := res.Details.(map[string]any)
	if d["totalLines"] != 3 || d["lineCount"] != 3 || d["truncated"] != false {
		t.Fatalf("details = %v", d)
	}
}

func TestReadWindowFooter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lines.txt")
	var sb strings.Builder
	for i := range 10 {
		fmt.Fprintf(&sb, "L%d\n", i+1)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := NewReadTool().Execute(t.Context(), fsToolArgs(t, map[string]any{"path": path, "limit": 4}))
	if err != nil {
		t.Fatal(err)
	}
	want := "1:L1\n2:L2\n3:L3\n4:L4\n[Showing lines 1-4 of 10. Use :5 to continue]"
	if res.Text != want {
		t.Fatalf("text = %q, want %q", res.Text, want)
	}
	d := res.Details.(map[string]any)
	if d["truncated"] != true || d["lineCount"] != 4 || d["totalLines"] != 10 {
		t.Fatalf("details = %v", d)
	}
}

func TestReadOffsetWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lines.txt")
	var sb strings.Builder
	for i := range 10 {
		fmt.Fprintf(&sb, "L%d\n", i+1)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	// Offset window that reaches EOF: real numbering, no footer.
	res, err := NewReadTool().Execute(t.Context(), fsToolArgs(t, map[string]any{"path": path, "offset": 8}))
	if err != nil {
		t.Fatal(err)
	}
	want := "8:L8\n9:L9\n10:L10"
	if res.Text != want {
		t.Fatalf("text = %q, want %q", res.Text, want)
	}
	// Offset window cut mid-file: footer shows the next real line.
	res, err = NewReadTool().Execute(t.Context(), fsToolArgs(t, map[string]any{"path": path, "offset": 3, "limit": 2}))
	if err != nil {
		t.Fatal(err)
	}
	want = "3:L3\n4:L4\n[Showing lines 3-4 of 10. Use :5 to continue]"
	if res.Text != want {
		t.Fatalf("text = %q, want %q", res.Text, want)
	}
}

func TestReadDefaultLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.txt")
	var sb strings.Builder
	for i := range 2001 {
		fmt.Fprintf(&sb, "line %d\n", i+1)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := NewReadTool().Execute(t.Context(), fsToolArgs(t, map[string]any{"path": path}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(res.Text, "[Showing lines 1-2000 of 2001. Use :2001 to continue]") {
		t.Fatalf("missing default-limit footer: %q", res.Text[len(res.Text)-80:])
	}
	if first := strings.SplitN(res.Text, "\n", 2)[0]; first != "1:line 1" {
		t.Fatalf("first line = %q", first)
	}
}

func TestReadLongLineTruncated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "long.txt")
	raw := strings.Repeat("x", 2500) + "\nshort\n"
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := NewReadTool().Execute(t.Context(), fsToolArgs(t, map[string]any{"path": path}))
	if err != nil {
		t.Fatal(err)
	}
	want := "1:" + strings.Repeat("x", 2000) + "…\n2:short"
	if res.Text != want {
		t.Fatalf("text mismatch (len %d)", len(res.Text))
	}
	// Raw bytes on disk are untouched.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != raw {
		t.Fatal("read tool must not rewrite the file")
	}
}

func TestReadBinaryFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bin.dat")
	if err := os.WriteFile(path, []byte("ABC\x00DEF\x00more bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := NewReadTool().Execute(t.Context(), fsToolArgs(t, map[string]any{"path": path}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("binary detection is not an error: %s", res.Text)
	}
	want := fmt.Sprintf("binary file: %s (18 bytes, application/octet-stream)", path)
	if res.Text != want {
		t.Fatalf("text = %q, want %q", res.Text, want)
	}
	if res.Details.(map[string]any)["binary"] != true {
		t.Fatalf("details = %v", res.Details)
	}
}

func TestReadImageFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tiny.gif")
	// Minimal GIF89a header: 6-byte signature + LE16 width(4) + LE16
	// height(2) + flags/bg/aspect.
	gif := []byte{'G', 'I', 'F', '8', '9', 'a', 4, 0, 2, 0, 0, 0, 0}
	if err := os.WriteFile(path, gif, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := NewReadTool().Execute(t.Context(), fsToolArgs(t, map[string]any{"path": path}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Text)
	}
	want := fmt.Sprintf("image file: %s (4x2, 13 bytes)", path)
	if res.Text != want {
		t.Fatalf("text = %q, want %q", res.Text, want)
	}
	if res.Details.(map[string]any)["image"] != true {
		t.Fatalf("details = %v", res.Details)
	}
}

func TestReadMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nope.txt")
	res, err := NewReadTool().Execute(t.Context(), fsToolArgs(t, map[string]any{"path": path}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected IsError for missing file")
	}
	if want := "file not found: " + path; res.Text != want {
		t.Fatalf("text = %q, want %q", res.Text, want)
	}
}

func TestReadDirectoryListing(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := NewReadTool().Execute(t.Context(), fsToolArgs(t, map[string]any{"path": dir}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("directory read should be a helpful error")
	}
	for _, want := range []string{
		"- docs/ ",
		"- main.go 13 B ",
		"Open files directly",
	} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("text missing %q:\n%s", want, res.Text)
		}
	}
	d := res.Details.(map[string]any)
	if d["isDirectory"] != true {
		t.Fatalf("details = %v", d)
	}
}

func TestReadDirectoryOverflow(t *testing.T) {
	dir := t.TempDir()
	for i := range 35 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%02d.txt", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, err := NewReadTool().Execute(t.Context(), fsToolArgs(t, map[string]any{"path": dir}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "... and 5 more") {
		t.Fatalf("missing overflow footer:\n%s", res.Text)
	}
	if got := strings.Count(res.Text, "- f"); got != 30 {
		t.Fatalf("listed %d entries, want 30", got)
	}
}

func TestReadEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := NewReadTool().Execute(t.Context(), fsToolArgs(t, map[string]any{"path": path}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || res.Text != "(empty file)" {
		t.Fatalf("text = %q, isError = %v", res.Text, res.IsError)
	}
	if res.Details.(map[string]any)["totalLines"] != 0 {
		t.Fatalf("details = %v", res.Details)
	}
}
