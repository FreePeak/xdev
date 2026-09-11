package session

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ForkSession copies src's session file to destPath with a rewritten
// header: new id, parentSession = src's id, fresh timestamp. Entry lines
// are copied verbatim so ids/parent links stay intact. The returned
// Store is open on the new file. Callers build destPath via
// SessionFilePath. Title "" gets a timestamped default.
func ForkSession(srcPath, destPath, title string) (*Store, error) {
	raw, err := os.ReadFile(srcPath)
	if err != nil {
		return nil, err
	}
	// Line 1 = title slot, line 2 = session header, rest = entries.
	lines := splitLines(raw)
	if len(lines) < 2 {
		return nil, os.ErrInvalid
	}
	srcHeader, ok := ParseHeader(lines[1])
	if !ok {
		return nil, os.ErrInvalid
	}

	now := time.Now().UTC()
	newID := sessionIDFromPath(destPath)
	newHeader := SessionHeader{
		Version:       3,
		ID:            newID,
		ParentSession: srcHeader.ID,
		Timestamp:     now,
		CWD:           srcHeader.CWD,
		Title:         title,
		TitleSource:   TitleSourceAuto,
	}
	if title == "" {
		if src, ok := ParseTitleSlot(lines[0]); ok && src != "" {
			newHeader.Title = src
		} else {
			newHeader.Title = "fork " + now.Format("2006-01-02 15:04")
		}
	}

	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return nil, err
	}
	dest := destPath
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Title slot: rewrite with the fork's title (the pad spaces are part
	// of line 1's fixed width, so the newline is added explicitly). The
	// header fallback already resolved the display title.
	if _, err := f.Write(MarshalTitleSlot(newHeader.Title, TitleSourceAuto, now)); err != nil {
		return nil, err
	}
	if _, err := f.Write(MarshalHeader(newHeader)); err != nil {
		return nil, err
	}
	for _, ln := range lines[2:] { // entries verbatim
		if _, err := f.Write(append(ln, '\n')); err != nil {
			return nil, err
		}
	}
	return Open(dest)
}

// splitLines splits raw into lines WITHOUT trailing newlines (empty
// trailing line dropped).
func splitLines(raw []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range raw {
		if b == '\n' {
			lines = append(lines, raw[start:i])
			start = i + 1
		}
	}
	if start < len(raw) {
		lines = append(lines, raw[start:])
	}
	return lines
}

// sessionIDFromPath extracts the session id carried by a canonical
// <timestamp>_<uuid>.jsonl file name so the header id always equals the
// file name id — /resume <prefix> matches header ids, so a mismatched
// header would make the fork unaddressable by its own file. Falls back to
// a fresh uuid when the name carries none (hand-built paths in tests).
func sessionIDFromPath(destPath string) string {
	base := strings.TrimSuffix(filepath.Base(destPath), ".jsonl")
	if i := strings.LastIndex(base, "_"); i >= 0 && base[i+1:] != "" {
		return base[i+1:]
	}
	return newUUID()
}
