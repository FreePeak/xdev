package tool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fsToolArgs marshals tool arguments for Execute calls in these tests.
func fsToolArgs(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSnapshotReadLines(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		content string
		want    []string
	}{
		{"empty", "", []string{}},
		{"no trailing newline", "a", []string{"a"}},
		{"trailing newline", "a\nb\n", []string{"a", "b"}},
		{"inner empty line", "a\n\nb", []string{"a", "", "b"}},
		{"double trailing newline", "a\n\n", []string{"a", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".txt")
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := ReadLines(path)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ReadLines = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestSnapshotWriteLinesAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	if err := WriteLinesAtomic(path, []string{"one", "", "two"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "one\n\ntwo\n" {
		t.Fatalf("file = %q, want %q", data, "one\n\ntwo\n")
	}

	// Mode preservation on rewrite.
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteLinesAtomic(path, []string{"x"}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600 preserved", st.Mode().Perm())
	}
}

func TestSnapshotRenderWindow(t *testing.T) {
	lines := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}

	// Window omitting lines at both edges: ellipsis rows mark them.
	got := RenderWindow(lines, 5, 2)
	want := "…\n3:c\n4:d\n5:e\n6:f\n7:g\n…"
	if got != want {
		t.Fatalf("both edges = %q, want %q", got, want)
	}

	// Window omitting only the top edge.
	got = RenderWindow(lines, 2, 2)
	want = "1:a\n2:b\n3:c\n4:d\n…"
	if got != want {
		t.Fatalf("bottom gap = %q, want %q", got, want)
	}

	// Window omitting only the bottom edge.
	got = RenderWindow(lines, 9, 2)
	want = "…\n7:g\n8:h\n9:i\n10:j"
	if got != want {
		t.Fatalf("top gap = %q, want %q", got, want)
	}

	// Window covering the whole file: no ellipsis anywhere.
	got = RenderWindow(lines, 5, 5)
	want = "1:a\n2:b\n3:c\n4:d\n5:e\n6:f\n7:g\n8:h\n9:i\n10:j"
	if got != want {
		t.Fatalf("full coverage = %q, want %q", got, want)
	}

	// Clamping: center beyond EOF and file shorter than radius.
	if got := RenderWindow([]string{"x"}, 99, 3); got != "1:x" {
		t.Fatalf("clamped = %q, want %q", got, "1:x")
	}
	if got := RenderWindow(nil, 1, 3); got != "" {
		t.Fatalf("empty = %q, want empty", got)
	}
}

func TestSnapshotResolvePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := resolvePath(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "f.txt") || !filepath.IsAbs(got) {
		t.Fatalf("resolvePath = %q, want absolute path ending in f.txt", got)
	}
	// Missing file still resolves to an absolute path.
	missing := filepath.Join(dir, "nope.txt")
	got, err = resolvePath(missing)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("missing resolvePath = %q, want absolute", got)
	}
}
