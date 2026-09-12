package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// This file is the CLI-facing surface of the local backend (M15 #72: the
// mnemopi-CLI equivalent). It only reads and writes the same two markdown
// files the agent's prompt injection uses — no second store, no second
// format — plus a bounded scratch file that was never part of the injected
// summary.

// ScratchpadName is the bounded scratch file inside the memory dir.
const ScratchpadName = "scratchpad.md"

// DefaultScratchpadCap bounds the scratchpad in bytes. A scratchpad that can
// grow without limit is a second memory store nobody prunes; over-cap writes
// fail loudly rather than truncating the user's text.
const DefaultScratchpadCap = 16 << 10

// Lesson is one parsed learned.md entry.
// Wire shape of a line: "- 2026-09-12 — text _(context: ...)_".
type Lesson struct {
	Date    string `json:"date,omitempty"`
	Text    string `json:"text"`
	Context string `json:"context,omitempty"`
}

// Lessons parses learned.md in file order (oldest first, as appended).
// Malformed lines are skipped: the log is append-only and a hand edit must
// never make the CLI unreadable.
func (b *Backend) Lessons() []Lesson {
	if b.Off() {
		return nil
	}
	_, lessonsPath := b.paths()
	var out []Lesson
	for _, line := range strings.Split(readFileOrEmpty(lessonsPath), "\n") {
		if l, ok := parseLessonLine(line); ok {
			out = append(out, l)
		}
	}
	return out
}

func parseLessonLine(line string) (Lesson, bool) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "- ") {
		return Lesson{}, false
	}
	rest := strings.TrimSpace(s[2:])
	var l Lesson
	if i := strings.Index(rest, " — "); i >= 0 {
		l.Date = strings.TrimSpace(rest[:i])
		rest = strings.TrimSpace(rest[i+len(" — "):])
	}
	// SaveLesson appends " _(context: ...)_" to the text.
	if i := strings.Index(rest, "_(context: "); i >= 0 && strings.HasSuffix(rest, ")_") {
		l.Context = strings.TrimSpace(rest[i+len("_(context: ") : len(rest)-2])
		rest = strings.TrimSpace(rest[:i])
	}
	l.Text = rest
	if l.Text == "" {
		return Lesson{}, false
	}
	return l, true
}

// LessonsRaw returns learned.md verbatim ("" when absent).
func (b *Backend) LessonsRaw() string {
	if b.Off() {
		return ""
	}
	_, lessonsPath := b.paths()
	return readFileOrEmpty(lessonsPath)
}

// ScratchpadPath is <memory dir>/scratchpad.md.
func (b *Backend) ScratchpadPath() string {
	if b.Off() {
		return ""
	}
	return filepath.Join(b.Dir, ScratchpadName)
}

// Scratchpad returns the scratch file's text ("" when absent).
func (b *Backend) Scratchpad() string {
	p := b.ScratchpadPath()
	if p == "" {
		return ""
	}
	return readFileOrEmpty(p)
}

