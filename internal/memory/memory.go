// Package memory implements the local long-term memory backend (M12,
// research F1 CORE): two markdown files under the agent data directory
// plus the memory:// read seam and the learn tool's landing zone.
//
//	MEMORY.md  the consolidated summary injected into the system prompt
//	learned.md append-only lessons, newest last
//
// Memory is heuristic context, never authoritative (omp semantics): a
// lesson that changes the plan must be cited by path.
package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Backend is the local store. The zero value is usable (off) — every
// method is nil/off-safe so an unconfigured session pays nothing.
type Backend struct {
	// Dir is the memory directory. Empty = off.
	Dir string
	// SummaryCapChars bounds the injected summary (token budget
	// protection; ~4 chars/token, so the default is ~1250 tokens).
	SummaryCapChars int
	// LessonCap bounds how many recent lessons the prompt sees.
	LessonCap int

	mu sync.Mutex
}

// Defaults for injection caps (the omp shared cap is 5000 tokens; xdev
// keeps the injected block well under it because it also carries the
// tool list).
const (
	DefaultSummaryCapChars = 5000
	DefaultLessonCap       = 20
)

// Store is the backend contract every memory consumer programs against:
// prompt injection (GuidanceBlock), the memory:// read seam (Read), the
// learn tool (SaveLesson), and /memory (Summary/Stats/Paths/Clear). The
// local Backend and the friction-gated SharpShooter both implement it, so
// memory.backend selects storage without touching a single caller.
type Store interface {
	// Off reports whether the backend is disabled (nil-safe).
	Off() bool
	// Summary is the text injected as Memory Guidance.
	Summary() string
	// GuidanceBlock wraps Summary in the injected block shape ("" = nothing).
	GuidanceBlock() string
	// Read resolves a memory:// URL to text.
	Read(uri string) (string, error)
	// Stats is the /memory stats report.
	Stats() string
	// Clear drops the backend's stored data (/memory clear).
	Clear() error
	// Paths exposes the backend's artifact paths for diagnostics.
	Paths() (summary, lessons string)
	// SaveLesson records one durable lesson (the learn tool).
	SaveLesson(text, context string) error
}

// Both shipped backends satisfy the seam; a compile-time check, so a
// signature drift is a build failure, not a runtime nil.
var (
	_ Store = (*Backend)(nil)
	_ Store = (*SharpShooter)(nil)
)

// Off reports whether the backend is disabled.
func (b *Backend) Off() bool { return b == nil || b.Dir == "" }

func (b *Backend) paths() (summary, lessons string) {
	return filepath.Join(b.Dir, "MEMORY.md"), filepath.Join(b.Dir, "learned.md")
}

// Ensure creates the directory (first use).
func (b *Backend) Ensure() error {
	if b.Off() {
		return nil
	}
	return os.MkdirAll(b.Dir, 0o755)
}

