package session

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// SessionMeta is one listed session (metadata only — never a full parse).
type SessionMeta struct {
	Path      string
	ID        string
	Title     string
	CWD       string
	Timestamp time.Time // session header timestamp
	SizeBytes int64
	ModTime   time.Time
}

// EncodeCWDBucket maps a canonical cwd path to its session-bucket name:
// "/" → "-", home-dir prefix "/Users/x" → "-Users-x" (already covered by the
// slash rule), "/tmp/..." → "-tmp-...". Empty cwd maps to "-".
func EncodeCWDBucket(cwd string) string {
	cwd = filepath.Clean(cwd)
	if cwd == "/" {
		return "-"
	}
	return strings.ReplaceAll(cwd, "/", "-")
}

// SessionsRoot returns dataDir/sessions.
func SessionsRoot(dataDir string) string {
	return filepath.Join(dataDir, "sessions")
}

// SessionFilePath builds the canonical session file path for a new session:
// dataDir/sessions/<bucket>/<timestamp>_<id>.jsonl.
func SessionFilePath(dataDir, cwd string, now time.Time, id string) string {
	bucket := EncodeCWDBucket(cwd)
	name := now.UTC().Format("2006-01-02T15-04-05.000Z") + "_" + id + ".jsonl"
	return filepath.Join(SessionsRoot(dataDir), bucket, name)
}

// List walks dataDir/sessions/<bucket>/*.jsonl for ALL buckets, reading only
// the first 4 KiB of each file (title slot + session header + maybe first
// entry) plus os.Stat for size/mtime. Results are sorted by ModTime desc.
// A stat-keyed cache reuses parsed metadata across calls and invalidates on
// stat change.
func List(dataDir string) ([]SessionMeta, error) {
	return listWithCache(dataDir, newStatCache())
}

type statKey struct {
	path     string
	size     int64
	modUnix  int64
	modNanos int64
}

type statCache struct {
	mu sync.Mutex
	m  map[statKey]SessionMeta
}

func newStatCache() *statCache { return &statCache{m: map[statKey]SessionMeta{}} }

func (c *statCache) get(k statKey) (SessionMeta, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[k]
	return v, ok
}

func (c *statCache) put(k statKey, v SessionMeta) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[k] = v
}

func listWithCache(dataDir string, cache *statCache) ([]SessionMeta, error) {
	root := SessionsRoot(dataDir)
	buckets, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("session: list %s: %w", root, err)
	}

	// Collect candidate files across all buckets.
	var paths []string
	for _, b := range buckets {
		if !b.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, b.Name()))
		if err != nil {
			continue // bucket vanished mid-scan
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			paths = append(paths, filepath.Join(root, b.Name(), f.Name()))
		}
	}

	metas := scanPaths(paths, cache)
	sort.Slice(metas, func(i, j int) bool { return metas[i].ModTime.After(metas[j].ModTime) })
	return metas, nil
}

// scanPaths reads metadata for paths with a bounded worker pool (8).
func scanPaths(paths []string, cache *statCache) []SessionMeta {
	out := make([]SessionMeta, len(paths))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, p := range paths {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if m, ok := statPath(p, cache); ok {
				out[i] = m
			}
		}(i, p)
	}
	wg.Wait()
	// Compact (drop failed reads, preserve order for the caller's sort).
	kept := out[:0]
	for _, m := range out {
		if m.Path != "" {
			kept = append(kept, m)
		}
	}
	return kept
}

// statPath stats one file and parses its 4 KiB prefix (cache-first).
func statPath(path string, cache *statCache) (SessionMeta, bool) {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return SessionMeta{}, false
	}
	key := statKey{path: path, size: fi.Size(), modUnix: fi.ModTime().Unix(), modNanos: int64(fi.ModTime().Nanosecond())}
	if m, ok := cache.get(key); ok {
		return m, true
	}

	f, err := os.Open(path)
	if err != nil {
		return SessionMeta{}, false
	}
	prefix := make([]byte, 4096)
	n, _ := f.Read(prefix)
	f.Close()
	lines := strings.Split(strings.TrimRight(string(prefix[:n]), "\n"), "\n")

	meta := SessionMeta{Path: path, SizeBytes: fi.Size(), ModTime: fi.ModTime()}
	if len(lines) > 0 {
		if title, ok := ParseTitleSlot([]byte(lines[0])); ok {
			meta.Title = title
		}
	}
	// Header is line 2 when line 1 was a slot, else line 1.
	for _, hIdx := range []int{1, 0} {
		if hIdx >= len(lines) {
			continue
		}
		if h, ok := ParseHeader([]byte(lines[hIdx])); ok {
			meta.ID = h.ID
			meta.CWD = h.CWD
			meta.Timestamp = h.Timestamp
			if meta.Title == "" {
				meta.Title = h.Title
			}
			break
		}
	}
	if meta.ID == "" {
		return SessionMeta{}, false // not a session file
	}
	cache.put(key, meta)
	return meta, true
}
