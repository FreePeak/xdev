package share

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/session"
)

// boomStore builds a session holding every entry type the export knows, with
// markup in the text so escaping is exercised too.
func boomStore(t *testing.T) *session.Store {
	t.Helper()
	st := session.OpenMem("/proj", "export test")
	ts := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	entries := []session.Entry{
		&session.MessageEntry{Env: session.Envelope{ID: "e01", Timestamp: ts}, Message: ai.Message{
			Role:    ai.RoleUser,
			Content: []ai.Block{ai.TextBlock{Text: "hello <script>alert(1)</script>"}},
		}},
		&session.MessageEntry{Env: session.Envelope{ID: "e02", Timestamp: ts}, Message: ai.Message{
			Role: ai.RoleAssistant,
			Content: []ai.Block{
				ai.ThinkingBlock{Thinking: "need <b>the</b> file"},
				ai.TextBlock{Text: "reading it"},
				ai.ToolCallBlock{ID: "call-1", Name: "read", Arguments: []byte(`{"path":"a<b>.go"}`)},
				ai.ImageBlock{Source: ai.ImageSource{Type: "base64", MediaType: "image/png"}},
			},
			Model: "onegw/free",
		}},
		&session.MessageEntry{Env: session.Envelope{ID: "e03", Timestamp: ts}, Message: ai.Message{
			Role:       ai.RoleToolResult,
			ToolCallID: "call-1",
			ToolName:   "read",
			IsError:    true,
			Content:    []ai.Block{ai.TextBlock{Text: "no such file"}},
		}},
		&session.ModelChangeEntry{Env: session.Envelope{ID: "e04", Timestamp: ts}, Model: "onegw/good"},
		&session.CompactionEntry{Env: session.Envelope{ID: "e05", Timestamp: ts}, TokensBefore: 12345,
			Summary: ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "compacted <summary>"}}}},
		&session.BranchSummaryEntry{Env: session.Envelope{ID: "e06", Timestamp: ts},
			Summary: ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "abandoned branch"}}}},
		&session.ResetBoundaryEntry{Env: session.Envelope{ID: "e07", Timestamp: ts}},
		&session.CustomEntry{Env: session.Envelope{ID: "e08", Timestamp: ts}, CustomType: "session_exit",
			Data: map[string]any{"mode": "tui", "code": 0}},
		&session.GoalUpdatedEntry{Env: session.Envelope{ID: "e09", Timestamp: ts}, Goal: session.GoalPayload{
			Objective: "ship <export>", Status: "active", TokenBudget: 100, Spent: 7,
			Evidence: []string{"tests green", "html written"},
		}},
		&session.CheckpointEntry{Env: session.Envelope{ID: "e10", Timestamp: ts},
			Checkpoint: session.CheckpointPayload{Name: "before-rewrite", EntryID: "e02", Note: "safe point"}},
		&session.UnknownEntry{Env: session.Envelope{ID: "e11", Timestamp: ts}, EntryType: "title_change",
			Raw: []byte(`{"type":"title_change","title":"foreign"}`)},
	}
	for _, e := range entries {
		if err := st.Append(e); err != nil {
			t.Fatalf("append %T: %v", e, err)
		}
	}
	return st
}

