package tool

// EditTool: omp hashline edit UX ported to JSON ops (PRD §3.6). PUT/CUT/MV
// over 1-based inclusive line ranges, applied sequentially with line
// renumbering after each op; validation happens entirely in memory so a bad
// op writes nothing.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// editSnapshotRadius bounds the confirmation snapshot: center±radius rows
// (7 numbered lines plus edge ellipses) around the first edit region.
const editSnapshotRadius = 3

var _ Tool = (*EditTool)(nil)

// EditTool edits files with ordered line operations.
type EditTool struct{}

// NewEditTool returns an EditTool.
func NewEditTool() *EditTool { return &EditTool{} }

// Name implements Tool.
func (t *EditTool) Name() string { return "edit" }

// Description implements Tool.
func (t *EditTool) Description() string {
	return "Ordered line ops on one file; each op's line numbers refer to the state AFTER the previous op. PUT replaces a range or anchors an insert (<N before, >N after, N* = the block starting at N); body rows are final content already prefixed \"+\", so a literal leading \"-\"/\"+\" must be doubled. CUT deletes, MV renames, REM removes. Numbers come from a fresh read; unanchorable ops are rejected."
}

// Parameters implements Tool.
func (t *EditTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "required": ["path", "ops"],
  "properties": {
    "path": {"type": "string", "description": "File to edit"},
    "ops": {
      "type": "array",
      "minItems": 1,
      "description": "Line operations, applied in order",
      "items": {
        "type": "object",
        "required": ["op"],
        "properties": {
          "op": {"type": "string", "enum": ["PUT", "CUT", "MV"], "description": "Operation kind"},
          "range": {
            "type": "object",
            "description": "1-based inclusive line range; {\"line\":N} is single-line shorthand",
            "properties": {
              "start": {"type": "integer", "minimum": 1, "description": "First line"},
              "end": {"type": "integer", "minimum": 1, "description": "Last line (inclusive; defaults to start)"},
              "line": {"type": "integer", "minimum": 1, "description": "Single line"}
            }
          },
          "lines": {
            "type": "array",
            "items": {"type": "string"},
            "description": "PUT body: final content of each line, each prefixed with '+' (use '++' for a literal leading '+')"
          },
          "dest": {"type": "string", "description": "MV: destination path"}
        }
      }
    }
  }
}`)
}

// editRange is a 1-based inclusive line range; Line is single-line shorthand.
type editRange struct {
	Start int `json:"start,omitempty"`
	End   int `json:"end,omitempty"`
	Line  int `json:"line,omitempty"`
}

// editOp is one operation in an edit call.
type editOp struct {
	Op    string     `json:"op"`
	Range *editRange `json:"range,omitempty"`
	Lines []string   `json:"lines,omitempty"`
	Dest  string     `json:"dest,omitempty"`
}

type editArgs struct {
	Path string   `json:"path"`
	Ops  []editOp `json:"ops"`
}

// mvPlan is a validated MV destination plus its 1-based op index.
type mvPlan struct {
	index   int
	dest    string
	display string
}

// Execute implements Tool.
func (t *EditTool) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{IsError: true, Text: fmt.Sprintf("edit: canceled: %v", err)}, nil
	}
	var a editArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{}, fmt.Errorf("edit: invalid arguments: %w", err)
	}
	if a.Path == "" {
		return Result{IsError: true, Text: "edit: path is required"}, nil
	}
	if len(a.Ops) == 0 {
		return Result{IsError: true, Text: "edit: no ops provided"}, nil
	}
	resolved, err := resolvePath(a.Path)
	if err != nil {
		return Result{IsError: true, Text: fmt.Sprintf("edit: %v", err)}, nil
	}
	lines, err := ReadLines(resolved)
	if err != nil {
		return Result{IsError: true, Text: fmt.Sprintf("edit: cannot read %s: %v", a.Path, err)}, nil
	}
	linesBefore := len(lines)

	// Validate MV destinations up front so a bad dest cannot leave a
	// half-applied edit (line ops written, move failed).
	moves := make([]mvPlan, 0, len(a.Ops))
	for i, op := range a.Ops {
		if !strings.EqualFold(op.Op, "MV") {
			continue
		}
		if op.Dest == "" {
			return Result{IsError: true, Text: fmt.Sprintf("edit: op %d (MV): dest is required", i+1)}, nil
		}
		dest, derr := resolvePath(op.Dest)
		if derr != nil {
			return Result{IsError: true, Text: fmt.Sprintf("edit: op %d (MV): %v", i+1, derr)}, nil
		}
		if st, serr := os.Stat(dest); serr == nil && st.IsDir() {
			return Result{IsError: true, Text: fmt.Sprintf("edit: op %d (MV): destination %s is an existing directory", i+1, op.Dest)}, nil
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return Result{IsError: true, Text: fmt.Sprintf("edit: op %d (MV): %v", i+1, err)}, nil
		}
		moves = append(moves, mvPlan{index: i + 1, dest: dest, display: op.Dest})
	}

	// Apply line ops in memory against the evolving state; every op's
	// numbers refer to the result of the previous op. Any failure returns
	// here, before the file is touched.
	summary := make([]string, 0, 3)
	firstEdit := 0
	lineOps := 0
	for i, op := range a.Ops {
		switch kind := strings.ToUpper(op.Op); kind {
		case "PUT", "CUT":
			start, end, rerr := normalizeEditRange(op.Range, len(lines))
			if rerr != nil {
				return Result{IsError: true, Text: fmt.Sprintf("edit: op %d (%s): %v", i+1, kind, rerr)}, nil
			}
			body := []string(nil)
			if kind == "PUT" {
				body = make([]string, len(op.Lines))
				for j, l := range op.Lines {
					// The leading "+" is JSON transport for a verbatim
					// line; "++x" means a literal "+x".
					body[j] = strings.TrimPrefix(l, "+")
				}
			}
			next := make([]string, 0, len(lines)-(end-start+1)+len(body))
			next = append(next, lines[:start-1]...)
			next = append(next, body...)
			next = append(next, lines[end:]...)
			lines = next
			lineOps++
			if firstEdit == 0 {
				firstEdit = start
			}
			if len(summary) < 3 {
				summary = append(summary, fmt.Sprintf("%d-%d: +%d lines -%d lines", start, end, len(body), end-start+1))
			}
		case "MV":
			// Planned above; executed after the line ops.
		default:
			return Result{IsError: true, Text: fmt.Sprintf("edit: op %d: unknown op %q (want PUT, CUT, or MV)", i+1, op.Op)}, nil
		}
	}

	if lineOps > 0 {
		if err := WriteLinesAtomic(resolved, lines); err != nil {
			return Result{IsError: true, Text: fmt.Sprintf("edit: write failed: %v", err)}, nil
		}
	}

	finalPath := resolved
	display := a.Path
	for _, mv := range moves {
		if mv.dest == finalPath {
			continue
		}
		if err := movePath(finalPath, mv.dest); err != nil {
			return Result{IsError: true, Text: fmt.Sprintf("edit: op %d (MV): %v", mv.index, err)}, nil
		}
		finalPath = mv.dest
		display = mv.display
	}

	raw := ""
	if len(lines) > 0 {
		raw = strings.Join(lines, "\n") + "\n"
	}
	text := fmt.Sprintf("[%s#%s]", display, ShortHash(raw))
	if len(lines) == 0 {
		text += "\n(file is now empty)"
	} else {
		text += "\n" + RenderWindow(lines, max(firstEdit, 1), editSnapshotRadius)
	}
	if finalPath != resolved {
		text = fmt.Sprintf("Moved %s to %s\n", a.Path, display) + text
	}

	return Result{
		Text: text,
		Details: map[string]any{
			"resolvedPath": finalPath,
			"opsApplied":   len(a.Ops),
			"linesBefore":  linesBefore,
			"linesAfter":   len(lines),
			"diffSummary":  summary,
		},
	}, nil
}

// normalizeEditRange resolves {line} / {start,end} shorthands and bounds-checks
// against the current (post-previous-op) line count.
func normalizeEditRange(r *editRange, total int) (start, end int, err error) {
	if r == nil {
		return 0, 0, errors.New("range is required")
	}
	switch {
	case r.Line != 0:
		start, end = r.Line, r.Line
	case r.End != 0:
		start, end = r.Start, r.End
	default:
		start, end = r.Start, r.Start
	}
	if start < 1 || end < start {
		return 0, 0, fmt.Errorf("invalid range %d-%d (1-based, end >= start)", start, end)
	}
	if end > total {
		return 0, 0, fmt.Errorf("range %d-%d out of bounds: file has %d lines", start, end, total)
	}
	return start, end, nil
}

// movePath renames src to dst, falling back to copy+delete across devices.
func movePath(src, dst string) error {
	defer func() { // both sides of a rename are stale regardless of outcome
		SharedFSCache().Invalidate(src)
		SharedFSCache().Invalidate(dst)
	}()
	if err := os.Rename(src, dst); err != nil {
		var linkErr *os.LinkError
		if errors.As(err, &linkErr) && errors.Is(linkErr.Err, syscall.EXDEV) {
			data, rerr := os.ReadFile(src)
			if rerr != nil {
				return rerr
			}
			if werr := writeBytesAtomic(dst, data); werr != nil {
				return werr
			}
			return os.Remove(src)
		}
		return err
	}
	return nil
}
