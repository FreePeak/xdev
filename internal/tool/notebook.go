package tool

// Jupyter notebook (.ipynb) virtual text.
//
// `read` serves a notebook as editable cell blocks instead of raw JSON:
//
//	# %% [code] cell:0
//	<source>
//	# %%[out] execution_count: 3
//	# %%[out] stream stdout: hello
//	# %%[out] display_data: text/plain, image/png
//
// `edit` parses that text back into notebook JSON, carrying over every field of
// the cell a marker names (metadata, outputs, execution_count, id) plus every
// other top-level field untouched. Execution is deliberately absent: cells run
// through the `eval` tool, never through read or edit.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	// notebookCellSourceMax caps the source lines rendered per cell. A
	// truncated cell is marked, and write-back refuses it (an edit against
	// a shortened view would silently drop the omitted lines).
	notebookCellSourceMax = 200
	// notebookOutputsMax caps the outputs summarized per cell.
	notebookOutputsMax = 10
	// notebookMimesMax caps the mime types listed for display_data and
	// execute_result outputs.
	notebookMimesMax = 6
	// notebookOutputMaxRunes caps one stream/error summary line.
	notebookOutputMaxRunes = 500
	// notebookLargeBytes is the notebook size above which outputs are
	// summarized per cell (one line) instead of inlined output by output.
	notebookLargeBytes = 2 << 20
)

// notebookMarkerRe matches a cell marker, "# %% [code] cell:0". The index is
// optional on parse: a marker without one (or with an already-used index)
// starts a brand-new cell.
var notebookMarkerRe = regexp.MustCompile(`^# %% \[(code|markdown|raw)\](?: cell:(\d+))?$`)

const (
	// notebookOutputPrefix marks a display-only output summary line. Source
	// lines that would look like one are escaped on render, so parse is
	// unambiguous.
	notebookOutputPrefix = "# %%[out] "
	// notebookSourceTruncatedPrefix flags source the render dropped; parse
	// refuses to write a cell carrying it back.
	notebookSourceTruncatedPrefix = "# %%[truncated] "
)

// isNotebookPath reports whether the path is a Jupyter notebook.
func isNotebookPath(p string) bool { return strings.EqualFold(filepath.Ext(p), ".ipynb") }

// notebookDoc is a parsed notebook: the top-level object and every cell kept as
// raw JSON, so untouched values are re-emitted byte-for-byte.
type notebookDoc struct {
	top   map[string]json.RawMessage
	cells []map[string]json.RawMessage
	minor int
	size  int
}

// parseNotebook parses data as an .ipynb document. ok is false for anything
// that is not notebook JSON (a broken or half-written file stays editable as
// plain text rather than dead-ending every tool).
func parseNotebook(data []byte) (doc *notebookDoc, ok bool) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, false
	}
	rawCells, present := top["cells"]
	if !present {
		return nil, false
	}
	var cells []map[string]json.RawMessage
	if err := json.Unmarshal(rawCells, &cells); err != nil {
		return nil, false
	}
	var minor int
	if raw, present := top["nbformat_minor"]; present {
		_ = json.Unmarshal(raw, &minor)
	}
	return &notebookDoc{top: top, cells: cells, minor: minor, size: len(data)}, true
}

// notebookFromDisk parses the notebook at path. ok is false when the file
// cannot be read or is not notebook JSON.
func notebookFromDisk(path string) (*notebookDoc, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return parseNotebook(data)
}

// notebookFromLines parses already-read file lines as a notebook.
func notebookFromLines(lines []string) (*notebookDoc, bool) {
	return parseNotebook([]byte(strings.Join(lines, "\n")))
}

// renderLines renders the notebook as the virtual text read serves. Outputs are
// summarized (never inlined raw) and capped; a notebook over
// notebookLargeBytes collapses each cell's outputs to one line.
func (d *notebookDoc) renderLines() []string {
	inline := d.size <= notebookLargeBytes
	out := make([]string, 0, len(d.cells)*4)
	for i, cell := range d.cells {
		typ := notebookCellType(cell)
		out = append(out, fmt.Sprintf("# %%%% [%s] cell:%d", typ, i))
		out = append(out, notebookSourceLines(notebookCellSource(cell))...)
		if typ != "code" {
			continue
		}
		out = append(out, notebookOutputPrefix+"execution_count: "+notebookCellExecutionCount(cell))
		out = append(out, notebookOutputSummaryLines(cell["outputs"], inline)...)
	}
	return out
}

