package tool

// WriteTool: pi-schema `write` — atomic file creation/overwrite with parent
// directory creation (PRD §3.6).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var _ Tool = (*WriteTool)(nil)

// WriteTool creates or overwrites files atomically. Successful writes
// refresh the freshness record so a following edit is not flagged stale.
type WriteTool struct {
	reg *Registry
}

// NewWriteTool returns a WriteTool.
func NewWriteTool() *WriteTool { return &WriteTool{} }

// setRegistry receives the owning registry from Registry.Register.
func (t *WriteTool) setRegistry(r *Registry) { t.reg = r }

// Name implements Tool.
func (t *WriteTool) Name() string { return "write" }

// Description implements Tool.
func (t *WriteTool) Description() string {
	return "Create or overwrite a file with content, creating parent directories as needed. The write is atomic (temp file + rename)."
}

// Parameters implements Tool.
func (t *WriteTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "required": ["path", "content"],
  "properties": {
    "path": {"type": "string", "description": "File path to write"},
    "content": {"type": "string", "description": "Full file content"},
    "overwrite": {"type": "boolean", "description": "Advisory in this MVP: overwrites of existing files are always permitted"}
  }
}`)
}

type writeArgs struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Overwrite bool   `json:"overwrite,omitempty"`
}

// Execute implements Tool.
func (t *WriteTool) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{IsError: true, Text: fmt.Sprintf("write: canceled: %v", err)}, nil
	}
	var a writeArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{}, fmt.Errorf("write: invalid arguments: %w", err)
	}
	if a.Path == "" {
		return Result{IsError: true, Text: "write: path is required"}, nil
	}
	// xd://propose etc.: registered URI write devices take this path
	// before the FS layer (M11 #36). handled-but-error is a tool error,
	// never a literal file write.
	if text, handled, werr := WriteURI(a.Path, a.Content); handled {
		if werr != nil {
			return Result{IsError: true, Text: "write: " + werr.Error()}, nil
		}
		return Result{Text: text}, nil
	}
	resolved, err := resolvePath(a.Path)
	if err != nil {
		return Result{IsError: true, Text: fmt.Sprintf("write: %v", err)}, nil
	}
	if err := CheckProtectedPath(a.Path, resolved); err != nil {
		return Result{IsError: true, Text: "write: " + err.Error()}, nil
	}
	if st, err := os.Stat(resolved); err == nil && st.IsDir() {
		return Result{IsError: true, Text: fmt.Sprintf("write: %s is a directory", a.Path)}, nil
	}

	priorBytes := int64(-1)
	if st, err := os.Stat(resolved); err == nil {
		priorBytes = st.Size()
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return Result{IsError: true, Text: fmt.Sprintf("write: %v", err)}, nil
	}
	data := []byte(a.Content)
	if err := writeBytesAtomic(resolved, data); err != nil {
		return Result{IsError: true, Text: fmt.Sprintf("write: %v", err)}, nil
	}

	// Refresh the freshness record: the model's next edit against this
	// path is anchored on the content just written.
	wlines := strings.Split(strings.TrimSuffix(a.Content, "\n"), "\n")
	if a.Content == "" {
		wlines = []string{}
	}
	t.reg.recordSnapshot(resolved, linesHash(wlines), nil)

	details := map[string]any{
		"resolvedPath": resolved,
		"bytesWritten": len(data),
		"created":      priorBytes < 0,
	}
	if priorBytes >= 0 {
		// MVP has no read-before-write gate; surface what was replaced.
		details["priorBytes"] = priorBytes
	}
	return Result{
		Text:    fmt.Sprintf("Wrote %s (%d bytes, %d lines)", a.Path, len(data), writeContentLines(a.Content)),
		Details: details,
	}, nil
}

// writeContentLines counts lines the same way ReadLines does: a single
// trailing newline does not create an extra empty line.
func writeContentLines(s string) int {
	if s == "" {
		return 0
	}
	return len(strings.Split(strings.TrimSuffix(s, "\n"), "\n"))
}
