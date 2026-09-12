// Foreign-transcript import (issue #28): Claude Code and Codex transcripts
// are read-only sources; a selected transcript is mapped to xdev session
// entries and persisted as a NEW xdev session. The shared writer and the
// listing/resolution helpers live here; the per-source parsers are
// import_claude.go / import_codex.go.
package session

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
)

// ImportResult reports what one import produced.
type ImportResult struct {
	Store     *Store // new session, already on disk
	Turns     int    // user/assistant/toolResult entries appended
	ToolCalls int    // tool calls mapped (assistant toolCall blocks)
	Dropped   int    // opaque records skipped (counter, never silent)
}

// importScan is one parser's view of a foreign transcript.
type importScan struct {
	title   string       // generated title, else "" (first prompt is used)
	cwd     string       // source-recorded cwd ("" when the layout has none)
	turns   []importTurn // file order
	dropped int          // opaque records skipped
}

// importTurn is one mapped conversation message.
type importTurn struct {
	stamp time.Time // zero → import time
	msg   ai.Message
}

// importForeign runs a parser over the transcript and writes a new xdev
// session: import header entry first, then the mapped turns. The source
// file is opened read-only and never mutated. dataDir places the new file;
// cwd is the session's working directory (the importer's cwd, not the
// foreign one — the import continues in the current project).
func importForeign(kind, path, dataDir, cwd string, scan func(io.Reader) (importScan, error)) (*ImportResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	sc, err := scan(bufio.NewReader(f))
	f.Close() // read-only handle; the source is never mutated
	if err != nil {
		return nil, err
	}
	if len(sc.turns) == 0 {
		return nil, fmt.Errorf("import %s: no conversation turns found in %s", kind, path)
	}

	orig := sc.title
	if orig == "" {
		orig = firstUserText(sc.turns)
	}
	title := "imported: " + kind + " " + clipText(orig, 80)

	now := time.Now().UTC()
	s := OpenMem(cwd, title)
	if _, err := s.EnsureOnDisk(SessionFilePath(dataDir, cwd, now, s.ID()), Options{}); err != nil {
		return nil, err
	}
	toolCalls := 0
	for _, t := range sc.turns {
		toolCalls += len(t.msg.ToolCalls())
	}
	header := &CustomEntry{CustomType: "imported", Data: map[string]any{
		"source":     kind,
		"path":       path,
		"importedAt": FormatStamp(now),
		"turns":      len(sc.turns),
		"toolCalls":  toolCalls,
		"dropped":    sc.dropped,
	}}
	if err := s.Append(header); err != nil {
		return nil, err
	}
	for _, t := range sc.turns {
		if err := s.Append(&MessageEntry{Env: Envelope{Timestamp: t.stamp}, Message: t.msg}); err != nil {
			return nil, err
		}
	}
	return &ImportResult{Store: s, Turns: len(sc.turns), ToolCalls: toolCalls, Dropped: sc.dropped}, nil
}

// firstUserText returns the first real user turn's text (title fallback).
// Harness-injected slash-command noise (<local-command-caveat>,
// <command-name>) is skipped: it is transcript content but a terrible title.
func firstUserText(turns []importTurn) string {
	for _, t := range turns {
		if t.msg.Role != ai.RoleUser {
			continue
		}
		text := t.msg.Text()
		trimmed := strings.TrimSpace(text)
		if strings.HasPrefix(trimmed, "<local-command-") || strings.HasPrefix(trimmed, "<command-name>") {
			continue
		}
		return text
	}
	return ""
}

// clipText trims s to one line of at most n runes.
func clipText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// readTimestamp parses a foreign RFC3339 stamp; zero on any problem.
func readTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// ForeignTranscript is one foreign transcript found on disk. Titles are not
// extracted during listing (a full parse) — only the cheap metadata.
type ForeignTranscript struct {
	Kind      string // "claude" | "codex"
	Path      string
	ID        string
	CWD       string // source-recorded cwd ("" when unknown)
	ModTime   time.Time
	SizeBytes int64
}