// notebookCellType returns a cell's type, defaulting to raw for anything
// unrecognized (render must never fail on a malformed cell).
func notebookCellType(cell map[string]json.RawMessage) string {
	var typ string
	if raw, present := cell["cell_type"]; present {
		_ = json.Unmarshal(raw, &typ)
	}
	switch typ {
	case "code", "markdown", "raw":
		return typ
	}
	return "raw"
}

// notebookCellExecutionCount renders the cell's execution_count verbatim
// ("null" when unset).
func notebookCellExecutionCount(cell map[string]json.RawMessage) string {
	raw, present := cell["execution_count"]
	if !present || len(bytes.TrimSpace(raw)) == 0 {
		return "null"
	}
	return string(bytes.TrimSpace(raw))
}

// notebookSourceLines splits a cell's source into virtual lines, escaping
// marker-like lines and capping the count.
func notebookSourceLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	truncated := 0
	if len(lines) > notebookCellSourceMax {
		truncated = len(lines) - notebookCellSourceMax
		lines = lines[:notebookCellSourceMax]
	}
	out := make([]string, 0, len(lines)+1)
	for _, line := range lines {
		out = append(out, notebookEscapeSource(line))
	}
	if truncated > 0 {
		out = append(out, fmt.Sprintf("%s+%d source lines not shown (cell source cap is %d lines)",
			notebookSourceTruncatedPrefix, truncated, notebookCellSourceMax))
	}
	return out
}

// notebookCellSource joins a cell's source, which nbformat stores either as one
// string or as an array of lines that keep their newlines.
func notebookCellSource(cell map[string]json.RawMessage) string {
	raw, present := cell["source"]
	if !present {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var parts []string
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	return strings.Join(parts, "")
}

// notebookEscapeSource adds one '%' after a leading "# %%" so source that looks
// like a marker or an output summary line cannot be misparsed on write-back.
func notebookEscapeSource(s string) string {
	if strings.HasPrefix(s, "# %%") {
		return "# %%%" + s[len("# %%"):]
	}
	return s
}

// notebookUnescapeSource reverses notebookEscapeSource.
func notebookUnescapeSource(s string) string {
	if strings.HasPrefix(s, "# %%%") {
		return s[:len("# %%")] + s[len("# %%%"):]
	}
	return s
}

// notebookOutputSummaryLines summarizes a code cell's outputs.
func notebookOutputSummaryLines(raw json.RawMessage, inline bool) []string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var outputs []json.RawMessage
	if err := json.Unmarshal(raw, &outputs); err != nil || len(outputs) == 0 {
		return nil
	}
	if !inline {
		return []string{fmt.Sprintf("%s%d outputs (large notebook; not inlined)", notebookOutputPrefix, len(outputs))}
	}
	lines := make([]string, 0, len(outputs)+1)
	for i, output := range outputs {
		if i == notebookOutputsMax {
			lines = append(lines, fmt.Sprintf("%s… (+%d more outputs)", notebookOutputPrefix, len(outputs)-i))
			break
		}
		lines = append(lines, notebookOutputSummary(output))
	}
	return lines
}

// notebookOutputSummary renders one output as a single summary line.
func notebookOutputSummary(raw json.RawMessage) string {
	var output struct {
		Type   string                     `json:"output_type"`
		Name   string                     `json:"name"`
		Text   json.RawMessage            `json:"text"`
		Data   map[string]json.RawMessage `json:"data"`
		EName  string                     `json:"ename"`
		EValue string                     `json:"evalue"`
	}
	if err := json.Unmarshal(raw, &output); err != nil {
		return notebookOutputPrefix + "unreadable output"
	}
	switch output.Type {
	case "stream":
		name := output.Name
		if name == "" {
			name = "stdout"
		}
		return notebookOutputPrefix + "stream " + name + ": " + notebookOneLine(notebookText(output.Text))
	case "display_data", "execute_result":
		return notebookOutputPrefix + output.Type + ": " + notebookMimeList(output.Data)
	case "error":
		return notebookOutputPrefix + "error: " + notebookOneLine(output.EName+": "+output.EValue)
	default:
		if output.Type == "" {
			return notebookOutputPrefix + "output"
		}
		return notebookOutputPrefix + "output: " + output.Type
	}
}

