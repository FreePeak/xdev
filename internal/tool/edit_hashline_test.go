package tool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The hashline grammar is the model-facing edit interface. Each test here
// mirrors a call shape that live sessions actually produced (see the repairs
// in decodeEditArgs): what must be observed is that the patch lands, or that
// the rejection teaches the accepted shape instead of a parser complaint.
func hashlineEdit(t *testing.T, tl Tool, body map[string]any) Result {
	t.Helper()
	return runTool(t, tl, body)
}

func mustEditText(t *testing.T, tl Tool, body map[string]any) string {
	t.Helper()
	res := hashlineEdit(t, tl, body)
	if res.IsError {
		t.Fatalf("edit rejected: %s", res.Text)
	}
	return res.Text
}

func TestEditHashlineReplaceInsertDelete(t *testing.T) {
	dir := t.TempDir()
	path := editFile(t, dir, "f.txt", "a\nb\nc\nd\ne\n")
	et := NewEditTool()
	mustEditText(t, et, map[string]any{"input": "[" + path + "]\nPUT 2.=3:\n+x\n" + "PUT >1:\n+y\n"})
	// Replace b,c with x, then insert y after line 1 of the result.
	if data, _ := os.ReadFile(path); string(data) != "a\ny\nx\nd\ne\n" {
		t.Fatalf("file = %q", data)
	}
	mustEditText(t, et, map[string]any{"input": "[" + path + "]\nCUT 3\n"})
	if data, _ := os.ReadFile(path); string(data) != "a\ny\nd\ne\n" {
		t.Fatalf("after CUT file = %q", data)
	}
}

// TestEditHashlineBodyRowTransport pins the row convention the description
// teaches: one leading "+" is transport, "++" is a literal "+", a lone "+" is
// a blank line, and a "-" row is plain content — never a deletion.
func TestEditHashlineBodyRowTransport(t *testing.T) {
	dir := t.TempDir()
	path := editFile(t, dir, "f.txt", "a\n")
	et := NewEditTool()
	mustEditText(t, et, map[string]any{"input": "[" + path + "]\nPUT 1.=1:\n+++x\n+\n+-y\n"})
	if data, _ := os.ReadFile(path); string(data) != "++x\n\n-y\n" {
		t.Fatalf("file = %q, want \"++x\", blank, \"-y\"", data)
	}
}

// TestEditHashlineQuotedOpsString is the live failure this change exists for:
// the model quoted the op array as a JSON string, and the answer was
// "cannot unmarshal string into Go struct field editArgs.ops".
func TestEditHashlineQuotedOpsString(t *testing.T) {
	dir := t.TempDir()
	path := editFile(t, dir, "f.txt", "a\nb\nc\n")
	et := NewEditTool()
	ops := `[{"op": "PUT", "range": {"start": 2, "end": 2}, "lines": ["+x"]}]`
	mustEditText(t, et, map[string]any{"path": path, "ops": ops})
	if data, _ := os.ReadFile(path); string(data) != "a\nx\nc\n" {
		t.Fatalf("file = %q", data)
	}
}

// TestEditHashlinePatchInsideOps covers a patch text sent in the ops field.
func TestEditHashlinePatchInsideOps(t *testing.T) {
	dir := t.TempDir()
	path := editFile(t, dir, "f.txt", "a\nb\nc\n")
	et := NewEditTool()
	mustEditText(t, et, map[string]any{"path": path, "ops": "[" + path + "]\nCUT 2\n"})
	if data, _ := os.ReadFile(path); string(data) != "a\nc\n" {
		t.Fatalf("file = %q", data)
	}
}

// TestEditHashlineBareStringArguments covers the whole arguments blob being a
// patch string, with no JSON object around it at all.
func TestEditHashlineBareStringArguments(t *testing.T) {
	dir := t.TempDir()
	path := editFile(t, dir, "f.txt", "a\nb\nc\n")
	patch := "[" + path + "]\nPUT 1.=2:\n+z\n"
	raw, err := json.Marshal(patch)
	if err != nil {
		t.Fatal(err)
	}
	res, err := NewEditTool().Execute(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("bare patch string rejected: %s", res.Text)
	}
	if data, _ := os.ReadFile(path); string(data) != "z\nc\n" {
		t.Fatalf("file = %q", data)
	}
}

// TestEditHashlinePathInsideOp covers "path" written into the op object rather
// than the call — the shape behind "edit: path is required".
func TestEditHashlinePathInsideOp(t *testing.T) {
	dir := t.TempDir()
	path := editFile(t, dir, "f.txt", "a\nb\nc\n")
	et := NewEditTool()
	mustEditText(t, et, map[string]any{"ops": []map[string]any{
		{"op": "CUT", "range": map[string]any{"line": 2}, "path": path},
	}})
	if data, _ := os.ReadFile(path); string(data) != "a\nc\n" {
		t.Fatalf("file = %q", data)
	}
}

