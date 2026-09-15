package tool

// .ipynb virtual text (M13 #47): read rendering, edit write-back, bounds.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// nbFixture is a small notebook in nbformat's canonical shape (sorted keys,
// one-space indent) — the form nbformat itself writes.
const nbFixture = `{
 "cells": [
  {
   "cell_type": "code",
   "execution_count": 3,
   "id": "cell-code",
   "metadata": {
    "tags": [
     "keep"
    ]
   },
   "outputs": [
    {
     "name": "stdout",
     "output_type": "stream",
     "text": [
      "hello\n",
      "world\n"
     ]
    },
    {
     "data": {
      "image/png": "iVBORw0KGgo=",
      "text/plain": [
       "<Figure>"
      ]
     },
     "metadata": {},
     "output_type": "display_data"
    },
    {
     "ename": "ValueError",
     "evalue": "bad input",
     "output_type": "error",
     "traceback": [
      "line 1"
     ]
    }
   ],
   "source": [
    "print(\"hi\")\n",
    "x = 1"
   ]
  },
  {
   "attachments": {},
   "cell_type": "markdown",
   "id": "cell-md",
   "metadata": {},
   "source": [
    "# Title\n",
    "text"
   ]
  },
  {
   "cell_type": "raw",
   "id": "cell-raw",
   "metadata": {
    "format": "text/latex"
   },
   "source": [
    "\\begin{x}"
   ]
  }
 ],
 "metadata": {
  "kernelspec": {
   "display_name": "Python 3",
   "language": "python",
   "name": "python3"
  }
 },
 "nbformat": 4,
 "nbformat_minor": 5
}
`