// notebookText joins a text field stored as a string or an array of lines.
func notebookText(raw json.RawMessage) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var parts []string
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	return strings.Join(parts, "")
}

// notebookMimeList lists the mime types a rich output carries, capped.
func notebookMimeList(data map[string]json.RawMessage) string {
	if len(data) == 0 {
		return "(no data)"
	}
	mimes := make([]string, 0, len(data))
	for mime := range data {
		mimes = append(mimes, mime)
	}
	sort.Strings(mimes)
	if len(mimes) > notebookMimesMax {
		return fmt.Sprintf("%s (+%d more)", strings.Join(mimes[:notebookMimesMax], ", "), len(mimes)-notebookMimesMax)
	}
	return strings.Join(mimes, ", ")
}

// notebookOneLine flattens and truncates one output line of text.
func notebookOneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	runes := []rune(s)
	if len(runes) > notebookOutputMaxRunes {
		return string(runes[:notebookOutputMaxRunes]) + fmt.Sprintf("…(+%d chars)", len(runes)-notebookOutputMaxRunes)
	}
	return s
}

// parsedCell is one marker-delimited block of edited virtual text.
type parsedCell struct {
	typ string
	idx int // original cell index named by the marker, -1 when absent
	src []string
}

// serializeVirtualText rewrites the notebook from edited virtual text. Only
// cells, their type and their source change; every other field survives.
func (d *notebookDoc) serializeVirtualText(lines []string) ([]byte, error) {
	cells, err := d.buildCells(lines)
	if err != nil {
		return nil, err
	}
	rawCells, err := notebookMarshal(cells)
	if err != nil {
		return nil, err
	}
	top := make(map[string]json.RawMessage, len(d.top)+1)
	for k, v := range d.top {
		top[k] = v
	}
	top["cells"] = rawCells
	return notebookEncodeDoc(top)
}

// buildCells parses the virtual text and materializes the resulting cells.
func (d *notebookDoc) buildCells(lines []string) ([]map[string]json.RawMessage, error) {
	parsed, err := d.parseVirtualText(lines)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]json.RawMessage, 0, len(parsed))
	used := make([]bool, len(d.cells))
	for _, pc := range parsed {
		var cell map[string]json.RawMessage
		if pc.idx >= 0 && pc.idx < len(d.cells) && !used[pc.idx] {
			// A marker naming an existing, still-unused cell reuses it and
			// keeps everything the edit did not touch (outputs, metadata,
			// attachments, id).
			used[pc.idx] = true
			cell = make(map[string]json.RawMessage, len(d.cells[pc.idx])+2)
			for k, v := range d.cells[pc.idx] {
				cell[k] = v
			}
		} else {
			cell = map[string]json.RawMessage{}
		}
		rawType, err := notebookMarshal(pc.typ)
		if err != nil {
			return nil, err
		}
		rawSource, err := notebookMarshal(notebookSourceArray(strings.Join(pc.src, "\n")))
		if err != nil {
			return nil, err
		}
		cell["cell_type"] = rawType
		cell["source"] = rawSource
		if _, present := cell["metadata"]; !present {
			cell["metadata"] = json.RawMessage("{}")
		}
		if pc.typ == "code" {
			if _, present := cell["execution_count"]; !present {
				cell["execution_count"] = json.RawMessage("null")
			}
			if _, present := cell["outputs"]; !present {
				cell["outputs"] = json.RawMessage("[]")
			}
		} else {
			// nbformat allows these on code cells only.
			delete(cell, "execution_count")
			delete(cell, "outputs")
		}
		if d.minor >= 5 {
			if _, present := cell["id"]; !present {
				rawID, err := notebookMarshal(notebookNewCellID())
				if err != nil {
					return nil, err
				}
				cell["id"] = rawID
			}
		}
		out = append(out, cell)
	}
	return out, nil
}