// Summary returns the injected guidance block: MEMORY.md (capped) plus
// the most recent lessons. Empty when nothing is stored.
func (b *Backend) Summary() string {
	if b.Off() {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	summaryPath, lessonsPath := b.paths()
	head := capText(readFileOrEmpty(summaryPath), b.summaryCap())
	lessons := tailLines(readFileOrEmpty(lessonsPath), b.lessonCap())
	var parts []string
	if head != "" {
		parts = append(parts, head)
	}
	if lessons != "" {
		parts = append(parts, "Lessons:\n"+lessons)
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

// GuidanceBlock wraps the summary in the Memory Guidance shape omp
// injects, or "" when there is nothing to inject.
func (b *Backend) GuidanceBlock() string {
	return guidanceBlock(b.Summary())
}

// SaveLesson appends a lesson (newest last) with a timestamp header.
func (b *Backend) SaveLesson(text, context string) error {
	if b.Off() {
		return fmt.Errorf("memory: backend off")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("memory: lesson text is required")
	}
	if err := b.Ensure(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	_, lessonsPath := b.paths()
	f, err := os.OpenFile(lessonsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	stamp := time.Now().UTC().Format("2006-01-02")
	if context != "" {
		text += " _(context: " + collapseWS(context) + ")_"
	}
	_, err = fmt.Fprintf(f, "\n- %s — %s\n", stamp, collapseWS(text))
	return err
}

// Read resolves a memory:// URL to text:
//
//	memory://root              the injected summary (what the prompt saw)
//	memory://root/MEMORY.md    the raw consolidated file
//	memory://root/learned.md   the raw lesson log
func (b *Backend) Read(uri string) (string, error) {
	if b.Off() {
		return "", fmt.Errorf("memory: backend off (set memory: local in settings)")
	}
	rest := strings.TrimPrefix(uri, "memory://")
	if rest == uri {
		return "", fmt.Errorf("memory: not a memory URL: %q", uri)
	}
	rest = strings.Trim(rest, "/")
	summaryPath, lessonsPath := b.paths()
	switch rest {
	case "", "root":
		s := b.Summary()
		if s == "" {
			return "(no memories stored yet)", nil
		}
		return s, nil
	case "root/MEMORY.md", "MEMORY.md":
		return readFileOrEmpty(summaryPath), nil
	case "root/learned.md", "learned.md":
		return readFileOrEmpty(lessonsPath), nil
	default:
		return "", fmt.Errorf("memory: unknown path %q (root, root/MEMORY.md, root/learned.md)", rest)
	}
}

// WriteSummary replaces MEMORY.md (consolidation / manual sync).
func (b *Backend) WriteSummary(text string) error {
	if b.Off() {
		return fmt.Errorf("memory: backend off")
	}
	if err := b.Ensure(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	summaryPath, _ := b.paths()
	return os.WriteFile(summaryPath, []byte(strings.TrimSpace(text)+"\n"), 0o644)
}

// Clear removes both files (the /memory clear action). Missing files are
// not an error.
func (b *Backend) Clear() error {
	if b.Off() {
		return fmt.Errorf("memory: backend off")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	summaryPath, lessonsPath := b.paths()
	for _, p := range []string{summaryPath, lessonsPath} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// Stats reports the on-disk sizes for /memory stats.
func (b *Backend) Stats() string {
	if b.Off() {
		return "memory: off"
	}
	summaryPath, lessonsPath := b.paths()
	sc, sl := fileSize(summaryPath), fileSize(lessonsPath)
	nLessons := 0
	for _, l := range strings.Split(readFileOrEmpty(lessonsPath), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "- ") {
			nLessons++
		}
	}
	return fmt.Sprintf("memory: local at %s\n  MEMORY.md %d B\n  learned.md %d B, %d lesson(s)",
		b.Dir, sc, sl, nLessons)
}

// Paths exposes the two file paths (for /memory view and diagnostics).
func (b *Backend) Paths() (summary, lessons string) {
	if b.Off() {
		return "", ""
	}
	return b.paths()
}

// --- helpers ---

func (b *Backend) summaryCap() int {
	if b.SummaryCapChars > 0 {
		return b.SummaryCapChars
	}
	return DefaultSummaryCapChars
}

func (b *Backend) lessonCap() int {
	if b.LessonCap > 0 {
		return b.LessonCap
	}
	return DefaultLessonCap
}

func readFileOrEmpty(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

// capText truncates on a line boundary with an explicit marker, so the model
// can tell the summary was clipped.
func capText(s string, max int) string {
	return capTextHint(s, max, "read memory://root/MEMORY.md for the rest")
}

// capTextHint is capText with a backend-specific "where is the rest" pointer;
// an empty hint leaves just the truncation ellipsis.
func capTextHint(s string, max int, hint string) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := s[:max]
	if i := strings.LastIndex(cut, "\n"); i > 0 {
		cut = cut[:i]
	}
	if hint == "" {
		return cut + "…"
	}
	return cut + "\n… (summary capped; " + hint + ")"
}

// tailLines returns the last n non-empty lines.
func tailLines(s string, n int) string {
	if n <= 0 {
		return s
	}
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// guidanceBlock is the Memory Guidance shape omp injects, shared by every
// backend so the header text stays identical across seam consumers.
func guidanceBlock(summary string) string {
	if summary == "" {
		return ""
	}
	return "# Memory Guidance\n\nHeuristic context from earlier sessions — not authoritative. " +
		"When it changes your plan, read the source (memory://root) and cite it.\n\n" + summary
}

func collapseWS(s string) string { return strings.Join(strings.Fields(s), " ") }
