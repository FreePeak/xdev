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
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])[:4]
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