// parseVirtualText splits edited virtual text into marker-delimited cells.
func (d *notebookDoc) parseVirtualText(lines []string) ([]parsedCell, error) {
	var cells []parsedCell
	for i, line := range lines {
		if m := notebookMarkerRe.FindStringSubmatch(line); m != nil {
			pc := parsedCell{typ: m[1], idx: -1}
			if m[2] != "" {
				if n, err := strconv.Atoi(m[2]); err == nil {
					pc.idx = n
				}
			}
			cells = append(cells, pc)
			continue
		}
		if len(cells) == 0 {
			return nil, fmt.Errorf("line %d (%q) is outside any cell: notebook text starts with a marker like %q", i+1, linePreview([]string{line}), "# %% [code] cell:0")
		}
		cur := &cells[len(cells)-1]
		switch {
		case strings.HasPrefix(line, "# %%%"):
			cur.src = append(cur.src, notebookUnescapeSource(line))
		case strings.HasPrefix(line, notebookOutputPrefix):
			// Display-only summary: outputs are preserved from the notebook,
			// never re-parsed from this text.
		case strings.HasPrefix(line, notebookSourceTruncatedPrefix):
			return nil, fmt.Errorf("cell %d source was truncated in the read view (%s) — writing it back would drop the omitted lines; read the notebook again and edit a cell within the %d-line source cap",
				len(cells)-1, strings.TrimSpace(strings.TrimPrefix(line, notebookSourceTruncatedPrefix)), notebookCellSourceMax)
		default:
			cur.src = append(cur.src, line)
		}
	}
	return cells, nil
}

// notebookSourceArray encodes text the way nbformat stores source: one array
// entry per line, each keeping its newline, no trailing newline invented, empty
// text as [].
func notebookSourceArray(text string) []string {
	parts := []string{}
	for i := 0; i < len(text); {
		if j := strings.IndexByte(text[i:], '\n'); j >= 0 {
			parts = append(parts, text[i:i+j+1])
			i += j + 1
			continue
		}
		parts = append(parts, text[i:])
		break
	}
	return parts
}

// notebookNewCellID mints an nbformat 4.5+ cell id.
func notebookNewCellID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "xdev0000"
	}
	return hex.EncodeToString(b[:])
}

// notebookMarshal encodes v without HTML escaping: notebook sources routinely
// carry "<", and \u003c churn would otherwise rewrite untouched cells.
func notebookMarshal(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// notebookEncodeDoc renders the document in nbformat's canonical shape (sorted
// keys, one-space indent, no HTML escaping) with a trailing newline.
//
// ponytail: the whole file is re-encoded, so a notebook that was not already in
// canonical form gets reformatted once (values are verbatim: outputs, base64
// payloads and metadata are never re-parsed). Upgrade path if formatting churn
// ever matters: splice the edited cell's byte span into the original bytes.
func notebookEncodeDoc(top map[string]json.RawMessage) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(top); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// notebookRawJSONRefusal returns a clear error when an edit's line numbers were
// anchored to the notebook's raw JSON instead of its virtual text, or when the
// notebook was never rendered to the model at all. empty means "proceed".
func notebookRawJSONRefusal(reg *Registry, resolved, display, wantTag string, rawLines, virtualLines []string) string {
	rawHash, virtHash := linesHash(rawLines), linesHash(virtualLines)
	rec, hasRec := reg.snapshot(resolved)

	if wantTag != "" && wantTag == hashTag(rawHash) {
		return notebookRawJSONError(display)
	}
	if hasRec && rawHash != virtHash && rec.hash == rawHash {
		return notebookRawJSONError(display)
	}
	if !hasRec {
		return fmt.Sprintf("edit: %s is a Jupyter notebook — read it first: notebooks are edited through the virtual text read renders (%q blocks), never through raw JSON",
			display, "# %% [code] cell:0")
	}
	return ""
}

// notebookRawJSONError is the shared refusal text: it points at the editable
// form and at the tool that actually executes cells.
func notebookRawJSONError(display string) string {
	return fmt.Sprintf("edit: %s is a Jupyter notebook; its raw JSON is not the editable form — read %s to get the virtual text (%q blocks), edit those line numbers, and run cells with the eval tool",
		display, display, "# %% [code] cell:0")
}
