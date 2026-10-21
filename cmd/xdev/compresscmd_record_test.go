package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// compressRecordSession writes a session file with 30 messages, big enough
// that a compaction cut exists at the default kept tail.
func compressRecordSession(t *testing.T, dataDir, cwd string) string {
	t.Helper()
	store := session.OpenMem(cwd, "compress record test")
	path := session.SessionFilePath(dataDir, cwd, time.Now(), store.ID())
	if _, err := store.EnsureOnDisk(path, session.Options{}); err != nil {
		t.Fatalf("materialize the session: %v", err)
	}
	now := time.Now().UnixMilli()
	for i := range 30 {
		// Multi-line bodies: the deterministic members keep the first line
		// of each message, so a realistic transcript is what leaves a
		// droppable prefix at the default kept tail.
		text := "turn " + strconv.Itoa(i) + "\n" + strings.Repeat("alpha alpha alpha\n", 400)
		msg := ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: text}}, UserTS: now}
		if err := store.Append(&session.MessageEntry{Message: msg}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return path
}

// TestCompressCmdFillsTheCompactionRecord pins the CLI path: the line the
// CLI appends carries the size of the context that SURVIVED and the
// one-line summary, and the surviving size is the number the report
// printed ("context 60060 -> 35047 tokens").
func TestCompressCmdFillsTheCompactionRecord(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dataDir)
	cwd := t.TempDir()
	path := compressRecordSession(t, dataDir, cwd)

	var out, errOut bytes.Buffer
	code := compressCmd([]string{"--session", path, "--method", "soft", "--force"}, cwd, nil, &out, &errOut)
	if code != 0 {
		t.Fatalf("compressCmd = %d (stderr: %s)", code, errOut.String())
	}
	report := out.String()
	if !strings.Contains(report, "compacted") {
		t.Fatalf("the report does not say it compacted:\n%s", report)
	}

	// The number the report printed for the surviving context.
	after := reportTokensAfter(t, report)

	// The line the CLI appended to the session file.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	last := lines[len(lines)-1]
	var wire map[string]any
	if err := json.Unmarshal([]byte(last), &wire); err != nil {
		t.Fatalf("the appended line is not JSON: %v (%s)", err, last)
	}
	if wire["type"] != "compaction" {
		t.Fatalf("the appended line is a %v, want a compaction", wire["type"])
	}
	tokensAfter, ok := wire["tokensAfter"].(float64)
	if !ok {
		t.Fatalf("the appended line carries no tokensAfter: %s", last)
	}
	if int64(tokensAfter) != after {
		t.Errorf("tokensAfter = %d, want %d (the number the report printed)", int64(tokensAfter), after)
	}
	short, ok := wire["shortSummary"].(string)
	if !ok || short == "" {
		t.Errorf("the appended line carries no shortSummary: %s", last)
	}
}

// reportTokensAfter reads the surviving-context size out of the report's
// "context 60060 -> 35047 tokens" line.
func reportTokensAfter(t *testing.T, report string) int64 {
	t.Helper()
	for _, line := range strings.Split(report, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "context ") {
			continue
		}
		rest := strings.TrimPrefix(line, "context ")
		arrow := strings.Index(rest, "->")
		if arrow < 0 {
			continue
		}
		tokens := strings.TrimSpace(rest[arrow+2:])
		tokens = strings.TrimSuffix(tokens, " tokens")
		n, err := strconv.ParseInt(strings.TrimSpace(tokens), 10, 64)
		if err != nil {
			t.Fatalf("the report's context line is unreadable: %q", line)
		}
		return n
	}
	t.Fatalf("the report has no context line:\n%s", report)
	return 0
}