// TestEditHashlineQuotedJSONOpsIsNotAPatch guards the sibling trap: a quoted
// op array must not be mistaken for a [path] header because both start with
// a bracket.
func TestEditHashlineQuotedJSONOpsIsNotAPatch(t *testing.T) {
	dir := t.TempDir()
	path := editFile(t, dir, "f.txt", "a\nb\nc\n")
	et := NewEditTool()
	res := hashlineEdit(t, et, map[string]any{"path": path, "ops": `[{"op": "CUT", "range": {"line": 9}}]`})
	if !res.IsError {
		t.Fatalf("out-of-range op through the repair path should fail, got %q", res.Text)
	}
	if strings.Contains(res.Text, "not a file header") {
		t.Errorf("the quoted op list was read as a path header: %s", res.Text)
	}
	if !strings.Contains(res.Text, "op 1 (CUT)") {
		t.Errorf("error should name the failing op, got %q", res.Text)
	}
}

// TestEditHashlineAnchoredOnPreviousResult closes the loop the format is
// designed around: quote the header the last edit printed, so the numbers are
// taken as fresh without re-reading.
func TestEditHashlineAnchoredOnPreviousResult(t *testing.T) {
	dir := t.TempDir()
	path := editFile(t, dir, "f.txt", "a\nb\nc\n")
	reg := freshnessRegistry(t)
	et := toolFrom(t, reg, "edit")
	first := mustEditText(t, et, map[string]any{"input": "[" + path + "]\nPUT 2.=2:\n+x\n"})
	head := strings.SplitN(first, "\n", 2)[0]
	if !strings.HasPrefix(head, "["+path+"#") {
		t.Fatalf("result header = %q", head)
	}
	second := mustEditText(t, et, map[string]any{"input": head + "\nCUT 2\n"})
	if strings.Contains(second, "file changed since it was read") {
		t.Errorf("a quoted tag should need no recovery note: %s", second)
	}
	if data, _ := os.ReadFile(path); string(data) != "a\nc\n" {
		t.Fatalf("file = %q", data)
	}
	// A fabricated tag is refused, and says which tags it compared.
	res := hashlineEdit(t, et, map[string]any{"input": "[" + path + "#beef]\nCUT 1\n"})
	if !res.IsError || !strings.Contains(res.Text, "changed since it was read") {
		t.Fatalf("made-up tag = %q (isError=%v)", res.Text, res.IsError)
	}
}

// TestEditHashlineReadTagRoundTrip is the live bench trace fix (2026-09-15,
// medium runs): the model writes the colon range spelling "PUT 15:=24:" from
// muscle memory, and — because read printed no snapshot header — quoted a
// fabricated "[f#1]" that opened as an ENOENT. Both are fixed: the colon
// spelling normalizes, and the header read actually prints round-trips
// through edit untouched.
func TestEditHashlineReadTagRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := editFile(t, dir, "f.txt", "a\nb\nc\nd\ne\n")
	reg := freshnessRegistry(t)
	read := toolFrom(t, reg, "read")
	et := toolFrom(t, reg, "edit")
	res := runTool(t, read, map[string]any{"path": path})
	header := strings.SplitN(res.Text, "\n", 2)[0]
	if !strings.HasPrefix(header, "["+path+"#") {
		t.Fatalf("read header = %q", header)
	}
	mustEditText(t, et, map[string]any{"input": header + "\nPUT 2:=3:\n+x\n"})
	if data, _ := os.ReadFile(path); string(data) != "a\nx\nd\ne\n" {
		t.Fatalf("file = %q", data)
	}
}

// TestEditHashlineShortFabricatedTag: "[f.txt#1]" is a tag attempt, not a
// path — the bare file exists, so the refusal must teach the grammar instead
// of answering ENOENT on "f.txt#1".
func TestEditHashlineShortFabricatedTag(t *testing.T) {
	dir := t.TempDir()
	path := editFile(t, dir, "f.txt", "a\nb\n")
	et := NewEditTool()
	res := hashlineEdit(t, et, map[string]any{"input": "[" + path + "#1]\nPUT 1.=1:\n+z\n"})
	if !res.IsError {
		t.Fatalf("fabricated tag must be refused: %s", res.Text)
	}
	if strings.Contains(res.Text, "no such file") {
		t.Fatalf("must refuse as a tag, not a path: %s", res.Text)
	}
}

