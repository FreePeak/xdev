package tool

// File-editing helpers shared by the read/write/edit tools: newline-faithful
// line IO, atomic staged writes, content hashing, and snapshot rendering.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ReadLines reads a text file into its lines, preserving empty lines. A
// single trailing newline is not a line; "a\n" is one line, "a" is one line,
// "" is zero lines, "a\n\n" is two lines ("a" and ""). Other bytes
// (including "\r") are preserved verbatim.
func ReadLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return []string{}, nil
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"), nil
}

// WriteLinesAtomic writes lines (each terminated with "\n") via a temp file
// in the destination directory followed by rename. The existing file's mode
// is preserved; new files get 0644.
func WriteLinesAtomic(path string, lines []string) error {
	var buf bytes.Buffer
	for _, line := range lines {
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	return writeBytesAtomic(path, buf.Bytes())
}

// writeBytesAtomic stages data in a temp file next to path, then renames it
// into place. On any failure the temp file is removed and path is untouched.
func writeBytesAtomic(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".xdev-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	// Single choke point for every in-repo write (write tool, edit tool,
	// snapshot restore): the shared scan cache must not serve a listing
	// that predates this change.
	SharedFSCache().Invalidate(path)
	return nil
}

// resolvePath returns the absolute path with symlinks resolved for targets
// that exist; not-yet-existing targets fall back to the plain absolute path.
func resolvePath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

// ShortHash returns the first 4 hex chars of sha256(content) — the snapshot
// anchor the model can quote back in later edits.
func ShortHash(content string) string {
	h := HashContent(content)
	if len(h) < 4 {
		return h
	}
	return h[:4]
}

// HashContent returns the full sha256 hex of content.
func HashContent(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// linesHash hashes the canonical reconstruction of a file's lines (what
// ReadLines reverses). Read/write/edit all record this same value, so a
// freshness comparison is like-for-like even when a file lacks a trailing
// newline.
func linesHash(lines []string) string {
	if len(lines) == 0 {
		return HashContent("")
	}
	return HashContent(strings.Join(lines, "\n") + "\n")
}

// fileSnapshot is the freshness record one path carries in the Registry:
// the content hash at record time plus, when the source rendered line text
// to the model, that exact text (1-based line → content; windowed reads
// record only their window).
type fileSnapshot struct {
	hash  string
	lines map[int]string
	max   int // highest recorded line number
}

// tag is the 4-char snapshot anchor ([path#TAG]) for the recorded content.
func (s *fileSnapshot) tag() string { return hashTag(s.hash) }

// window returns the recorded text of lines start..end, reporting whether
// the record covers the whole range.
func (s *fileSnapshot) window(start, end int) ([]string, bool) {
	if start < 1 || end > s.max {
		return nil, false
	}
	out := make([]string, 0, end-start+1)
	for n := start; n <= end; n++ {
		l, ok := s.lines[n]
		if !ok {
			return nil, false
		}
		out = append(out, l)
	}
	return out, true
}

// splitSnapshotTag strips an optional "#TAG" snapshot anchor from a path
// argument ("f.go#A1B2" — the header form tool results render), returning
// the bare path and the tag ("" when absent or not a plausible hex tag).
func splitSnapshotTag(p string) (path, tag string) {
	i := strings.LastIndex(p, "#")
	if i <= 0 {
		return p, ""
	}
	t := p[i+1:]
	if len(t) < 4 || len(t) > 64 {
		return p, ""
	}
	for _, c := range t {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return p, ""
		}
	}
	return p[:i], strings.ToLower(t)
}

// RenderWindow renders lines as 1-based "N:content" rows in a
// centerLine±radius window, with an "…" row marking a gap at either edge of
// the file. The caller prepends the "[path#hash]" header line.
func RenderWindow(lines []string, centerLine, radius int) string {
	if len(lines) == 0 {
		return ""
	}
	if radius < 0 {
		radius = 0
	}
	centerLine = min(max(centerLine, 1), len(lines))
	start := max(centerLine-radius, 1)
	end := min(centerLine+radius, len(lines))

	var b strings.Builder
	if start > 1 {
		b.WriteString("…\n")
	}
	for i, line := range lines[start-1 : end] {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(strconv.Itoa(start + i))
		b.WriteByte(':')
		b.WriteString(line)
	}
	if end < len(lines) {
		b.WriteString("\n…")
	}
	return b.String()
}
