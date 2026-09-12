package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// galleryTestSession materializes one real session JSONL under dataDir and
// returns its full id. The user message carries "needle-xyz" so an export can
// be asserted on content, not just on the file existing.
//
// Ordering matters: a memory-only store writes nothing until the first
// ASSISTANT message (omp's "no junk files for aborted starts" rule), so the
// user turn has to be appended before it, and the assistant turn is what puts
// both on disk.
func galleryTestSession(t *testing.T, dataDir, cwd string) string {
	t.Helper()
	store := session.OpenMem(cwd, "gallery fixture")
	store.EnableAutoPersist(session.SessionFilePath(dataDir, cwd, time.Now(), store.ID()), session.Options{})
	if err := store.Append(&session.MessageEntry{Message: ai.Message{
		Role:    ai.RoleUser,
		Content: []ai.Block{ai.TextBlock{Text: "needle-xyz"}},
	}}); err != nil {
		t.Fatalf("append user message: %v", err)
	}
	if err := store.Append(&session.MessageEntry{Message: ai.Message{
		Role:    ai.RoleAssistant,
		Content: []ai.Block{ai.TextBlock{Text: "ack"}},
	}}); err != nil {
		t.Fatalf("append assistant message: %v", err)
	}
	if store.Path() == "" {
		t.Fatal("session never materialized: no assistant-triggered flush")
	}
	id := store.ID()
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	return id
}

// galleryRun is the common sandboxed invocation: the data dir is explicit (the
// command takes it) and XDEV_AGENT_DIR is pinned too so nothing in the render
// path can reach the real store.
func galleryRun(t *testing.T, mode string, args []string, dataDir, cwd string) (int, string, string) {
	t.Helper()
	t.Setenv("XDEV_AGENT_DIR", dataDir)
	var out, errOut bytes.Buffer
	code := galleryCmdAs(mode, args, dataDir, cwd, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestGalleryListsSessions(t *testing.T) {
	dataDir := t.TempDir()
	cwd := t.TempDir()
	id := galleryTestSession(t, dataDir, cwd)

	code, out, errOut := galleryRun(t, "gallery", nil, dataDir, cwd)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut)
	}
	if errOut != "" {
		t.Fatalf("listing wrote to stderr: %s", errOut)
	}
	if !strings.Contains(out, id[:8]) {
		t.Errorf("listing is missing the short id %s:\n%s", id[:8], out)
	}
	if !strings.Contains(out, "gallery fixture") {
		t.Errorf("listing is missing the session title:\n%s", out)
	}
	if !strings.Contains(out, truncate(cwd, galleryCWDWidth)) { // CWD cell truncates to the table width
		t.Errorf("listing is missing the session cwd:\n%s", out)
	}
}

func TestGalleryEmptyStoreSaysSo(t *testing.T) {
	dataDir := t.TempDir()
	code, out, errOut := galleryRun(t, "gallery", nil, dataDir, t.TempDir())
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut)
	}
	if !strings.Contains(out, "no sessions") {
		t.Errorf("empty store should say so, got:\n%s", out)
	}
	if strings.Contains(out, "TITLE") {
		t.Errorf("empty store printed a table header:\n%s", out)
	}
}

func TestGalleryListRespectsLimit(t *testing.T) {
	dataDir := t.TempDir()
	cwd := t.TempDir()
	ids := []string{
		galleryTestSession(t, dataDir, cwd),
		galleryTestSession(t, dataDir, cwd),
		galleryTestSession(t, dataDir, cwd),
	}
	code, out, errOut := galleryRun(t, "gallery", []string{"--limit", "1"}, dataDir, cwd)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut)
	}
	shown := 0
	for _, id := range ids {
		shown += strings.Count(out, id[:8])
	}
	if shown != 1 {
		t.Errorf("--limit 1 listed %d sessions, want 1:\n%s", shown, out)
	}
	if !strings.Contains(out, "2 more") {
		t.Errorf("--limit 1 should report the hidden sessions:\n%s", out)
	}
}

func TestGalleryJSONListing(t *testing.T) {
	dataDir := t.TempDir()
	cwd := t.TempDir()
	id := galleryTestSession(t, dataDir, cwd)

	code, out, errOut := galleryRun(t, "gallery", []string{"--json"}, dataDir, cwd)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut)
	}
	var rows []galleryRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("--json output does not unmarshal: %v\n%s", err, out)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1:\n%s", len(rows), out)
	}
	if rows[0].ID != id || rows[0].ShortID != id[:8] {
		t.Errorf("row id = %q/%q, want %q/%q", rows[0].ID, rows[0].ShortID, id, id[:8])
	}
	if rows[0].Title != "gallery fixture" || rows[0].CWD != cwd {
		t.Errorf("row title/cwd = %q/%q, want %q/%q", rows[0].Title, rows[0].CWD, "gallery fixture", cwd)
	}
	if rows[0].SizeBytes <= 0 || rows[0].ModTime.IsZero() || rows[0].Subagent {
		t.Errorf("row metadata looks wrong: %+v", rows[0])
	}
}

