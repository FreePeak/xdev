package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// freshnessRegistry builds a registry with the three participants so the
// tools receive the shared snapshot map (Register injects it).
func freshnessRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	reg.Register(NewReadTool())
	reg.Register(NewWriteTool())
	reg.Register(NewEditTool())
	return reg
}

func toolFrom(t *testing.T, reg *Registry, name string) Tool {
	t.Helper()
	res, ok := reg.Get(name)
	if !ok {
		t.Fatalf("registry has no %s tool", name)
	}
	return res
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runTool(t *testing.T, tl Tool, body map[string]any) Result {
	t.Helper()
	res, err := tl.Execute(context.Background(), args(t, body))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestEditReanchorsAfterStaleRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	writeFile(t, path, "alpha\nbeta\ngamma\ndelta\n")
	reg := freshnessRegistry(t)
	read := toolFrom(t, reg, "read")
	runTool(t, read, map[string]any{"path": path})

	// The file changes underneath the model (another tool, another agent):
	// two lines appear at the top, so every recorded number shifted by 2.
	writeFile(t, path, "new1\nnew2\nalpha\nbeta\ngamma\ndelta\n")

	edit := toolFrom(t, reg, "edit")
	res := runTool(t, edit, map[string]any{
		"path": path,
		"ops": []map[string]any{{
			"op": "PUT", "range": map[string]any{"line": 2}, "lines": []string{"+BETA"},
		}},
	})
	if res.IsError {
		t.Fatalf("expected a repaired edit, got %s", res.Text)
	}
	if !strings.Contains(res.Text, "recovered by exact text") || !strings.Contains(res.Text, "op 1 (PUT): lines 2-2 → 4-4") {
		t.Fatalf("result must report the recovery: %q", res.Text)
	}
	if got := readFile(t, path); got != "new1\nnew2\nalpha\nBETA\ngamma\ndelta\n" {
		t.Fatalf("file = %q", got)
	}
}

func TestEditStaleTextGoneStillRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	writeFile(t, path, "alpha\nbeta\ngamma\n")
	reg := freshnessRegistry(t)
	runTool(t, toolFrom(t, reg, "read"), map[string]any{"path": path})

	// Total replacement: the recorded text is nowhere to be found.
	writeFile(t, path, "x\ny\nz\n")

	res := runTool(t, toolFrom(t, reg, "edit"), map[string]any{
		"path": path,
		"ops": []map[string]any{{
			"op": "PUT", "range": map[string]any{"line": 2}, "lines": []string{"+BETA"},
		}},
	})
	if !res.IsError {
		t.Fatalf("unrecoverable staleness must be rejected, got %q", res.Text)
	}
	if !strings.Contains(res.Text, path) || !strings.Contains(res.Text, "changed since it was read") {
		t.Fatalf("error must name the file and the staleness: %q", res.Text)
	}
	if !strings.Contains(res.Text, "no longer exist") {
		t.Fatalf("error should say the text is gone: %q", res.Text)
	}
	if got := readFile(t, path); got != "x\ny\nz\n" {
		t.Fatalf("rejected edit touched the file: %q", got)
	}
}

func TestEditStaleAmbiguousTargetRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	writeFile(t, path, "alpha\nbeta\ngamma\n")
	reg := freshnessRegistry(t)
	runTool(t, toolFrom(t, reg, "read"), map[string]any{"path": path})

	// "beta" now appears twice: re-anchoring cannot be safe.
	writeFile(t, path, "beta\nalpha\nbeta\n")

	res := runTool(t, toolFrom(t, reg, "edit"), map[string]any{
		"path": path,
		"ops": []map[string]any{{
			"op": "CUT", "range": map[string]any{"line": 2},
		}},
	})
	if !res.IsError || !strings.Contains(res.Text, "match 2 places") {
		t.Fatalf("ambiguous target must be rejected with a count: %q", res.Text)
	}
}

func TestEditTagMatchingCurrentSnapshotSkipsRepair(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	writeFile(t, path, "alpha\nbeta\n")
	reg := freshnessRegistry(t)
	read := toolFrom(t, reg, "read")
	runTool(t, read, map[string]any{"path": path})

	// The model quotes the tag it has just seen: numbers are authoritative.
	lines, err := ReadLines(path)
	if err != nil {
		t.Fatal(err)
	}
	tag := hashTag(linesHash(lines))
	res := runTool(t, toolFrom(t, reg, "edit"), map[string]any{
		"path": path + "#" + tag,
		"ops": []map[string]any{{
			"op": "PUT", "range": map[string]any{"line": 2}, "lines": []string{"+BETA"},
		}},
	})
	if res.IsError {
		t.Fatalf("matching tag must be accepted: %s", res.Text)
	}
	if got := readFile(t, path); got != "alpha\nBETA\n" {
		t.Fatalf("file = %q", got)
	}
}

func TestEditMismatchedTagRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	writeFile(t, path, "alpha\nbeta\n")
	reg := freshnessRegistry(t)
	runTool(t, toolFrom(t, reg, "read"), map[string]any{"path": path})

	res := runTool(t, toolFrom(t, reg, "edit"), map[string]any{
		"path": path + "#dead",
		"ops": []map[string]any{{
			"op": "PUT", "range": map[string]any{"line": 1}, "lines": []string{"+A"},
		}},
	})
	if !res.IsError || !strings.Contains(res.Text, "snapshot tag dead") {
		t.Fatalf("unknown tag must be rejected with the tag named: %q", res.Text)
	}
}

func TestEditWriteRefreshesFreshness(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	reg := freshnessRegistry(t)
	// Read an old version, then overwrite it with the write tool: the
	// write is the newest anchor, so the edit must not be stale.
	writeFile(t, path, "old\ncontent\n")
	runTool(t, toolFrom(t, reg, "read"), map[string]any{"path": path})
	runTool(t, toolFrom(t, reg, "write"), map[string]any{"path": path, "content": "one\ntwo\nthree\n"})

	res := runTool(t, toolFrom(t, reg, "edit"), map[string]any{
		"path": path,
		"ops": []map[string]any{{
			"op": "PUT", "range": map[string]any{"line": 2}, "lines": []string{"+TWO"},
		}},
	})
	if res.IsError {
		t.Fatalf("edit after write must not be stale: %s", res.Text)
	}
	if got := readFile(t, path); got != "one\nTWO\nthree\n" {
		t.Fatalf("file = %q", got)
	}
}

func TestEditAfterEditStaysFresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	writeFile(t, path, "a\nb\nc\n")
	reg := freshnessRegistry(t)
	runTool(t, toolFrom(t, reg, "read"), map[string]any{"path": path})

	edit := toolFrom(t, reg, "edit")
	first := runTool(t, edit, map[string]any{
		"path": path,
		"ops": []map[string]any{{
			"op": "PUT", "range": map[string]any{"line": 1}, "lines": []string{"+A1", "+A2"},
		}},
	})
	if first.IsError {
		t.Fatalf("first edit failed: %s", first.Text)
	}
	// The model anchors the next edit on the first result's window.
	second := runTool(t, edit, map[string]any{
		"path": path,
		"ops": []map[string]any{{
			"op": "CUT", "range": map[string]any{"line": 3},
		}},
	})
	if second.IsError {
		t.Fatalf("second edit must not be stale: %s", second.Text)
	}
	if got := readFile(t, path); got != "A1\nA2\nc\n" {
		t.Fatalf("file = %q", got)
	}
}

func TestEditSnapshotFollowsMV(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.txt")
	dst := filepath.Join(dir, "b.txt")
	writeFile(t, src, "one\ntwo\n")
	reg := freshnessRegistry(t)
	runTool(t, toolFrom(t, reg, "read"), map[string]any{"path": src})

	edit := toolFrom(t, reg, "edit")
	res := runTool(t, edit, map[string]any{
		"path": src,
		"ops":  []map[string]any{{"op": "MV", "dest": dst}},
	})
	if res.IsError {
		t.Fatalf("move failed: %s", res.Text)
	}
	res = runTool(t, edit, map[string]any{
		"path": dst,
		"ops": []map[string]any{{
			"op": "PUT", "range": map[string]any{"line": 2}, "lines": []string{"+TWO"},
		}},
	})
	if res.IsError {
		t.Fatalf("edit after MV must not be stale: %s", res.Text)
	}
	if got := readFile(t, dst); got != "one\nTWO\n" {
		t.Fatalf("file = %q", got)
	}
}

func TestEditStaleOpOutsideRecordedWindowRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	writeFile(t, path, "l1\nl2\nl3\nl4\n")
	reg := freshnessRegistry(t)
	// A windowed read records only lines 1-2.
	runTool(t, toolFrom(t, reg, "read"), map[string]any{"path": path, "offset": 1, "limit": 2})
	writeFile(t, path, "l1\nl2\nl3\nl4\nl5\n")

	res := runTool(t, toolFrom(t, reg, "edit"), map[string]any{
		"path": path,
		"ops": []map[string]any{{
			"op": "PUT", "range": map[string]any{"line": 4}, "lines": []string{"+L4"},
		}},
	})
	if !res.IsError || !strings.Contains(res.Text, "did not record") {
		t.Fatalf("unverifiable op on a changed file must be rejected: %q", res.Text)
	}
	if got := readFile(t, path); got != "l1\nl2\nl3\nl4\nl5\n" {
		t.Fatalf("rejected edit touched the file: %q", got)
	}
}

func TestEditStandaloneToolHasNoFreshnessGate(t *testing.T) {
	// Unregistered tools keep the pre-guard contract (bounds checks only):
	// the existing out-of-range test relies on this.
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	writeFile(t, path, "a\nb\n")
	res := runTool(t, NewEditTool(), map[string]any{
		"path": path,
		"ops": []map[string]any{{
			"op": "CUT", "range": map[string]any{"line": 3},
		}},
	})
	if !res.IsError || !strings.Contains(res.Text, "out of bounds") {
		t.Fatalf("standalone tool must keep the bounds error: %q", res.Text)
	}
}