// TestRenderEscapesTranscriptMarkup: transcript text is data, never markup —
// a message carrying <script> must render as escaped text so the page cannot
// execute it, and the raw tag must not survive anywhere in the document.
func TestRenderEscapesTranscriptMarkup(t *testing.T) {
	st := boomStore(t)
	html, err := Render(FromStore(st, Options{SystemPrompt: "system <script>alert('sys')</script>"}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(html, "<script>alert(1)</script>") {
		t.Fatal("message markup was emitted unescaped")
	}
	if !strings.Contains(html, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Fatalf("escaped message text missing:\n%s", html)
	}
	if strings.Contains(html, "<b>the</b>") {
		t.Fatal("thinking markup was emitted unescaped")
	}
	if strings.Contains(html, "a<b>.go") {
		t.Fatal("tool arguments were emitted unescaped")
	}
	// The only script element in the document is none at all: the export
	// needs no JavaScript.
	if strings.Contains(strings.ToLower(html), "<script") {
		t.Fatal("export must not contain any <script> element")
	}
	if !strings.Contains(html, "system &lt;script&gt;") {
		t.Fatal("system prompt was not escaped/rendered")
	}
}

// TestRenderCoversEveryEntryKind: an export is an archive — every entry type
// reaches the page, including the ones the model context drops.
func TestRenderCoversEveryEntryKind(t *testing.T) {
	s := boomStore(t)
	html, err := Render(FromStore(s, Options{Model: "onegw/live"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{KindUser, KindAssistant, KindToolResult, KindModelChange, KindCompaction,
		KindBranchSummary, KindResetBoundary, KindCustom, KindGoal, KindCheckpoint, KindUnknown} {
		if !strings.Contains(html, `data-kind="`+kind+`"`) {
			t.Errorf("entry kind %q missing from the export", kind)
		}
	}
	for _, want := range []string{
		"hello", "reading it", "need", "read", "call-1", "no such file", "[image: image/png]",
		"onegw/good", "compacted", "abandoned branch", "session_exit", "ship",
		"before-rewrite", "title_change", "evidence:",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("export lost %q", want)
		}
	}
	// The metadata header carries session id, model, cwd and timestamps.
	for _, want := range []string{s.ID(), s.ID()[:8], "onegw/live", "/proj", "2026-09-12 10:00:00 UTC", "exported"} {
		if !strings.Contains(html, want) {
			t.Errorf("metadata header lost %q", want)
		}
	}
	if strings.Count(html, "<article") != strings.Count(html, "</article>") || strings.Count(html, "<article") < 11 {
		t.Fatalf("unbalanced or missing entry blocks: %d/%d", strings.Count(html, "<article"), strings.Count(html, "</article>"))
	}
}

// TestRenderEmptySessionIsValidHTML: a session with no entries still yields a
// complete, standalone document.
func TestRenderEmptySessionIsValidHTML(t *testing.T) {
	s := session.OpenMem("/empty", "")
	html, err := Render(FromStore(s, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(html, "<!doctype html>") {
		t.Fatalf("missing doctype:\n%.80s", html)
	}
	for _, want := range []string{`<html lang="en">`, "</head>", "<body>", "</body>", "</html>", "<style>"} {
		if !strings.Contains(html, want) {
			t.Errorf("empty export missing %q", want)
		}
	}
	if strings.Count(html, "<article") != 0 {
		t.Error("empty session rendered entries")
	}
	if strings.Contains(html, "http://") || strings.Contains(html, "src=") {
		t.Error("export must be self-contained: no external asset, no absolute URL")
	}
}

// TestExportWritesRequestedPath: /export <path> writes exactly there, and the
// default path lands under the agent data dir.
func TestExportWritesRequestedPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDEV_AGENT_DIR", filepath.Join(home, "agent"))
	st := boomStore(t)

	path := filepath.Join(home, "nested", "session.html")
	written, err := Export(st, Options{}, path)
	if err != nil {
		t.Fatal(err)
	}
	if written != path {
		t.Fatalf("written = %q, want %q", written, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "hello") || !strings.Contains(string(data), "<!doctype html>") {
		t.Fatalf("file content is not the export:\n%.200s", data)
	}

	def, err := Export(st, Options{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(config.DataDir(), "exports", st.ID()[:8]+".html"); def != want {
		t.Fatalf("default export path = %q, want %q", def, want)
	}
	if _, err := os.Stat(def); err != nil {
		t.Fatalf("default export not written: %v", err)
	}
}

// TestSealOpenRoundTrip: gzip + AES-256-GCM round-trips arbitrary bytes.
func TestSealOpenRoundTrip(t *testing.T) {
	plain := []byte(strings.Repeat("xdev session transcript <script> ", 500))
	blob, key, err := Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != KeySize {
		t.Fatalf("key size = %d", len(key))
	}
	if len(blob) >= len(plain) {
		t.Fatalf("gzip did not shrink the payload: %d >= %d", len(blob), len(plain))
	}
	got, err := Open(blob, key)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(plain) {
		t.Fatal("round-trip changed the payload")
	}
	// The link's fragment form round-trips the key.
	decoded, err := DecodeKey(EncodeKey(key))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(blob, decoded); err != nil {
		t.Fatalf("key from the link fragment does not open the blob: %v", err)
	}
	// Two seals of the same plaintext differ (fresh key + IV).
	blob2, _, err := Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if string(blob2) == string(blob) {
		t.Fatal("two seals produced identical ciphertext")
	}
}

// TestOpenRejectsTampering: wrong key, flipped byte, truncation and a short
// blob all fail loudly rather than yielding partial plaintext.
func TestOpenRejectsTampering(t *testing.T) {
	plain := []byte("secret transcript contents")
	blob, key, err := Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	flipped := append([]byte(nil), blob...)
	flipped[len(flipped)-1] ^= 0x01

	otherKey, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		blob []byte
		key  []byte
	}{
		"flipped byte":  {flipped, key},
		"truncated":     {blob[:len(blob)-1], key},
		"wrong key":     {blob, otherKey},
		"short blob":    {[]byte{1, 2, 3}, key},
		"raw plaintext": {plain, key},
	}
	for name, c := range cases {
		if _, err := Open(c.blob, c.key); !errors.Is(err, ErrTampered) {
			t.Errorf("%s: err = %v, want ErrTampered", name, err)
		}
	}
	if _, err := DecodeKey("not-a-key"); err == nil {
		t.Error("DecodeKey accepted garbage")
	}
	if _, err := DecodeKey(EncodeKey([]byte("short"))); err == nil {
		t.Error("DecodeKey accepted a short key")
	}
}

// TestServerServesSealedSnapshot walks the share link end to end over HTTP:
// the viewer page is served without the key, the blob is served as
// ciphertext, other paths 404, and the key from the link fragment is what
// opens the blob.
func TestServerServesSealedSnapshot(t *testing.T) {
	html, err := Render(FromStore(boomStore(t), Options{}))
	if err != nil {
		t.Fatal(err)
	}
	blob, key, err := Seal([]byte(html))
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer("abc12345", blob, key)
	if err := srv.Start(0); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	link := srv.Link()
	base, fragment, ok := strings.Cut(link, "#")
	if !ok {
		t.Fatalf("link has no key fragment: %q", link)
	}
	if !strings.HasPrefix(base, "http://127.0.0.1:") {
		t.Fatalf("link is not loopback: %q", link)
	}

	get := func(url string) (int, []byte) {
		res, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, body
	}

	code, viewer := get(base)
	if code != http.StatusOK {
		t.Fatalf("viewer status = %d", code)
	}
	if !strings.Contains(string(viewer), "AES-GCM") || !strings.Contains(string(viewer), "DecompressionStream") {
		t.Fatalf("viewer is not the decrypting page:\n%.200s", viewer)
	}
	if strings.Contains(string(viewer), fragment) {
		t.Fatal("viewer response leaked the key")
	}

	code, got := get(base + "/blob")
	if code != http.StatusOK {
		t.Fatalf("blob status = %d", code)
	}
	if string(got) != string(blob) {
		t.Fatal("served blob is not the sealed snapshot")
	}
	if strings.Contains(string(got), "hello") {
		t.Fatal("blob is not encrypted")
	}

	// The key the link carries decrypts the blob the server hands out.
	linkKey, err := DecodeKey(fragment)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := Open(got, linkKey)
	if err != nil {
		t.Fatalf("link key does not open the served blob: %v", err)
	}
	if string(plain) != html {
		t.Fatal("decrypted snapshot differs from the export")
	}

	if code, _ := get(strings.Replace(base, "abc12345", "other-id", 1)); code != http.StatusNotFound {
		t.Fatalf("unknown snapshot id status = %d, want 404", code)
	}
	if code, _ := get(base + "/blob/extra"); code != http.StatusNotFound {
		t.Fatalf("unknown subpath status = %d, want 404", code)
	}
}

// TestPublishSealsLiveSession: Publish is the one call the TUI/CLI share
// paths make; it must yield a working link for a real store.
func TestPublishSealsLiveSession(t *testing.T) {
	srv, err := Publish(boomStore(t), Options{SystemPrompt: "sys"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if !strings.HasPrefix(srv.Link(), "http://127.0.0.1:") || !strings.Contains(srv.Link(), "#") {
		t.Fatalf("link = %q", srv.Link())
	}
	if !strings.HasPrefix(srv.Addr(), "127.0.0.1:") {
		t.Fatalf("addr = %q", srv.Addr())
	}
	if _, err := OpenRef(strings.Repeat("z", 40)); err == nil {
		t.Fatal("OpenRef accepted a bogus reference")
	}
}
