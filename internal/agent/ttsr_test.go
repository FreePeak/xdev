package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/tool"
)

// recordingInterceptor records Emit events (the hooks/extensions surface).
type recordingInterceptor struct{ events []map[string]any }

func (r *recordingInterceptor) ToolCall(_ context.Context, _ string, args json.RawMessage) (json.RawMessage, error) {
	return args, nil
}

func (r *recordingInterceptor) ToolResult(_ context.Context, _ string, _, result json.RawMessage) json.RawMessage {
	return result
}

func (r *recordingInterceptor) Emit(_ context.Context, event string, payload any) {
	if m, ok := payload.(map[string]any); ok {
		m["event"] = event
		r.events = append(r.events, m)
	}
}

func (r *recordingInterceptor) triggered() []map[string]any {
	var out []map[string]any
	for _, e := range r.events {
		if e["event"] == "ttsr_triggered" {
			out = append(out, e)
		}
	}
	return out
}

func ttsrEngine(t *testing.T, rules ...config.TTSRRule) *TTSR {
	t.Helper()
	if tsr := NewTTSR(&config.TTSRSettings{Rules: rules}); tsr != nil {
		return tsr
	}
	t.Fatal("engine is nil for a usable rule set")
	return nil
}

// --- engine-level: regex matching, mode gating, repeat gap, AST ---

func TestTTSRObserveMatchesAcrossDeltas(t *testing.T) {
	tsr := ttsrEngine(t, config.TTSRRule{Name: "no-secret", Condition: "SECRET"})
	m := tsr.observe(context.Background(), ttsrProse, "here is the SEC", 0)
	if m != nil {
		t.Fatalf("fired on a partial window: %+v", m)
	}
	m = tsr.observe(context.Background(), ttsrProse, "RET value", 0)
	if m == nil || m.Rule.Name != "no-secret" || !m.Interrupt {
		t.Fatalf("match = %+v, want interrupt on the accumulated window", m)
	}
	if again := tsr.observe(context.Background(), ttsrProse, "SECRET", 0); again != nil {
		t.Fatalf("rule fired twice in one turn: %+v", again)
	}
}

func TestTTSRInterruptModeGating(t *testing.T) {
	tests := []struct {
		mode      string
		kind      ttsrKind
		interrupt bool
		reminder  bool
	}{
		{"always", ttsrProse, true, false},
		{"always", ttsrTool, true, false},
		{"prose-only", ttsrProse, true, false},
		{"prose-only", ttsrTool, false, true},
		{"tool-only", ttsrProse, false, false}, // no fire at all on prose
		{"tool-only", ttsrTool, true, false},
		{"never", ttsrProse, false, false},
		{"never", ttsrTool, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.mode+"/"+tc.kind.String(), func(t *testing.T) {
			tsr := ttsrEngine(t, config.TTSRRule{Name: "r", Condition: "hit", InterruptMode: tc.mode})
			m := tsr.observe(context.Background(), tc.kind, "hit", 3)
			if m == nil {
				if tc.interrupt || tc.reminder {
					t.Fatal("rule did not fire")
				}
				return
			}
			if m.Interrupt != tc.interrupt {
				t.Fatalf("interrupt = %v, want %v", m.Interrupt, tc.interrupt)
			}
			if got := len(tsr.pending[3]) > 0; got != tc.reminder {
				t.Fatalf("reminder queued = %v, want %v", got, tc.reminder)
			}
		})
	}
}

func TestTTSRRepeatGapSuppression(t *testing.T) {
	tsr := ttsrEngine(t, config.TTSRRule{Name: "r", Condition: "hit", RepeatGap: 3})
	tsr.beginTurn() // turn 1
	if m := tsr.observe(context.Background(), ttsrProse, "hit", 0); m == nil {
		t.Fatal("first fire suppressed")
	}
	tsr.beginTurn() // turn 2 — inside the gap
	if m := tsr.observe(context.Background(), ttsrProse, "hit", 0); m != nil {
		t.Fatalf("fired inside the repeat gap: %+v", m)
	}
	tsr.beginTurn() // turn 3 — still inside
	if m := tsr.observe(context.Background(), ttsrProse, "hit", 0); m != nil {
		t.Fatalf("fired inside the repeat gap: %+v", m)
	}
	tsr.beginTurn() // turn 4 — gap elapsed
	if m := tsr.observe(context.Background(), ttsrProse, "hit", 0); m == nil {
		t.Fatal("rule did not re-fire after the gap")
	}
}

