package tool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// editFile writes a fresh 5-line fixture and returns its path.
func editFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEditPutReplace(t *testing.T) {
	path := editFile(t, t.TempDir(), "f.txt", "a\nb\nc\nd\ne\n")
	res, err := NewEditTool().Execute(t.Context(), fsToolArgs(t, map[string]any{
		"path": path,
		"ops": []map[string]any{{
			"op":    "PUT",
			"range": map[string]any{"start": 2, "end": 3},
			"lines": []string{"+x1", "+x2", "+x3"},
		}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Text)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "a\nx1\nx2\nx3\nd\ne\n" {
		t.Fatalf("file = %q", data)
	}
	d := res.Details.(map[string]any)
	if d["linesBefore"] != 5 || d["linesAfter"] != 6 || d["opsApplied"] != 1 {
		t.Fatalf("details = %v", d)
	}
	summary := d["diffSummary"].([]string)
	if len(summary) != 1 || summary[0] != "2-3: +3 lines -2 lines" {
		t.Fatalf("diffSummary = %#v", summary)
	}
}

func TestEditMultiOpRenumbering(t *testing.T) {
	path := editFile(t, t.TempDir(), "f.txt", "a\nb\nc\nd\ne\n")
	res, err := NewEditTool().Execute(t.Context(), fsToolArgs(t, map[string]any{
		"path": path,
		"ops": []map[string]any{
			// Op 1: replace b (line 2) with two lines -> a,x,y,c,d,e.
			{"op": "PUT", "range": map[string]any{"line": 2}, "lines": []string{"+x", "+y"}},
			// Op 2: numbers refer to post-op-1 state; line 4 is now c.
			{"op": "CUT", "range": map[string]any{"line": 4}},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Text)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "a\nx\ny\nd\ne\n" {
		t.Fatalf("file = %q, want renumbered result", data)
	}
	if res.Details.(map[string]any)["opsApplied"] != 2 {
		t.Fatalf("details = %v", res.Details)
	}
}

func TestEditOutOfRangeLeavesFileUntouched(t *testing.T) {
	dir := t.TempDir()
	path := editFile(t, dir, "f.txt", "a\nb\nc\n")
	res, err := NewEditTool().Execute(t.Context(), fsToolArgs(t, map[string]any{
		"path": path,
		"ops": []map[string]any{
			{"op": "PUT", "range": map[string]any{"line": 1}, "lines": []string{"+z"}},
			{"op": "CUT", "range": map[string]any{"line": 5}},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected IsError for out-of-range op")
	}
	if !strings.Contains(res.Text, "op 2") {
		t.Fatalf("error must name the failing op: %s", res.Text)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "a\nb\nc\n" {
		t.Fatalf("file changed despite failed edit: %q", data)
	}
}

func TestEditEmptyBodyDeletes(t *testing.T) {
	path := editFile(t, t.TempDir(), "f.txt", "a\nb\nc\nd\ne\n")
	res, err := NewEditTool().Execute(t.Context(), fsToolArgs(t, map[string]any{
		"path": path,
		"ops": []map[string]any{{
			"op":    "PUT",
			"range": map[string]any{"start": 2, "end": 3},
			"lines": []string{},
		}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Text)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "a\nd\ne\n" {
		t.Fatalf("file = %q, want b/c deleted", data)
	}
}

func TestEditPlusEscape(t *testing.T) {
	path := editFile(t, t.TempDir(), "f.txt", "one\n")
	res, err := NewEditTool().Execute(t.Context(), fsToolArgs(t, map[string]any{
		"path": path,
		"ops": []map[string]any{{
			"op":    "PUT",
			"range": map[string]any{"line": 1},
			"lines": []string{"++literal-plus", "+plain"},
		}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Text)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "+literal-plus\nplain\n" {
		t.Fatalf("file = %q", data)
	}
}

func TestEditSnapshotConfirmation(t *testing.T) {
	path := editFile(t, t.TempDir(), "f.txt", "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n")
	res, err := NewEditTool().Execute(t.Context(), fsToolArgs(t, map[string]any{
		"path": path,
		"ops": []map[string]any{{
			"op":    "PUT",
			"range": map[string]any{"line": 3},
			"lines": []string{"+THREE"},
		}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Text)
	}
	// Header is "[path#hash]" and the window centers on the first edit
	// region with 1-based numbering.
	if !strings.HasPrefix(res.Text, "["+path+"#") {
		t.Fatalf("missing snapshot header: %q", res.Text[:min(40, len(res.Text))])
	}
	for _, want := range []string{"\n1:1", "\n3:THREE", "\n6:6", "\n…"} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("snapshot missing %q:\n%s", want, res.Text)
		}
	}
	if strings.Contains(res.Text, "\n7:7") {
		t.Fatalf("window exceeds radius:\n%s", res.Text)
	}
}

func TestEditMVFile(t *testing.T) {
	dir := t.TempDir()
	path := editFile(t, dir, "old.txt", "content\n")
	dest := filepath.Join(dir, "moved", "new.txt")
	res, err := NewEditTool().Execute(t.Context(), fsToolArgs(t, map[string]any{
		"path": path,
		"ops":  []map[string]any{{"op": "MV", "dest": dest}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Text)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("source still exists (err=%v)", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "content\n" {
		t.Fatalf("moved file = %q", data)
	}
	if res.Details.(map[string]any)["resolvedPath"] != dest {
		t.Fatalf("details = %v", res.Details)
	}
	if !strings.HasPrefix(res.Text, "Moved "+path+" to "+dest+"\n["+dest+"#") {
		t.Fatalf("text = %q", res.Text)
	}
}

func TestEditMVAfterLineOps(t *testing.T) {
	dir := t.TempDir()
	path := editFile(t, dir, "old.txt", "a\nb\nc\n")
	dest := filepath.Join(dir, "new.txt")
	res, err := NewEditTool().Execute(t.Context(), fsToolArgs(t, map[string]any{
		"path": path,
		"ops": []map[string]any{
			{"op": "PUT", "range": map[string]any{"line": 2}, "lines": []string{"+B2"}},
			{"op": "MV", "dest": dest},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Text)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "a\nB2\nc\n" {
		t.Fatalf("moved file = %q", data)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("source still exists")
	}
}

func TestEditMVBadDestLeavesFileUntouched(t *testing.T) {
	dir := t.TempDir()
	existingDir := filepath.Join(dir, "subdir")
	if err := os.Mkdir(existingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := editFile(t, dir, "f.txt", "a\n")
	res, err := NewEditTool().Execute(t.Context(), fsToolArgs(t, map[string]any{
		"path": path,
		"ops":  []map[string]any{{"op": "MV", "dest": existingDir}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected IsError for directory destination")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "a\n" {
		t.Fatalf("file changed despite failed MV: %q", data)
	}
}

func TestEditUnknownOp(t *testing.T) {
	path := editFile(t, t.TempDir(), "f.txt", "a\n")
	res, err := NewEditTool().Execute(t.Context(), fsToolArgs(t, map[string]any{
		"path": path,
		"ops":  []map[string]any{{"op": "DELETE"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "unknown op") {
		t.Fatalf("text = %q, isError = %v", res.Text, res.IsError)
	}
}

func TestEditNoOps(t *testing.T) {
	path := editFile(t, t.TempDir(), "f.txt", "a\n")
	res, err := NewEditTool().Execute(t.Context(), fsToolArgs(t, map[string]any{"path": path, "ops": []map[string]any{}}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected IsError for empty ops")
	}
}

// TestEditLiteralDashBodyLine: the row transport has exactly one prefix, "+",
// so a leading "-" is ordinary content. Description() used to claim a literal
// "-" had to be doubled, and a model that obeyed wrote an extra dash into the
// file.
func TestEditLiteralDashBodyLine(t *testing.T) {
	path := editFile(t, t.TempDir(), "f.txt", "old\n")
	res, err := NewEditTool().Execute(t.Context(), fsToolArgs(t, map[string]any{
		"path": path,
		"ops": []map[string]any{{
			"op":    "PUT",
			"range": map[string]any{"line": 1},
			"lines": []string{"+- dash", "- bullet", "--flag", "++plus"},
		}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Text)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "- dash\n- bullet\n--flag\n+plus\n" {
		t.Fatalf("file = %q, want dashes verbatim and exactly one \"+\" stripped", data)
	}
}

// TestEditCRLFFileKeepsLineEndings: editing a CRLF file keeps it all-CRLF.
// ReadLines leaves the "\r" on the rows it read while WriteLinesAtomic rejoins
// with "\n", so a row the model typed landed as a bare LF — the live fixture
// wrote mixed "first\r\nREPLACED\nthird\r\n" with nothing in the result saying
// so.
func TestEditCRLFFileKeepsLineEndings(t *testing.T) {
	cases := []struct {
		name    string
		content string
		line    int
		lines   []string
		want    string
	}{
		{"replace in the middle", "first\r\nsecond\r\nthird\r\n", 2, []string{"+REPLACED"}, "first\r\nREPLACED\r\nthird\r\n"},
		{"append a row", "first\r\nsecond\r\n", 2, []string{"+second", "+third"}, "first\r\nsecond\r\nthird\r\n"},
		{"a typed trailing CR is not doubled", "a\r\nb\r\n", 1, []string{"+x\r"}, "x\r\nb\r\n"},
		{"an LF file stays LF", "first\nsecond\n", 1, []string{"+REPLACED"}, "REPLACED\nsecond\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := editFile(t, t.TempDir(), "f.txt", tc.content)
			res, err := NewEditTool().Execute(t.Context(), fsToolArgs(t, map[string]any{
				"path": path,
				"ops": []map[string]any{{
					"op":    "PUT",
					"range": map[string]any{"line": tc.line},
					"lines": tc.lines,
				}},
			}))
			if err != nil {
				t.Fatal(err)
			}
			if res.IsError {
				t.Fatalf("unexpected error: %s", res.Text)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != tc.want {
				t.Fatalf("file = %q, want %q", data, tc.want)
			}
		})
	}
}

// TestHashlineVerbsAreAdvertisedAndExecute: Description() is the model's only
// contract, so every verb and every form it names must execute from patch text
// and land the bytes it promises. The inverse guard matters as much: the prose
// must not name an op the parser rejects (Description() once promised REM
// while Execute answered `unknown op "REM"`, and later advertised "N*" anchors
// the executor never implemented).
func TestHashlineVerbsAreAdvertisedAndExecute(t *testing.T) {
	cases := []struct {
		verb, patch, text, want string
	}{
		{"PUT", "PUT 2.=3:\n+x\n", "a\nb\nc\nd\n", "a\nx\nd\n"},
		{"PUT", "PUT 2:\n+x\n", "a\nb\nc\n", "a\nx\nc\n"},
		{"PUT", "PUT 2.=3:\n", "a\nb\nc\nd\n", "a\nd\n"},
		{"PUT", "PUT <1:\n+x\n", "a\nb\nc\n", "x\na\nb\nc\n"},
		{"PUT", "PUT <2:\n+x\n", "a\nb\nc\n", "a\nx\nb\nc\n"},
		{"PUT", "PUT >2:\n+x\n", "a\nb\nc\n", "a\nb\nx\nc\n"},
		{"CUT", "CUT 2\n", "a\nb\nc\n", "a\nc\n"},
		{"REM", "REM 2.=3\n", "a\nb\nc\nd\n", "a\nd\n"},
	}
	for _, tc := range cases {
		label := strings.SplitN(tc.patch, "\n", 2)[0]
		t.Run(label, func(t *testing.T) {
			dir := t.TempDir()
			path := editFile(t, dir, "f.txt", tc.text)
			et := NewEditTool()
			res, err := et.Execute(t.Context(), fsToolArgs(t, map[string]any{
				"input": "[" + path + "]\n" + tc.patch,
			}))
			if err != nil {
				t.Fatal(err)
			}
			if res.IsError {
				t.Fatalf("%s rejected: %s", tc.verb, res.Text)
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != tc.want {
				t.Fatalf("file = %q (%v), want %q", data, err, tc.want)
			}
		})
	}
	// The prose names exactly the verbs that work, and the schema hands the
	// patch through the field the prose says to use it on.
	et := NewEditTool()
	desc := et.Description()
	for _, tok := range []string{"PUT", "CUT", "REM", "MV"} {
		if !strings.Contains(desc, tok) {
			t.Errorf("Description() never mentions %s, so a model is never told it exists", tok)
		}
	}
	for _, word := range opWordRe.FindAllString(desc, -1) {
		switch word {
		case "PUT", "CUT", "REM", "MV":
		default:
			t.Errorf("Description() names %q, which the hashline grammar does not accept", word)
		}
	}
	var params struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(et.Parameters(), &params); err != nil {
		t.Fatalf("Parameters() is not valid JSON: %v", err)
	}
	if _, ok := params.Properties["input"]; !ok || !strings.Contains(desc, "input") {
		t.Errorf("Parameters() advertises %v but the description says %q", params.Required, "input")
	}
}

// TestHashlineMoveRenamesTheFile covers MV, whose destination has to be
// reported back the way the structured form already does.
func TestHashlineMoveRenamesTheFile(t *testing.T) {
	dir := t.TempDir()
	path := editFile(t, dir, "f.txt", "a\nb\n")
	dest := filepath.Join(dir, "g.txt")
	et := NewEditTool()
	res, err := et.Execute(t.Context(), fsToolArgs(t, map[string]any{
		"input": "[" + path + "]\nMV " + dest + "\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("MV rejected: %s", res.Text)
	}
	if !strings.HasPrefix(res.Text, "Moved ") {
		t.Errorf("result text = %q, want a Moved report", res.Text)
	}
	if data, err := os.ReadFile(dest); err != nil || string(data) != "a\nb\n" {
		t.Fatalf("moved file = %q (%v)", data, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the source is still there: %v", err)
	}
}

// opWordRe finds the op words a Description() can be naming (ops are spelled
// in caps; prose that is not an op must not be).
var opWordRe = regexp.MustCompile(`[A-Z]{2,}`)

// The renderer gets the change itself, not just the op tally: Details carries
// a unified diff of the file's before/after state while Text keeps the exact
// shape the model and the session log depend on.
func TestEditAttachesUnifiedDiff(t *testing.T) {
	path := editFile(t, t.TempDir(), "f.txt", "a\nb\nc\nd\ne\n")
	res, err := NewEditTool().Execute(t.Context(), fsToolArgs(t, map[string]any{
		"path": path,
		"ops": []map[string]any{{
			"op":    "PUT",
			"range": map[string]any{"start": 2, "end": 3},
			"lines": []string{"+x1"},
		}},
	}))
	if err != nil || res.IsError {
		t.Fatalf("edit failed: %v %s", err, res.Text)
	}
	d := res.Details.(map[string]any)
	diff, _ := d["unifiedDiff"].(string)
	// The header names the path as the call gave it (an absolute one here), so
	// only its prefix is asserted; the rows are the change itself.
	if !strings.HasPrefix(diff, "--- ") || !strings.Contains(diff, "\n+++ ") {
		t.Errorf("no file header in unifiedDiff:\n%s", diff)
	}
	for _, want := range []string{"-b", "-c", "+x1", "\n a\n", "\n d\n"} {
		if !strings.Contains(diff, want) {
			t.Errorf("unifiedDiff missing %q:\n%s", want, diff)
		}
	}
	// Text is the model-visible snapshot: the tag line plus the render window,
	// with no diff appended.
	if !strings.HasPrefix(res.Text, "[") || !strings.Contains(res.Text, "#") || strings.Contains(res.Text, "@@") {
		t.Errorf("Text changed shape: %q", res.Text)
	}
}

// An edit whose ops net out to no content change (write the same lines back)
// attaches no diff: the box would paint an empty change as a change.
func TestEditNoDiffWhenContentUnchanged(t *testing.T) {
	path := editFile(t, t.TempDir(), "f.txt", "a\nb\n")
	res, err := NewEditTool().Execute(t.Context(), fsToolArgs(t, map[string]any{
		"path": path,
		"ops": []map[string]any{{
			"op":    "PUT",
			"range": map[string]any{"start": 1, "end": 2},
			"lines": []string{"+a", "+b"},
		}},
	}))
	if err != nil || res.IsError {
		t.Fatalf("edit failed: %v %s", err, res.Text)
	}
	if d := res.Details.(map[string]any); d["unifiedDiff"] != nil {
		t.Errorf("unchanged content produced a diff: %v", d["unifiedDiff"])
	}
}