// TestEditHashlineRejectionsTeachTheGrammar checks that a malformed patch is
// answered with the shape that works, at the line that broke — the class of
// failure the JSON-ops dialect produced was an unmarshal trace no model could
// act on.
func TestEditHashlineRejectionsTeachTheGrammar(t *testing.T) {
	dir := t.TempDir()
	path := editFile(t, dir, "f.txt", "a\nb\nc\n")
	cases := []struct {
		name, patch, want string
	}{
		{"prose", "please replace the second line", "PUT"},
		{"dash row", "[f.txt]\nPUT 1.=1:\n-x\n", "\"-\" rows are not how this grammar deletes"},
		{"bare row", "[f.txt]\nPUT 1.=1:\nno plus sign\n", "body rows start with \"+\""},
		{"unified diff", "--- a/f.txt\n+++ b/f.txt\n@@ -1,3 +1,3 @@\n a\n-b\n+x\n", "does not take diffs"},
		{"two files", "[" + path + "]\nCUT 1\n[other.go]\nCUT 2\n", "one [path] header"},
		{"no ops", "[" + path + "]\n", "names no ops"},
		{"no number", "[f.txt]\nPUT =:\n+x\n", "op line"},
		{"star anchor", "[f.txt]\nPUT 2*:\n+x\n", "PUT 3.=5:"},
		{"json as header", "[{\"op\": \"CUT\"}]\n", "not a file header"},
	}
	et := NewEditTool()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := hashlineEdit(t, et, map[string]any{"input": tc.patch})
			if !res.IsError {
				t.Fatalf("%q accepted: %s", tc.patch, res.Text)
			}
			if !strings.Contains(res.Text, tc.want) {
				t.Fatalf("error for %q = %q, want it to name %q", tc.patch, res.Text, tc.want)
			}
			if data, _ := os.ReadFile(path); string(data) != "a\nb\nc\n" {
				t.Fatalf("a rejected patch wrote to the file: %q", data)
			}
		})
	}
}

// TestEditHashlineOutOfRangeNamesTheOp keeps the bounds failure the same
// actionable shape the structured path has: which op, what range, how long
// the file is.
// TestEditErrorsCarryOnePrefix pins that a rejection reads "edit: line N: ..."
// and never "edit: edit: ...": Execute wraps decodeEditArgs' message, and the
// grammar already leads its own with the tool name.
func TestEditErrorsCarryOnePrefix(t *testing.T) {
	res := hashlineEdit(t, NewEditTool(), map[string]any{"input": "[f.txt]\n@@ -1,3 +1,3 @@\n"})
	if !res.IsError {
		t.Fatalf("diff-format patch accepted: %s", res.Text)
	}
	if strings.Contains(res.Text, "edit: edit:") {
		t.Fatalf("doubled prefix: %q", res.Text)
	}
	if !strings.HasPrefix(res.Text, "edit: ") {
		t.Fatalf("no tool prefix: %q", res.Text)
	}
}

func TestEditHashlineOutOfRangeNamesTheOp(t *testing.T) {
	dir := t.TempDir()
	path := editFile(t, dir, "f.txt", "a\nb\nc\n")
	res := hashlineEdit(t, NewEditTool(), map[string]any{"input": "[" + path + "]\nPUT 2.=9:\n+x\n"})
	if !res.IsError {
		t.Fatalf("out-of-bounds range accepted: %s", res.Text)
	}
	if !strings.Contains(res.Text, "out of bounds: file has 3 lines") || !strings.Contains(res.Text, "op 1 (PUT)") {
		t.Fatalf("error = %q", res.Text)
	}
	if data, _ := os.ReadFile(path); string(data) != "a\nb\nc\n" {
		t.Fatalf("file changed despite the failure: %q", data)
	}
}

// TestEditHashlineRelativePathResolves: a header may name the file relatively,
// the same way the path field always could.
func TestEditHashlineRelativePathResolves(t *testing.T) {
	dir := t.TempDir()
	editFile(t, dir, "f.txt", "a\nb\n")
	t.Chdir(dir)
	et := NewEditTool()
	mustEditText(t, et, map[string]any{"input": "[f.txt]\nPUT 1.=1:\n+x\n"})
	if data, _ := os.ReadFile(filepath.Join(dir, "f.txt")); string(data) != "x\nb\n" {
		t.Fatalf("file = %q", data)
	}
}

// TestEditHashlineKeepsCRLF is the text-form counterpart of the structured
// CRLF test: rows written into a CRLF file must not turn it mixed.
func TestEditHashlineKeepsCRLF(t *testing.T) {
	dir := t.TempDir()
	path := editFile(t, dir, "f.txt", "a\r\nb\r\nc\r\n")
	et := NewEditTool()
	mustEditText(t, et, map[string]any{"input": "[" + path + "]\nPUT 2.=2:\n+x\n"})
	mustEditText(t, et, map[string]any{"input": "[" + path + "]\nPUT >2:\n+y\n"})
	data, _ := os.ReadFile(path)
	if string(data) != "a\r\nx\r\ny\r\nc\r\n" {
		t.Fatalf("file = %q", data)
	}
}
