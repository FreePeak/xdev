package tui

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// storeIndex publishes an index synchronously from literal paths — a test that
// only cares about the MENU's answer should not walk a temp tree to get one.
// It sorts the way the walk does, because match() binary-searches.
func storeIndex(idx *pathIndex, paths ...string) {
	hits := make([]pathHit, 0, len(paths))
	for _, p := range paths {
		hits = append(hits, pathHit{lower: strings.ToLower(filepath.Base(p)), path: p})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].lower != hits[j].lower {
			return hits[i].lower < hits[j].lower
		}
		return hits[i].path < hits[j].path
	})
	idx.hits.Store(&hits)
}

// indexTree writes a small repo: a nested file, a hidden one, a vendored one
// and a VCS dir, which is the whole inclusion policy in four lines.
func indexTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for path, body := range map[string]string{
		"main.go":                   "package main\n",
		"internal/tui/paths.go":     "package tui\n",
		"internal/tool/git.go":      "package tool\n",
		"docs/parser.md":            "# parser\n",
		".env":                      "SECRET=1\n",
		"node_modules/dep/index.js": "1\n",
		".git/config":               "[core]\n",
	} {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func hitPaths(hits []pathHit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.path)
	}
	return out
}

// TestIndexPathHitsListsEveryFileUnderRoot: every file, in base-name order —
// the name is what the user types, so it is what the menu sorts on.
func TestIndexPathHitsListsEveryFileUnderRoot(t *testing.T) {
	got := hitPaths(indexPathHits(indexTree(t)))
	want := []string{
		".env", "internal/tool/git.go", "main.go", "docs/parser.md",
		"internal/tui/paths.go",
	}
	if len(got) != len(want) {
		t.Fatalf("index = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("index = %v, want %v", got, want)
		}
	}
}

// TestIndexPathHitsSortedByBaseName: the menu order is the sort order and
// match() binary-searches it, so the two must agree on what "first" means.
func TestIndexPathHitsSortedByBaseName(t *testing.T) {
	hits := indexPathHits(indexTree(t))
	for i := 1; i < len(hits); i++ {
		if hits[i-1].lower > hits[i].lower ||
			(hits[i-1].lower == hits[i].lower && hits[i-1].path > hits[i].path) {
			t.Fatalf("unsorted at %d: %q then %q", i, hits[i-1], hits[i])
		}
	}
}

// TestIndexPathHitsPrunesVCSAndVendor: .git is machine state and never a
// mention; node_modules is a dependency tree 95% of the files on this
// polyrepo — the directory drill still reaches it, the index does not pay
// for it. Hidden and gitignored files ARE indexed: a file you can drill into
// is a file you can name (.env above is the check).
func TestIndexPathHitsPrunesVCSAndVendor(t *testing.T) {
	got := hitPaths(indexPathHits(indexTree(t)))
	for _, rel := range got {
		if strings.HasPrefix(rel, ".git/") {
			t.Fatalf("VCS metadata indexed: %q", rel)
		}
		if strings.Contains(rel, "node_modules/") {
			t.Fatalf("vendored file indexed: %q", rel)
		}
	}
	if !slices.Contains(got, ".env") {
		t.Fatalf("hidden file not indexed: %v", got)
	}
}

func TestIndexPathHitsEmptyRoot(t *testing.T) {
	if got := indexPathHits(""); got != nil {
		t.Fatalf("index of no root = %v", got)
	}
	if got := indexPathHits(filepath.Join(t.TempDir(), "absent")); len(got) != 0 {
		t.Fatalf("index of an absent root = %v", got)
	}
}

// TestPathIndexStartPublishesOffThread is the guarantee #329 was about: start
// returns before the walk does, and the listing is there when it finishes.
func TestPathIndexStartPublishesOffThread(t *testing.T) {
	var idx pathIndex
	idx.start(indexTree(t))
	deadline := time.Now().Add(5 * time.Second)
	for !idx.built.Load() {
		if time.Now().After(deadline) {
			t.Fatal("background index never published")
		}
		time.Sleep(time.Millisecond)
	}
	if !idx.takeBuilt() {
		t.Fatal("takeBuilt must report the build exactly once")
	}
	if idx.takeBuilt() {
		t.Fatal("takeBuilt reported the same build twice")
	}
	got := idx.match("paths")
	if len(got) != 1 || got[0] != "internal/tui/paths.go" {
		t.Fatalf("match(paths) = %v", got)
	}
	// A nil index answers nothing, which is what the dropdown showed before
	// the walk landed.
	var cold pathIndex
	if cold.match("paths") != nil {
		t.Fatalf("cold index matched: %v", cold.match("paths"))
	}
}

// TestPathIndexMatchIsPrefixAndCapped pins the two things a keystroke relies
// on: base-name prefix matching (a directory in the path must not help or
// hurt the match) and a bounded result. The binary search is only correct
// against a sorted index, so the ordering is checked through the answer.
func TestPathIndexMatchIsPrefixAndCapped(t *testing.T) {
	var idx pathIndex
	storeIndex(&idx,
		"internal/tui/paths.go", "internal/tool/git.go", "docs/parser.md",
		"node_modules/dep/index.js", "main.go", "internal/tui/paths_test.go")
	for _, tc := range []struct{ seg, want string }{
		{"git", "internal/tool/git.go"},
		{"parser", "docs/parser.md"},
		{"index", "node_modules/dep/index.js"},
		{"PATH", ""}, // case is the caller's job: seg arrives lowercased
		{"nope", ""},
	} {
		got := idx.match(tc.seg)
		if tc.want == "" {
			if len(got) != 0 {
				t.Fatalf("match(%q) = %v, want nothing", tc.seg, got)
			}
			continue
		}
		if len(got) != 1 || got[0] != tc.want {
			t.Fatalf("match(%q) = %v, want [%s]", tc.seg, got, tc.want)
		}
	}
	// One prefix run spans several base names, in the index's own order —
	// which is the whole point of sorting by name rather than by path.
	got := idx.match("p")
	want := []string{"docs/parser.md", "internal/tui/paths.go", "internal/tui/paths_test.go"}
	if len(got) != len(want) {
		t.Fatalf("match(p) = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("match(p) = %v, want %v", got, want)
		}
	}
	// A root-level file is offered by the readdir, never by the index.
	if got := idx.match("main"); got != nil {
		t.Fatalf("root-level file listed twice: %v", got)
	}
	// An empty prefix is a bare `@`, which the readdir answers on its own.
	if got := idx.match(""); got != nil {
		t.Fatalf("match(\"\") = %v, want nothing", got)
	}
	// Many hits under one name still land in a single capped run.
	var many []pathHit
	for i := range 500 {
		many = append(many, pathHit{lower: "same.go", path: "d/same.go-" + string(rune('a'+i%26))})
	}
	idx.hits.Store(&many)
	if got := idx.match("same"); len(got) != maxPathCandidates {
		t.Fatalf("match(same) = %d rows, want the %d cap", len(got), maxPathCandidates)
	}
}
