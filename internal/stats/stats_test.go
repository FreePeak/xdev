package stats

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// --- fixtures (built with the real session writer, so the on-disk shape is
// the production one, not a hand-copied guess) ---

func userEntry(id string, ts time.Time, text string) session.Entry {
	return &session.MessageEntry{
		Env: session.Envelope{ID: id, Timestamp: ts},
		Message: ai.Message{
			Role:    ai.RoleUser,
			Content: []ai.Block{ai.TextBlock{Text: text}},
		},
	}
}

func assistantEntry(id, model string, ts time.Time, tools ...string) session.Entry {
	blocks := []ai.Block{ai.TextBlock{Text: "ok"}}
	for i, name := range tools {
		blocks = append(blocks, ai.ToolCallBlock{ID: id + "-t" + string(rune('0'+i)), Name: name})
	}
	return &session.MessageEntry{
		Env: session.Envelope{ID: id, Timestamp: ts},
		Message: ai.Message{
			Role:       ai.RoleAssistant,
			Model:      model,
			StopReason: ai.StopReasonStop,
			Content:    blocks,
			Usage: &ai.Usage{
				Input: 100, Output: 10, CacheRead: 5, TotalTokens: 115,
				Cost: &ai.UsageCost{Input: 0.001, Output: 0.001, Total: 0.001},
			},
		},
	}
}

func toolResultEntry(id, name string, ts time.Time, isErr bool) session.Entry {
	return &session.MessageEntry{
		Env: session.Envelope{ID: id, Timestamp: ts},
		Message: ai.Message{
			Role:       ai.RoleToolResult,
			ToolCallID: id + "-t0",
			ToolName:   name,
			IsError:    isErr,
			Content:    []ai.Block{ai.TextBlock{Text: "result"}},
		},
	}
}