// nbFixturePath writes nbFixture into a temp dir.
func nbFixturePath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nb.ipynb")
	if err := os.WriteFile(path, []byte(nbFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// nbWrite marshals a notebook canonically and writes it as name.
func nbWrite(t *testing.T, name string, nb map[string]any) string {
	t.Helper()
	b, err := json.MarshalIndent(nb, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// nbRead runs the read tool (no registry needed for a plain read).
func nbRead(t *testing.T, path string, args map[string]any) Result {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	args["path"] = path
	res, err := NewReadTool().Execute(t.Context(), fsToolArgs(t, args))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// nbTools returns a read+edit pair on one registry: the freshness record read
// writes is what lets edit accept virtual line numbers.
func nbTools(t *testing.T) (Tool, Tool) {
	t.Helper()
	reg := NewRegistry()
	reg.Register(NewReadTool())
	reg.Register(NewEditTool())
	read, _ := reg.Get("read")
	edit, _ := reg.Get("edit")
	return read, edit
}

func nbExec(t *testing.T, tl Tool, args map[string]any) Result {
	t.Helper()
	res, err := tl.Execute(t.Context(), fsToolArgs(t, args))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// nbPut builds a PUT op whose body lines are the given final content.
func nbPut(start, end int, lines ...string) map[string]any {
	body := make([]string, 0, len(lines))
	for _, l := range lines {
		body = append(body, "+"+l)
	}
	return map[string]any{"op": "PUT", "range": map[string]any{"start": start, "end": end}, "lines": body}
}

// nbCells unmarshals the notebook's cells from disk.
func nbCells(t *testing.T, path string) []map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatalf("notebook JSON: %v\n%s", err, data)
	}
	var cells []map[string]json.RawMessage
	if err := json.Unmarshal(top["cells"], &cells); err != nil {
		t.Fatalf("cells: %v", err)
	}
	return cells
}

// canonJSON renders v canonically for field-level comparison.
func canonJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestNotebookReadRendersCells(t *testing.T) {
	res := nbRead(t, nbFixturePath(t), nil)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Text)
	}
	want := "1:# %% [code] cell:0\n" +
		"2:print(\"hi\")\n" +
		"3:x = 1\n" +
		"4:# %%[out] execution_count: 3\n" +
		"5:# %%[out] stream stdout: hello\\nworld\\n\n" +
		"6:# %%[out] display_data: image/png, text/plain\n" +
		"7:# %%[out] error: ValueError: bad input\n" +
		"8:# %% [markdown] cell:1\n" +
		"9:# Title\n" +
		"10:text\n" +
		"11:# %% [raw] cell:2\n" +
		"12:\\begin{x}"
	if res.Text != want {
		t.Fatalf("render mismatch:\ngot:\n%s\nwant:\n%s", res.Text, want)
	}
	d := res.Details.(map[string]any)
	if d["notebook"] != true || d["cells"] != 3 || d["totalLines"] != 12 {
		t.Fatalf("details = %v", d)
	}
}

func TestNotebookEditRoundTrip(t *testing.T) {
	path := nbFixturePath(t)
	read, edit := nbTools(t)
	first := nbExec(t, read, map[string]any{"path": path})
	if first.IsError {
		t.Fatalf("read: %s", first.Text)
	}
	res := nbExec(t, edit, map[string]any{"path": path, "ops": []any{nbPut(2, 3, "import numpy as np", "y = np.arange(3)")}})
	if res.IsError {
		t.Fatalf("edit: %s", res.Text)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Canonical in, canonical out: only the edited cell's source lines differ.
	before := strings.Split(strings.TrimSuffix(nbFixture, "\n"), "\n")
	got := strings.Split(strings.TrimSuffix(string(after), "\n"), "\n")
	if len(got) != len(before) {
		t.Fatalf("line count %d -> %d:\n%s", len(before), len(got), after)
	}
	var changed []int
	for i := range before {
		if before[i] != got[i] {
			changed = append(changed, i+1)
		}
	}
	if !reflect.DeepEqual(changed, []int{41, 42}) {
		t.Fatalf("changed lines = %v, want [41 42]:\n%s", changed, after)
	}
	if got[40] != "    \"import numpy as np\\n\"," || got[41] != "    \"y = np.arange(3)\"" {
		t.Fatalf("source lines = %q / %q", got[40], got[41])
	}

	// Field stability: every top-level field but cells, and every other cell,
	// survives unchanged — including outputs, execution_count, ids, metadata.
	var origTop, newTop map[string]json.RawMessage
	if err := json.Unmarshal([]byte(nbFixture), &origTop); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after, &newTop); err != nil {
		t.Fatalf("rewritten notebook is not valid JSON: %v", err)
	}
	for key, want := range origTop {
		if key == "cells" {
			continue
		}
		if string(newTop[key]) != string(want) {
			t.Errorf("top-level %q changed: %s -> %s", key, want, newTop[key])
		}
	}
	var origCells, newCells []map[string]json.RawMessage
	if err := json.Unmarshal(origTop["cells"], &origCells); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(newTop["cells"], &newCells); err != nil {
		t.Fatal(err)
	}
	if len(newCells) != 3 {
		t.Fatalf("cells = %d, want 3", len(newCells))
	}
	for _, i := range []int{1, 2} {
		if canonJSON(t, origCells[i]) != canonJSON(t, newCells[i]) {
			t.Errorf("cell %d changed: %s -> %s", i, canonJSON(t, origCells[i]), canonJSON(t, newCells[i]))
		}
	}
	for _, key := range []string{"cell_type", "execution_count", "id", "metadata", "outputs"} {
		if string(newCells[0][key]) != string(origCells[0][key]) {
			t.Errorf("cell 0 %s changed: %s -> %s", key, origCells[0][key], newCells[0][key])
		}
	}
	var src []string
	if err := json.Unmarshal(newCells[0]["source"], &src); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(src, []string{"import numpy as np\n", "y = np.arange(3)"}) {
		t.Fatalf("cell 0 source = %q", src)
	}

	// read -> edit -> read: the untouched cells and cell 0's output summary
	// render exactly as before.
	second := nbExec(t, read, map[string]any{"path": path, "offset": 4})
	if second.IsError {
		t.Fatalf("re-read: %s", second.Text)
	}
	wantTail := strings.Join(strings.Split(first.Text, "\n")[3:], "\n")
	if second.Text != wantTail {
		t.Fatalf("re-read mismatch:\ngot:\n%s\nwant:\n%s", second.Text, wantTail)
	}
}

func TestNotebookEditRefusesRawJSON(t *testing.T) {
	path := nbFixturePath(t)
	resolved, err := resolvePath(path)
	if err != nil {
		t.Fatal(err)
	}
	rawLines, err := ReadLines(resolved)
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	reg.Register(NewReadTool())
	reg.Register(NewEditTool())
	edit, _ := reg.Get("edit")

	// (a) the model quotes the raw JSON's own snapshot tag.
	res := nbExec(t, edit, map[string]any{
		"path": path + "#" + hashTag(linesHash(rawLines)),
		"ops":  []any{nbPut(41, 42, "x = 2")},
	})
	if !res.IsError || !strings.Contains(res.Text, "raw JSON") || !strings.Contains(res.Text, "cell:0") {
		t.Fatalf("edit with a raw-JSON tag = %q (isError=%v)", res.Text, res.IsError)
	}

	// (b) the freshness record is the raw JSON itself (a pre-notebook read, or
	// the write tool's record).
	window := make(map[int]string, len(rawLines))
	for i, l := range rawLines {
		window[i+1] = l
	}
	reg.recordSnapshot(resolved, linesHash(rawLines), window)
	res = nbExec(t, edit, map[string]any{"path": path, "ops": []any{nbPut(41, 42, "x = 2")}})
	if !res.IsError || !strings.Contains(res.Text, "raw JSON") {
		t.Fatalf("edit after a raw-JSON record = %q (isError=%v)", res.Text, res.IsError)
	}

	if data, err := os.ReadFile(path); err != nil || string(data) != nbFixture {
		t.Fatalf("a refused edit still wrote the notebook: %v", err)
	}
}

func TestNotebookEditRequiresReadFirst(t *testing.T) {
	path := nbFixturePath(t)
	reg := NewRegistry()
	reg.Register(NewEditTool())
	edit, _ := reg.Get("edit")

	res := nbExec(t, edit, map[string]any{"path": path, "ops": []any{nbPut(2, 3, "x = 2")}})
	if !res.IsError || !strings.Contains(res.Text, "read it first") {
		t.Fatalf("edit without a read = %q (isError=%v)", res.Text, res.IsError)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != nbFixture {
		t.Fatal("a refused edit still wrote the notebook")
	}
}

func TestNotebookSourceCap(t *testing.T) {
	source := make([]any, 0, notebookCellSourceMax+5)
	for i := range notebookCellSourceMax + 5 {
		source = append(source, fmt.Sprintf("x%d = %d\n", i, i))
	}
	path := nbWrite(t, "bigcell.ipynb", map[string]any{
		"cells": []any{map[string]any{
			"cell_type": "code", "execution_count": nil, "id": "cell-big", "metadata": map[string]any{},
			"outputs": []any{}, "source": source,
		}},
		"metadata": map[string]any{}, "nbformat": 4, "nbformat_minor": 5,
	})
	read, edit := nbTools(t)
	res := nbExec(t, read, map[string]any{"path": path})
	if res.IsError {
		t.Fatalf("read: %s", res.Text)
	}
	if !strings.Contains(res.Text, "# %%[truncated] +6 source lines not shown (cell source cap is 200 lines)") {
		t.Fatalf("source cap marker missing:\n%s", res.Text)
	}
	// A write-back against a truncated view would drop the omitted lines.
	er := nbExec(t, edit, map[string]any{"path": path, "ops": []any{nbPut(2, 2, "x = 1")}})
	if !er.IsError || !strings.Contains(er.Text, "truncated") {
		t.Fatalf("edit on a truncated cell = %q (isError=%v)", er.Text, er.IsError)
	}
	if data, err := os.ReadFile(path); err != nil || !strings.Contains(string(data), `x204 = 204`) {
		t.Fatal("a refused edit dropped source lines")
	}
}

func TestNotebookOutputCaps(t *testing.T) {
	outputs := make([]any, 0, 12)
	for i := range 12 {
		outputs = append(outputs, map[string]any{
			"name": "stdout", "output_type": "stream", "text": []any{fmt.Sprintf("out%d\n", i)},
		})
	}
	mimes := map[string]any{}
	for _, m := range []string{"application/json", "application/pdf", "image/png", "image/svg+xml", "text/csv", "text/html", "text/latex", "text/plain"} {
		mimes[m] = "x"
	}
	path := nbWrite(t, "caps.ipynb", map[string]any{
		"cells": []any{
			map[string]any{
				"cell_type": "code", "execution_count": 1, "id": "cell-many", "metadata": map[string]any{},
				"outputs": outputs, "source": []any{"pass"},
			},
			map[string]any{
				"cell_type": "code", "execution_count": 2, "id": "cell-long", "metadata": map[string]any{},
				"outputs": []any{map[string]any{"name": "stdout", "output_type": "stream", "text": []any{strings.Repeat("x", 600)}}},
				"source":  []any{"print(1)"},
			},
			map[string]any{
				"cell_type": "code", "execution_count": 3, "id": "cell-mimes", "metadata": map[string]any{},
				"outputs": []any{map[string]any{"data": mimes, "metadata": map[string]any{}, "output_type": "display_data"}},
				"source":  []any{"plot()"},
			},
			map[string]any{
				"cell_type": "code", "execution_count": 4, "id": "cell-err", "metadata": map[string]any{},
				"outputs": []any{map[string]any{"ename": "ValueError", "evalue": "bad", "output_type": "error", "traceback": []any{"line 1"}}},
				"source":  []any{"boom()"},
			},
		},
		"metadata": map[string]any{}, "nbformat": 4, "nbformat_minor": 5,
	})
	res := nbRead(t, path, nil)
	if res.IsError {
		t.Fatalf("read: %s", res.Text)
	}
	for _, want := range []string{
		"# %%[out] stream stdout: out0", // outputs are listed, not inlined raw
		"# %%[out] … (+2 more outputs)",
		"stream stdout: xxx", // 600 runes capped at 500 + a size note
		"…(+100 chars)",
		"display_data: application/json, application/pdf, image/png, image/svg+xml, text/csv, text/html (+2 more)",
		"# %%[out] error: ValueError: bad",
	} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("render missing %q:\n%s", want, res.Text)
		}
	}
	if strings.Contains(res.Text, "text/plain") {
		t.Errorf("mime list exceeded its cap:\n%s", res.Text)
	}
}

func TestNotebookLargeGuard(t *testing.T) {
	large := nbWrite(t, "large.ipynb", map[string]any{
		"cells": []any{map[string]any{
			"cell_type": "code", "execution_count": 1, "id": "cell-big", "metadata": map[string]any{},
			"outputs": []any{map[string]any{"name": "stdout", "output_type": "stream", "text": []any{strings.Repeat("x", notebookLargeBytes+1)}}},
			"source":  []any{"print(1)"},
		}},
		"metadata": map[string]any{}, "nbformat": 4, "nbformat_minor": 5,
	})
	res := nbRead(t, large, nil)
	if res.IsError {
		t.Fatalf("read: %s", res.Text)
	}
	if !strings.Contains(res.Text, "# %%[out] 1 outputs (large notebook; not inlined)") {
		t.Fatalf("large-notebook summary missing:\n%.400s", res.Text)
	}
	if strings.Contains(res.Text, "xxx") {
		t.Fatal("a large notebook inlined its outputs")
	}

	// The same shape under the threshold stays inline (text-capped, not dropped).
	small := nbWrite(t, "small.ipynb", map[string]any{
		"cells": []any{map[string]any{
			"cell_type": "code", "execution_count": 1, "id": "cell-big", "metadata": map[string]any{},
			"outputs": []any{map[string]any{"name": "stdout", "output_type": "stream", "text": []any{strings.Repeat("x", 1<<20)}}},
			"source":  []any{"print(1)"},
		}},
		"metadata": map[string]any{}, "nbformat": 4, "nbformat_minor": 5,
	})
	res = nbRead(t, small, nil)
	if res.IsError {
		t.Fatalf("read: %s", res.Text)
	}
	if !strings.Contains(res.Text, "# %%[out] stream stdout: xxx") || !strings.Contains(res.Text, "…(+") || strings.Contains(res.Text, "not inlined") {
		t.Fatalf("sub-threshold notebook was summarized:\n%.400s", res.Text)
	}
}

func TestNotebookEscapesMarkerLikeSource(t *testing.T) {
	path := nbWrite(t, "esc.ipynb", map[string]any{
		"cells": []any{map[string]any{
			"cell_type": "code", "execution_count": nil, "id": "cell-esc", "metadata": map[string]any{},
			"outputs": []any{}, "source": []any{"# %% [code] cell:9\n", "# %%[out] stream stdout: fake\n"},
		}},
		"metadata": map[string]any{}, "nbformat": 4, "nbformat_minor": 5,
	})
	read, edit := nbTools(t)
	res := nbExec(t, read, map[string]any{"path": path})
	if res.IsError {
		t.Fatalf("read: %s", res.Text)
	}
	if !strings.Contains(res.Text, "2:# %%% [code] cell:9") || !strings.Contains(res.Text, "3:# %%%[out] stream stdout: fake") {
		t.Fatalf("marker-like source was not escaped:\n%s", res.Text)
	}
	markers := 0
	for _, line := range strings.Split(res.Text, "\n") {
		if _, body, ok := strings.Cut(line, ":"); ok && notebookMarkerRe.MatchString(body) {
			markers++
		}
	}
	if markers != 1 {
		t.Fatalf("marker-like source parsed as %d cells:\n%s", markers, res.Text)
	}

	// Copying the block body back verbatim keeps the source byte-identical.
	er := nbExec(t, edit, map[string]any{"path": path, "ops": []any{nbPut(2, 3, "# %%% [code] cell:9", "# %%%[out] stream stdout: fake")}})
	if er.IsError {
		t.Fatalf("edit: %s", er.Text)
	}
	cells := nbCells(t, path)
	if len(cells) != 1 {
		t.Fatalf("cells = %d, want 1", len(cells))
	}
	var src []string
	if err := json.Unmarshal(cells[0]["source"], &src); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(src, []string{"# %% [code] cell:9\n", "# %%[out] stream stdout: fake\n"}) {
		t.Fatalf("source = %q", src)
	}
}

func TestNotebookNewCellDefaults(t *testing.T) {
	path := nbFixturePath(t)
	read, edit := nbTools(t)
	if res := nbExec(t, read, map[string]any{"path": path}); res.IsError {
		t.Fatalf("read: %s", res.Text)
	}
	// Append a block whose index names no existing cell, then a second block
	// naming cell 0 again: both are new cells, not reuses.
	res := nbExec(t, edit, map[string]any{"path": path, "ops": []any{
		nbPut(12, 12, `\begin{x}`, "# %% [code] cell:9", "print(1)", "# %% [code] cell:0", "print(2)"),
	}})
	if res.IsError {
		t.Fatalf("edit: %s", res.Text)
	}
	cells := nbCells(t, path)
	if len(cells) != 5 {
		t.Fatalf("cells = %d, want 5", len(cells))
	}
	for _, i := range []int{3, 4} {
		var typ string
		json.Unmarshal(cells[i]["cell_type"], &typ)
		if typ != "code" {
			t.Errorf("cell %d type = %q", i, typ)
		}
		if string(cells[i]["metadata"]) != "{}" {
			t.Errorf("cell %d metadata = %s", i, cells[i]["metadata"])
		}
		if string(cells[i]["execution_count"]) != "null" {
			t.Errorf("cell %d execution_count = %s", i, cells[i]["execution_count"])
		}
		if string(cells[i]["outputs"]) != "[]" {
			t.Errorf("cell %d outputs = %s", i, cells[i]["outputs"])
		}
		var id string
		if err := json.Unmarshal(cells[i]["id"], &id); err != nil || len(id) != 8 {
			t.Errorf("cell %d id = %q (%v)", i, id, err)
		}
	}
	var src []string
	json.Unmarshal(cells[3]["source"], &src)
	if !reflect.DeepEqual(src, []string{"print(1)"}) {
		t.Errorf("new cell source = %q", src)
	}
	// The fixture cells keep their own ids: reuse still wins where it applies.
	for i, want := range []string{"cell-code", "cell-md", "cell-raw"} {
		var id string
		json.Unmarshal(cells[i]["id"], &id)
		if id != want {
			t.Errorf("cell %d id = %q, want %q", i, id, want)
		}
	}
}

func TestNotebookInvalidJSONStaysText(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"broken.ipynb":  `{"cells": [`,
		"nocells.ipynb": `{"nbformat": 4}`,
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		res := nbRead(t, path, nil)
		if res.IsError {
			t.Fatalf("%s: unexpected error: %s", name, res.Text)
		}
		if strings.Contains(res.Text, "cell:") {
			t.Fatalf("%s rendered as a notebook: %s", name, res.Text)
		}
		if got := stripReadHeader(t, res.Text); got != "1:"+content {
			t.Fatalf("%s text = %q", name, got)
		}
	}

	// A half-written notebook stays repairable: edit treats it as text.
	path := filepath.Join(dir, "broken.ipynb")
	reg := NewRegistry()
	reg.Register(NewEditTool())
	edit, _ := reg.Get("edit")
	if res := nbExec(t, edit, map[string]any{"path": path, "ops": []any{nbPut(1, 1, `{"cells": [], "nbformat": 4, "nbformat_minor": 5}`)}}); res.IsError {
		t.Fatalf("edit: %s", res.Text)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "{\"cells\": [], \"nbformat\": 4, \"nbformat_minor\": 5}\n" {
		t.Fatalf("repaired file = %q (%v)", data, err)
	}
}

func TestNotebookWithoutCells(t *testing.T) {
	path := nbWrite(t, "empty.ipynb", map[string]any{
		"cells": []any{}, "metadata": map[string]any{}, "nbformat": 4, "nbformat_minor": 5,
	})
	res := nbRead(t, path, nil)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Text)
	}
	if res.Text != "(notebook has no cells)" {
		t.Fatalf("text = %q", res.Text)
	}
}
