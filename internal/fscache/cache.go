// Package fscache is the shared filesystem-scan cache (M7 #8): the one
// Rust-natives concept worth re-implementing in Go (PRD §2, research
// §II.1). Full directory walks are expensive; editor path completion,
// glob, and grep all want the same answer, so one process-local map
// serves them all.
//
// Semantics ported from omp's FS-scan cache: keyed by (canonical root,
// traversal options), TTL 1s, max 16 entries, empty results revalidated
// after 200 ms, invalidated on write/delete/rename.
package fscache

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Defaults (omp parity).
const (
	// TTL of a cached walk.
	TTL = 1 * time.Second
	// MaxEntries bounds the cache (oldest key evicted first).
	MaxEntries = 16
	// EmptyTTL revalidates empty results sooner: an empty directory walk
	// is usually a race with a just-created file.
	EmptyTTL = 200 * time.Millisecond
)

// Options parameterize one walk. Distinct option sets are distinct cache
// entries (a walk that skips hidden files is not the same answer as one
// that does not). MaxEntries is deliberately NOT part of the key: the
// cache always stores the complete walk and applies the cap per caller,
// so a capped scan can never poison an uncapped one.
type Options struct {
	// Roots are the directories to walk (absolute).
	Roots []string
	// IncludeHidden walks dot-entries.
	IncludeHidden bool
	// RespectGitignore prunes paths ignored by root .gitignore files.
	// Parsing is intentionally shallow (root-level patterns only) —
	// ponytail: nested .gitignore semantics are git's job; shell out to
	// rg/fd when the caller needs exactness.
	RespectGitignore bool
	// MaxEntries caps the returned slice (0 = unlimited).
	MaxEntries int
}

// Entry is one file found by a walk.
type Entry struct {
	Path    string // absolute
	Rel     string // relative to its root, slash-separated
	Size    int64
	ModTime time.Time
	IsDir   bool
}

// key canonicalizes the option set (excluding the result cap).
func (o Options) key() string {
	roots := append([]string(nil), o.Roots...)
	sort.Strings(roots)
	var b strings.Builder
	for _, r := range roots {
		b.WriteString(canonical(r))
		b.WriteByte(0)
	}
	b.WriteString(boolKey(o.IncludeHidden))
	b.WriteString(boolKey(o.RespectGitignore))
	return b.String()
}