// writeSessionFile writes a session at dataDir/sessions/<bucket>/<stamp>_<id>.jsonl
// and returns its path.
func writeSessionFile(t *testing.T, dataDir, bucket, id, cwd, titleSource string, ts time.Time, entries []session.Entry) string {
	t.Helper()
	dir := filepath.Join(dataDir, "sessions", bucket)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var b strings.Builder
	// Production files always carry the fixed-width title slot on line 1
	// (the session-picker source of truth for subagent children).
	b.Write(session.MarshalTitleSlot("fixture", titleSource, ts))
	b.Write(session.MarshalHeader(session.SessionHeader{
		ID: id, Timestamp: ts, CWD: cwd, Title: "fixture", TitleSource: titleSource,
	}))
	for _, e := range entries {
		line, err := session.MarshalEntry(e)
		if err != nil {
			t.Fatalf("marshal entry: %v", err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	path := filepath.Join(dir, ts.UTC().Format("2006-01-02T15-04-05.000Z")+"_"+id+".jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}
	return path
}

// fiveSessions builds 5 sessions with 1..5 turns each, alternating models,
// one tool call per turn plus one error result.
func fiveSessions(t *testing.T) string {
	t.Helper()
	dataDir := t.TempDir()
	day := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	for i := 1; i <= 5; i++ {
		ts := day.Add(time.Duration(i) * time.Minute)
		model := "m1"
		src := "auto"
		if i%2 == 0 {
			model = "m2"
		}
		if i == 5 {
			src = "subagent"
		}
		var entries []session.Entry
		entries = append(entries, userEntry("u"+string(rune('0'+i)), ts, "do it"))
		for turn := 1; turn <= i; turn++ {
			id := string(rune('a'+i)) + string(rune('0'+turn))
			entries = append(entries, assistantEntry(id, model, ts.Add(time.Duration(turn)*time.Second), "bash"))
		}
		if i == 1 {
			entries = append(entries, toolResultEntry("e1", "bash", ts.Add(time.Minute), true))
		}
		writeSessionFile(t, dataDir, "-tmp-fixture", "sess-"+string(rune('0'+i)), "/tmp/fixture", src, ts, entries)
	}
	return dataDir
}

func TestScanAggregatesSessionsTurnsTokensModelsTools(t *testing.T) {
	dataDir := fiveSessions(t)
	rep, err := Scan(Options{DataDir: dataDir})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if rep.FilesScanned != 5 || rep.Truncated {
		t.Fatalf("files = %d truncated=%v, want 5/false", rep.FilesScanned, rep.Truncated)
	}
	tt := rep.Totals
	if tt.Sessions != 5 || tt.Subagents != 1 {
		t.Errorf("sessions = %d (sub %d), want 5/1", tt.Sessions, tt.Subagents)
	}
	if tt.UserMessages != 5 || tt.Turns != 15 {
		t.Errorf("user=%d turns=%d, want 5/15", tt.UserMessages, tt.Turns)
	}
	if tt.ToolCalls != 15 || tt.ToolErrors != 1 {
		t.Errorf("tools = %d (errors %d), want 15/1", tt.ToolCalls, tt.ToolErrors)
	}
	if tt.Input != 1500 || tt.Output != 150 || tt.CacheRead != 75 || tt.TotalTokens != 1725 {
		t.Errorf("tokens = in %d out %d read %d total %d, want 1500/150/75/1725",
			tt.Input, tt.Output, tt.CacheRead, tt.TotalTokens)
	}
	if tt.PricedTurns != 15 || math.Abs(tt.CostUSD-0.015) > 1e-9 {
		t.Errorf("cost = %v over %d priced turns, want 0.015/15", tt.CostUSD, tt.PricedTurns)
	}
	if tt.FirstSession.IsZero() || tt.LastSession.IsZero() || !tt.LastSession.After(tt.FirstSession) {
		t.Errorf("window = %v → %v", tt.FirstSession, tt.LastSession)
	}

	// Distribution: 1..5 turns per session → nearest-rank p50=3, p90=5.
	d := rep.Distribution
	if d.SessionsIn != 5 || d.TurnsP50 != 3 || d.TurnsP90 != 5 || d.TurnsMax != 5 {
		t.Errorf("turns dist = %+v, want p50 3 p90 5 max 5", d)
	}
	if d.TokensP50 != 345 || d.TokensP90 != 575 || d.TokensMax != 575 {
		t.Errorf("token dist = %+v, want p50 345 p90 575 max 575", d)
	}

	if len(rep.Models) != 2 {
		t.Fatalf("models = %+v, want 2", rep.Models)
	}
	// m1 has more turns (9) than m2 (6), so it sorts first.
	if rep.Models[0].Model != "m1" || rep.Models[0].Turns != 9 || rep.Models[0].Sessions != 3 {
		t.Errorf("model[0] = %+v, want m1 9 turns / 3 sessions", rep.Models[0])
	}
	if rep.Models[1].Model != "m2" || rep.Models[1].Turns != 6 || rep.Models[1].Sessions != 2 {
		t.Errorf("model[1] = %+v, want m2 6 turns / 2 sessions", rep.Models[1])
	}

	if len(rep.Tools) != 1 || rep.Tools[0].Name != "bash" || rep.Tools[0].Calls != 15 || rep.Tools[0].Errors != 1 {
		t.Errorf("tools = %+v, want bash 15 calls / 1 error", rep.Tools)
	}

	if len(rep.Days) != 1 || rep.Days[0].Day != "2026-09-12" || rep.Days[0].Turns != 15 || rep.Days[0].Sessions != 5 {
		t.Errorf("days = %+v, want one 2026-09-12 row", rep.Days)
	}
	if rep.Days[0].Tokens != 1725 {
		t.Errorf("day tokens = %d, want 1725", rep.Days[0].Tokens)
	}
}

func TestScanSinceAndCapBoundTheScan(t *testing.T) {
	dataDir := t.TempDir()
	base := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	paths := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		ts := base.Add(time.Duration(i) * time.Hour)
		p := writeSessionFile(t, dataDir, "-tmp-cap", "cap-"+string(rune('a'+i)), "/tmp/cap", "auto", ts,
			[]session.Entry{userEntry("u", ts, "x"), assistantEntry("a", "m", ts.Add(time.Second), "bash")})
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
		paths = append(paths, p)
	}

	// --since drops everything written before the middle session.
	rep, err := Scan(Options{DataDir: dataDir, Since: base.Add(30 * time.Minute), NoRollup: true})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if rep.FilesScanned != 2 || rep.FilesFiltered != 1 {
		t.Errorf("since scan = %d scanned / %d filtered, want 2/1", rep.FilesScanned, rep.FilesFiltered)
	}
	if rep.Totals.Sessions != 2 || rep.Totals.Turns != 2 {
		t.Errorf("since totals = %+v", rep.Totals)
	}

	// A cap of 1 file keeps the newest and reports the truncation.
	rep, err = Scan(Options{DataDir: dataDir, MaxFiles: 1, NoRollup: true})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !rep.Truncated || rep.FilesScanned != 1 || rep.FilesOmitted != 2 {
		t.Errorf("cap scan = scanned %d omitted %d truncated %v, want 1/2/true",
			rep.FilesScanned, rep.FilesOmitted, rep.Truncated)
	}
	if rep.Totals.Sessions != 1 {
		t.Errorf("cap sessions = %d, want 1 (newest only)", rep.Totals.Sessions)
	}
}

func TestScanRollupCacheAvoidsRereadingUnchangedFiles(t *testing.T) {
	dataDir := fiveSessions(t)
	first, err := Scan(Options{DataDir: dataDir})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if first.CacheHits != 0 {
		t.Fatalf("first scan cache hits = %d, want 0", first.CacheHits)
	}
	if _, err := os.Stat(rollupPath(dataDir)); err != nil {
		t.Fatalf("rollup not written: %v", err)
	}

	second, err := Scan(Options{DataDir: dataDir})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if second.CacheHits != 5 {
		t.Errorf("second scan cache hits = %d, want 5", second.CacheHits)
	}
	if second.Totals.Turns != first.Totals.Turns || second.Totals.TotalTokens != first.Totals.TotalTokens {
		t.Errorf("cached totals %+v != fresh %+v", second.Totals, first.Totals)
	}

	// Touching one file invalidates only that entry.
	p := filepath.Join(dataDir, "sessions", "-tmp-fixture", "2026-09-12T10-01-00.000Z_sess-1.jsonl")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	extra, _ := session.MarshalEntry(assistantEntry("zz", "m1", time.Date(2026, 9, 12, 11, 0, 0, 0, time.UTC), "grep"))
	f.Write(append(extra, '\n'))
	f.Close()

	third, err := Scan(Options{DataDir: dataDir})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if third.CacheHits != 4 {
		t.Errorf("third scan cache hits = %d, want 4 (one file changed)", third.CacheHits)
	}
	if third.Totals.Turns != first.Totals.Turns+1 {
		t.Errorf("turns = %d, want %d after the append", third.Totals.Turns, first.Totals.Turns+1)
	}
}

func TestScanToleratesCorruptLineAndMissingStore(t *testing.T) {
	dataDir := t.TempDir()
	ts := time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)
	p := writeSessionFile(t, dataDir, "-tmp-corrupt", "corrupt-1", "/tmp/c", "auto", ts,
		[]session.Entry{userEntry("u", ts, "x"), assistantEntry("a", "m", ts.Add(time.Second), "bash")})
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	f.WriteString(`{"type":"message","id":"zz"`) // crash-truncated tail
	f.Close()

	rep, err := Scan(Options{DataDir: dataDir})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if rep.Totals.Turns != 1 || rep.FilesUnreadable != 0 {
		t.Errorf("turns = %d unreadable = %d, want 1/0 (bad tail is skipped)", rep.Totals.Turns, rep.FilesUnreadable)
	}

	// No sessions directory at all is an empty report, not an error.
	empty, err := Scan(Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Scan(empty): %v", err)
	}
	if empty.Totals.Sessions != 0 || empty.Models == nil || empty.Tools == nil || empty.Days == nil {
		t.Errorf("empty report = %+v (slices must marshal as [] not null)", empty)
	}
}

func TestScanJSONShape(t *testing.T) {
	dataDir := fiveSessions(t)
	rep, err := Scan(Options{DataDir: dataDir, NoRollup: true})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		GeneratedAt string `json:"generatedAt"`
		DataDir     string `json:"dataDir"`
		Totals      struct {
			Sessions    int     `json:"sessions"`
			Turns       int     `json:"turns"`
			TotalTokens int64   `json:"totalTokens"`
			CostUSD     float64 `json:"costUsd"`
		} `json:"totals"`
		Models []struct {
			Model  string `json:"model"`
			Turns  int    `json:"turns"`
			Tokens int64  `json:"totalTokens"`
		} `json:"models"`
		Tools []struct {
			Name  string `json:"name"`
			Calls int    `json:"calls"`
		} `json:"tools"`
		Days         []map[string]any `json:"days"`
		Distribution struct {
			TurnsP50 int `json:"turnsP50"`
		} `json:"distribution"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.DataDir != dataDir || got.GeneratedAt == "" {
		t.Errorf("envelope = %q / %q", got.DataDir, got.GeneratedAt)
	}
	if got.Totals.Sessions != 5 || got.Totals.Turns != 15 || got.Totals.TotalTokens != 1725 {
		t.Errorf("totals = %+v", got.Totals)
	}
	if len(got.Models) != 2 || got.Models[0].Model != "m1" {
		t.Errorf("models = %+v", got.Models)
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "bash" || got.Tools[0].Calls != 15 {
		t.Errorf("tools = %+v", got.Tools)
	}
	if len(got.Days) != 1 || got.Distribution.TurnsP50 != 3 {
		t.Errorf("days = %+v dist = %+v", got.Days, got.Distribution)
	}
}

func TestHumanTokens(t *testing.T) {
	cases := map[int64]string{
		0: "0", 999: "999", 1234: "1.2k", 15000: "15k",
		1_500_000: "1.5M", 2_500_000_000: "2.5B",
	}
	for in, want := range cases {
		if got := HumanTokens(in); got != want {
			t.Errorf("HumanTokens(%d) = %q, want %q", in, got, want)
		}
	}
}
