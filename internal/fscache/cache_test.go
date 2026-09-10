package fscache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func seed(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, p := range []string{"a.go", "b.go", "sub/c.go", ".hidden.go", "node_modules/dep.js"} {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func names(entries []Entry) map[string]bool {
	out := map[string]bool{}
	for _, e := range entries {
		out[e.Rel] = true
	}
	return out
}

func TestWalkSkipsHiddenAndNodeModules(t *testing.T) {
	dir := seed(t)
	got := names(mustWalk(t, Options{Roots: []string{dir}}))
	for _, want := range []string{"a.go", "b.go", "sub/c.go"} {
		if !got[want] {
			t.Fatalf("missing %q in %v", want, got)
		}
	}
	for _, unwanted := range []string{".hidden.go", "node_modules/dep.js", "node_modules"} {
		if got[unwanted] {
			t.Fatalf("should be pruned: %q", unwanted)
		}
	}
}

func TestWalkIncludeHidden(t *testing.T) {
	dir := seed(t)
	got := names(mustWalk(t, Options{Roots: []string{dir}, IncludeHidden: true}))
	if !got[".hidden.go"] {
		t.Fatalf("hidden file missing: %v", got)
	}
}

func TestWalkRespectsGitignore(t *testing.T) {
	dir := seed(t)
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.go\nsub/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := names(mustWalk(t, Options{Roots: []string{dir}, RespectGitignore: true}))
	if got["a.go"] || got["sub/c.go"] {
		t.Fatalf("gitignore not applied: %v", got)
	}
}

func TestCacheServesWithinTTLAndInvalidates(t *testing.T) {
	dir := seed(t)
	c := New()
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	opts := Options{Roots: []string{dir}}

	first, _, _ := c.Scan(opts)
	if len(first) == 0 {
		t.Fatal("empty first scan")
	}
	// A new file added inside the TTL is invisible (cache hit)...
	newFile := filepath.Join(dir, "late.go")
	if err := os.WriteFile(newFile, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, _, _ := c.Scan(opts)
	if len(second) != len(first) {
		t.Fatalf("cache should serve stale within TTL: %d vs %d", len(second), len(first))
	}
	// ...until the TTL expires.
	now = now.Add(TTL + time.Millisecond)
	third, _, _ := c.Scan(opts)
	if len(third) != len(first)+1 {
		t.Fatalf("scan after TTL = %d entries, want %d", len(third), len(first)+1)
	}
	// Invalidate-on-write makes it immediate.
	if err := os.WriteFile(filepath.Join(dir, "later.go"), []byte("z"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.Invalidate(filepath.Join(dir, "later.go"))
	fourth, _, _ := c.Scan(opts)
	if len(fourth) != len(third)+1 {
		t.Fatalf("invalidate-on-write failed: %d vs %d", len(fourth), len(third))
	}
}

func TestCacheRevalidatesEmptyFaster(t *testing.T) {
	dir := t.TempDir()
	c := New()
	now := time.Unix(2000, 0)
	c.now = func() time.Time { return now }
	opts := Options{Roots: []string{dir}}

	if entries, _, _ := c.Scan(opts); len(entries) != 0 {
		t.Fatalf("expected empty dir scan, got %d", len(entries))
	}
	if err := os.WriteFile(filepath.Join(dir, "new.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Past EmptyTTL but well inside TTL: the empty result must be redone.
	now = now.Add(EmptyTTL + time.Millisecond)
	entries2, _, _ := c.Scan(opts)
	if len(entries2) != 1 {
		t.Fatalf("empty result not revalidated: %d entries", len(entries2))
	}
}

func TestCacheEvictsBeyondMax(t *testing.T) {
	c := New()
	c.now = func() time.Time { return time.Unix(3000, 0) }
	for range MaxEntries + 2 {
		c.Scan(Options{Roots: []string{t.TempDir()}})
	}
	c.mu.Lock()
	n := len(c.m)
	c.mu.Unlock()
	if n > MaxEntries {
		t.Fatalf("cache size %d exceeds cap %d", n, MaxEntries)
	}
}

func TestMaxEntriesTruncates(t *testing.T) {
	dir := seed(t)
	entries, truncated, total := Walk(Options{Roots: []string{dir}, MaxEntries: 1})
	if len(entries) != 1 || !truncated || total < 3 {
		t.Fatalf("entries=%d truncated=%v total=%d", len(entries), truncated, total)
	}
}

func mustWalk(t *testing.T, opts Options) []Entry {
	t.Helper()
	entries, _, _ := Walk(opts)
	return entries
}
