package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
)

// SessionMeta is one listed session (metadata only — never a full parse).
type SessionMeta struct {
	Path  string
	ID    string
	Title string
	// Preview is what a picker row shows: the title when the session has
	// one, and otherwise the fallback chain — the last compaction's one-line
	// summary, then the first user message, then "Untitled · HH:MM". It is
	// never empty: a session with no title still has to say WHICH session it
	// is (omp's title/preview fallbacks, rules 35 and 45).
	Preview string
	CWD     string
	// Timestamp is the session header timestamp.
	Timestamp time.Time
	SizeBytes int64
	ModTime   time.Time
	// ParentSession is non-empty for forks and subagent children (lineage).
	ParentSession string
	// TitleSource is the title-slot source ("auto"/"manual"/"subagent").
	// Resume paths skip "subagent" — a child is never a user continuation.
	TitleSource string
	// Status is the lifecycle badge derived from the tail of the file
	// (#107): omp's six verdicts — "complete" when the last turn finished,
	// "error", "aborted", "interrupted", "pending" and "unknown" otherwise,
	// plus xdev's "empty" for a file with no message at all. A session the
	// user killed mid-turn must not look like one that finished.
	Status SessionStatus
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
// entry) plus the last 32 KiB for the lifecycle status, and os.Stat for
// size/mtime. Results are sorted by ModTime desc. A stat-keyed cache reuses
// parsed metadata across calls and invalidates on stat change.
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
	// Newest first. ModTime ties (two sessions in one filesystem tick, or
	// coarse mtime granularity on overlayfs/tmpfs) fall back to the header
	// timestamp so "most recent session" is deterministic: callers like
	// --continue must not resolve arbitrarily.
	sort.Slice(metas, func(i, j int) bool {
		if !metas[i].ModTime.Equal(metas[j].ModTime) {
			return metas[i].ModTime.After(metas[j].ModTime)
		}
		return metas[i].Timestamp.After(metas[j].Timestamp)
	})
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

// SanitizeTitle folds a title to the single line a row can paint: the first
// line only, control characters stripped, trimmed. A title that holds
// nothing after that is "" — the caller falls back (omp's title sanitiser,
// rule 35). A title is model- and user-written text, so it is never painted
// as it came off disk.
func SanitizeTitle(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

// DisplayTitle is the listing's fallback chain (omp rules 35, 45): the
// sanitised title, then the last compaction's one-line summary, then the
// first user message, and finally "Untitled · HH:MM" — the session's own
// creation time, so two untitled sessions are told apart.
func DisplayTitle(title, shortSummary, firstMessage string, created time.Time) string {
	for _, s := range []string{title, shortSummary, firstMessage} {
		if t := SanitizeTitle(s); t != "" {
			return t
		}
	}
	return "Untitled · " + created.Format("15:04")
}

// listingMessageWire is the minimal decode of a persisted message for the
// listing: the role and the text blocks. It is deliberately not ai.Message —
// the strict block union would fail on a block type this build does not know
// and cost the whole line, and the listing only needs the words.
type listingMessageWire struct {
	Type    string `json:"type"`
	Message *struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

// firstUserMessage returns the text of the first user prompt in the window:
// the fallback a session with no title shows, so the row says what the
// session was about (omp rule 45). "" when the window holds no readable user
// message — the first prompt can be longer than the prefix the listing
// reads, and a truncated line is not a message.
func firstUserMessage(lines []string) string {
	for _, line := range lines {
		if line == "" || line[0] != '{' {
			continue
		}
		var w listingMessageWire
		if err := json.Unmarshal([]byte(line), &w); err != nil {
			continue
		}
		if w.Type != TypeMessage || w.Message == nil || w.Message.Role != string(ai.RoleUser) {
			continue
		}
		var b strings.Builder
		for _, c := range w.Message.Content {
			if c.Type == "text" {
				b.WriteString(c.Text)
			}
		}
		if t := strings.TrimSpace(b.String()); t != "" {
			return t
		}
	}
	return ""
}

// statPath stats one file and parses its 4 KiB prefix plus its status tail
// (cache-first).
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
	// The terminal entry sits at the END of the file, and the store appends
	// bookkeeping after it (model_change, session_exit, branch markers), so
	// the prefix never shows it. One extra bounded read on the descriptor we
	// already hold — never a whole-file parse.
	var tail []byte
	if fi.Size() <= int64(len(prefix)) {
		tail = prefix[:n] // the prefix already reached EOF
	} else {
		buf := make([]byte, statusTailMax)
		if m, rerr := f.ReadAt(buf, max(0, fi.Size()-int64(len(buf)))); rerr == nil || m > 0 {
			tail = buf[:m] // the offset may split a line; ClassifyStatus drops the fragment
		}
	}
	f.Close()
	lines := strings.Split(strings.TrimRight(string(prefix[:n]), "\n"), "\n")

	status := ClassifyStatus(tail)
	meta := SessionMeta{Path: path, SizeBytes: fi.Size(), ModTime: fi.ModTime(), Status: status}
	if len(lines) > 0 {
		if title, src, ok := ParseTitleSlotSource([]byte(lines[0])); ok {
			meta.Title = SanitizeTitle(title)
			meta.TitleSource = src
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
			meta.ParentSession = h.ParentSession
			if meta.Title == "" {
				meta.Title = SanitizeTitle(h.Title)
			}
			break
		}
	}
	if meta.ID == "" {
		return SessionMeta{}, false // not a session file
	}
	// The display fallback chain (omp rules 35, 45): title, then the last
	// compaction's one-line summary, then the first user message, then
	// "Untitled · HH:MM". A row always has something to say. The fallbacks
	// are only computed for a session with no title — the common case
	// already has its answer.
	created := meta.Timestamp
	if created.IsZero() {
		created = meta.ModTime
	}
	if meta.Title != "" {
		meta.Preview = meta.Title
	} else {
		meta.Preview = DisplayTitle("", TailCompactionSummary(tail), firstUserMessage(lines), created)
	}
	cache.put(key, meta)
	return meta, true
}
