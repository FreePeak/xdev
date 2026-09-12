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
	"slices"
	"strings"
	"syscall"
)

// editSnapshotRadius bounds the confirmation snapshot: center±radius rows
// (7 numbered lines plus edge ellipses) around the first edit region.
const editSnapshotRadius = 3

var _ Tool = (*EditTool)(nil)

// EditTool edits files with ordered line operations. When registered in a
// Registry it participates in the freshness guard: the registry remembers
// what read/write last saw for each path, a mismatching edit is re-anchored
// by exact text where possible, and unrecoverable staleness is rejected
// with a fresh-read advisory.
type EditTool struct {
	reg *Registry
}

// NewEditTool returns an EditTool.
func NewEditTool() *EditTool { return &EditTool{} }

// setRegistry receives the owning registry from Registry.Register.
func (t *EditTool) setRegistry(r *Registry) { t.reg = r }

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
	display, wantTag := splitSnapshotTag(a.Path)
	resolved, err := resolvePath(display)
	if err != nil {
		return Result{IsError: true, Text: fmt.Sprintf("edit: %v", err)}, nil
	}
	lines, err := ReadLines(resolved)
	if err != nil {
		return Result{IsError: true, Text: fmt.Sprintf("edit: cannot read %s: %v", display, err)}, nil
	}
	// Notebooks (M13 #47) are edited through the virtual text read renders,
	// never through their raw JSON; the write path below re-serializes cells
	// and preserves every field the edit did not touch.
	var nb *notebookDoc
	if isNotebookPath(resolved) {
		if doc, ok := notebookFromLines(lines); ok {
			virtual := doc.renderLines()
			if msg := notebookRawJSONRefusal(t.reg, resolved, display, wantTag, lines, virtual); msg != "" {
				return Result{IsError: true, Text: msg}, nil
			}
			nb = doc
			lines = virtual
		}
	}
	linesBefore := len(lines)

	// Freshness guard + arg repair: compare the file against what read/
	// write last recorded; on a stale match, re-anchor ops by exact text
	// and only reject when recovery is impossible.
	recovered, err := t.checkFreshness(resolved, display, wantTag, a.Ops, lines)
	if err != nil {
		return Result{IsError: true, Text: err.Error()}, nil
	}
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
		if nb != nil {
			out, serr := nb.serializeVirtualText(lines)
			if serr != nil {
				return Result{IsError: true, Text: fmt.Sprintf("edit: %s: %v", display, serr)}, nil
			}
			if err := writeBytesAtomic(resolved, out); err != nil {
				return Result{IsError: true, Text: fmt.Sprintf("edit: write failed: %v", err)}, nil
			}
		} else if err := WriteLinesAtomic(resolved, lines); err != nil {
			return Result{IsError: true, Text: fmt.Sprintf("edit: write failed: %v", err)}, nil
		}
	}

	finalPath := resolved
	outDisplay := display
	for _, mv := range moves {
		if mv.dest == finalPath {
			continue
		}
		if err := movePath(finalPath, mv.dest); err != nil {
			return Result{IsError: true, Text: fmt.Sprintf("edit: op %d (MV): %v", mv.index, err)}, nil
		}
		finalPath = mv.dest
		outDisplay = mv.display
	}

	raw := ""
	if len(lines) > 0 {
		raw = strings.Join(lines, "\n") + "\n"
	}
	text := fmt.Sprintf("[%s#%s]", outDisplay, ShortHash(raw))
	if len(lines) == 0 {
		text += "\n(file is now empty)"
	} else {
		text += "\n" + RenderWindow(lines, max(firstEdit, 1), editSnapshotRadius)
	}
	if finalPath != resolved {
		text = fmt.Sprintf("Moved %s to %s\n", a.Path, outDisplay) + text
	}
	if recovered != "" {
		text = recovered + "\n" + text
	}
	// Record the post-edit state so a following edit against this path is
	// anchored on what the model just saw. An MV moves the record.
	full := make(map[int]string, len(lines))
	for i, l := range lines {
		full[i+1] = l
	}
	t.reg.recordSnapshot(finalPath, linesHash(lines), full)
	if finalPath != resolved {
		t.reg.forgetSnapshot(resolved)
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

// hashTag is the 4-char snapshot anchor for a full content hash.
func hashTag(h string) string {
	if len(h) < 4 {
		return h
	}
	return h[:4]
}

// checkFreshness validates the current file against the registry's
// read/write record for the path and repairs stale line numbers by exact
// text where it can. It returns a recovery note ("" when nothing needed
// repairing) or an error when the edit must not proceed.
//
// Rules, in order:
//   - A quoted tag equal to the current snapshot tag → numbers as-is.
//   - A quoted tag equal to the recorded read's tag → the model's numbers
//     refer to that read: verify/re-anchor each op by its recorded text.
//   - No tag: hash equality is freshness; a mismatch re-anchors by text.
//   - No record at all → unchanged behavior (bounds checks only).
//   - Any op whose recorded text is missing, ambiguous, or was never
//     rendered → reject with a fresh-read advisory.
func (t *EditTool) checkFreshness(resolved, display, wantTag string, ops []editOp, cur []string) (string, error) {
	rec, hasRec := t.reg.snapshot(resolved)
	curHash := linesHash(cur)
	curTag := hashTag(curHash)

	if wantTag != "" {
		switch {
		case wantTag == curTag:
			return "", nil
		case hasRec && rec.tag() == wantTag:
			// Stale tag that identifies the recorded read: repair below.
		default:
			return "", fmt.Errorf("edit: %s changed since it was read (snapshot tag %s, current %s) — re-read the file and redo the edit with fresh line numbers", display, wantTag, curTag)
		}
	} else if !hasRec || rec.hash == curHash {
		// No record, or the file is exactly as recorded: nothing to guard.
		return "", nil
	}

	notes := make([]string, 0, len(ops))
	for i := range ops {
		op := &ops[i]
		kind := strings.ToUpper(op.Op)
		if kind != "PUT" && kind != "CUT" {
			continue
		}
		start, end, ok := editRangeBounds(op.Range)
		if !ok {
			// Malformed range: normalizeEditRange reports it as before.
			continue
		}
		exp, covered := rec.window(start, end)
		if !covered {
			return "", fmt.Errorf("edit: %s changed since it was read; op %d (%s) targets lines %d-%d that the last read or write did not record — re-read the file and redo the edit", display, i+1, kind, start, end)
		}
		hits := lineRuns(cur, exp)
		switch {
		case len(hits) == 0:
			return "", fmt.Errorf("edit: %s changed since it was read; op %d (%s) referenced lines %d-%d (%s) which no longer exist — re-read the file and redo the edit", display, i+1, kind, start, end, linePreview(exp))
		case slices.Contains(hits, start-1):
			// The numbers still point at exactly this text: accept as-is.
		case len(hits) == 1:
			op.Range = &editRange{Start: hits[0] + 1, End: hits[0] + len(exp)}
			notes = append(notes, fmt.Sprintf("op %d (%s): lines %d-%d → %d-%d", i+1, kind, start, end, hits[0]+1, hits[0]+len(exp)))
		default:
			return "", fmt.Errorf("edit: %s changed since it was read; op %d (%s) referenced lines %d-%d (%s) which now match %d places — re-read the file and redo the edit", display, i+1, kind, start, end, linePreview(exp), len(hits))
		}
	}
	if len(notes) == 0 {
		return "", nil
	}
	return "edit: file changed since it was read; recovered by exact text:\n  " + strings.Join(notes, "\n  "), nil
}

// editRangeBounds resolves the {line}/{start,end} shorthands without
// bounds-checking, reporting false for malformed ranges.
func editRangeBounds(r *editRange) (start, end int, ok bool) {
	if r == nil {
		return 0, 0, false
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
		return 0, 0, false
	}
	return start, end, true
}

// lineRuns returns the 0-based start indexes where exp occurs verbatim in
// cur (every occurrence — uniqueness is what makes a re-anchor safe).
func lineRuns(cur, exp []string) []int {
	if len(exp) == 0 || len(exp) > len(cur) {
		return nil
	}
	var hits []int
	for i := 0; i+len(exp) <= len(cur); i++ {
		match := true
		for j := range exp {
			if cur[i+j] != exp[j] {
				match = false
				break
			}
		}
		if match {
			hits = append(hits, i)
		}
	}
	return hits
}

// linePreview renders the first recorded line of an op's text for error
// messages, truncated to keep the message one line.
func linePreview(exp []string) string {
	s := exp[0]
	if len(s) > 40 {
		s = s[:37] + "…"
	}
	if len(exp) > 1 {
		return fmt.Sprintf("%q +%d more lines", s, len(exp)-1)
	}
	return fmt.Sprintf("%q", s)
}
