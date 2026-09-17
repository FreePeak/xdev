package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// Cross-provider context handoff (M8 contract, Part V risk): when the run
// moves to a different wire adapter mid-session (failover, promotion, or
// prewalk into another provider's model), the history must arrive intact
// and the message provenance must follow the NEW target — a handoff that
// silently truncated, dropped tool results, or mislabeled the provider
// would corrupt every later turn.
func TestCrossProviderHandoffKeepsContextAndProvenance(t *testing.T) {
	// Turn 0 answers with a tool call, then the ladder drains: two
	// transient 5xx responses (MaxRetries=1) before the handoff fires.
	primary := &fakeProvider{calls: []fakeScript{
		{events: toolCallEvents("echo", `{"text":"pong"}`)},
		{err: &ai.HTTPError{API: "anthropic-messages", Status: 500, Body: "boom"}},
		{err: &ai.HTTPError{API: "anthropic-messages", Status: 500, Body: "boom"}},
	}}
	backup := &backupProvider{}
	reg := tool.NewRegistry()
	reg.Register(echoTool{})
	ag := &Agent{
		Provider: primary, Tools: reg, Model: "primary-model",
		Retry:     RetryPolicy{MaxRetries: 1, BaseDelay: time.Millisecond},
		Failovers: []FailoverTarget{{Provider: backup, Model: "backup1", ContextWindow: 1_000_000}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := ag.Run(ctx, "sys", []ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "ping"}}},
	}); err != nil && !strings.Contains(err.Error(), "script exhausted") {
		t.Fatal(err)
	}

	if len(backup.gotReqs) == 0 {
		t.Fatal("backup provider never received a request after the handoff")
	}
	req := backup.gotReqs[0]

	// 1. Provenance follows the new target.
	if req.Model != "backup1" {
		t.Fatalf("handoff request model = %q, want backup1", req.Model)
	}
	// 2. The full history arrives: the original user turn, the assistant's
	// tool call, and the tool result — in order, none dropped.
	if len(req.Messages) < 3 {
		t.Fatalf("handoff carried %d messages, want the full chain", len(req.Messages))
	}
	if req.Messages[0].Role != ai.RoleUser {
		t.Fatalf("first message role = %v", req.Messages[0].Role)
	}
	var sawToolCall, sawToolResult bool
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if tc, ok := b.(ai.ToolCallBlock); ok && tc.Name == "echo" {
				sawToolCall = true
			}
		}
		if m.Role == ai.RoleToolResult && m.ToolName == "echo" {
			sawToolResult = true
		}
	}
	if !sawToolCall || !sawToolResult {
		t.Fatalf("handoff lost context: toolCall=%v toolResult=%v", sawToolCall, sawToolResult)
	}
	// 3. Tool definitions survive too — a handoff that forgot the schema
	// would make the new provider answer without tools.
	if len(req.Tools) == 0 || req.Tools[0].Name != "echo" {
		t.Fatalf("handoff tools = %+v", req.Tools)
	}
	// 4. The system prompt is carried, not re-invented.
	if req.System == "" {
		t.Fatal("handoff dropped the system prompt")
	}
}

// backupProvider is a different "wire adapter" (distinct Name/API) so the
// test proves the swap is a real provider change, not a relabel.
type backupProvider struct {
	gotReqs []ai.StreamRequest
}

func (b *backupProvider) Name() string { return "backup-host" }
func (b *backupProvider) API() string  { return "openai-responses" }
func (b *backupProvider) Stream(_ context.Context, req ai.StreamRequest) (<-chan ai.Event, error) {
	b.gotReqs = append(b.gotReqs, req)
	ch := make(chan ai.Event, 4)
	go func() {
		defer close(ch)
		ch <- ai.Event{Type: ai.EventStart, Provider: b.Name(), API: b.API(), Model: req.Model}
		ch <- ai.Event{Type: ai.EventTextDelta, Delta: "handoff ok"}
		ch <- ai.Donef(ai.StopReasonStop, nil, &ai.Message{
			Role:       ai.RoleAssistant,
			Content:    []ai.Block{ai.TextBlock{Text: "handoff ok"}},
			StopReason: ai.StopReasonStop,
		})
	}()
	return ch, nil
}
