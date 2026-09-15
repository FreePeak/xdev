package tool

// EditTool: omp's hashline edit UX (PRD §3.6). A call carries the patch text
// in "input" — a "[path#tag]" header, then PUT/CUT/REM/MV op lines over 1-based
// line ranges, then "+" body rows. The same verbs also decode from a
// structured {path, ops} object (decodeEditArgs), which is what the harness's
// tests speak. Ops apply sequentially with line renumbering after each one,
// and validation happens entirely in memory so a bad op writes nothing.

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
//
// The recap in the system prompt is capped at agent.MaxToolDescriptionChars,
// while the parameters schema goes to the provider verbatim — so this is the
// one-line contract, and the grammar itself lives in Parameters().
func (t *EditTool) Description() string {
	return "Line-anchored edits on one file. Send the patch as input: a [path] header (or [path#tag] quoting a previous edit's header), then PUT/CUT/REM/MV op lines, each PUT followed by \"+\" body rows. PUT 3.=5: replaces lines 3-5; PUT 3: one line; PUT <3: / PUT >3: insert before or after line 3; CUT 3 / REM 3.=5: delete; MV new/path.go renames. Read the file before editing it."
}

// Parameters implements Tool. The patch grammar is the model's whole interface
// for this tool; the structured {path, ops} encoding (same verbs, honored by
// decodeEditArgs) is deliberately not advertised: it is what the harness's own
// tests and the argument-repair path speak.
func (t *EditTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "required": ["input"],
  "properties": {
    "input": {
      "type": "string",
      "description": "One patch for one file. Line 1 is a header naming the file: [path] or [path#tag], where the tag is quoted from the header a previous edit of this file printed (never invent it; without a tag the lines must still match what you read). Each op line names 1-based lines of the file as the previous op left it, and a PUT's body rows sit under it:\n  PUT 3.=5:   replace lines 3 through 5 with the rows that follow\n  PUT 3:      replace one line (PUT with no rows deletes the range)\n  PUT <3:     insert the rows before line 3, keeping line 3\n  PUT >3:     insert the rows after line 3, keeping line 3\n  CUT 3       delete one line\n  REM 3.=5:   delete a range\n  MV new.go   rename the file\nA body row is final content carrying exactly one leading \"+\": '++x' writes '+x', a lone '+' writes a blank line, and a leading '-' is plain content, never a deletion (the range an op names says what disappears). Rows may be indented; the whitespace after the '+' is kept verbatim.\nExample:\n[src/a.go#1a2b]\nPUT 3.=4:\n+func main() {\n+\txdev.Run()\nREM 9\nMV cmd/main.go"
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
	// Anchor names a splice point for the "PUT <N:" / "PUT >N:" insert forms
	// ("before" / "after"): the named line is kept, not replaced.
	Anchor string `json:"anchor,omitempty"`
}

// lenientOps decodes the structured "ops" field. Live calls send it three
// ways: the array, that array quoted as a JSON string, and patch text.
type lenientOps struct {
	ops   []editOp
	input string
}

// UnmarshalJSON accepts an op array, a string holding one, or a string
// holding patch text (which parseHashline reads downstream).
func (l *lenientOps) UnmarshalJSON(b []byte) error {
	if err := json.Unmarshal(b, &l.ops); err == nil {
		return nil
	}
	var s string
	if json.Unmarshal(b, &s) != nil {
		return fmt.Errorf("ops must be a list of operations or patch text")
	}
	if looksLikeHashline(s) {
		l.input = s
		return nil
	}
	var inner []editOp
	if json.Unmarshal([]byte(s), &inner) == nil {
		l.ops = inner
		return nil
	}
	return fmt.Errorf("ops is neither a list of operations nor patch text this grammar can read")
}

// editCall is one edit invocation after its argument shape is normalised.
type editCall struct {
	Path string
	Ops  []editOp
}

// editUsageHint is the failure text for a call that names nothing: it teaches
// the accepted shape instead of only rejecting it.
const editUsageHint = `edit: nothing to apply — send the patch as {"input": "[path#tag]\nPUT 3.=5:\n+content"}`

// decodeEditArgs reads an edit call. The advertised interface is the hashline
// patch text in "input"; the structured {path, ops} object is accepted too
// because it carries the same verbs and the harness's tests speak it. The rest
// are repairs for shapes seen in live sessions: the whole arguments blob sent
// as a bare patch string, "ops" quoted as a JSON string, patch text inside
// "ops", and "path" pushed into an op instead of the call.
func decodeEditArgs(args json.RawMessage) (editCall, string) {
	var probe struct {
		Input string     `json:"input"`
		Path  string     `json:"path"`
		Ops   lenientOps `json:"ops"`
	}
	if err := json.Unmarshal(args, &probe); err != nil {
		var whole string
		if json.Unmarshal(args, &whole) == nil && looksLikeHashline(whole) {
			sec, serr := parseHashline(whole, "")
			if serr != nil {
				return editCall{}, serr.Error()
			}
			return editCall{Path: sec.path, Ops: sec.ops}, ""
		}
		return editCall{}, "invalid arguments: " + err.Error() + " — " + strings.TrimPrefix(editUsageHint, "edit: ")
	}
	if text := strings.TrimSpace(probe.Input); text != "" {
		sec, serr := parseHashline(text, strings.TrimSpace(probe.Path))
		if serr != nil {
			return editCall{}, serr.Error()
		}
		if len(probe.Ops.ops) > 0 {
			return editCall{}, "patch text and structured ops cannot be mixed in one call"
		}
		return editCall{Path: sec.path, Ops: sec.ops}, ""
	}
	if text := strings.TrimSpace(probe.Ops.input); text != "" {
		sec, serr := parseHashline(text, strings.TrimSpace(probe.Path))
		if serr != nil {
			return editCall{}, serr.Error()
		}
		return editCall{Path: sec.path, Ops: sec.ops}, ""
	}
	path := strings.TrimSpace(probe.Path)
	if path == "" {
		path = hoistOpPath(args)
	}
	return editCall{Path: path, Ops: probe.Ops.ops}, ""
}

// hoistOpPath recovers a "path" written inside an op object (the shape live
// calls show when the model puts the file in the wrong place).
func hoistOpPath(args json.RawMessage) string {
	var raw struct {
		Ops []map[string]json.RawMessage `json:"ops"`
	}
	if json.Unmarshal(args, &raw) != nil {
		return ""
	}
	for _, op := range raw.Ops {
		for _, key := range []string{"path", "file_path"} {
			var s string
			if v, ok := op[key]; ok && json.Unmarshal(v, &s) == nil && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
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
	a, fail := decodeEditArgs(args)
	if fail != "" {
		return Result{IsError: true, Text: "edit: " + fail}, nil
	}
	if a.Path == "" {
		return Result{IsError: true, Text: editUsageHint}, nil
	}
	if len(a.Ops) == 0 {
		return Result{IsError: true, Text: editUsageHint}, nil
	}
	display, wantTag := splitSnapshotTag(a.Path)
	resolved, err := resolvePath(display)
	if err != nil {
		return Result{IsError: true, Text: fmt.Sprintf("edit: %v", err)}, nil
	}
	if err := CheckProtectedPath(display, resolved); err != nil {
		return Result{IsError: true, Text: "edit: " + err.Error()}, nil
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
	// The read file's dominant line ending, re-applied to the rows PUT
	// writes: ReadLines keeps the "\r" on every row it read, so a row the
	// model typed (which never carries one) would land as a bare LF and leave
	// a CRLF file silently mixed — the defect the live fixture showed.
	crlf := dominantCRLF(lines)
	before := append([]string(nil), lines...)
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
		case "PUT", "CUT", "REM":
			start, end, rerr := normalizeEditRange(op.Range, len(lines))
			if rerr != nil {
				return Result{IsError: true, Text: fmt.Sprintf("edit: op %d (%s): %v", i+1, kind, rerr)}, nil
			}
			// at..keep is the window the op rewrites. An insert anchor makes
			// it empty, which is exactly "splice here, delete nothing": the
			// line the op names survives.
			at, keep := start-1, end
			switch op.Anchor {
			case "":
			case "before":
				if kind != "PUT" {
					return Result{IsError: true, Text: fmt.Sprintf("edit: op %d (%s): only PUT takes an insert anchor", i+1, kind)}, nil
				}
				keep = at
			case "after":
				if kind != "PUT" {
					return Result{IsError: true, Text: fmt.Sprintf("edit: op %d (%s): only PUT takes an insert anchor", i+1, kind)}, nil
				}
				at, keep = end, end
			default:
				return Result{IsError: true, Text: fmt.Sprintf("edit: op %d (%s): unknown insert anchor %q (want \"before\" or \"after\")", i+1, kind, op.Anchor)}, nil
			}
			body := []string(nil)
			if kind == "PUT" {
				body = make([]string, len(op.Lines))
				for j, l := range op.Lines {
					// A body row carries one leading "+" as transport for
					// verbatim content in both encodings, so "++x" means a
					// literal "+x". A leading "-" carries no meaning here and
					// is never stripped.
					row := strings.TrimPrefix(l, "+")
					if crlf && !strings.HasSuffix(row, "\r") {
						row += "\r"
					}
					body[j] = row
				}
			}
			next := make([]string, 0, len(lines)-(keep-at)+len(body))
			next = append(next, lines[:at]...)
			next = append(next, body...)
			next = append(next, lines[keep:]...)
			lines = next
			lineOps++
			if firstEdit == 0 {
				firstEdit = at + 1
			}
			if len(summary) < 3 {
				summary = append(summary, editOpSummary(op, start, end, len(body), keep-at))
			}
		case "MV":
			// Planned above; executed after the line ops.
		default:
			return Result{IsError: true, Text: fmt.Sprintf("edit: op %d: unknown op %q (want PUT, CUT, REM, or MV)", i+1, op.Op)}, nil
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
	details := map[string]any{
		"resolvedPath": finalPath,
		"opsApplied":   len(a.Ops),
		"linesBefore":  linesBefore,
		"linesAfter":   len(lines),
		"diffSummary":  summary,
	}
	// The renderer gets the actual change, not just the op tally: the text
	// above stays what the model was shown, while the frame paints these
	// rows in the theme's diff colours. An MV still diffs as a content
	// change under the path the model named — the move is in the text.
	if diff, ok := UnifiedDiff(outDisplay, stripCR(before), stripCR(lines)); ok {
		details["unifiedDiff"] = diff
	}
	return Result{
		Text:    text,
		Details: details,
	}, nil
}

// stripCR drops the carriage returns ReadLines kept, so a CRLF file's diff
// shows what changed rather than every line differing by one invisible byte.
func stripCR(lines []string) []string {
	if !strings.Contains(strings.Join(lines, ""), "\r") {
		return lines
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = strings.TrimSuffix(l, "\r")
	}
	return out
}

// editOpSummary renders one applied op's change for the result metadata. The
// plain replace keeps the "start-end: +n lines -m lines" spelling the
// transcript and the tests read; an insert anchor says what it did instead,
// because its deleted window is empty by design.
func editOpSummary(op editOp, start, end, added, deleted int) string {
	if op.Anchor != "" {
		return fmt.Sprintf("%d: +%d lines (insert %s)", start, added, op.Anchor)
	}
	return fmt.Sprintf("%d-%d: +%d lines -%d lines", start, end, added, deleted)
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

// dominantCRLF reports whether the read file's rows were mostly CRLF-ended.
// A row ending in "\r" was CRLF-terminated (or is the final row of a CRLF file
// with no trailing newline, which ReadLines cannot distinguish), so a tie goes
// to CRLF; a file with no "\r" anywhere is LF.
func dominantCRLF(lines []string) bool {
	crlf := 0
	for _, l := range lines {
		if strings.HasSuffix(l, "\r") {
			crlf++
		}
	}
	return crlf > 0 && crlf*2 >= len(lines)
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
		if kind != "PUT" && kind != "CUT" && kind != "REM" {
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