// SetScratchpad replaces the scratch file. Text over DefaultScratchpadCap is
// rejected (not truncated) so a paste can never silently lose its tail.
func (b *Backend) SetScratchpad(text string) error {
	if b.Off() {
		return fmt.Errorf("memory: backend off")
	}
	body := ensureNewline(text)
	if len(body) > DefaultScratchpadCap {
		return fmt.Errorf("memory: scratchpad is %d bytes, cap is %d", len(body), DefaultScratchpadCap)
	}
	if err := b.Ensure(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return writeOrRemove(b.ScratchpadPath(), body)
}

// ClearScratchpad removes the scratch file (missing is not an error).
func (b *Backend) ClearScratchpad() error {
	p := b.ScratchpadPath()
	if p == "" {
		return fmt.Errorf("memory: backend off")
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// FileInfo describes one memory file.
type FileInfo struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
	Lines int    `json:"lines"`
}

// Info is the structured form of Backend.Stats (the CLI's --json view).
type Info struct {
	Dir           string   `json:"dir"`
	Summary       FileInfo `json:"summary"`
	Lessons       FileInfo `json:"lessons"`
	Scratchpad    FileInfo `json:"scratchpad"`
	LessonCount   int      `json:"lessonCount"`
	ScratchpadCap int      `json:"scratchpadCap"`
}

// Info reports on-disk state for `xdev memory stats`.
func (b *Backend) Info() Info {
	if b.Off() {
		return Info{}
	}
	summaryPath, lessonsPath := b.paths()
	lessonsRaw := readFileOrEmpty(lessonsPath)
	return Info{
		Dir:         b.Dir,
		Summary:     fileInfo(summaryPath, readFileOrEmpty(summaryPath)),
		Lessons:     fileInfo(lessonsPath, lessonsRaw),
		Scratchpad:  fileInfo(b.ScratchpadPath(), b.Scratchpad()),
		LessonCount: len(b.Lessons()),

		ScratchpadCap: DefaultScratchpadCap,
	}
}

func fileInfo(path, content string) FileInfo {
	fi := FileInfo{Path: path, Bytes: fileSize(path)}
	if content != "" {
		fi.Lines = len(strings.Split(strings.TrimRight(content, "\n"), "\n"))
	}
	return fi
}

// BundleVersion is the export format version.
const BundleVersion = 1

// Bundle is a whole-store snapshot for `xdev memory export` / `import`.
// Lessons is raw text (not parsed entries) so a round trip is lossless.
type Bundle struct {
	Version    int    `json:"version"`
	ExportedAt string `json:"exportedAt,omitempty"`
	Dir        string `json:"dir,omitempty"`
	Summary    string `json:"summary,omitempty"`
	Lessons    string `json:"lessons,omitempty"`
	Scratchpad string `json:"scratchpad,omitempty"`
}

// Export snapshots summary, lessons and scratchpad.
func (b *Backend) Export() (Bundle, error) {
	if b.Off() {
		return Bundle{}, fmt.Errorf("memory: backend off")
	}
	summaryPath, _ := b.paths()
	return Bundle{
		Version:    BundleVersion,
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Dir:        b.Dir,
		Summary:    readFileOrEmpty(summaryPath),
		Lessons:    b.LessonsRaw(),
		Scratchpad: b.Scratchpad(),
	}, nil
}

// Import restores a bundle. merge=true keeps the existing summary (unless it
// is empty) and appends only lessons whose line is not already present;
// merge=false replaces all three files. Everything is validated before the
// first write, so a rejected bundle leaves the store untouched.
func (b *Backend) Import(bundle Bundle, merge bool) error {
	if b.Off() {
		return fmt.Errorf("memory: backend off")
	}
	if bundle.Version != 0 && bundle.Version != BundleVersion {
		return fmt.Errorf("memory: unsupported bundle version %d (want %d)", bundle.Version, BundleVersion)
	}
	scratch := ensureNewline(bundle.Scratchpad)
	if len(scratch) > DefaultScratchpadCap {
		return fmt.Errorf("memory: bundle scratchpad is %d bytes, cap is %d", len(scratch), DefaultScratchpadCap)
	}
	if err := b.Ensure(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	summaryPath, lessonsPath := b.paths()

	if !merge || strings.TrimSpace(readFileOrEmpty(summaryPath)) == "" {
		if err := writeOrRemove(summaryPath, ensureNewline(bundle.Summary)); err != nil {
			return err
		}
	}
	if merge {
		if err := appendMissingLessons(lessonsPath, bundle.Lessons); err != nil {
			return err
		}
	} else if err := writeOrRemove(lessonsPath, ensureNewline(bundle.Lessons)); err != nil {
		return err
	}
	if !merge || strings.TrimSpace(b.Scratchpad()) == "" {
		if err := writeOrRemove(b.ScratchpadPath(), scratch); err != nil {
			return err
		}
	}
	return nil
}

// writeOrRemove writes text verbatim, or removes the file when text is
// blank (an "empty" memory file must not become a lone "\n" file). Callers
// pass an ensureNewline-normalized body, which is exactly what the readers
// hand back (readFileOrEmpty trims), so export → import → export is stable.
func writeOrRemove(path, text string) error {
	if strings.TrimSpace(text) == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return os.WriteFile(path, []byte(text), 0o644)
}

// ensureNewline normalizes a text-file body: trimmed, terminated by exactly
// one newline, "" when blank (which writeOrRemove turns into a delete).
func ensureNewline(text string) string {
	s := strings.TrimSpace(text)
	if s == "" {
		return ""
	}
	return s + "\n"
}

// lessonKey is the content identity of a lesson (the date is not part of it,
// so a re-import from another day does not duplicate the same lesson).
func lessonKey(l Lesson) string { return collapseWS(l.Text + "|" + l.Context) }

// appendMissingLessons appends the lesson lines of raw whose lesson is not
// already in the file. The dedupe key is the lesson content (text+context),
// not the dated line: re-importing a bundle from another day must not
// duplicate what it restores.
func appendMissingLessons(path, raw string) error {
	existing := map[string]bool{}
	for _, line := range strings.Split(readFileOrEmpty(path), "\n") {
		if l, ok := parseLessonLine(line); ok {
			existing[lessonKey(l)] = true
		}
	}
	var add []string
	for _, line := range strings.Split(raw, "\n") {
		s := strings.TrimSpace(line)
		l, ok := parseLessonLine(s)
		if !ok || existing[lessonKey(l)] {
			continue
		}
		existing[lessonKey(l)] = true
		add = append(add, s)
	}
	if len(add) == 0 {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, l := range add {
		if _, err := fmt.Fprintf(f, "\n%s\n", l); err != nil {
			return err
		}
	}
	return nil
}
