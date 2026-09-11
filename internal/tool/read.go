package tool

// ReadTool: pi-schema `read` — line-numbered windows over text files,
// directory listings, binary/image detection (PRD §3.6).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// readDefaultLimit is the default line window (pi's read cap).
	readDefaultLimit = 2000
	// readMaxLineRunes caps the rendered length of a single line; the raw
	// bytes on disk are never touched.
	readMaxLineRunes = 2000
	// readBinaryProbe is how many leading bytes are scanned for NUL to
	// classify a file as binary.
	readBinaryProbe = 8 * 1024
	// readDirMax is how many directory entries a listing shows.
	readDirMax = 30
)

var _ Tool = (*ReadTool)(nil)

// ReadTool reads files as line-numbered windows, with directory and
// binary/image fallbacks.
type ReadTool struct{}

// NewReadTool returns a ReadTool.
func NewReadTool() *ReadTool { return &ReadTool{} }

// Name implements Tool.
func (t *ReadTool) Name() string { return "read" }

// Description implements Tool.
func (t *ReadTool) Description() string {
	return "Read a file. Returns line-numbered lines (N:content); use offset (1-based line) and limit to window large files. Directories return an entry listing — open files directly instead. Binary and image files are detected and reported, not dumped."
}

// Parameters implements Tool.
func (t *ReadTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "required": ["path"],
  "properties": {
    "path": {"type": "string", "description": "File path to read"},
    "offset": {"type": "integer", "minimum": 1, "description": "1-based line number to start from"},
    "limit": {"type": "integer", "minimum": 1, "description": "Maximum number of lines to return (default 2000)"}
  }
}`)
}

type readArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// Execute implements Tool.
func (t *ReadTool) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{IsError: true, Text: fmt.Sprintf("read: canceled: %v", err)}, nil
	}
	var a readArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{}, fmt.Errorf("read: invalid arguments: %w", err)
	}
	if a.Path == "" {
		return Result{IsError: true, Text: "read: path is required"}, nil
	}
	// URI seam: skill:// and memory:// resolve to synthesized text, not
	// files (omp exposes both through read).
	if text, matched, uerr := resolveURI(a.Path); matched {
		if uerr != nil {
			return Result{IsError: true, Text: "read: " + uerr.Error()}, nil
		}
		return readURIText(a.Path, text, a.Offset, a.Limit), nil
	}
	resolved, err := resolvePath(a.Path)
	if err != nil {
		return Result{IsError: true, Text: fmt.Sprintf("read: %v", err)}, nil
	}
	st, err := os.Stat(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			return Result{IsError: true, Text: fmt.Sprintf("file not found: %s", a.Path)}, nil
		}
		return Result{IsError: true, Text: fmt.Sprintf("read: %v", err)}, nil
	}
	if st.IsDir() {
		return readDirectory(resolved), nil
	}

	head, fileSize, err := readHead(resolved, readBinaryProbe)
	if err != nil {
		return Result{IsError: true, Text: fmt.Sprintf("read: %v", err)}, nil
	}
	if isImageExt(resolved) {
		return readImageResult(a.Path, resolved, head, fileSize), nil
	}
	if bytes.IndexByte(head, 0) >= 0 {
		mime := http.DetectContentType(head[:min(int64(len(head)), 512)])
		return Result{
			Text: fmt.Sprintf("binary file: %s (%d bytes, %s)", a.Path, fileSize, mime),
			Details: map[string]any{
				"resolvedPath": resolved,
				"lineCount":    0,
				"totalLines":   0,
				"truncated":    false,
				"binary":       true,
				"mime":         mime,
				"size":         fileSize,
			},
		}, nil
	}
	return readTextWindow(a.Path, resolved, a.Offset, a.Limit)
}

// readHead returns up to n leading bytes plus the full file size.
func readHead(path string, n int) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	buf := make([]byte, min(int64(n), st.Size()))
	if _, err := io.ReadFull(f, buf); err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, 0, err
	}
	return buf, st.Size(), nil
}

// readTextWindow renders the offset/limit window with real 1-based line
// numbers and the omp continuation footer.
func readTextWindow(display, resolved string, offset, limit int) (Result, error) {
	lines, err := ReadLines(resolved)
	if err != nil {
		return Result{IsError: true, Text: fmt.Sprintf("read: %v", err)}, nil
	}
	total := len(lines)
	details := map[string]any{
		"resolvedPath": resolved,
		"lineCount":    0,
		"totalLines":   total,
		"truncated":    false,
	}
	if total == 0 {
		return Result{Text: "(empty file)", Details: details}, nil
	}

	start := max(offset, 1)
	if start > total {
		return Result{
			Text:    fmt.Sprintf("(no lines in range: file has %d lines, offset %d is past the end)", total, offset),
			Details: details,
		}, nil
	}
	if limit <= 0 {
		limit = readDefaultLimit
	}
	end := min(start+limit-1, total)

	var b strings.Builder
	for i, line := range lines[start-1 : end] {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(strconv.Itoa(start + i))
		b.WriteByte(':')
		b.WriteString(readTruncateLine(line))
	}
	truncated := end < total
	if truncated {
		fmt.Fprintf(&b, "\n[Showing lines %d-%d of %d. Use :%d to continue]", start, end, total, end+1)
	}
	details["lineCount"] = end - start + 1
	details["truncated"] = truncated
	return Result{Text: b.String(), Details: details}, nil
}

// readTruncateLine visually truncates over-long lines; raw file bytes are
// preserved on disk.
func readTruncateLine(s string) string {
	if len(s) <= readMaxLineRunes {
		return s
	}
	runes := []rune(s)
	if len(runes) <= readMaxLineRunes {
		return s
	}
	return string(runes[:readMaxLineRunes]) + "…"
}

// readDirectory rejects a directory read with an omp-style entry listing and
// an intent hint.
func readDirectory(resolved string) Result {
	entries, err := os.ReadDir(resolved)
	if err != nil {
		return Result{
			IsError: true,
			Text:    fmt.Sprintf("read: %v", err),
			Details: map[string]any{"isDirectory": true, "resolvedPath": resolved},
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
	})

	var b strings.Builder
	fmt.Fprintf(&b, "%s is a directory (%d entries):\n", resolved, len(entries))
	shown := entries
	if len(shown) > readDirMax {
		shown = shown[:readDirMax]
	}
	for _, e := range shown {
		info, ierr := e.Info()
		if ierr != nil {
			fmt.Fprintf(&b, "- %s\n", e.Name())
			continue
		}
		if e.IsDir() {
			fmt.Fprintf(&b, "- %s/ %s\n", e.Name(), readEntryAge(info.ModTime()))
		} else {
			fmt.Fprintf(&b, "- %s %s %s\n", e.Name(), readEntrySize(info.Size()), readEntryAge(info.ModTime()))
		}
	}
	if more := len(entries) - len(shown); more > 0 {
		fmt.Fprintf(&b, "... and %d more\n", more)
	}
	b.WriteString("Open files directly — read a specific file inside this directory.")
	return Result{
		IsError: true,
		Text:    b.String(),
		Details: map[string]any{"isDirectory": true, "resolvedPath": resolved},
	}
}

// readImageResult reports an image file without dumping bytes (no vision in
// the MVP); dimensions come from the stdlib header decoders when available.
func readImageResult(display, resolved string, head []byte, size int64) Result {
	dims := ""
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(head)); err == nil {
		dims = fmt.Sprintf("%dx%d, ", cfg.Width, cfg.Height)
	}
	mime := http.DetectContentType(head[:min(int64(len(head)), 512)])
	return Result{
		Text: fmt.Sprintf("image file: %s (%s%d bytes)", display, dims, size),
		Details: map[string]any{
			"resolvedPath": resolved,
			"lineCount":    0,
			"totalLines":   0,
			"truncated":    false,
			"image":        true,
			"mime":         mime,
			"size":         size,
		},
	}
}

func isImageExt(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return true
	default:
		return false
	}
}

// readEntryAge renders a modtime the way omp listings do ("2h ago").
func readEntryAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", max(int(d.Seconds()), 1))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dmo ago", int(d.Hours()/(24*30)))
	default:
		return fmt.Sprintf("%dy ago", int(d.Hours()/(24*365)))
	}
}

// readEntrySize renders a humanized size ("13 B", "1.2 KB").
func readEntrySize(n int64) string {
	f := float64(n)
	unit := "B"
	for _, next := range [...]string{"KB", "MB", "GB", "TB", "PB"} {
		if f < 1024 {
			break
		}
		f /= 1024
		unit = next
	}
	if unit == "B" {
		return fmt.Sprintf("%.0f %s", f, unit)
	}
	return fmt.Sprintf("%.1f %s", f, unit)
}

// readURIText renders synthesized URI content with the same line-number
// windowing the file path uses, so the model sees one shape everywhere.
func readURIText(uri, text string, offset, limit int) Result {
	if offset <= 0 {
		offset = 1
	}
	if limit <= 0 {
		limit = readDefaultLimit
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1] // trailing newline is not a line
	}
	total := len(lines)
	start := offset - 1
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%d lines)\n", uri, total)
	for i := start; i < end; i++ {
		fmt.Fprintf(&b, "%d:%s\n", i+1, lines[i])
	}
	if end < total {
		fmt.Fprintf(&b, "… (%d more lines; use offset=%d)\n", total-end, end+1)
	}
	return Result{Text: strings.TrimRight(b.String(), "\n")}
}
