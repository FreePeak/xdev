package agent

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/FreePeak/xdev/internal/session"
)

// ReadChildTranscript reads a subagent child's session file and returns its
// transcript entries for a read-only viewer.
//
// It reuses the existing parse path instead of writing a second JSONL parser:
// session.Open (internal/session/store.go:144) opens the file with os.Open and
// never installs a writer, so nothing is created, appended, rewritten or
// deleted, and no ownership lease is taken — the lease belongs to
// session.OpenForWrite (internal/session/lease.go:239), which a reader must
// not use. The entries come from the store's own accessor
// (internal/session/store.go:482) and the role/text extraction is the one
// Hub.Transcript already uses (internal/agent/hub.go:440, extraction at
// internal/agent/hub.go:455-466 via messageText, internal/agent/advisor.go:398),
// so a viewer's rows look exactly like the live hub's rows.
//
// A file that is still being written is readable: session.Open skips an
// unterminated trailing line (internal/session/store.go:158-177) instead of
// failing the load, and it never repairs the file at read time.
//
// A file without a session header is not a session transcript — the same rule
// the session lister applies (internal/session/listing.go:312).
func ReadChildTranscript(path string) ([]TranscriptEntry, error) {
	if path == "" {
		return nil, errors.New("agent: read child transcript: empty path")
	}
	store, err := session.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("agent: child transcript %s: no such file", path)
		}
		if !hasSessionHeader(path) {
			return nil, fmt.Errorf("agent: child transcript %s: not a session transcript", path)
		}
		return nil, fmt.Errorf("agent: child transcript %s: %w", path, err)
	}
	defer store.Close()
	if store.ID() == "" {
		return nil, fmt.Errorf("agent: child transcript %s: not a session transcript", path)
	}
	var out []TranscriptEntry
	seq := 0
	for _, e := range store.Entries() {
		me, isMsg := e.(*session.MessageEntry)
		if !isMsg {
			continue
		}
		out = append(out, TranscriptEntry{
			Seq:  seq,
			Role: string(me.Message.Role),
			Text: messageText(me.Message),
		})
		seq++
	}
	return out, nil
}

// hasSessionHeader reports whether the file carries a session header — the
// header is line 2 when line 1 is the title slot, line 1 otherwise, exactly
// as the session lister reads it (internal/session/listing.go:288-313). It
// exists only to tell "this file is not a session transcript" apart from
// "this session transcript is broken", so the reader's error names the real
// problem. It reads two lines and nothing else.
func hasSessionHeader(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 8*1024)
	var lines [][]byte
	for len(lines) < 2 {
		line, err := r.ReadBytes('\n')
		line = bytes.TrimRight(line, "\r\n")
		if len(line) > 0 {
			lines = append(lines, line)
		}
		if err != nil {
			break
		}
	}
	for _, idx := range []int{1, 0} {
		if idx >= len(lines) {
			continue
		}
		if _, ok := session.ParseHeader(lines[idx]); ok {
			return true
		}
	}
	return false
}
