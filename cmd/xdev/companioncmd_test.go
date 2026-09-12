package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/stats"
)

// --- memory CLI (M15 #72) ---

func runMemory(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := memoryCmd(args, strings.NewReader(stdin), &out, &errOut, &config.Settings{Memory: "local"})
	return code, out.String(), errOut.String()
}

func TestMemoryCmdAddEditExportImport(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	dir := filepath.Join(config.DataDir(), "memory")

	code, out, errOut := runMemory(t, "", "add", "stage explicit paths", "--context", "xdev repo")
	if code != 0 {
		t.Fatalf("add = %d (%s)", code, errOut)
	}
	if !strings.Contains(out, "added lesson 1 of 1") {
		t.Errorf("add output = %q", out)
	}

	code, out, _ = runMemory(t, "", "lessons")
	if code != 0 || !strings.Contains(out, "stage explicit paths") || !strings.Contains(out, "(context: xdev repo)") {
		t.Errorf("lessons = %d %q", code, out)
	}

	code, out, _ = runMemory(t, "# Memory\n\n## Decisions\n- stdlib first\n", "edit", "-")
	if code != 0 || !strings.Contains(out, "MEMORY.md updated") {
		t.Fatalf("edit = %d %q", code, out)
	}
	code, out, _ = runMemory(t, "", "show")
	if code != 0 || !strings.Contains(out, "## Decisions") {
		t.Errorf("show = %d %q", code, out)
	}
	code, out, _ = runMemory(t, "", "show", "--injected")
	if code != 0 || !strings.Contains(out, "stdlib first") || !strings.Contains(out, "stage explicit paths") {
		t.Errorf("show --injected = %d %q", code, out)
	}

	if code, _, _ = runMemory(t, "", "scratchpad", "half-thought"); code != 0 {
		t.Fatalf("scratchpad set = %d", code)
	}
	if code, out, _ = runMemory(t, "", "scratchpad"); code != 0 || !strings.Contains(out, "half-thought") {
		t.Errorf("scratchpad = %d %q", code, out)
	}
	bundle := filepath.Join(t.TempDir(), "bundle.json")
	if code, out, errOut = runMemory(t, "", "export", "-o", bundle); code != 0 {
		t.Fatalf("export = %d (%s)", code, errOut)
	}
	if !strings.Contains(out, "exported 1 lesson(s)") {
		t.Errorf("export output = %q", out)
	}

	// stats --json reports the on-disk state.
	code, out, _ = runMemory(t, "", "stats", "--json")
	if code != 0 {
		t.Fatalf("stats = %d", code)
	}
	var statsPayload struct {
		Dir  string     `json:"dir"`
		Info memoryInfo `json:"info"`
		Pipe bool       `json:"pipelineOn"`
	}
	if err := json.Unmarshal([]byte(out), &statsPayload); err != nil {
		t.Fatalf("stats json: %v (%q)", err, out)
	}
	if statsPayload.Dir != dir || statsPayload.Info.LessonCount != 1 || statsPayload.Info.Summary.Bytes == 0 {
		t.Errorf("stats payload = %+v", statsPayload)
	}

	// clear --yes wipes it, import brings it back.
	if code, out, _ = runMemory(t, "", "clear", "--yes"); code != 0 || !strings.Contains(out, "cleared") {
		t.Fatalf("clear = %d %q", code, out)
	}
	if code, out, _ = runMemory(t, "", "lessons"); code != 0 || !strings.Contains(out, "no lessons") {
		t.Errorf("lessons after clear = %d %q", code, out)
	}
	if code, out, errOut = runMemory(t, "", "import", bundle); code != 0 {
		t.Fatalf("import = %d (%s)", code, errOut)
	}
	if !strings.Contains(out, "replaced: summary") {
		t.Errorf("import output = %q", out)
	}
	if code, out, _ = runMemory(t, "", "lessons"); code != 0 || !strings.Contains(out, "stage explicit paths") {
		t.Errorf("lessons after import = %d %q", code, out)
	}
	if code, out, _ = runMemory(t, "", "show"); code != 0 || !strings.Contains(out, "## Decisions") {
		t.Errorf("show after import = %d %q", code, out)
	}
	if code, out, _ = runMemory(t, "", "scratchpad"); code != 0 || !strings.Contains(out, "half-thought") {
		t.Errorf("scratchpad after import = %d %q", code, out)
	}

	// Merging the same bundle twice must not duplicate lessons.
	if code, _, errOut = runMemory(t, "", "import", "--merge", bundle); code != 0 {
		t.Fatalf("merge import = %d (%s)", code, errOut)
	}
	code, out, _ = runMemory(t, "", "lessons", "--json")
	if code != 0 {
		t.Fatalf("lessons --json = %d", code)
	}
	var lessons []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(out), &lessons); err != nil {
		t.Fatalf("lessons json: %v (%q)", err, out)
	}
	if len(lessons) != 1 {
		t.Errorf("lessons after merge = %d, want 1", len(lessons))
	}
}

// memoryInfo mirrors the subset of memory.Info the CLI test asserts on.
type memoryInfo struct {
	Summary     struct{ Bytes int64 } `json:"summary"`
	LessonCount int                   `json:"lessonCount"`
}