func TestNewTTSRInertCases(t *testing.T) {
	off := false
	on := true
	if got := NewTTSR(nil); got != nil {
		t.Fatal("nil settings produced an engine")
	}
	if got := NewTTSR(&config.TTSRSettings{Enabled: &off, Rules: []config.TTSRRule{{Name: "r", Condition: "x"}}}); got != nil {
		t.Fatal("disabled group produced an engine")
	}
	if got := NewTTSR(&config.TTSRSettings{Enabled: &on}); got != nil {
		t.Fatal("rule-less group produced an engine")
	}
	if got := NewTTSR(&config.TTSRSettings{Enabled: &on, Rules: []config.TTSRRule{{Name: "bad", Condition: "("}}}); got != nil {
		t.Fatal("only-invalid rule group produced an engine")
	}
	// A broken rule is skipped; a usable sibling still runs.
	tsr := ttsrEngine(t,
		config.TTSRRule{Name: "bad", Condition: "("},
		config.TTSRRule{Name: "good", Condition: "hit"},
	)
	if m := tsr.observe(context.Background(), ttsrProse, "hit", 0); m == nil || m.Rule.Name != "good" {
		t.Fatalf("good rule did not run beside a broken one: %+v", m)
	}
}

func TestTTSRASTCondition(t *testing.T) {
	digest := `{"filePath":"` + t.TempDir() + `/main.go","content":"func main() {}"}`
	rule := config.TTSRRule{Name: "ast", Condition: "func main", ASTCondition: "func main($$$)", InterruptMode: "never"}

	stub := func(t *testing.T, script string) {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "ast-grep"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir)
	}

	t.Run("match", func(t *testing.T) {
		stub(t, "#!/bin/sh\necho matched\n")
		tsr := ttsrEngine(t, rule)
		if m := tsr.observe(context.Background(), ttsrTool, digest, 0); m == nil || len(tsr.pending[0]) == 0 {
			t.Fatalf("ast condition match did not fire: %+v", m)
		}
	})
	t.Run("no match", func(t *testing.T) {
		stub(t, "#!/bin/sh\nexit 1\n")
		tsr := ttsrEngine(t, rule)
		if m := tsr.observe(context.Background(), ttsrTool, digest, 0); m != nil {
			t.Fatalf("rule fired without an ast match: %+v", m)
		}
	})
	t.Run("binary absent degrades to the regex", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		tsr := ttsrEngine(t, rule)
		if m := tsr.observe(context.Background(), ttsrTool, digest, 0); m == nil {
			t.Fatal("rule suppressed without ast-grep installed (must degrade)")
		}
	})
}

// --- end-to-end: abort, injection, context mode, tool reminders ---

func textDelta(s string) ai.Event { return ai.Event{Type: ai.EventTextDelta, Delta: s} }

func doneMsg(text string) ai.Event {
	return ai.Event{Type: ai.EventDone, StopReason: ai.StopReasonStop,
		Message: &ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: text}}, StopReason: ai.StopReasonStop}}
}

// runScripted runs the agent over a scripted provider with one rule.
func runScripted(t *testing.T, rule config.TTSRRule, scripts []fakeScript, interceptor Interceptor) (*fakeProvider, *[]*ai.Message, *ai.Message) {
	t.Helper()
	p := &fakeProvider{calls: scripts}
	reg := tool.NewRegistry()
	reg.Register(echoTool{})
	var results []*ai.Message
	a := &Agent{Provider: p, Tools: reg, TTSR: ttsrEngine(t, rule), Intercept: interceptor,
		Hooks: TurnHooksFunc{OnToolResultMsgF: func(m *ai.Message) { results = append(results, m) }}}
	final, err := a.Run(context.Background(), "sys", []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "hi"}}}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return p, &results, final
}

func lastReq(p *fakeProvider) ai.StreamRequest { return p.gotReqs[len(p.gotReqs)-1] }

func requestText(req ai.StreamRequest) string {
	var b strings.Builder
	for _, m := range req.Messages {
		b.WriteString(messageText(m))
		b.WriteString("\n")
	}
	return b.String()
}