// ListForeignRoot walks a foreign transcript root (e.g. ~/.claude/projects)
// for *.jsonl transcripts, newest first. cwd filters where the source
// layout encodes it; empty means no filter. The root is read-only.
func ListForeignRoot(kind, root, cwd string) ([]ForeignTranscript, error) {
	var out []ForeignTranscript
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if root == p { // absent root = nothing to list, not a failure
				return filepath.SkipAll
			}
			return nil // unreadable subtree: skip, keep listing the rest
		}
		if d.IsDir() || !strings.HasSuffix(p, ".jsonl") {
			return nil
		}
		ft, ok := foreignMeta(kind, p)
		if !ok {
			return nil
		}
		// Claude's layout encodes the cwd in the project dir name
		// (ambiguous to decode) — match the encoded form; Codex records
		// the raw cwd in session_meta. A transcript with no recorded cwd
		// is never filtered out.
		want := cwd
		if kind == "claude" {
			want = ClaudeProjectSlug(cwd)
		}
		if cwd != "" && ft.CWD != "" && ft.CWD != want {
			return nil
		}
		out = append(out, ft)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out, nil
}

// foreignMeta stats one transcript and extracts its id (+ cwd where the
// layout records it, bounded to the file head).
func foreignMeta(kind, p string) (ForeignTranscript, bool) {
	st, err := os.Stat(p)
	if err != nil {
		return ForeignTranscript{}, false
	}
	ft := ForeignTranscript{
		Kind: kind, Path: p,
		ID:        foreignID(kind, filepath.Base(p)),
		ModTime:   st.ModTime(),
		SizeBytes: st.Size(),
	}
	if ft.ID == "" {
		return ForeignTranscript{}, false
	}
	ft.CWD = foreignCWD(kind, p)
	return ft, true
}

// ResolveForeign resolves a --from-claude/--from-codex argument to one
// transcript file. A query containing a path separator is a path or path
// prefix (glob, newest wins); otherwise it matches transcript ids (or, for
// Codex, the rollout file-name stem) case-insensitively by prefix, newest
// first.
func ResolveForeign(kind, query, root string) (string, error) {
	if query == "" {
		return "", fmt.Errorf("import %s: empty transcript reference", kind)
	}
	if strings.ContainsRune(query, '/') || strings.ContainsRune(query, os.PathSeparator) {
		pattern := query
		if !strings.HasSuffix(pattern, ".jsonl") {
			pattern += "*.jsonl"
		}
		matches, _ := filepath.Glob(pattern)
		if len(matches) == 0 {
			return "", fmt.Errorf("import %s: no transcript at %q", kind, query)
		}
		sort.Slice(matches, func(i, j int) bool {
			a, b := mustModTime(matches[i]), mustModTime(matches[j])
			return a.After(b)
		})
		return matches[0], nil
	}
	q := strings.ToLower(query)
	var best ForeignTranscript
	found := false
	all, err := ListForeignRoot(kind, root, "")
	if err != nil {
		return "", err
	}
	for _, ft := range all {
		stem := strings.TrimSuffix(filepath.Base(ft.Path), ".jsonl")
		if !strings.HasPrefix(strings.ToLower(stem), q) && !strings.HasPrefix(strings.ToLower(ft.ID), q) {
			continue
		}
		if !found || ft.ModTime.After(best.ModTime) {
			best, found = ft, true
		}
	}
	if !found {
		return "", fmt.Errorf("import %s: no transcript matching %q under %s", kind, query, root)
	}
	return best.Path, nil
}

func mustModTime(p string) time.Time {
	st, err := os.Stat(p)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

// decodeHead reads the first lim bytes of a file and returns the complete
// lines it contains (cheap head-parse for listing metadata).
func decodeHead(p string, lim int64) []string {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, lim)
	n, _ := f.Read(buf)
	return strings.Split(string(buf[:n]), "\n")
}
