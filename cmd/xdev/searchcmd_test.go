package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// searchTestSession writes one real session file with the given turns.
func searchTestSession(t *testing.T, dataDir, cwd, title string, turns [][2]string) session.SessionMeta {
	t.Helper()
	store := session.OpenMem(cwd, title)
	path := session.SessionFilePath(dataDir, cwd, time.Now(), store.ID())
	store.EnableAutoPersist(path, session.Options{})
	ts := time.Now().UnixMilli()
	for _, turn := range turns {
		role := ai.RoleUser
		if turn[0] == "assistant" {
			role = ai.RoleAssistant
		}
		if err := store.Append(&session.MessageEntry{Message: ai.Message{
			Role:    role,
			Content: []ai.Block{ai.TextBlock{Text: turn[1]}},
			UserTS:  ts,
		}}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	metas, err := session.List(dataDir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, m := range metas {
		if m.Path == path {
			return m
		}
	}
	t.Fatalf("session %s not listed", path)
	return session.SessionMeta{}
}

func searchTestStore(t *testing.T) (string, string, session.SessionMeta, session.SessionMeta) {
	t.Helper()
	dataDir := t.TempDir()
	cwd := t.TempDir()
	other := t.TempDir()
	hit := searchTestSession(t, dataDir, cwd, "the deploy session", [][2]string{
		{"user", "please deploy the widget service"},
		{"assistant", "deploying now; the needle-xyz marker is in this reply"},
	})
	miss := searchTestSession(t, dataDir, other, "unrelated session", [][2]string{
		{"user", "just chatting about haystacks"},
		{"assistant", "understood"},
	})
	_ = miss
	return dataDir, cwd, hit, miss
}

// TestSearchStoreFindsSessionsByMessageText is the core contract: the query
// matches reconstructed conversation text, and only the session that has it.
func TestSearchStoreFindsSessionsByMessageText(t *testing.T) {
	dataDir, cwd, hit, miss := searchTestStore(t)

	results, scanned, total, err := searchStore(dataDir, searchQuery{Text: "needle-xyz", Limit: 10, MaxHits: 3, cwd: cwd, Scanned: 50})
	if err != nil {
		t.Fatalf("searchStore: %v", err)
	}
	if total != 2 || scanned != 2 {
		t.Fatalf("scanned %d of %d sessions, want 2 of 2", scanned, total)
	}
	if len(results) != 1 {
		t.Fatalf("results = %+v, want exactly the matching session", results)
	}
	if results[0].SessionID != hit.ID {
		t.Fatalf("hit session = %s, want %s", results[0].SessionID, hit.ID)
	}
	if len(results[0].Matches) != 1 || results[0].Matches[0].Role != "assistant" {
		t.Fatalf("matches = %+v", results[0].Matches)
	}
	if !strings.Contains(results[0].Matches[0].Snippet, "needle-xyz") {
		t.Fatalf("snippet lost the match: %q", results[0].Matches[0].Snippet)
	}

	// Case-insensitive substring form.
	insensitive, _, _, err := searchStore(dataDir, searchQuery{Text: "NEEDLE-XYZ", Limit: 10, MaxHits: 3, Scanned: 50})
	if err != nil || len(insensitive) != 1 {
		t.Fatalf("case-insensitive search: %v results=%d", err, len(insensitive))
	}

	// The miss is addressable only through its own text.
	wrong, _, _, err := searchStore(dataDir, searchQuery{Text: "haystacks", Limit: 10, MaxHits: 3, Scanned: 50})
	if err != nil || len(wrong) != 1 || wrong[0].SessionID != miss.ID {
		t.Fatalf("haystacks search: %v results=%+v", err, wrong)
	}

	// --here restricts to the current directory.
	here, _, _, err := searchStore(dataDir, searchQuery{Text: "haystacks", Here: true, cwd: cwd, Limit: 10, MaxHits: 3, Scanned: 50})
	if err != nil || len(here) != 0 {
		t.Fatalf("--here search leaked another directory: %v results=%+v", err, here)
	}
}

// TestSearchCmdSurfaces pins the CLI: JSON output, the empty-result line, and
// the usage errors.
func TestSearchCmdSurfaces(t *testing.T) {
	dataDir, cwd, _, _ := searchTestStore(t)

	var out, errOut bytes.Buffer
	if code := searchCmd([]string{"--json", "needle-xyz"}, dataDir, cwd, &out, &errOut); code != 0 {
		t.Fatalf("searchCmd --json = %d (%s)", code, errOut.String())
	}
	var results []searchResult
	if err := json.Unmarshal(out.Bytes(), &results); err != nil {
		t.Fatalf("json: %v (%s)", err, out.String())
	}
	if len(results) != 1 || !strings.Contains(results[0].Matches[0].Snippet, "needle-xyz") {
		t.Fatalf("json results = %+v", results)
	}

	out.Reset()
	if code := searchCmd([]string{"--regex", `needle-\w+`}, dataDir, cwd, &out, &errOut); code != 0 {
		t.Fatalf("searchCmd --regex = %d (%s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "1 sessions, 1 matches") {
		t.Fatalf("regex summary:\n%s", out.String())
	}

	out.Reset()
	if code := searchCmd([]string{"nothing-matches-this"}, dataDir, cwd, &out, &errOut); code != 0 {
		t.Fatalf("searchCmd(no match) = %d", code)
	}
	if !strings.Contains(out.String(), "no session matched") {
		t.Fatalf("empty result output:\n%s", out.String())
	}

	for _, args := range [][]string{{}, {"a", "b"}, {"--regex", "[unclosed"}} {
		if code := searchCmd(args, dataDir, cwd, &out, &errOut); code != 2 {
			t.Fatalf("searchCmd(%v) = %d, want 2", args, code)
		}
	}
}

// TestSearchSnippetWindowsAroundTheMatch keeps the reported context anchored on
// the hit instead of dumping the whole message.
func TestSearchSnippetWindowsAroundTheMatch(t *testing.T) {
	long := strings.Repeat("filler ", 200) + "needle-xyz" + strings.Repeat(" tail", 200)
	got := searchSnippet(searchQuery{Text: "needle-xyz"}, long)
	if !strings.Contains(got, "needle-xyz") {
		t.Fatalf("snippet lost the match: %q", got)
	}
	if len(got) > 140 {
		t.Fatalf("snippet is not windowed: %d bytes", len(got))
	}
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") {
		t.Fatalf("snippet is not marked as a window: %q", got)
	}
}