func TestTTSRInterruptDiscardVsKeep(t *testing.T) {
	scripts := func() []fakeScript {
		return []fakeScript{
			{events: []ai.Event{{Type: ai.EventStart}, {Type: ai.EventTextStart},
				textDelta("writing SEC"), textDelta("RET here"), textDelta("more"),
				doneMsg("writing SECRET here more")}},
			{events: []ai.Event{{Type: ai.EventStart}, {Type: ai.EventTextStart}, textDelta("stopped"), doneMsg("stopped")}},
		}
	}

	t.Run("discard", func(t *testing.T) {
		p, _, final := runScripted(t, config.TTSRRule{Name: "no-secret", Condition: "SECRET", ContextMode: "discard"}, scripts(), nil)
		if len(p.gotReqs) != 2 {
			t.Fatalf("provider calls = %d, want abort+retry", len(p.gotReqs))
		}
		got := requestText(lastReq(p))
		if !strings.Contains(got, "<system-interrupt>") || !strings.Contains(got, "no-secret") {
			t.Fatalf("retry request missing the system-interrupt notice:\n%s", got)
		}
		if strings.Contains(got, "writing SECRET") {
			t.Fatalf("aborted content leaked into the context (discard):\n%s", got)
		}
		if final.Text() != "stopped" {
			t.Fatalf("final = %q", final.Text())
		}
	})

	t.Run("keep", func(t *testing.T) {
		p, _, _ := runScripted(t, config.TTSRRule{Name: "no-secret", Condition: "SECRET", ContextMode: "keep"}, scripts(), nil)
		got := requestText(lastReq(p))
		if !strings.Contains(got, "writing SECRET") {
			t.Fatalf("contextMode keep dropped the aborted content:\n%s", got)
		}
	})
}

func TestTTSRToolReminderFoldedIntoResult(t *testing.T) {
	args := json.RawMessage(`{"text":"rm -rf /"}`)
	callDone := ai.Event{Type: ai.EventDone, StopReason: ai.StopReasonStop, Message: &ai.Message{
		Role: ai.RoleAssistant,
		Content: []ai.Block{
			ai.TextBlock{Text: "running echo"},
			ai.ToolCallBlock{ID: "c1", Name: "echo", Arguments: args, StreamIndex: 0},
		},
		StopReason: ai.StopReasonStop,
	}}
	rec := &recordingInterceptor{}
	p, results, final := runScripted(t, config.TTSRRule{Name: "no-rm", Condition: "rm -rf", InterruptMode: "never"}, []fakeScript{
		{events: []ai.Event{{Type: ai.EventStart},
			{Type: ai.EventToolcallStart, ToolCallID: "c1", ToolName: "echo", StreamIndex: 0},
			{Type: ai.EventToolcallDelta, StreamIndex: 0, PartialJSON: `{"text":"rm -rf /"}`},
			{Type: ai.EventToolcallEnd, StreamIndex: 0, PartialJSON: `{"text":"rm -rf /"}`},
			callDone}},
		{events: []ai.Event{{Type: ai.EventStart}, {Type: ai.EventTextStart}, textDelta("done"), doneMsg("done")}},
	}, rec)

	if len(*results) != 1 {
		t.Fatalf("tool results = %d", len(*results))
	}
	rm := (*results)[0]
	if !strings.Contains(rm.Text(), "<system-reminder>") || !strings.Contains(rm.Text(), "no-rm") {
		t.Fatalf("tool result missing the folded reminder:\n%s", rm.Text())
	}
	if !strings.Contains(rm.Text(), "rm -rf /") {
		t.Fatalf("reminder replaced the tool output:\n%s", rm.Text())
	}
	if len(p.gotReqs) != 2 {
		t.Fatalf("provider calls = %d, want no abort (never interrupts)", len(p.gotReqs))
	}
	trig := rec.triggered()
	if len(trig) != 1 || trig[0]["rule"] != "no-rm" || trig[0]["interrupt"] != false || trig[0]["kind"] != "tool" {
		t.Fatalf("ttsr_triggered events = %+v", trig)
	}
	if final.Text() != "done" {
		t.Fatalf("final = %q", final.Text())
	}
}

func TestTTSRTriggeredEventOnInterrupt(t *testing.T) {
	rec := &recordingInterceptor{}
	p, _, _ := runScripted(t, config.TTSRRule{Name: "no-secret", Condition: "SECRET"}, []fakeScript{
		{events: []ai.Event{{Type: ai.EventStart}, {Type: ai.EventTextStart}, textDelta("SECRET"), doneMsg("SECRET")}},
		{events: []ai.Event{{Type: ai.EventStart}, {Type: ai.EventTextStart}, textDelta("ok"), doneMsg("ok")}},
	}, rec)
	if len(p.gotReqs) != 2 {
		t.Fatalf("provider calls = %d", len(p.gotReqs))
	}
	trig := rec.triggered()
	if len(trig) != 1 || trig[0]["rule"] != "no-secret" || trig[0]["interrupt"] != true || trig[0]["kind"] != "prose" {
		t.Fatalf("ttsr_triggered events = %+v", trig)
	}
	if trig[0]["mode"] != "always" || trig[0]["contextMode"] != "discard" {
		t.Fatalf("payload lost the resolved policy: %+v", trig[0])
	}
}