func TestMemoryCmdClearRequiresYes(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	if code, _, _ := runMemory(t, "", "add", "keep me"); code != 0 {
		t.Fatal("add failed")
	}
	code, _, errOut := runMemory(t, "", "clear")
	if code != 2 || !strings.Contains(errOut, "--yes") {
		t.Fatalf("clear without --yes = %d %q", code, errOut)
	}
	if code, out, _ := runMemory(t, "", "lessons"); code != 0 || !strings.Contains(out, "keep me") {
		t.Errorf("unconfirmed clear deleted the store: %d %q", code, out)
	}
}

func TestMemoryCmdExplainsDisabledBackend(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	var out, errOut bytes.Buffer
	code := memoryCmd([]string{"show"}, strings.NewReader(""), &out, &errOut, &config.Settings{Memory: "off"})
	if code != 2 || !strings.Contains(errOut.String(), "memory is off") {
		t.Fatalf("show with memory off = %d %q", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "xdev config set memory local") {
		t.Errorf("error is not actionable: %q", errOut.String())
	}
}

func TestMemoryCmdUnknownSubcommand(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	code, _, errOut := runMemory(t, "", "frobnicate")
	if code != 2 || !strings.Contains(errOut, "unknown subcommand") {
		t.Fatalf("unknown sub = %d %q", code, errOut)
	}
}

// --- stats CLI (M15 #72) ---

// writeCmdSession writes one session file with n assistant turns.
func writeCmdSession(t *testing.T, dataDir, id string, turns int) {
	t.Helper()
	ts := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	dir := filepath.Join(dataDir, "sessions", "-tmp-cmdstats")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.Write(session.MarshalTitleSlot("cmd fixture", "auto", ts))
	b.Write(session.MarshalHeader(session.SessionHeader{ID: id, Timestamp: ts, CWD: "/tmp/cmdstats", Title: "cmd fixture", TitleSource: "auto"}))
	entries := []session.Entry{&session.MessageEntry{
		Env:     session.Envelope{ID: "u1", Timestamp: ts},
		Message: ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "go"}}},
	}}
	for i := 0; i < turns; i++ {
		entries = append(entries, &session.MessageEntry{
			Env: session.Envelope{ID: "a" + string(rune('0'+i)), Timestamp: ts.Add(time.Duration(i) * time.Second)},
			Message: ai.Message{
				Role:    ai.RoleAssistant,
				Model:   "test-model",
				Content: []ai.Block{ai.TextBlock{Text: "ok"}, ai.ToolCallBlock{Name: "read"}},
				Usage:   &ai.Usage{Input: 1000, Output: 100, TotalTokens: 1100, Cost: &ai.UsageCost{Total: 0.002}},
			},
		})
	}
	for _, e := range entries {
		line, err := session.MarshalEntry(e)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "2026-09-12T08-00-00.000Z_"+id+".jsonl"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStatsCmdSummaryAndJSON(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	writeCmdSession(t, config.DataDir(), "cmd-a", 2)
	writeCmdSession(t, config.DataDir(), "cmd-b", 1)

	var out, errOut bytes.Buffer
	if code := statsCmd(context.Background(), nil, &out, &errOut); code != 0 {
		t.Fatalf("stats summary = %d (%s)", code, errOut.String())
	}
	table := out.String()
	for _, want := range []string{"xdev stats —", "sessions", "turns", "by model", "test-model", "top tools", "read"} {
		if !strings.Contains(table, want) {
			t.Errorf("summary table missing %q:\n%s", want, table)
		}
	}

	out.Reset()
	if code := statsCmd(context.Background(), []string{"--json"}, &out, &errOut); code != 0 {
		t.Fatalf("stats --json = %d", code)
	}
	var rep stats.Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("stats json: %v", err)
	}
	if rep.Totals.Sessions != 2 || rep.Totals.Turns != 3 || rep.Totals.ToolCalls != 3 {
		t.Errorf("totals = %+v", rep.Totals)
	}
	if rep.Totals.TotalTokens != 3300 || rep.Totals.PricedTurns != 3 {
		t.Errorf("tokens = %d priced = %d, want 3300/3", rep.Totals.TotalTokens, rep.Totals.PricedTurns)
	}
	if len(rep.Models) != 1 || rep.Models[0].Model != "test-model" || rep.Models[0].Turns != 3 {
		t.Errorf("models = %+v", rep.Models)
	}
	if len(rep.Tools) != 1 || rep.Tools[0].Name != "read" || rep.Tools[0].Calls != 3 {
		t.Errorf("tools = %+v", rep.Tools)
	}
}

func TestStatsCmdFlagErrors(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	var out, errOut bytes.Buffer
	cases := [][]string{
		{"--since", "not-a-window"},
		{"--json", "--summary"},
		{"extra-arg"},
		{"--serve", "--addr", "0.0.0.0:3847"},
	}
	for _, args := range cases {
		out.Reset()
		errOut.Reset()
		if code := statsCmd(context.Background(), args, &out, &errOut); code == 0 {
			t.Errorf("statsCmd(%v) = 0, want a failure\n%s", args, out.String())
		}
	}
	errOut.Reset()
	statsCmd(context.Background(), []string{"--serve", "--addr", "0.0.0.0:3847"}, &out, &errOut)
	if !strings.Contains(errOut.String(), "loopback") {
		t.Errorf("non-loopback dashboard bind not explained: %q", errOut.String())
	}
}