// canonical resolves symlinks so a cache key and an invalidation target
// agree: on macOS /var is a symlink to /private/var, and a mismatch there
// would silently disable invalidation for every temp/canonicalized root.
func canonical(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

func boolKey(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

type cacheEntry struct {
	at    time.Time
	empty bool
	full  []Entry // the complete walk; caps are applied per caller
}

// Cache is the process-local walk cache. Safe for concurrent use.
type Cache struct {
	mu  sync.Mutex
	m   map[string]*cacheEntry
	ord []string // live keys in insertion order (pruned with the map)
	now func() time.Time
}

// New returns an empty cache.
func New() *Cache {
	return &Cache{m: map[string]*cacheEntry{}, now: time.Now}
}

// Scan returns the cached walk for opts, walking on miss/expiry.
func (c *Cache) Scan(opts Options) (entries []Entry, truncated bool, total int) {
	key := opts.key()
	c.mu.Lock()
	if e, ok := c.m[key]; ok {
		ttl := TTL
		if e.empty {
			ttl = EmptyTTL
		}
		if c.now().Sub(e.at) < ttl {
			entries, truncated, total = applyCap(e.full, opts.MaxEntries)
			c.mu.Unlock()
			return entries, truncated, total
		}
		c.removeLocked(key)
	}
	c.mu.Unlock()

	full, _, _ := Walk(Options{
		Roots:            opts.Roots,
		IncludeHidden:    opts.IncludeHidden,
		RespectGitignore: opts.RespectGitignore,
	})

	c.mu.Lock()
	if _, exists := c.m[key]; !exists {
		c.ord = append(c.ord, key)
	}
	for len(c.ord) > MaxEntries {
		c.removeLocked(c.ord[0])
	}
	c.m[key] = &cacheEntry{at: c.now(), empty: len(full) == 0, full: full}
	c.mu.Unlock()
	return applyCap(full, opts.MaxEntries)
}

// applyCap truncates a full walk to the caller's cap.
func applyCap(full []Entry, max int) (entries []Entry, truncated bool, total int) {
	if max <= 0 || len(full) <= max {
		return full, false, len(full)
	}
	return full[:max], true, len(full)
}

// removeLocked drops a key from both the map and the eviction order, so
// ord can never outlive the map (a stale ord would make eviction pop
// deleted keys and let the map grow past MaxEntries).
func (c *Cache) removeLocked(key string) {
	delete(c.m, key)
	for i, k := range c.ord {
		if k == key {
			c.ord = append(c.ord[:i], c.ord[i+1:]...)
			break
		}
	}
}

// Invalidate drops every cached walk that could contain path — called on
// write/delete/rename so the next scan sees the change immediately
// instead of waiting out the TTL.
func (c *Cache) Invalidate(path string) {
	abs := canonical(path)
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, e := range c.m {
		if entryCovers(e, abs) || keyCoversRoot(key, abs) {
			c.removeLocked(key)
		}
	}
}

// InvalidateAll drops every cached walk (a bash command may touch anything).
func (c *Cache) InvalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m = map[string]*cacheEntry{}
	c.ord = nil
}

func entryCovers(e *cacheEntry, abs string) bool {
	for _, ent := range e.full {
		if ent.Path == abs {
			return true
		}
	}
	return false
}

// keyCoversRoot reports whether a cache key's roots contain abs.
func keyCoversRoot(key, abs string) bool {
	for _, root := range strings.Split(key, "\x00") {
		if root == "" || root == "0" || root == "1" || root == "00" || root == "01" || root == "10" || root == "11" {
			continue // trailing option flags, not a root
		}
		if strings.HasPrefix(abs, root+string(filepath.Separator)) || abs == root {
			return true
		}
	}
	return false
}

// Walk performs the actual traversal (exported for callers that want an
// uncached scan, and for tests).
func Walk(opts Options) (entries []Entry, truncated bool, total int) {
	for _, root := range opts.Roots {
		var patterns []string
		if opts.RespectGitignore {
			patterns = readGitignore(root)
		}
		if err := walkRoot(root, opts, patterns, &entries, &total); err != nil {
			continue // an unreadable root contributes nothing
		}
	}
	if opts.MaxEntries > 0 && len(entries) > opts.MaxEntries {
		entries = entries[:opts.MaxEntries]
		truncated = true
	}
	return entries, truncated, total
}

func walkRoot(root string, opts Options, patterns []string, out *[]Entry, total *int) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if path != root {
			name := d.Name()
			if !opts.IncludeHidden && strings.HasPrefix(name, ".") {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if d.IsDir() && name == "node_modules" {
				return fs.SkipDir
			}
			if ignored(relOf(root, path), patterns) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
		}
		if path == root {
			return nil // the root is the container, not an entry of itself
		}
		*total++
		e := Entry{Path: path, Rel: relOf(root, path), IsDir: d.IsDir()}
		if info, ierr := d.Info(); ierr == nil {
			e.Size = info.Size()
			e.ModTime = info.ModTime()
		}
		*out = append(*out, e)
		return nil
	})
}

func relOf(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}

// readGitignore reads root/.gitignore patterns (root-level only).
func readGitignore(root string) []string {
	raw, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		out = append(out, strings.TrimSuffix(line, "/"))
	}
	return out
}

// ignored matches a relative slash path against simple gitignore patterns
// (exact names, directory prefixes, and `*.ext` globs).
func ignored(rel string, patterns []string) bool {
	if len(patterns) == 0 {
		return false
	}
	base := rel
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		base = rel[i+1:]
	}
	for _, p := range patterns {
		if p == rel || p == base {
			return true
		}
		if strings.HasPrefix(rel, p+"/") {
			return true
		}
		if strings.HasPrefix(p, "*.") && strings.HasSuffix(base, p[1:]) {
			return true
		}
	}
	return false
}
