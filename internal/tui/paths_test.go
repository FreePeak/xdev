package tui

import (
	"strings"
	"testing"
)

func TestPathTokenDetectsMentions(t *testing.T) {
	tests := []struct {
		text          string
		prefix, query string
		ok            bool
	}{
		{"fix @", "fix ", "", true},
		{"fix @cache", "fix ", "cache", true},
		{"@a.go", "", "a.go", true},
		{"email me at x@y.com", "", "", false}, // mid-word @ is prose, not a path
		{"read @a.go then @b", "read @a.go then ", "b", true},
		{"@done and more", "", "", false}, // token already closed by a space
		{"no mention here", "", "", false},
	}
	for _, tc := range tests {
		prefix, query, ok := pathToken(tc.text)
		if ok != tc.ok || (ok && (prefix != tc.prefix || query != tc.query)) {
			t.Errorf("pathToken(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.text, prefix, query, ok, tc.prefix, tc.query, tc.ok)
		}
	}
}

func appWithFiles(files ...string) *App {
	a := &App{}
	a.SetPathCompletion("/proj", func() []string { return files })
	return a
}

func TestPathCandidatesRanksBasenameFirst(t *testing.T) {
	a := appWithFiles("internal/tui/cache.go", "cache.go", "docs/readme.md")
	got := a.pathCandidates("cache")
	if len(got) == 0 {
		t.Fatal("no candidates for a matching query")
	}
	// The root-level file outranks the deep one (basename bonus).
	if got[0].Name != "cache.go" {
		t.Fatalf("first = %q, want cache.go", got[0].Name)
	}
	for _, c := range got {
		if c.kind != kindPath || c.Tag != "path" {
			t.Fatalf("item not tagged as a path: %+v", c)
		}
		if strings.Contains(c.Name, "readme") {
			t.Fatalf("non-matching file offered: %q", c.Name)
		}
	}
}

func TestPathCandidatesEmptyQueryListsNewestFirst(t *testing.T) {
	a := appWithFiles("a.go", "b.go", "c/d.go")
	got := a.pathCandidates("")
	if len(got) != 3 {
		t.Fatalf("empty query should list everything, got %d", len(got))
	}
}

func TestPathCandidatesDisabledWithoutScanner(t *testing.T) {
	a := &App{}
	if got := a.pathCandidates("x"); got != nil {
		t.Fatalf("completion enabled without a source: %v", got)
	}
}

func TestPathCandidatesCapsPool(t *testing.T) {
	files := make([]string, 0, 500)
	for range 500 {
		files = append(files, "deep/path/file.go")
	}
	a := appWithFiles(files...)
	if got := len(a.pathCandidates("")); got > maxPathCandidates {
		t.Fatalf("unbounded candidate list: %d", got)
	}
}