func TestGalleryMetaView(t *testing.T) {
	dataDir := t.TempDir()
	cwd := t.TempDir()
	id := galleryTestSession(t, dataDir, cwd)

	code, out, errOut := galleryRun(t, "gallery", []string{id[:8]}, dataDir, cwd)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut)
	}
	for _, want := range []string{"gallery fixture", id, cwd, "--html"} {
		if !strings.Contains(out, want) {
			t.Errorf("metadata view missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, session.SessionsRoot(dataDir)) {
		t.Errorf("metadata view should show the .jsonl path:\n%s", out)
	}
}

func TestGalleryRendersHTML(t *testing.T) {
	dataDir := t.TempDir()
	cwd := t.TempDir()
	id := galleryTestSession(t, dataDir, cwd)

	t.Run("--out", func(t *testing.T) {
		outPath := filepath.Join(t.TempDir(), "transcript.html")
		code, out, errOut := galleryRun(t, "gallery",
			[]string{id[:8], "--html", "--out", outPath}, dataDir, cwd)
		if code != 0 {
			t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut)
		}
		body, err := os.ReadFile(outPath)
		if err != nil {
			t.Fatalf("read rendered file: %v", err)
		}
		if !strings.Contains(string(body), "needle-xyz") {
			t.Errorf("rendered HTML does not contain the transcript text:\n%s", body)
		}
		if !strings.Contains(out, "Rendered: "+outPath) {
			t.Errorf("stdout should name the written path, got:\n%s", out)
		}
	})

	t.Run("default path", func(t *testing.T) {
		code, out, errOut := galleryRun(t, "gallery", []string{id[:8], "--html"}, dataDir, cwd)
		if code != 0 {
			t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut)
		}
		want := filepath.Join(cwd, id[:8]+".html")
		if _, err := os.Stat(want); err != nil {
			t.Fatalf("default output path %s: %v (stdout: %s)", want, err, out)
		}
	})
}

func TestGalleryRenderAlias(t *testing.T) {
	dataDir := t.TempDir()
	cwd := t.TempDir()
	id := galleryTestSession(t, dataDir, cwd)

	// `render <selector>` needs no --html flag: the mode implies it.
	code, out, errOut := galleryRun(t, "render", []string{id[:8]}, dataDir, cwd)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut)
	}
	want := filepath.Join(cwd, id[:8]+".html")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("render did not write %s: %v (stdout: %s)", want, err, out)
	}
	if !strings.Contains(out, "Rendered: "+want) {
		t.Errorf("render stdout = %q, want the written path", out)
	}
}

func TestGallerySelectorByPath(t *testing.T) {
	dataDir := t.TempDir()
	cwd := t.TempDir()
	galleryTestSession(t, dataDir, cwd)
	metas, err := session.List(dataDir)
	if err != nil || len(metas) != 1 {
		t.Fatalf("session.List = %v entries, %v", len(metas), err)
	}

	outPath := filepath.Join(t.TempDir(), "by-path.html")
	code, _, errOut := galleryRun(t, "gallery",
		[]string{metas[0].Path, "--html", "--out", outPath}, dataDir, cwd)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut)
	}
	if body, err := os.ReadFile(outPath); err != nil || !strings.Contains(string(body), "needle-xyz") {
		t.Fatalf("path selector render failed: %v", err)
	}
}

func TestGalleryUnmatchedSelector(t *testing.T) {
	dataDir := t.TempDir()
	cwd := t.TempDir()
	galleryTestSession(t, dataDir, cwd)

	// Hex ids can never contain a 'z', so this selector cannot accidentally
	// prefix-match a real session.
	code, _, errOut := galleryRun(t, "gallery", []string{"zzzzzzzz"}, dataDir, cwd)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (stderr: %s)", code, errOut)
	}
	if !strings.Contains(errOut, "zzzzzzzz") {
		t.Errorf("error should name the selector, got: %s", errOut)
	}
}

func TestGalleryRenderNeedsSelector(t *testing.T) {
	dataDir := t.TempDir()
	cwd := t.TempDir()
	galleryTestSession(t, dataDir, cwd)

	for _, tc := range []struct {
		mode string
		args []string
	}{
		{"render", nil},
		{"gallery", []string{"--html"}},
	} {
		code, out, errOut := galleryRun(t, tc.mode, tc.args, dataDir, cwd)
		if code != 2 {
			t.Errorf("%s %v: exit = %d, want 2", tc.mode, tc.args, code)
		}
		if !strings.Contains(errOut, "usage:") {
			t.Errorf("%s %v: no usage block on stderr:\n%s", tc.mode, tc.args, errOut)
		}
		if strings.Contains(out, "no sessions") {
			t.Errorf("%s %v: fell through to the listing", tc.mode, tc.args)
		}
	}
}

func TestGalleryBytes(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{4300, "4.2 KiB"},
		{3 * 1024 * 1024, "3.0 MiB"},
		{2 * 1024 * 1024 * 1024, "2.0 GiB"},
	} {
		if got := galleryBytes(tc.in); got != tc.want {
			t.Errorf("galleryBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