// TestTTSRRepeatGapEndToEnd drives two turns: the same rule matches in both,
// and only a gap that has elapsed lets the second match interrupt.
func TestTTSRRepeatGapEndToEnd(t *testing.T) {
	callDone := func() ai.Event {
		return ai.Event{Type: ai.EventDone, StopReason: ai.StopReasonStop, Message: &ai.Message{
			Role: ai.RoleAssistant,
			Content: []ai.Block{
				ai.TextBlock{Text: "working"},
				ai.ToolCallBlock{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{"text":"ok"}`), StreamIndex: 0},
			},
			StopReason: ai.StopReasonStop,
		}}
	}
	scripts := func() []fakeScript {
		return []fakeScript{
			// turn 1: interrupt
			{events: []ai.Event{{Type: ai.EventStart}, {Type: ai.EventTextStart}, textDelta("TODO list"), doneMsg("TODO list")}},
			// turn 1 retry: a tool call advances the run to turn 2
			{events: []ai.Event{{Type: ai.EventStart},
				{Type: ai.EventToolcallStart, ToolCallID: "c1", ToolName: "echo", StreamIndex: 0},
				{Type: ai.EventToolcallDelta, StreamIndex: 0, PartialJSON: `{"text":"ok"}`},
				{Type: ai.EventToolcallEnd, StreamIndex: 0, PartialJSON: `{"text":"ok"}`},
				callDone()}},
			// turn 2: matches again
			{events: []ai.Event{{Type: ai.EventStart}, {Type: ai.EventTextStart}, textDelta("TODO again"), doneMsg("TODO again")}},
			// turn 2 retry (only reached when the gap has elapsed)
			{events: []ai.Event{{Type: ai.EventStart}, {Type: ai.EventTextStart}, textDelta("done"), doneMsg("done")}},
		}
	}

	t.Run("gap 3 suppresses the second fire", func(t *testing.T) {
		rec := &recordingInterceptor{}
		p, _, final := runScripted(t, config.TTSRRule{Name: "no-todo", Condition: "TODO", RepeatGap: 3}, scripts(), rec)
		if len(p.gotReqs) != 3 {
			t.Fatalf("provider calls = %d, want 3 (no second abort)", len(p.gotReqs))
		}
		if n := len(rec.triggered()); n != 1 {
			t.Fatalf("ttsr_triggered = %d, want 1", n)
		}
		if final.Text() != "TODO again" {
			t.Fatalf("final = %q", final.Text())
		}
	})

	t.Run("gap 1 lets it fire again", func(t *testing.T) {
		rec := &recordingInterceptor{}
		p, _, final := runScripted(t, config.TTSRRule{Name: "no-todo", Condition: "TODO", RepeatGap: 1}, scripts(), rec)
		if len(p.gotReqs) != 4 {
			t.Fatalf("provider calls = %d, want 4 (second abort+retry)", len(p.gotReqs))
		}
		if n := len(rec.triggered()); n != 2 {
			t.Fatalf("ttsr_triggered = %d, want 2", n)
		}
		if final.Text() != "done" {
			t.Fatalf("final = %q", final.Text())
		}
	})
}

// TestTTSRSettingsLoad drives the YAML group through the real settings
// loader: a field-name drift or a bad mode must fail here, not silently
// leave the engine inert (print mode builds the engine from this shape).
func TestTTSRSettingsLoad(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir()) // isolate the global layer
	load := func(t *testing.T, yaml string) (*config.Settings, error) {
		t.Helper()
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".xdev"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".xdev", "config.yml"), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		return config.LoadSettings(dir, nil)
	}

	s, err := load(t, "ttsr:\n  contextMode: keep\n  repeatGap: 5\n  rules:\n    - name: no-secret\n      condition: SECRET\n      interruptMode: prose-only\n      message: never echo secrets\n")
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	tsr := NewTTSR(s.TTSR)
	if tsr == nil {
		t.Fatal("engine is nil after loading a rule from settings")
	}
	m := tsr.observe(context.Background(), ttsrProse, "a SECRET here", 0)
	if m == nil || m.Rule.Name != "no-secret" {
		t.Fatalf("match = %+v", m)
	}
	if got := tsr.contextMode(m.Rule); got != "keep" {
		t.Fatalf("contextMode = %q, group default lost", got)
	}
	if got := tsr.repeatGap(m.Rule); got != 5 {
		t.Fatalf("repeatGap = %d, group default lost", got)
	}

	if _, err := load(t, "ttsr:\n  rules:\n    - name: bad\n      condition: x\n      interruptMode: sometimes\n"); err == nil {
		t.Fatal("unknown interruptMode was accepted")
	}
	if _, err := load(t, "ttsr:\n  rules:\n    - condition: x\n"); err == nil {
		t.Fatal("unnamed rule was accepted")
	}
}
