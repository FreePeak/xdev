package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/FreePeak/xdev/internal/config"
)

// FileOffloader is the artifact-offload backend that ships: an oversized tool
// result goes to a file under the xdev data dir, and the conversation keeps
// the head of the output plus a marker naming the file the model can read
// back.
//
// It is the same shape opencode produces (packages/core/src/tool-output.ts):
// the head is kept, the tail is dropped, and the marker says where the rest
// is. The spill is what makes the bound safe — a cap alone would delete output
// the model asked for; a cap plus a file keeps a way to it.
//
// #115 designed the seam (ArtifactOffloader) and left the backend to land
// with it. This is that backend: a local file, not a blob store, because the
// output has to outlive the turn and be readable by the read tool.
type FileOffloader struct {
	// Dir is where spilled outputs are written. Empty means the xdev data
	// dir's tool-output folder.
	Dir string
	// MaxLines is how many lines of the head the stub keeps.
	MaxLines int
	// MaxBytes is how many bytes of the head the stub keeps.
	MaxBytes int
}

// offloadLines / offloadBytes cap the head the stub keeps. opencode's
// tool-output.ts uses the same two numbers (MAX_LINES = 2_000, MAX_BYTES =
// 50 KiB), and the line cap matters as much as the byte one: a result of
// 10 000 short lines is small in bytes and huge in rows.
const (
	offloadLines = 2000
	offloadBytes = 50 * 1024
)

// toolOutputRetention is how long a spilled output stays readable. opencode
// keeps them for seven days (tool-output.ts RETENTION).
const toolOutputRetention = 7 * 24 * time.Hour

// fileOffloaderMu serializes the spill: tool calls run concurrently
// (runTools), and two of them can spill the same output at once — the same
// file, written twice — while the sweep below walks the same directory.
var fileOffloaderMu sync.Mutex

// lastSpillPrune rate-limits the sweep: the directory is walked at most once
// an hour, so a session that spills often does not stat every spilled file on
// every write.
var lastSpillPrune time.Time

// NewFileOffloader returns the default backend: a file under the xdev data
// dir, keeping opencode's two limits.
func NewFileOffloader() FileOffloader {
	return FileOffloader{
		Dir:      filepath.Join(config.DataDir(), "tool-output"),
		MaxLines: offloadLines,
		MaxBytes: offloadBytes,
	}
}

// Offload persists text out of band and returns the stub the conversation
// carries. It declines (ok=false) a result that already fits the limits — the
// cheap majority never reaches the disk.
func (f FileOffloader) Offload(toolName, callID, text string) (string, bool, error) {
	maxLines, maxBytes := f.MaxLines, f.MaxBytes
	if maxLines <= 0 {
		maxLines = offloadLines
	}
	if maxBytes <= 0 {
		maxBytes = offloadBytes
	}
	lines := strings.Split(text, "\n")
	// A text ending in "\n" splits into a trailing empty element; drop it so
	// the count is the number of real lines.
	if strings.HasSuffix(text, "\n") {
		lines = lines[:len(lines)-1]
	}
	if len(lines) <= maxLines && len(text) <= maxBytes {
		return "", false, nil
	}
	kept := lines[:min(len(lines), maxLines)]
	// The byte budget cuts the head short of the line budget: what survives is
	// the head that fits in maxBytes, newline separators included.
	used := 0
	for i, ln := range kept {
		size := len(ln)
		if i > 0 {
			size++ // the newline joining this line to the one before it
		}
		if used+size > maxBytes {
			kept = kept[:i]
			break
		}
		used += size
	}
	shown := "0 lines"
	if len(kept) > 0 {
		shown = fmt.Sprintf("lines 1-%d", len(kept))
	}
	path, err := f.spill(text)
	if err != nil {
		// A failed spill must be a named error, never a silent marker
		// claiming bytes that were not written (#115).
		return "", false, err
	}
	marker := fmt.Sprintf("[showing %s of %d; full output saved to %s]", shown, len(lines), path)
	head := strings.Join(kept, "\n")
	if head == "" {
		return marker, true, nil
	}
	return head + "\n" + marker, true, nil
}

// spill writes the full text of a truncated result to a file the model can
// read back, and returns its path.
//
// The name is the content's hash, so the same output is spilled once however
// many times it is offloaded — a repeated command does not leave a trail of
// copies, and the path in the marker stays valid for the life of the file.
func (f FileOffloader) spill(text string) (string, error) {
	fileOffloaderMu.Lock()
	defer fileOffloaderMu.Unlock()
	dir := f.Dir
	if dir == "" {
		dir = filepath.Join(config.DataDir(), "tool-output")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(text))
	path := filepath.Join(dir, "tool_"+hex.EncodeToString(sum[:6])+".txt")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		return "", err
	}
	pruneSpills(dir)
	return path, nil
}

// pruneSpills drops spilled outputs past the retention window. Without it the
// directory grows for as long as the tool does: every spilled output is the
// only copy of what the model did not get.
func pruneSpills(dir string) {
	now := time.Now()
	if now.Sub(lastSpillPrune) < time.Hour {
		return
	}
	lastSpillPrune = now
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := now.Add(-toolOutputRetention)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		os.Remove(filepath.Join(dir, e.Name()))
	}
}
