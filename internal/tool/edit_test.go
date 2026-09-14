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

// TestEditAdvertisedOpsAreAccepted: Description() is the model's only
// contract, and it used to advertise REM plus "<N"/">N"/"N*" anchors the
// executor rejected ("unknown op \"REM\"", "range is required"). Every op in
// the advertised vocabulary must execute, every documented form must land the
// bytes it promises, and the text must not name an anchor the JSON schema
// cannot express.
func TestEditAdvertisedOpsAreAccepted(t *testing.T) {
	et := NewEditTool()
	var params struct {
		Properties struct {
			Ops struct {
				Items struct {
					Properties struct {
						Op struct {
							Enum []string `json:"enum"`
						} `json:"op"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"ops"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(et.Parameters(), &params); err != nil {
		t.Fatalf("Parameters() is not valid JSON: %v", err)
	}
	advertised := params.Properties.Ops.Items.Properties.Op.Enum
	if len(advertised) == 0 {
		t.Fatal("Parameters() advertises no ops")
	}
	forms := map[string]struct {
		op   map[string]any
		text string
		want string
	}{
		"PUT": {map[string]any{"op": "PUT", "range": map[string]any{"start": 2, "end": 3}, "lines": []string{"+x"}}, "a\nb\nc\nd\n", "a\nx\nd\n"},
		"CUT": {map[string]any{"op": "CUT", "range": map[string]any{"line": 2}}, "a\nb\nc\n", "a\nc\n"},
		"REM": {map[string]any{"op": "REM", "range": map[string]any{"line": 2}}, "a\nb\nc\n", "a\nc\n"},
		"MV":  {map[string]any{"op": "MV"}, "a\n", "a\n"},
	}
	for _, tok := range advertised {
		t.Run(tok, func(t *testing.T) {
			form, ok := forms[tok]
			if !ok {
				t.Fatalf("Parameters() advertises op %q this test has no documented form for: implement it and add the form, or drop the token", tok)
			}
			path := editFile(t, t.TempDir(), "f.txt", form.text)
			read := path
			if tok == "MV" {
				dest := filepath.Join(filepath.Dir(path), "moved.txt")
				form.op["dest"] = dest
				read = dest
			}
			res, err := et.Execute(t.Context(), fsToolArgs(t, map[string]any{
				"path": path,
				"ops":  []map[string]any{form.op},
			}))
			if err != nil {
				t.Fatal(err)
			}
			if res.IsError {
				t.Fatalf("advertised op %s rejected: %s", tok, res.Text)
			}
			data, err := os.ReadFile(read)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != form.want {
				t.Fatalf("file = %q, want %q", data, form.want)
			}
		})
	}
	// The prose and the schema must name one vocabulary; they shipped out of
	// sync, with Description() promising REM while Execute answered
	// `unknown op "REM"`.
	desc := et.Description()
	known := map[string]bool{}
	for _, tok := range advertised {
		known[tok] = true
	}
	covered := map[string]bool{}
	for _, tok := range opWordRe.FindAllString(desc, -1) {
		if !known[tok] {
			t.Errorf("Description() names %q, which the op schema does not advertise and the executor rejects", tok)
			continue
		}
		covered[tok] = true
	}
	for _, tok := range advertised {
		if !covered[tok] {
			t.Errorf("Description() never mentions advertised op %q", tok)
		}
	}
	for _, lie := range []string{"<N", ">N", "N*"} {
		if strings.Contains(desc, lie) {
			t.Errorf("Description() promises the %q anchor, which the op schema cannot express", lie)
		}
	}
}

// opWordRe finds the op words a Description() can be naming (ops are spelled
// in caps; prose that is not an op must not be).
var opWordRe = regexp.MustCompile(`[A-Z]{2,}`)
